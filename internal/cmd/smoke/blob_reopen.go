// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"errors"
	"fmt"
	"io"

	"github.com/oskoi/sqlite-go"
)

func blobTextReopen(c *sqlite.Conn) {
	check(c.ExecScript(`
		CREATE TABLE navigation_cards (
			card INTEGER PRIMARY KEY,
			payload,
			edition INTEGER NOT NULL
		);
		INSERT INTO navigation_cards VALUES
			(-730, zeroblob(6), 1),
			(6209, 'ferry' || char(0) || 'schedule:水', 4);
	`))
	b, err := c.OpenBlob("main", "navigation_cards", "payload", -730, true)
	check(err)
	defer func() { check(b.Close()) }()
	if size, err := b.Size(); err != nil || size != 6 {
		panic(fmt.Sprintf("initial navigation blob size = (%d, %v), want (6, nil)", size, err))
	}
	check(b.Reopen(6209))
	const text = "ferry\x00schedule:水"
	if size, err := b.Size(); err != nil || size != int64(len(text)) {
		panic(fmt.Sprintf("navigation text size = (%d, %v), want (%d, nil)", size, err, len(text)))
	}
	if row, err := b.RowID(); err != nil || row != 6209 {
		panic(fmt.Sprintf("navigation text row = (%d, %v), want (6209, nil)", row, err))
	}
	if position, err := b.Seek(0, io.SeekCurrent); err != nil || position != 0 {
		panic(fmt.Sprintf("navigation reopen cursor = (%d, %v), want (0, nil)", position, err))
	}
	p := make([]byte, len(text)+2)
	n, err := b.Read(p)
	check(err)
	if n != len(text) || string(p[:n]) != text {
		panic(fmt.Sprintf("navigation text read = (%d, %q), want complete %q", n, p[:n], text))
	}
	if position, err := b.Seek(0, io.SeekStart); err != nil || position != 0 {
		panic(fmt.Sprintf("navigation rewind = (%d, %v), want (0, nil)", position, err))
	}
	check(c.ExecScript(`UPDATE navigation_cards SET edition = 9 WHERE card = 6209`))
	abortIsExpected := func(operation string, err error) {
		var native *sqlite.Error
		if !errors.Is(err, sqlite.CodeAbort) || !errors.As(err, &native) || native.Code != sqlite.CodeAbort || native.Offset != -1 {
			panic(fmt.Sprintf("%s = %#v, want CodeAbort with offset -1", operation, err))
		}
	}
	n, err = b.Read(make([]byte, 1))
	if n != 0 {
		panic(fmt.Sprintf("expired navigation read returned %d bytes", n))
	}
	abortIsExpected("expired navigation read", err)
	if size, err := b.Size(); err != nil || size != 0 {
		panic(fmt.Sprintf("aborted navigation size = (%d, %v), want (0, nil)", size, err))
	}
	if row, err := b.RowID(); err != nil || row != 6209 {
		panic(fmt.Sprintf("aborted navigation row = (%d, %v), want (6209, nil)", row, err))
	}
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
		for _, p := range [][]byte{nil, {}, {0x44}} {
			n, err := operation.call(p)
			if n != 0 {
				panic(fmt.Sprintf("aborted navigation %s returned %d bytes", operation.name, n))
			}
			abortIsExpected(operation.name, err)
		}
	}
	if position, err := b.Seek(0, io.SeekStart); position != 0 {
		panic(fmt.Sprintf("aborted navigation seek returned %d", position))
	} else {
		abortIsExpected("Seek", err)
	}
	abortIsExpected("Reopen", b.Reopen(-730))
	if err := c.Close(); !errors.Is(err, sqlite.CodeBusy) {
		panic(fmt.Sprintf("parent close with aborted navigation blob = %v, want CodeBusy", err))
	}
	check(b.Close())
	check(b.Close())
	if _, err := b.Size(); !errors.Is(err, sqlite.ErrClosed) {
		panic(fmt.Sprintf("closed navigation size = %v, want ErrClosed", err))
	}
	if _, err := b.RowID(); !errors.Is(err, sqlite.ErrClosed) {
		panic(fmt.Sprintf("closed navigation row = %v, want ErrClosed", err))
	}

	stored, _, err := c.Prepare(`SELECT payload, edition, typeof(payload) FROM navigation_cards WHERE card = 6209`, 0)
	check(err)
	defer func() { check(stored.Close()) }()
	row, err := stored.Step()
	check(err)
	if !row {
		panic("navigation text record disappeared")
	}
	value, valid, err := stored.ColumnText(0)
	check(err)
	if !valid || value != text {
		panic(fmt.Sprintf("SQL-visible navigation text = (%q, %t), want %q", value, valid, text))
	}
	edition, valid, err := stored.ColumnInt64(1)
	check(err)
	if !valid || edition != 9 {
		panic(fmt.Sprintf("SQL-visible navigation edition = (%d, %t), want 9", edition, valid))
	}
	kind, valid, err := stored.ColumnText(2)
	check(err)
	if !valid || kind != "text" {
		panic(fmt.Sprintf("SQL-visible navigation storage class = (%q, %t), want text", kind, valid))
	}
	row, err = stored.Step()
	check(err)
	if row {
		panic("navigation lookup returned more than one text record")
	}
	fmt.Println("blob text reopen: embedded NUL/UTF-8 preserved; nonsequential row; terminal ABORT; explicit close")
}
