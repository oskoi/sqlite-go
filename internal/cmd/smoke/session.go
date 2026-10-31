package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"reflect"

	sqlite "github.com/oskoi/sqlite-go"
)

type payloadError struct {
	data []byte
	err  error
}

func (r *payloadError) Read(p []byte) (int, error) {
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

type prefixWriter struct {
	prefix []byte
	err    error
}

func (w *prefixWriter) Write(p []byte) (int, error) {
	n := min(3, len(p))
	w.prefix = append(w.prefix, p[:n]...)
	return n, w.err
}
func sessionRows(c *sqlite.Conn) []string {
	s, _, err := c.Prepare("SELECT id||':'||v FROM replicated ORDER BY id", 0)
	check(err)
	var rows []string
	for {
		row, err := s.Step()
		check(err)
		if !row {
			break
		}
		text, ok, err := s.ColumnText(0)
		check(err)
		if !ok {
			panic("NULL row")
		}
		rows = append(rows, text)
	}
	check(s.Close())
	return rows
}
func sessionsAndChangesets() {
	a, err := sqlite.Open(":memory:", 0)
	check(err)
	b, err := sqlite.Open(":memory:", 0)
	check(err)
	check(a.ExecScript("CREATE TABLE replicated(id PRIMARY KEY,v); INSERT INTO replicated VALUES(1,'base')"))
	check(b.ExecScript("CREATE TABLE replicated(id PRIMARY KEY,v); INSERT INTO replicated VALUES(1,'other')"))
	session, err := a.NewSession("main")
	check(err)
	check(session.AttachAll())
	check(a.ExecScript("UPDATE replicated SET v='new'; INSERT INTO replicated VALUES(2,'added')"))
	var retained sqlite.Conflict
	var view sqlite.Value
	conflicts := 0
	// Independent native owners communicate through an unbuffered stream, not a
	// materialize/replay replacement. Only the later transform demo owns bytes.
	reader, writer := io.Pipe()
	written := make(chan error, 1)
	go func() { err := session.WriteChangeset(writer); writer.CloseWithError(err); written <- err }()
	err = b.ApplyChangeset(reader, sqlite.ApplyOptions{Conflict: func(cf sqlite.Conflict) (sqlite.ConflictAction, error) {
		retained = cf
		conflicts++
		kind, err := cf.Type()
		check(err)
		if kind != sqlite.ConflictData {
			panic(kind)
		}
		change, err := cf.Change()
		check(err)
		old, err := change.Old(1)
		check(err)
		oldText, _, err := old.Text()
		check(err)
		next, err := change.New(1)
		check(err)
		newText, _, err := next.Text()
		check(err)
		view, err = cf.Current(1)
		check(err)
		current, _, err := view.Text()
		check(err)
		if oldText != "base" || newText != "new" || current != "other" {
			panic("borrowed conflict values")
		}
		fmt.Printf("native DATA conflict: old=%q new=%q current=%q; REPLACE\n", oldText, newText, current)
		return sqlite.Replace, nil
	}})
	reader.CloseWithError(err)
	check(<-written)
	check(err)
	if conflicts != 1 || !reflect.DeepEqual(sessionRows(a), []string{"1:new", "2:added"}) || !reflect.DeepEqual(sessionRows(a), sessionRows(b)) {
		panic("stream replication rows")
	}
	if _, err := retained.Type(); !errors.Is(err, sqlite.ErrExpired) {
		panic("conflict lifetime")
	}
	if _, err := view.Kind(); !errors.Is(err, sqlite.ErrExpired) {
		panic("value lifetime")
	}
	fmt.Printf("stream A -> B rows=%v; borrowed views expired\n", sessionRows(b))
	var payload, inverse bytes.Buffer
	check(session.WriteChangeset(&payload))
	check(sqlite.InvertChangeset(&inverse, bytes.NewReader(payload.Bytes())))
	check(a.ApplyChangeset(bytes.NewReader(inverse.Bytes()), sqlite.ApplyOptions{}))
	check(b.ApplyChangeset(bytes.NewReader(inverse.Bytes()), sqlite.ApplyOptions{}))
	if !reflect.DeepEqual(sessionRows(a), []string{"1:base"}) || !reflect.DeepEqual(sessionRows(a), sessionRows(b)) {
		panic("inverse restoration")
	}
	sentinel := errors.New("complete payload with sentinel")
	err = b.ApplyChangeset(&payloadError{data: payload.Bytes(), err: sentinel}, sqlite.ApplyOptions{})
	if !errors.Is(err, sentinel) || !reflect.DeepEqual(sessionRows(b), []string{"1:base"}) {
		panic("strict paired-input rollback")
	}
	fmt.Printf("inverse rows=%v; complete payload+nonEOF sentinel: original error, unchanged target\n", sessionRows(b))
	checksumErr := errors.New("changeset checksum failed")
	joinedEOF := errors.Join(io.EOF, checksumErr)
	err = b.ApplyChangeset(&payloadError{data: payload.Bytes(), err: joinedEOF}, sqlite.ApplyOptions{})
	if !errors.Is(err, joinedEOF) || !errors.Is(err, checksumErr) || !reflect.DeepEqual(sessionRows(b), []string{"1:base"}) {
		panic("joined EOF failure lost atomicity")
	}
	fmt.Printf("complete payload+joined EOF/checksum: original error preserved; unchanged target=%v\n", sessionRows(b))
	group, err := sqlite.NewChangegroup()
	check(err)
	check(group.Add(bytes.NewReader(payload.Bytes())))
	failed := &prefixWriter{err: sentinel}
	n, err := group.WriteTo(failed)
	if n != 3 || len(failed.prefix) != 3 || !errors.Is(err, sentinel) {
		panic("failed prefix count")
	}
	var recovered bytes.Buffer
	n, err = group.WriteTo(&recovered)
	check(err)
	if n != int64(recovered.Len()) {
		panic("recovered byte count")
	}
	check(b.ApplyChangeset(bytes.NewReader(recovered.Bytes()), sqlite.ApplyOptions{}))
	if !reflect.DeepEqual(sessionRows(b), []string{"1:new", "2:added"}) {
		panic("group recovery rows")
	}
	// Filter panic has no native abort return. Explicit caller rollback owns policy.
	check(b.Begin())
	panicValue := &struct{}{}
	recoveredPanic := false
	func() {
		defer func() {
			if recover() != panicValue {
				panic("filter panic identity")
			}
			recoveredPanic = true
		}()
		b.ApplyChangeset(bytes.NewReader(payload.Bytes()), sqlite.ApplyOptions{Filter: func(string) bool { panic(panicValue) }})
	}()
	check(b.Rollback())
	if !recoveredPanic || !reflect.DeepEqual(sessionRows(b), []string{"1:new", "2:added"}) {
		panic("explicit panic rollback")
	}
	fmt.Println("failed group output: accepted prefix=3; group reused; filter panic recovered then caller transaction rolled back")
	check(group.Close())
	check(session.Close())
	check(a.Close())
	check(b.Close())
}
