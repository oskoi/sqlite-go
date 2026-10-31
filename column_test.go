// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"errors"
	"math"
	"reflect"
	"testing"
)

func TestColumnBlobAfterUnrelatedNoMem(t *testing.T) {
	for _, getter := range []struct {
		name string
		read func(*Stmt, int) ([]byte, error)
	}{
		{"copy", (*Stmt).ColumnBlob},
		{"view", (*Stmt).ColumnBlobView},
		{"value", func(s *Stmt, index int) ([]byte, error) {
			value, err := s.ColumnValue(index)
			return value.Blob, err
		}},
	} {
		t.Run(getter.name, func(t *testing.T) {
			c := openTest(t, ":memory:")
			s := prepareTest(t, c, "SELECT x''")
			rowTest(t, s)
			value, err := getter.read(s, 0)
			if err != nil || value == nil || len(value) != 0 {
				t.Fatalf("initial empty blob: value=%v err=%v", value, err)
			}
			limit, err := c.Limit(LimitVDBEOp, 1)
			if err != nil {
				t.Fatal(err)
			}
			other, _, err := c.Prepare("SELECT 1", 0)
			if other != nil {
				other.Close()
				t.Fatal("opcode-limited preparation unexpectedly succeeded")
			}
			if !errors.Is(err, CodeNoMem) {
				t.Fatalf("opcode-limited preparation: %v", err)
			}
			if _, err := c.Limit(LimitVDBEOp, limit); err != nil {
				t.Fatal(err)
			}
			value, err = getter.read(s, 0)
			if err != nil || value == nil || len(value) != 0 {
				t.Fatalf("empty blob after unrelated NOMEM: value=%v err=%v", value, err)
			}
		})
	}
}

func TestColumnValueOriginalKindAndOwnership(t *testing.T) {
	c := openTest(t, ":memory:")
	s := prepareTest(t, c, `SELECT NULL,'',x'',-9223372036854775808,9223372036854775807,2.5,'a'||char(0)||'b',x'0062ff','42'
		UNION ALL SELECT 1,2,3,4,5,6,7,8,9`)
	for _, index := range []int{-1, 0, math.MaxInt} {
		got, err := s.ColumnValue(index)
		if !errors.Is(err, ErrState) || !reflect.DeepEqual(got, ValueCopy{}) {
			t.Fatalf("before row column %d: value=%#v err=%v", index, got, err)
		}
	}
	rowTest(t, s)
	want := []ValueCopy{
		{Kind: KindNull},
		{Kind: KindText},
		{Kind: KindBlob, Blob: []byte{}},
		{Kind: KindInteger, Int64: math.MinInt64},
		{Kind: KindInteger, Int64: math.MaxInt64},
		{Kind: KindFloat, Float64: 2.5},
		{Kind: KindText, Text: "a\x00b"},
		{Kind: KindBlob, Blob: []byte{0, 'b', 255}},
		{Kind: KindText, Text: "42"},
	}
	owned := make([]ValueCopy, len(want))
	for i, expected := range want {
		got, err := s.ColumnValue(i)
		if err != nil || !reflect.DeepEqual(got, expected) {
			t.Fatalf("column %d: value=%#v want=%#v err=%v", i, got, expected, err)
		}
		owned[i] = got
		if _, _, err := s.ColumnText(i); err != nil {
			t.Fatal(err)
		}
		if _, err := s.ColumnBlob(i); err != nil {
			t.Fatal(err)
		}
		if _, _, err := s.ColumnInt64(i); err != nil {
			t.Fatal(err)
		}
		got, err = s.ColumnValue(i)
		if err != nil || !reflect.DeepEqual(got, expected) {
			t.Fatalf("coerced column %d: value=%#v want=%#v err=%v", i, got, expected, err)
		}
		if kind, err := s.ColumnType(i); err != nil || kind != expected.Kind {
			t.Fatalf("coerced column %d: kind=%v err=%v", i, kind, err)
		}
	}
	rowTest(t, s)
	for i := range want {
		got, err := s.ColumnValue(i)
		expected := ValueCopy{Kind: KindInteger, Int64: int64(i + 1)}
		if err != nil || !reflect.DeepEqual(got, expected) {
			t.Fatalf("next row column %d: value=%#v want=%#v err=%v", i, got, expected, err)
		}
	}
	if row, err := s.Step(); err != nil || row {
		t.Fatal(row, err)
	}
	if got, err := s.ColumnValue(0); !errors.Is(err, ErrState) || !reflect.DeepEqual(got, ValueCopy{}) {
		t.Fatalf("after done: value=%#v err=%v", got, err)
	}
	if err := s.Reset(); err != nil {
		t.Fatal(err)
	}
	if got, err := s.ColumnValue(0); !errors.Is(err, ErrState) || !reflect.DeepEqual(got, ValueCopy{}) {
		t.Fatalf("after reset: value=%#v err=%v", got, err)
	}
	rowTest(t, s)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(owned, want) {
		t.Fatalf("owned values changed after step/reset/close: %#v", owned)
	}
	for _, closed := range []*Stmt{s, nil} {
		if got, err := closed.ColumnValue(0); !errors.Is(err, ErrClosed) || !reflect.DeepEqual(got, ValueCopy{}) {
			t.Fatalf("closed statement: value=%#v err=%v", got, err)
		}
	}
}
