package sqlite

/*
#include "bridge.h"
*/
import "C"

import (
	"errors"
	"io"
	"runtime/cgo"
	"unsafe"
)

// callPanic keeps callbacks suppressed until native cleanup has completed.
// Connection operations share the connection's existing panic slot.
type callPanic struct {
	conn     *Conn
	panicked bool
	value    any
}

func (p *callPanic) failed() bool {
	if p.conn != nil {
		return p.conn.panicked
	}
	return p.panicked
}
func (p *callPanic) capture(returned *bool) {
	value := recover()
	if !*returned && !p.failed() {
		if p.conn != nil {
			p.conn.recordPanic(value)
		} else {
			p.panicked = true
			p.value = value
		}
	}
}
func (p *callPanic) finish() {
	if p.panicked {
		value := p.value
		p.value = nil
		p.panicked = false
		panic(value)
	}
}

func newContext(value any) (*C.sg_callback, error) {
	h := cgo.NewHandle(value)
	ctx := C.sg_callback_new(C.uintptr_t(h))
	if ctx == nil {
		h.Delete()
		return nil, codeError(C.SQLITE_NOMEM, "Stream.Context")
	}
	return ctx, nil
}

// A typed limit prevents a future increase from overflowing the compact counter.
const maxEmptyReads uint8 = 100

type stream struct {
	reader  io.Reader
	writer  io.Writer
	panic   *callPanic
	strict  bool
	empty   uint8
	err     error
	pending error
	written int64
	handle  cgo.Handle
	ctx     *C.sg_callback // Only iterator input retains a C-owned context.
}

func inputStream(r io.Reader, p *callPanic, strict bool) (*stream, error) {
	s := &stream{}
	if err := s.initInput(r, p, strict); err != nil {
		return nil, err
	}
	return s, nil
}
func outputStream(w io.Writer, p *callPanic) (*stream, error) {
	s := &stream{}
	if err := s.initOutput(w, p); err != nil {
		return nil, err
	}
	return s, nil
}

// Initialize streams only at their final addresses: each handle retains s.
func (s *stream) initInput(r io.Reader, p *callPanic, strict bool) error {
	if r == nil {
		return ErrInvalidArgument
	}
	s.reader, s.panic, s.strict = r, p, strict
	s.handle = cgo.NewHandle(s)
	return nil
}
func (s *stream) initOutput(w io.Writer, p *callPanic) error {
	if w == nil {
		return ErrInvalidArgument
	}
	s.writer, s.panic = w, p
	s.handle = cgo.NewHandle(s)
	return nil
}
func (s *stream) close() {
	if s.ctx != nil {
		C.sqlite3_free(unsafe.Pointer(s.ctx))
		s.ctx = nil
	}
	if s.handle != 0 {
		s.handle.Delete()
		s.handle = 0
	}
	// An expired borrowed Value may retain a coallocated operation owner.
	s.reader, s.writer = nil, nil
}
func (s *stream) failure() error {
	if s.err != nil {
		return s.err
	}
	if s.pending != nil && s.pending != io.EOF {
		return s.pending
	}
	return nil
}
func streamResult(rc C.int, op string, streams ...*stream) error {
	var err error
	for _, s := range streams {
		if e := s.failure(); e != nil && !errors.Is(err, e) {
			err = errors.Join(err, e)
		}
	}
	return transportResult(err, rc, op)
}

// transportResult uses only the returned code: standalone transports have no
// connection diagnostics, and session output may replace them during cleanup.
func transportResult(err error, rc C.int, op string) error {
	if rc == C.SQLITE_OK || rc == C.SQLITE_DONE || rc == C.SQLITE_ROW {
		return err
	}
	if err != nil && (Code(rc).Primary() == CodeIOErr || Code(rc).Primary() == CodeAbort) {
		return err
	}
	native := codeError(rc, op)
	if err == nil {
		return native
	}
	return errors.Join(err, native)
}

//export sg_go_input
func sg_go_input(token C.uintptr_t, data unsafe.Pointer, count *C.int) (rc C.int) {
	rc = C.SQLITE_IOERR
	s := cgo.Handle(token).Value().(*stream)
	returned := false
	defer s.panic.capture(&returned)
	capacity := int(*count)
	*count = 0
	if s.panic.failed() || s.err != nil {
		returned = true
		return rc
	}
	if s.pending != nil {
		if s.pending == io.EOF {
			returned = true
			return C.SQLITE_OK
		}
		s.err = s.pending
		returned = true
		return rc
	}
	if capacity < 0 {
		s.err = ErrInvalidArgument
		returned = true
		return rc
	}
	buffer := unsafe.Slice((*byte)(data), capacity)
	for {
		n, err := s.reader.Read(buffer)
		if n < 0 || n > capacity {
			s.err = ErrInvalidArgument
			returned = true
			return rc
		}
		// Only exact EOF is clean termination. Wrapped/joined EOF errors
		// carry failure information and must reach the caller unchanged.
		if err != nil && err != io.EOF && (s.strict || n == 0) {
			s.err = err
			returned = true
			return rc
		}
		if n > 0 {
			s.empty = 0
			s.pending = err
			*count = C.int(n)
			returned = true
			return C.SQLITE_OK
		}
		if err != nil {
			s.pending = err
			returned = true
			return C.SQLITE_OK
		}
		s.empty++
		if s.empty >= maxEmptyReads {
			s.err = io.ErrNoProgress
			returned = true
			return rc
		}
	}
}

//export sg_go_output
func sg_go_output(token C.uintptr_t, data unsafe.Pointer, count C.int) (rc C.int) {
	rc = C.SQLITE_IOERR
	s := cgo.Handle(token).Value().(*stream)
	returned := false
	defer s.panic.capture(&returned)
	if s.panic.failed() || s.err != nil {
		returned = true
		return rc
	}
	if count < 0 {
		s.err = ErrInvalidArgument
		returned = true
		return rc
	}
	buffer := unsafe.Slice((*byte)(data), int(count))
	n, err := s.writer.Write(buffer)
	if n < 0 || n > len(buffer) {
		s.err = ErrInvalidArgument
		returned = true
		return rc
	}
	s.written += int64(n)
	if err == nil && n != len(buffer) {
		err = io.ErrShortWrite
	}
	s.err = err
	returned = true
	if err == nil {
		return C.SQLITE_OK
	}
	return rc
}
