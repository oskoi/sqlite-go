// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"bytes"
	"errors"
	"io"
	"math"
	"testing"
)

func assertBlobContractIO(t testing.TB, operation string, n int, err error, wantN int, wantErr error) {
	t.Helper()
	if n != wantN || !errors.Is(err, wantErr) {
		t.Fatalf("%s = (%d, %v), want (%d, %v)", operation, n, err, wantN, wantErr)
	}
}

func assertBlobContractNative(t testing.TB, err error, code Code) {
	t.Helper()
	var native *Error
	if !errors.Is(err, code) || !errors.As(err, &native) || native.Code != code || native.Offset != -1 {
		t.Fatalf("native error = %#v, want code %v and offset -1", err, code)
	}
}

func assertBlobContractUnavailable(t testing.TB, b *Blob, want error, reopenRow int64) {
	t.Helper()
	operations := []struct {
		name string
		call func([]byte) (int, error)
	}{
		{"Read", b.Read},
		{"Write", b.Write},
		{"ReadAt", func(p []byte) (int, error) { return b.ReadAt(p, 0) }},
		{"WriteAt", func(p []byte) (int, error) { return b.WriteAt(p, 0) }},
	}
	for _, operation := range operations {
		for _, p := range [][]byte{nil, {}, {0x56}} {
			n, err := operation.call(p)
			assertBlobContractIO(t, operation.name, n, err, 0, want)
			if want == CodeAbort {
				assertBlobContractNative(t, err, CodeAbort)
			}
		}
	}
	position, err := b.Seek(0, io.SeekStart)
	if position != 0 || !errors.Is(err, want) {
		t.Fatalf("unavailable Seek = (%d, %v), want (0, %v)", position, err, want)
	}
	if want == CodeAbort {
		assertBlobContractNative(t, err, CodeAbort)
	}
	err = b.Reopen(reopenRow)
	if !errors.Is(err, want) {
		t.Fatalf("unavailable Reopen = %v, want %v", err, want)
	}
	if want == CodeAbort {
		assertBlobContractNative(t, err, CodeAbort)
	}
}

func assertBlobContractClosed(t testing.TB, b *Blob) {
	t.Helper()
	assertBlobContractUnavailable(t, b, ErrClosed, 0)
	if size, err := b.Size(); size != 0 || !errors.Is(err, ErrClosed) {
		t.Fatalf("closed Size = (%d, %v), want (0, ErrClosed)", size, err)
	}
	if row, err := b.RowID(); row != 0 || !errors.Is(err, ErrClosed) {
		t.Fatalf("closed RowID = (%d, %v), want (0, ErrClosed)", row, err)
	}
	must(t, b.Close())
	must(t, b.Close())
}

func assertBlobContractAborted(t testing.TB, b *Blob, lastRow int64) {
	t.Helper()
	size, err := b.Size()
	must(t, err)
	if size != 0 {
		t.Fatalf("aborted Size = %d, want zero", size)
	}
	row, err := b.RowID()
	must(t, err)
	if row != lastRow {
		t.Fatalf("aborted RowID = %d, want last successful row %d", row, lastRow)
	}
	assertBlobContractUnavailable(t, b, CodeAbort, lastRow)
}

func assertBlobContractEmptyIO(t testing.TB, b *Blob, offset int64) {
	t.Helper()
	for _, p := range [][]byte{nil, {}} {
		n, err := b.Read(p)
		assertBlobContractIO(t, "empty Read", n, err, 0, nil)
		n, err = b.Write(p)
		assertBlobContractIO(t, "empty Write", n, err, 0, nil)
		n, err = b.ReadAt(p, offset)
		assertBlobContractIO(t, "empty ReadAt", n, err, 0, nil)
		n, err = b.WriteAt(p, offset)
		assertBlobContractIO(t, "empty WriteAt", n, err, 0, nil)
	}
}

func TestBlobIOBoundaries(t *testing.T) {
	c := openTest(t, ":memory:")
	script(t, c, `
		CREATE TABLE freight_tags (slot INTEGER PRIMARY KEY, label BLOB NOT NULL);
		INSERT INTO freight_tags VALUES (72, CAST('north|0000|sealed' AS BLOB));
	`)
	b, err := c.OpenBlob("main", "freight_tags", "label", 72, true)
	must(t, err)
	t.Cleanup(func() { must(t, b.Close()) })
	if err := c.Close(); !errors.Is(err, CodeBusy) {
		t.Fatalf("parent close with open blob = %v, want CodeBusy", err)
	}
	const want = "south|4827|sealed"
	const size = int64(len(want))
	if got, err := b.Size(); err != nil || got != size {
		t.Fatalf("initial Size = (%d, %v), want (%d, nil)", got, err, size)
	}
	if got, err := b.RowID(); err != nil || got != 72 {
		t.Fatalf("initial RowID = (%d, %v), want (72, nil)", got, err)
	}
	positionIs := func(wantPosition int64) {
		t.Helper()
		got, err := b.Seek(0, io.SeekCurrent)
		must(t, err)
		if got != wantPosition {
			t.Fatalf("cursor = %d, want %d", got, wantPosition)
		}
	}

	n, err := b.Write([]byte("south"))
	assertBlobContractIO(t, "sequential Write", n, err, 5, nil)
	positionIs(5)
	separator := make([]byte, 1)
	n, err = b.Read(separator)
	assertBlobContractIO(t, "sequential Read", n, err, 1, nil)
	if string(separator) != "|" {
		t.Fatalf("separator = %q, want |", separator)
	}
	positionIs(6)
	n, err = b.WriteAt([]byte("4827"), 6)
	assertBlobContractIO(t, "positional Write", n, err, 4, nil)
	positionIs(6)
	digits := make([]byte, 4)
	n, err = b.ReadAt(digits, 6)
	assertBlobContractIO(t, "positional Read", n, err, 4, nil)
	if string(digits) != "4827" {
		t.Fatalf("positional bytes = %q, want 4827", digits)
	}
	positionIs(6)

	contentIsUnchanged := func() {
		t.Helper()
		p := make([]byte, len(want))
		n, err := b.ReadAt(p, 0)
		assertBlobContractIO(t, "complete positional Read", n, err, len(want), nil)
		if string(p) != want {
			t.Fatalf("cell = %q, want %q; rejected write changed a prefix", p, want)
		}
		positionIs(6)
	}
	n, err = b.Write([]byte("too-long-to-fit"))
	assertBlobContractIO(t, "overflowing sequential Write", n, err, 0, ErrBlobFull)
	positionIs(6)
	contentIsUnchanged()
	for _, offset := range []int64{size - 1, size, size + 1, math.MaxInt64} {
		n, err = b.WriteAt([]byte("??"), offset)
		assertBlobContractIO(t, "overflowing WriteAt", n, err, 0, ErrBlobFull)
		positionIs(6)
		contentIsUnchanged()
	}
	for _, offset := range []int64{-1, math.MinInt64} {
		for _, p := range [][]byte{nil, {}, {0x61}} {
			n, err = b.ReadAt(p, offset)
			assertBlobContractIO(t, "negative ReadAt", n, err, 0, ErrRange)
			n, err = b.WriteAt(p, offset)
			assertBlobContractIO(t, "negative WriteAt", n, err, 0, ErrRange)
			positionIs(6)
		}
	}
	for _, offset := range []int64{size, size + 1, math.MaxInt64} {
		n, err = b.ReadAt(make([]byte, 1), offset)
		assertBlobContractIO(t, "ReadAt beyond data", n, err, 0, io.EOF)
		positionIs(6)
	}
	short := bytes.Repeat([]byte{0xcc}, 5)
	n, err = b.ReadAt(short, size-3)
	assertBlobContractIO(t, "short ReadAt", n, err, 3, io.EOF)
	if string(short[:n]) != "led" || !bytes.Equal(short[n:], []byte{0xcc, 0xcc}) {
		t.Fatalf("short positional bytes = %x, want led followed by untouched padding", short)
	}
	positionIs(6)
	assertBlobContractEmptyIO(t, b, size)
	for _, p := range [][]byte{nil, {}} {
		n, err = b.ReadAt(p, math.MaxInt64)
		assertBlobContractIO(t, "empty far ReadAt", n, err, 0, nil)
		n, err = b.WriteAt(p, math.MaxInt64)
		assertBlobContractIO(t, "empty far WriteAt", n, err, 0, ErrBlobFull)
		positionIs(6)
	}

	for _, whence := range []int{io.SeekStart, io.SeekCurrent, io.SeekEnd} {
		for _, offset := range []int64{math.MinInt64, math.MaxInt64} {
			if _, err := b.Seek(offset, whence); !errors.Is(err, ErrRange) {
				t.Fatalf("Seek(%d, %d) = %v, want ErrRange", offset, whence, err)
			}
			positionIs(6)
		}
	}
	for _, whence := range []int{-9, 18} {
		if _, err := b.Seek(0, whence); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("Seek with whence %d = %v, want ErrInvalidArgument", whence, err)
		}
		positionIs(6)
	}
	if position, err := b.Seek(-size, io.SeekEnd); err != nil || position != 0 {
		t.Fatalf("seek from end to beginning = (%d, %v), want (0, nil)", position, err)
	}
	all := bytes.Repeat([]byte{0xcc}, len(want)+4)
	n, err = b.Read(all)
	assertBlobContractIO(t, "oversized sequential Read", n, err, len(want), nil)
	if string(all[:n]) != want || !bytes.Equal(all[n:], bytes.Repeat([]byte{0xcc}, 4)) {
		t.Fatalf("oversized sequential bytes = %x, want cell followed by untouched padding", all)
	}
	positionIs(size)
	n, err = b.Read(make([]byte, 1))
	assertBlobContractIO(t, "Read at EOF", n, err, 0, io.EOF)
	assertBlobContractEmptyIO(t, b, size)
	positionIs(size)
	if position, err := b.Seek(0, io.SeekStart); err != nil || position != 0 {
		t.Fatalf("rewind = (%d, %v), want (0, nil)", position, err)
	}
	exact := make([]byte, len(want))
	n, err = b.Read(exact)
	assertBlobContractIO(t, "exact final sequential Read", n, err, len(want), nil)
	if string(exact) != want {
		t.Fatalf("exact sequential bytes = %q, want %q", exact, want)
	}
	n, err = b.Read(make([]byte, 1))
	assertBlobContractIO(t, "Read after final bytes", n, err, 0, io.EOF)
	must(t, b.Close())
	assertBlobContractClosed(t, b)

	stored := prepareTest(t, c, `SELECT label FROM freight_tags WHERE slot = 72`)
	rowTest(t, stored)
	p, err := stored.ColumnBlob(0)
	must(t, err)
	if string(p) != want {
		t.Fatalf("SQL-visible label = %q, want %q", p, want)
	}
	must(t, stored.Close())
	must(t, c.Close())
}

func TestBlobReopenAbortAndClose(t *testing.T) {
	t.Run("text-and-empty-retargets", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, `
			CREATE TABLE ribbon_archive (ribbon INTEGER PRIMARY KEY, data, note TEXT);
			INSERT INTO ribbon_archive VALUES
				(24, CAST('compass-stamp' AS BLOB), 'wax mark'),
				(907, 'ice' || char(0) || 'chart/Ω', 'text map'),
				(-65, zeroblob(0), 'unused ribbon');
		`)
		b, err := c.OpenBlob("main", "ribbon_archive", "data", 24, true)
		must(t, err)
		t.Cleanup(func() { must(t, b.Close()) })
		n, err := b.Write([]byte("WAX"))
		assertBlobContractIO(t, "initial ribbon Write", n, err, 3, nil)
		must(t, b.Reopen(907))
		const text = "ice\x00chart/Ω"
		if size, err := b.Size(); err != nil || size != int64(len(text)) {
			t.Fatalf("text Size = (%d, %v), want (%d, nil)", size, err, len(text))
		}
		if row, err := b.RowID(); err != nil || row != 907 {
			t.Fatalf("text RowID = (%d, %v), want (907, nil)", row, err)
		}
		if position, err := b.Seek(0, io.SeekCurrent); err != nil || position != 0 {
			t.Fatalf("reopened cursor = (%d, %v), want (0, nil)", position, err)
		}
		p := make([]byte, len(text)+3)
		n, err = b.Read(p)
		assertBlobContractIO(t, "text Read", n, err, len(text), nil)
		if string(p[:n]) != text {
			t.Fatalf("text bytes = %q, want %q", p[:n], text)
		}
		must(t, b.Reopen(-65))
		if size, err := b.Size(); err != nil || size != 0 {
			t.Fatalf("empty Size = (%d, %v), want (0, nil)", size, err)
		}
		if row, err := b.RowID(); err != nil || row != -65 {
			t.Fatalf("empty RowID = (%d, %v), want (-65, nil)", row, err)
		}
		if position, err := b.Seek(0, io.SeekCurrent); err != nil || position != 0 {
			t.Fatalf("empty-cell cursor = (%d, %v), want (0, nil)", position, err)
		}
		n, err = b.Read(make([]byte, 1))
		assertBlobContractIO(t, "empty-cell Read", n, err, 0, io.EOF)
		n, err = b.ReadAt(make([]byte, 1), 0)
		assertBlobContractIO(t, "empty-cell ReadAt", n, err, 0, io.EOF)
		n, err = b.Write([]byte{1})
		assertBlobContractIO(t, "empty-cell Write", n, err, 0, ErrBlobFull)
		n, err = b.WriteAt([]byte{1}, 0)
		assertBlobContractIO(t, "empty-cell WriteAt", n, err, 0, ErrBlobFull)
		assertBlobContractEmptyIO(t, b, 0)
		if err := c.Close(); !errors.Is(err, CodeBusy) {
			t.Fatalf("parent close after reopen = %v, want CodeBusy", err)
		}
		must(t, b.Close())
		assertBlobContractClosed(t, b)
		stored := prepareTest(t, c, `SELECT data FROM ribbon_archive WHERE ribbon = 24`)
		rowTest(t, stored)
		p, err = stored.ColumnBlob(0)
		must(t, err)
		if string(p) != "WAXpass-stamp" {
			t.Fatalf("written ribbon = %q, want WAXpass-stamp", p)
		}
		must(t, stored.Close())
		storedText := prepareTest(t, c, `SELECT data, typeof(data) FROM ribbon_archive WHERE ribbon = 907`)
		rowTest(t, storedText)
		for i, want := range []string{text, "text"} {
			value, valid, err := storedText.ColumnText(i)
			must(t, err)
			if !valid || value != want {
				t.Fatalf("text SQL column %d = (%q, %t), want %q", i, value, valid, want)
			}
		}
		must(t, storedText.Close())
		must(t, c.Close())
	})

	t.Run("missing-retarget-aborts", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, `CREATE TABLE survey_film (frame INTEGER PRIMARY KEY, image BLOB);
			INSERT INTO survey_film VALUES (317, zeroblob(9));`)
		b, err := c.OpenBlob("main", "survey_film", "image", 317, true)
		must(t, err)
		t.Cleanup(func() { must(t, b.Close()) })
		assertBlobContractNative(t, b.Reopen(999991), CodeError)
		assertBlobContractAborted(t, b, 317)
		if err := c.Close(); !errors.Is(err, CodeBusy) {
			t.Fatalf("parent close after failed reopen = %v, want CodeBusy", err)
		}
		must(t, b.Close())
		assertBlobContractClosed(t, b)
		must(t, c.Close())
	})

	for _, observeWrite := range []bool{false, true} {
		name := "expired-read"
		if observeWrite {
			name = "expired-write"
		}
		t.Run(name, func(t *testing.T) {
			c := openTest(t, ":memory:")
			script(t, c, `
				CREATE TABLE chart_rolls (roll INTEGER PRIMARY KEY, data BLOB, note TEXT);
				INSERT INTO chart_rolls VALUES
					(612, CAST('planetarium' AS BLOB), 'first chart'),
					(1087, CAST('meridian' AS BLOB), 'second chart');
			`)
			b, err := c.OpenBlob("main", "chart_rolls", "data", 612, true)
			must(t, err)
			t.Cleanup(func() { must(t, b.Close()) })
			if _, err := b.Seek(4, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			script(t, c, `UPDATE chart_rolls SET note = 'catalogued' WHERE roll = 612`)
			if size, err := b.Size(); err != nil || size != int64(len("planetarium")) {
				t.Fatalf("unobserved expired Size = (%d, %v)", size, err)
			}
			if row, err := b.RowID(); err != nil || row != 612 {
				t.Fatalf("unobserved expired RowID = (%d, %v)", row, err)
			}
			assertBlobContractEmptyIO(t, b, int64(len("planetarium")))
			must(t, b.Reopen(1087))
			if size, err := b.Size(); err != nil || size != int64(len("meridian")) {
				t.Fatalf("recovered Size = (%d, %v)", size, err)
			}
			if row, err := b.RowID(); err != nil || row != 1087 {
				t.Fatalf("recovered RowID = (%d, %v)", row, err)
			}
			if position, err := b.Seek(0, io.SeekCurrent); err != nil || position != 0 {
				t.Fatalf("recovered cursor = (%d, %v), want (0, nil)", position, err)
			}
			p := make([]byte, len("meridian"))
			n, err := b.Read(p)
			assertBlobContractIO(t, "recovered Read", n, err, len(p), nil)
			if string(p) != "meridian" {
				t.Fatalf("recovered bytes = %q, want meridian", p)
			}
			if _, err := b.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			script(t, c, `UPDATE chart_rolls SET note = 'revised legend' WHERE roll = 1087`)
			if size, err := b.Size(); err != nil || size != int64(len("meridian")) {
				t.Fatalf("second unobserved expired Size = (%d, %v)", size, err)
			}
			if row, err := b.RowID(); err != nil || row != 1087 {
				t.Fatalf("second unobserved expired RowID = (%d, %v)", row, err)
			}
			assertBlobContractEmptyIO(t, b, int64(len("meridian")))
			if observeWrite {
				n, err = b.Write([]byte{0x51})
			} else {
				n, err = b.Read(make([]byte, 1))
			}
			assertBlobContractIO(t, "observe expiration", n, err, 0, CodeAbort)
			assertBlobContractNative(t, err, CodeAbort)
			assertBlobContractAborted(t, b, 1087)
			if err := c.Close(); !errors.Is(err, CodeBusy) {
				t.Fatalf("parent close after observed abort = %v, want CodeBusy", err)
			}
			must(t, b.Close())
			assertBlobContractClosed(t, b)
			must(t, c.Close())
		})
	}

	t.Run("commit-veto-consumes-handle", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, `CREATE TABLE print_finishes (batch INTEGER PRIMARY KEY, sample BLOB);
			INSERT INTO print_finishes VALUES (-802, CAST('matte-paper' AS BLOB));`)
		b, err := c.OpenBlob("main", "print_finishes", "sample", -802, true)
		must(t, err)
		t.Cleanup(func() { must(t, b.Close()) })
		commits := 0
		must(t, c.SetCommitHook(func() bool {
			commits++
			return true
		}))
		n, err := b.Write([]byte("glow"))
		assertBlobContractIO(t, "vetoed Write", n, err, 4, nil)
		assertBlobContractNative(t, b.Close(), CodeConstraintCommitHook)
		if commits != 1 {
			t.Fatalf("commit hook calls = %d, want one", commits)
		}
		assertBlobContractClosed(t, b)
		if commits != 1 {
			t.Fatalf("consumed handle invoked commit hook again: %d calls", commits)
		}
		must(t, c.SetCommitHook(nil))
		if got := scalarTest(t, c, `SELECT sample = CAST('matte-paper' AS BLOB) FROM print_finishes WHERE batch = -802`); got != 1 {
			t.Fatalf("vetoed write changed the SQL-visible sample: equality = %d", got)
		}
	})

	t.Run("nil-and-zero-handles", func(t *testing.T) {
		var absent *Blob
		assertBlobContractClosed(t, absent)
		var empty Blob
		assertBlobContractClosed(t, &empty)
		for _, c := range []*Conn{nil, new(Conn)} {
			b, err := c.OpenBlob("main", "unopened", "payload", 17, false)
			if b != nil || !errors.Is(err, ErrClosed) {
				t.Fatalf("OpenBlob on closed connection = (%v, %v), want (nil, ErrClosed)", b, err)
			}
		}
	})
}
