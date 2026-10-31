package sqlite

/*
#cgo noescape sg_bind_text
#cgo nocallback sg_bind_text
#cgo noescape sg_bind_blob
#cgo nocallback sg_bind_blob
#cgo noescape sqlite3_bind_parameter_index
#cgo nocallback sqlite3_bind_parameter_index
// Audited: bind helpers copy using SQLITE_TRANSIENT; parameter lookup only
// reads metadata. These transitive native calls retain no Go pointer and
// cannot invoke Go callbacks. Execution/finalization helpers are not annotated.
#include "bridge.h"
*/
import "C"

import (
	"errors"
	"runtime"
	"unsafe"
)

type stmtState uint8

const (
	stmtReady stmtState = iota
	stmtRow
	stmtDone
	stmtFailed
)

type columnKind struct {
	generation uint64
	native     C.int
}

// Result contains SQLite's connection counters after successful Exec.
type Result struct {
	Changes         int64
	LastInsertRowID int64
}

// Stmt owns a prepared statement and retains its parent. Do not copy it.
type Stmt struct {
	_           noCopy
	stmt        *C.sqlite3_stmt
	conn        *Conn
	state       stmtState
	columns     int
	parameters  int
	generation  uint64
	kinds       []columnKind
	nameScratch []byte
}

func (s *Stmt) invalidate() {
	s.generation++
	if s.generation == 0 {
		clear(s.kinds)
		s.generation = 1
	}
}

// Close consumes the statement even if finalization reports a prior failure.
func (s *Stmt) Close() error {
	if s == nil || s.stmt == nil {
		return nil
	}
	if err := s.check(); err != nil {
		return err
	}
	c := s.conn
	c.active = true
	defer c.endCall()
	e := C.sg_finalize(s.stmt)
	s.stmt = nil
	s.conn = nil
	s.kinds = nil
	s.nameScratch = nil
	s.invalidate()
	c.children--
	return takeError(e, "Stmt.Close")
}

// Reset makes the statement Ready, retaining bindings, even when it reports
// the previous execution's error.
func (s *Stmt) Reset() error {
	if err := s.check(); err != nil {
		return err
	}
	s.conn.active = true
	defer s.conn.endCall()
	e := C.sg_reset(s.stmt)
	s.state = stmtReady
	s.invalidate()
	return takeError(e, "Reset")
}

// ClearBindings sets parameters to NULL in Ready or Done without resetting.
func (s *Stmt) ClearBindings() error {
	if err := s.check(); err != nil {
		return err
	}
	if s.state != stmtReady && s.state != stmtDone {
		return ErrState
	}
	return codeError(C.sqlite3_clear_bindings(s.stmt), "ClearBindings")
}

// ResetClearBindings resets the statement and then sets all parameters to NULL.
// The two results describe Reset and ClearBindings respectively; a failed owner
// check returns only the first error. A reset error does not prevent clearing,
// but a callback panic propagates before bindings are cleared.
func (s *Stmt) ResetClearBindings() (resetErr, clearErr error) {
	if err := s.check(); err != nil {
		return err, nil
	}
	c := s.conn
	if c.sessions != nil || c.callbacks != ([callbackCount]*C.sg_callback{}) {
		return s.Reset(), s.ClearBindings()
	}
	c.active = true
	defer c.endCall()
	r := C.sg_reset_clear(s.stmt)
	s.state = stmtReady
	s.invalidate()
	return codeError(r.reset_code, "Reset"), codeError(r.clear_code, "ClearBindings")
}

// Step advances once. DONE is sticky until Reset; errors also require Reset.
func (s *Stmt) Step() (bool, error) {
	if err := s.check(); err != nil {
		return false, err
	}
	if s.state == stmtDone {
		return false, nil
	}
	if s.state == stmtFailed {
		return false, ErrState
	}
	s.invalidate()
	s.conn.active = true
	defer s.conn.endCall()
	r := C.sg_step(s.stmt)
	s.columns = int(r.columns)
	if s.columns > len(s.kinds) {
		s.kinds = append(s.kinds, make([]columnKind, s.columns-len(s.kinds))...)
	}
	switch r.code {
	case C.SQLITE_ROW:
		s.state = stmtRow
		return true, nil
	case C.SQLITE_DONE:
		s.state = stmtDone
		return false, nil
	default:
		s.state = stmtFailed
		return false, takeError(r.error, "Step")
	}
}

// Exec drains a Ready statement and always resets it, preserving bindings.
func (s *Stmt) Exec() (Result, error) {
	if err := s.ready(); err != nil {
		return Result{}, err
	}
	s.invalidate()
	s.conn.active = true
	defer s.conn.endCall()
	r := C.sg_exec(s.stmt)
	s.state = stmtReady
	s.columns = int(r.columns)
	if s.columns > len(s.kinds) {
		s.kinds = append(s.kinds, make([]columnKind, s.columns-len(s.kinds))...)
	}
	first, second := takeError(r.error, "Exec"), takeError(r.reset_error, "Exec.Reset")
	if first != nil || second != nil {
		if first == nil {
			return Result{}, second
		}
		if second == nil {
			return Result{}, first
		}
		return Result{}, errors.Join(first, second)
	}
	return Result{Changes: int64(r.changes), LastInsertRowID: int64(r.rowid)}, nil
}

// ParameterCount returns the highest SQLite parameter index, including gaps.
func (s *Stmt) ParameterCount() (int, error) {
	if err := s.check(); err != nil {
		return 0, err
	}
	return s.parameters, nil
}

// ParameterName copies the exact sigil-prefixed name, or empty for a gap/anonymous slot.
func (s *Stmt) ParameterName(index int) (string, error) {
	if err := s.check(); err != nil {
		return "", err
	}
	if index < 1 || index > s.parameters {
		return "", ErrRange
	}
	return C.GoString(C.sqlite3_bind_parameter_name(s.stmt, C.int(index))), nil
}

// ParameterIndex looks up an exact name; unknown names return zero.
func (s *Stmt) ParameterIndex(name string) (int, error) {
	if err := s.check(); err != nil {
		return 0, err
	}
	if err := validCString(name); err != nil {
		return 0, err
	}
	// SQLite's lookup requires a C string. Reuse a dynamically sized buffer;
	// no fixed name limit, C allocation, or secondary parameter scan is needed.
	if cap(s.nameScratch) < len(name)+1 {
		s.nameScratch = make([]byte, len(name)+1)
	}
	z := s.nameScratch[:len(name)+1]
	copy(z, name)
	z[len(name)] = 0
	index := C.sqlite3_bind_parameter_index(s.stmt, (*C.char)(unsafe.Pointer(unsafe.SliceData(z))))
	runtime.KeepAlive(z)
	return int(index), nil
}

// ColumnCount returns current width, refreshed during Step after reprepare.
func (s *Stmt) ColumnCount() (int, error) {
	if err := s.check(); err != nil {
		return 0, err
	}
	return s.columns, nil
}

// ColumnName copies current native metadata; no lifetime metadata cache is used.
func (s *Stmt) ColumnName(index int) (string, error) {
	if err := s.column(index, false); err != nil {
		return "", err
	}
	p := C.sqlite3_column_name(s.stmt, C.int(index))
	if p == nil {
		return "", codeError(C.SQLITE_NOMEM, "ColumnName")
	}
	return C.GoString(p), nil
}

// ColumnDeclType copies a declared type, empty when SQLite supplies none.
func (s *Stmt) ColumnDeclType(index int) (string, error) {
	if err := s.column(index, false); err != nil {
		return "", err
	}
	return C.GoString(C.sqlite3_column_decltype(s.stmt, C.int(index))), nil
}

// ReadOnly reports SQLite's classification, not whether execution has effects outside the database.
func (s *Stmt) ReadOnly() (bool, error) {
	if err := s.check(); err != nil {
		return false, err
	}
	return C.sqlite3_stmt_readonly(s.stmt) != 0, nil
}

// Status reads a native statement counter and optionally resets it.
func (s *Stmt) Status(op StmtStatus, reset bool) (int, error) {
	if err := s.check(); err != nil {
		return 0, err
	}
	if (op < StmtStatusFullScanStep || op > StmtStatusFilterHit) && op != StmtStatusMemUsed {
		return 0, ErrRange
	}
	return int(C.sqlite3_stmt_status(s.stmt, C.int(op), C.int(boolInt(reset)))), nil
}

// BindNull binds SQL NULL to a one-based parameter in Ready.
func (s *Stmt) BindNull(index int) error {
	if err := s.parameter(index); err != nil {
		return err
	}
	return dbError(s.conn.db, C.sqlite3_bind_null(s.stmt, C.int(index)), "BindNull")
}

// BindInt64 binds an integer in Ready.
func (s *Stmt) BindInt64(index int, value int64) error {
	if err := s.parameter(index); err != nil {
		return err
	}
	return dbError(s.conn.db, C.sqlite3_bind_int64(s.stmt, C.int(index), C.sqlite3_int64(value)), "BindInt64")
}

// BindFloat64 binds a real in Ready using native SQLite conversion semantics.
func (s *Stmt) BindFloat64(index int, value float64) error {
	if err := s.parameter(index); err != nil {
		return err
	}
	return dbError(s.conn.db, C.sqlite3_bind_double(s.stmt, C.int(index), C.double(value)), "BindFloat64")
}

// BindText copies text, including embedded NUL. Empty input remains TEXT.
func (s *Stmt) BindText(index int, value string) error {
	if err := s.parameter(index); err != nil {
		return err
	}
	rc := C.sg_bind_text(s.stmt, C.int(index), (*C.char)(unsafe.Pointer(unsafe.StringData(value))), C.sqlite3_uint64(len(value)))
	runtime.KeepAlive(value)
	return dbError(s.conn.db, rc, "BindText")
}

// BindBlob copies bytes; nil is NULL and a non-nil empty slice is an empty BLOB.
func (s *Stmt) BindBlob(index int, value []byte) error {
	if err := s.parameter(index); err != nil {
		return err
	}
	if value == nil {
		return s.BindNull(index)
	}
	rc := C.sg_bind_blob(s.stmt, C.int(index), unsafe.Pointer(unsafe.SliceData(value)), C.sqlite3_uint64(len(value)))
	runtime.KeepAlive(value)
	return dbError(s.conn.db, rc, "BindBlob")
}

// BindZeroBlob binds a native zero-filled blob without allocating a Go payload.
func (s *Stmt) BindZeroBlob(index int, size int64) error {
	if err := s.parameter(index); err != nil {
		return err
	}
	if size < 0 {
		return ErrRange
	}
	// The 64-bit API can return TOOBIG before refreshing db diagnostics.
	return codeError(C.sqlite3_bind_zeroblob64(s.stmt, C.int(index), C.sqlite3_uint64(size)), "BindZeroBlob")
}
