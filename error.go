package sqlite

/*
#include "bridge.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"unsafe"
)

// Code is a primary or extended SQLite result code.
type Code int32

// Primary returns the low-byte result family.
func (c Code) Primary() Code { return c & 255 }

// Error returns SQLite's static description of this result code.
func (c Code) Error() string { return C.GoString(C.sqlite3_errstr(C.int(c))) }

// Error owns a snapshot of a native failure. Offset is a SQL byte offset or -1.
type Error struct {
	Code    Code
	Message string
	Offset  int
	Op      string
}

// Error formats the operation, owned message, code and available SQL offset.
func (e *Error) Error() string {
	if e == nil {
		return "<nil>"
	}
	if e.Offset >= 0 {
		return fmt.Sprintf("sqlite %s: %s (code %d, offset %d)", e.Op, e.Message, e.Code, e.Offset)
	}
	return fmt.Sprintf("sqlite %s: %s (code %d)", e.Op, e.Message, e.Code)
}

// Is matches a Code using its primary-family/exact-extended semantics.
func (e *Error) Is(target error) bool {
	t, ok := target.(Code)
	return e != nil && ok && (e.Code == t || t == t.Primary() && e.Code.Primary() == t)
}

// Wrapper misuse and lifetime errors remain distinct from native result codes.
var (
	ErrClosed          = errors.New("sqlite: closed handle")
	ErrState           = errors.New("sqlite: invalid handle state")
	ErrRange           = errors.New("sqlite: out of range")
	ErrInvalidArgument = errors.New("sqlite: invalid argument")
	ErrExpired         = errors.New("sqlite: expired view")
	ErrReentrant       = errors.New("sqlite: reentrant operation")
	ErrBlobFull        = errors.New("sqlite: blob is full")
	// ErrMultiwriterUnavailable means this build lacks sqlite_multiwriter support.
	ErrMultiwriterUnavailable = errors.New("sqlite: multiwriter unavailable")
)

// Kind is the original SQLite storage class; Missing is reserved for changesets.
type Kind uint8

const (
	KindMissing Kind = iota
	KindNull
	KindInteger
	KindFloat
	KindText
	KindBlob
)

func kindFromNative(k C.int) Kind {
	switch k {
	case C.SQLITE_NULL:
		return KindNull
	case C.SQLITE_INTEGER:
		return KindInteger
	case C.SQLITE_FLOAT:
		return KindFloat
	case C.SQLITE_TEXT:
		return KindText
	case C.SQLITE_BLOB:
		return KindBlob
	}
	return KindMissing
}
func takeError(e C.sg_error, op string) error {
	if e.code == 0 {
		return nil
	}
	message := C.GoString(e.message)
	if e.message != nil {
		C.sqlite3_free(unsafe.Pointer(e.message))
	} else {
		message = Code(e.code).Error()
	}
	return &Error{Code: Code(e.code), Message: message, Offset: int(e.offset), Op: op}
}

// codeError reports a result without consulting connection diagnostics.
func codeError(rc C.int, op string) error {
	if rc == C.SQLITE_OK {
		return nil
	}
	return &Error{Code: Code(rc), Message: Code(rc).Error(), Offset: -1, Op: op}
}

// dbError requires a non-nil db whose failed operation refreshed its message.
// SQL offsets are captured separately by the SQL-aware native helpers.
func dbError(db *C.sqlite3, rc C.int, op string) error {
	if rc == C.SQLITE_OK {
		return nil
	}
	return takeError(C.sg_error_db(db, rc), op)
}
