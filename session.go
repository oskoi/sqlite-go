package sqlite

/*
#include "bridge.h"
#include <stdlib.h>
*/
import "C"

import (
	"io"
	"runtime/cgo"
	"unsafe"
)

// Session records native changes for one schema. Close it before its connection.
// Like its parent, a Session has one simultaneous owner and must not be copied.
type Session struct {
	_       noCopy
	session *C.sqlite3_session
	conn    *Conn
	next    *Session
	filter  *C.sg_callback
}
type tableFilter struct {
	panic *callPanic
	fn    func(string) bool
}

// NewSession starts a native session for schema, without starting a transaction.
func (c *Conn) NewSession(schema string) (*Session, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	if err := validCString(schema); err != nil {
		return nil, err
	}
	z := C.CString(schema)
	defer C.free(unsafe.Pointer(z))
	c.active = true
	defer c.endCall()
	var ptr *C.sqlite3_session
	rc := C.sqlite3session_create(c.db, z, &ptr)
	if rc != C.SQLITE_OK {
		return nil, codeError(rc, "NewSession")
	}
	s := &Session{session: ptr, conn: c, next: c.sessions}
	c.sessions = s
	c.children++
	return s, nil
}
func (s *Session) check() error {
	if s == nil || s.session == nil {
		return ErrClosed
	}
	return s.conn.check()
}

// Close deletes the native session before releasing its filter and parent.
func (s *Session) Close() error {
	if s == nil || s.session == nil {
		return nil
	}
	if err := s.check(); err != nil {
		return err
	}
	c := s.conn
	c.active = true
	defer c.endCall()
	C.sqlite3session_delete(s.session)
	s.session = nil
	freeCallback(s.filter)
	s.filter = nil
	for link := &c.sessions; *link != nil; link = &(*link).next {
		if *link == s {
			*link = s.next
			break
		}
	}
	s.next = nil
	s.conn = nil
	c.children--
	return nil
}
func (c *Conn) clearSessionFilters() {
	for s := c.sessions; s != nil; s = s.next {
		if s.filter != nil {
			C.sg_session_filter(s.session, nil)
			freeCallback(s.filter)
			s.filter = nil
		}
	}
}

// Attach records changes to table. Empty names are invalid; use AttachAll.
func (s *Session) Attach(table string) error {
	if err := s.check(); err != nil {
		return err
	}
	if table == "" {
		return ErrInvalidArgument
	}
	if err := validCString(table); err != nil {
		return err
	}
	z := C.CString(table)
	defer C.free(unsafe.Pointer(z))
	return s.attach(z)
}

// AttachAll attaches existing and future tables according to SQLite's rules.
func (s *Session) AttachAll() error {
	if err := s.check(); err != nil {
		return err
	}
	return s.attach(nil)
}
func (s *Session) attach(table *C.char) error {
	s.conn.active = true
	defer s.conn.endCall()
	return codeError(C.sqlite3session_attach(s.session, table), "Session.Attach")
}

// SetTableFilter installs a filter for automatically attached tables; nil removes it.
func (s *Session) SetTableFilter(f func(table string) bool) error {
	if err := s.check(); err != nil {
		return err
	}
	var next *C.sg_callback
	if f != nil {
		var err error
		next, err = newContext(&tableFilter{panic: &callPanic{conn: s.conn}, fn: f})
		if err != nil {
			return err
		}
	}
	C.sg_session_filter(s.session, next)
	old := s.filter
	s.filter = next
	freeCallback(old)
	return nil
}

// SetEnabled enables or disables recording, retaining already recorded changes.
func (s *Session) SetEnabled(enabled bool) error {
	if err := s.check(); err != nil {
		return err
	}
	C.sqlite3session_enable(s.session, C.int(boolInt(enabled)))
	return nil
}

// Enabled reports whether recording is enabled.
func (s *Session) Enabled() (bool, error) {
	if err := s.check(); err != nil {
		return false, err
	}
	return C.sqlite3session_enable(s.session, -1) != 0, nil
}

// SetIndirect marks subsequent changes as indirect or direct.
func (s *Session) SetIndirect(indirect bool) error {
	if err := s.check(); err != nil {
		return err
	}
	C.sqlite3session_indirect(s.session, C.int(boolInt(indirect)))
	return nil
}

// Indirect reports the session's indirect-recording flag.
func (s *Session) Indirect() (bool, error) {
	if err := s.check(); err != nil {
		return false, err
	}
	return C.sqlite3session_indirect(s.session, -1) != 0, nil
}

// Empty reports whether SQLite has recorded changes (not whether output is empty).
func (s *Session) Empty() (bool, error) {
	if err := s.check(); err != nil {
		return false, err
	}
	return C.sqlite3session_isempty(s.session) != 0, nil
}

// Diff records changes from the compatible table in fromSchema to this schema.
func (s *Session) Diff(fromSchema, table string) error {
	if err := s.check(); err != nil {
		return err
	}
	if err := validCString(fromSchema); err != nil {
		return err
	}
	if err := validCString(table); err != nil {
		return err
	}
	a, b := C.CString(fromSchema), C.CString(table)
	defer C.free(unsafe.Pointer(a))
	defer C.free(unsafe.Pointer(b))
	s.conn.active = true
	defer s.conn.endCall()
	var message *C.char
	rc := C.sqlite3session_diff(s.session, a, b, &message)
	if message != nil {
		defer C.sqlite3_free(unsafe.Pointer(message))
	}
	if rc != C.SQLITE_OK {
		if message != nil {
			return &Error{Code: Code(rc), Message: C.GoString(message), Offset: -1, Op: "Session.Diff"}
		}
		return codeError(rc, "Session.Diff")
	}
	return nil
}

// WriteChangeset streams recorded changes without resetting the session.
// On error, the Writer may contain an unusable prefix.
func (s *Session) WriteChangeset(w io.Writer) error { return s.write(w, false) }

// WritePatchset streams SQLite's compact patchset format without resetting.
// Patchsets omit old non-key values and cannot detect DATA conflicts.
func (s *Session) WritePatchset(w io.Writer) error { return s.write(w, true) }
func (s *Session) write(w io.Writer, patch bool) error {
	if err := s.check(); err != nil {
		return err
	}
	op := &struct {
		panic callPanic
		out   stream
	}{panic: callPanic{conn: s.conn}}
	out := &op.out
	if err := out.initOutput(w, &op.panic); err != nil {
		return err
	}
	defer out.close()
	s.conn.active = true
	defer s.conn.endCall()
	rc := C.sg_session_output(s.session, C.uintptr_t(out.handle), C.int(boolInt(patch)))
	return transportResult(out.failure(), rc, "Session.Write")
}

//export sg_go_filter
func sg_go_filter(token C.uintptr_t, table *C.char) (include C.int) {
	value := cgo.Handle(token).Value()
	var p *callPanic
	var fn func(string) bool
	switch cb := value.(type) {
	case *tableFilter:
		p, fn = cb.panic, cb.fn
	case *applyState:
		p, fn = &cb.panic, cb.options.Filter
	}
	returned := false
	defer p.capture(&returned)
	if !p.failed() && (fn == nil || fn(C.GoString(table))) {
		include = 1
	}
	returned = true
	return include
}
