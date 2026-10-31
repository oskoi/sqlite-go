package sqlite

/*
#include "bridge.h"
*/
import "C"

import "unsafe"

// readColumn lazily captures the original storage class before conversion, in
// the same crossing as the requested value. No locks or registries are needed.
func (s *Stmt) readColumn(index int, mode C.int) (C.sg_column_result, error) {
	if err := s.column(index, true); err != nil {
		return C.sg_column_result{}, err
	}
	slot := &s.kinds[index]
	original := C.int(0)
	if slot.generation == s.generation {
		original = slot.native
		if original == C.SQLITE_NULL {
			return C.sg_column_result{kind: original}, nil
		}
	}
	r := C.sg_column(s.stmt, C.int(index), original, mode)
	slot.generation = s.generation
	slot.native = r.kind
	if r.code != 0 {
		return r, codeError(r.code, "Column")
	}
	return r, nil
}

// ColumnType returns the original storage class, even after conversion.
func (s *Stmt) ColumnType(index int) (Kind, error) {
	if err := s.column(index, true); err != nil {
		return KindMissing, err
	}
	slot := &s.kinds[index]
	if slot.generation == s.generation {
		return kindFromNative(slot.native), nil
	}
	r, err := s.readColumn(index, 0)
	if err != nil {
		return KindMissing, err
	}
	return kindFromNative(r.kind), nil
}

// ColumnValue owns the original kind and its current native value. The result
// survives Step, Reset, and Close. Earlier conversions may change representation;
// call ColumnValue before coercion if that matters.
func (s *Stmt) ColumnValue(index int) (ValueCopy, error) {
	r, err := s.readColumn(index, C.SG_READ_ORIGINAL)
	if err != nil {
		return ValueCopy{}, err
	}
	return copyValue(r), nil
}

// ColumnInt64 converts using SQLite rules and reports whether the value is non-NULL.
func (s *Stmt) ColumnInt64(index int) (int64, bool, error) {
	r, err := s.readColumn(index, 1)
	if err != nil {
		return 0, false, err
	}
	return int64(r.integer), r.kind != C.SQLITE_NULL, nil
}

// ColumnFloat64 converts using SQLite rules and reports whether the value is non-NULL.
func (s *Stmt) ColumnFloat64(index int) (float64, bool, error) {
	r, err := s.readColumn(index, 2)
	if err != nil {
		return 0, false, err
	}
	return float64(r.real), r.kind != C.SQLITE_NULL, nil
}

// ColumnText returns a Go-owned copy, distinguishing NULL from empty text.
func (s *Stmt) ColumnText(index int) (string, bool, error) {
	r, err := s.readColumn(index, 3)
	if err != nil {
		return "", false, err
	}
	if r.kind == C.SQLITE_NULL {
		return "", false, nil
	}
	return C.GoStringN((*C.char)(r.data), r.length), true, nil
}

// ColumnBlob returns a Go-owned copy. Nil is NULL; empty non-nil is not NULL.
func (s *Stmt) ColumnBlob(index int) ([]byte, error) {
	r, err := s.readColumn(index, 4)
	if err != nil {
		return nil, err
	}
	if r.kind == C.SQLITE_NULL {
		return nil, nil
	}
	if r.length == 0 {
		return []byte{}, nil
	}
	return C.GoBytes(r.data, r.length), nil
}

// ColumnTextView borrows read-only native memory. It expires at Step, Reset,
// Close, or conversion of this column's representation. Do not retain it.
func (s *Stmt) ColumnTextView(index int) (string, bool, error) {
	r, err := s.readColumn(index, 3)
	if err != nil {
		return "", false, err
	}
	if r.kind == C.SQLITE_NULL {
		return "", false, nil
	}
	return unsafe.String((*byte)(r.data), int(r.length)), true, nil
}

// ColumnBlobView borrows read-only native memory. Do not mutate or retain it
// after Step, Reset, Close, or conversion of this column's representation.
func (s *Stmt) ColumnBlobView(index int) ([]byte, error) {
	r, err := s.readColumn(index, 4)
	if err != nil {
		return nil, err
	}
	if r.kind == C.SQLITE_NULL {
		return nil, nil
	}
	if r.length == 0 {
		return []byte{}, nil
	}
	return unsafe.Slice((*byte)(r.data), int(r.length)), nil
}
