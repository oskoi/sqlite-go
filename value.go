package sqlite

/*
#include "bridge.h"
*/
import "C"

// ValueCopy owns a value. Only the field corresponding to Kind is populated.
type ValueCopy struct {
	Kind    Kind
	Int64   int64
	Float64 float64
	Text    string
	Blob    []byte
}

// Value is a copyable borrowed changeset value. It expires with its position.
// Refetched and copied Values share the position's original-kind cache.
type Value struct {
	position   *changePosition
	generation uint64
	vector     int
	column     int
	value      *C.sqlite3_value
}

func (v Value) check() error { return v.position.check(v.generation) }
func (v Value) read(mode C.int) (C.sg_column_result, error) {
	if err := v.check(); err != nil {
		return C.sg_column_result{}, err
	}
	slot := &v.position.kinds[v.vector*v.position.columns+v.column]
	if v.value == nil {
		slot.native, slot.generation = 0, v.generation
		if mode != 0 && mode != C.SG_READ_ORIGINAL {
			return C.sg_column_result{}, ErrState
		}
		return C.sg_column_result{}, nil
	}
	original := C.int(0)
	if slot.generation == v.generation {
		original = slot.native
		if original == C.SQLITE_NULL {
			return C.sg_column_result{kind: original}, nil
		}
	}
	r := C.sg_value(v.value, original, mode)
	slot.native, slot.generation = r.kind, v.generation
	if r.code != C.SQLITE_OK {
		return r, codeError(r.code, "Value")
	}
	return r, nil
}

// Kind returns the original kind, including Missing independently of NULL.
func (v Value) Kind() (Kind, error) {
	if err := v.check(); err != nil {
		return KindMissing, err
	}
	slot := &v.position.kinds[v.vector*v.position.columns+v.column]
	if slot.generation == v.generation {
		return kindFromNative(slot.native), nil
	}
	r, err := v.read(0)
	return kindFromNative(r.kind), err
}

// Int64 uses SQLite's native integer conversion. Missing values return ErrState.
func (v Value) Int64() (value int64, nonNull bool, err error) {
	r, err := v.read(1)
	if err != nil {
		return 0, false, err
	}
	return int64(r.integer), r.kind != C.SQLITE_NULL, nil
}

// Float64 uses SQLite's native real conversion.
func (v Value) Float64() (value float64, nonNull bool, err error) {
	r, err := v.read(2)
	if err != nil {
		return 0, false, err
	}
	return float64(r.real), r.kind != C.SQLITE_NULL, nil
}

// Text returns an owned text conversion; nonNull is false only for NULL.
func (v Value) Text() (value string, nonNull bool, err error) {
	r, err := v.read(3)
	if err != nil {
		return "", false, err
	}
	if r.kind == C.SQLITE_NULL {
		return "", false, nil
	}
	return C.GoStringN((*C.char)(r.data), r.length), true, nil
}

// Blob returns owned bytes. Nil is NULL; empty non-nil is a non-NULL value.
func (v Value) Blob() ([]byte, error) {
	r, err := v.read(4)
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

// Copy owns the original kind and its current native value. Earlier conversions
// may change representation; call Copy before coercion if that matters.
func (v Value) Copy() (ValueCopy, error) {
	r, err := v.read(C.SG_READ_ORIGINAL)
	if err != nil {
		return ValueCopy{}, err
	}
	return copyValue(r), nil
}

func copyValue(r C.sg_column_result) ValueCopy {
	out := ValueCopy{Kind: kindFromNative(r.kind)}
	switch out.Kind {
	case KindInteger:
		out.Int64 = int64(r.integer)
	case KindFloat:
		out.Float64 = float64(r.real)
	case KindText:
		out.Text = C.GoStringN((*C.char)(r.data), r.length)
	case KindBlob:
		if r.length == 0 {
			out.Blob = []byte{}
		} else {
			out.Blob = C.GoBytes(r.data, r.length)
		}
	}
	return out
}
