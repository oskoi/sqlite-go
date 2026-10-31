// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"bytes"
	"errors"
	"reflect"
	"strings"
	"testing"
)

func must(t testing.TB, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
func sessionTest(t testing.TB, c *Conn) *Session {
	t.Helper()
	s, err := c.NewSession("main")
	must(t, err)
	t.Cleanup(func() { must(t, s.Close()) })
	must(t, s.AttachAll())
	return s
}
func sessionBytes(t testing.TB, s *Session, patch bool) []byte {
	t.Helper()
	var b bytes.Buffer
	if patch {
		must(t, s.WritePatchset(&b))
	} else {
		must(t, s.WriteChangeset(&b))
	}
	return b.Bytes()
}
func rowsText(t testing.TB, c *Conn, sql string) []string {
	t.Helper()
	s := prepareTest(t, c, sql)
	defer func() { must(t, s.Close()) }()
	var rows []string
	for {
		row, err := s.Step()
		must(t, err)
		if !row {
			break
		}
		text, ok, err := s.ColumnText(0)
		must(t, err)
		if !ok {
			t.Fatal("NULL row")
		}
		rows = append(rows, text)
	}
	return rows
}
func requireRows(t testing.TB, c *Conn, sql string, want []string) {
	t.Helper()
	got := rowsText(t, c, sql)
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("rows=%v want=%v", got, want)
	}
}
func eachChange(t testing.TB, data []byte, fn func(Change)) int {
	t.Helper()
	it, err := NewChangesetIter(bytes.NewReader(data))
	must(t, err)
	n := 0
	for {
		row, err := it.Next()
		must(t, err)
		if !row {
			break
		}
		ch, err := it.Change()
		must(t, err)
		fn(ch)
		n++
	}
	row, err := it.Next()
	must(t, err)
	if row {
		t.Fatal("DONE restarted")
	}
	must(t, it.Close())
	return n
}

func TestSessionRoundTrip(t *testing.T) {
	t.Run("changeset-inverse-patchset", testSessionRoundTripContract)
	t.Run("lifetime-filter-flags-restrictions", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, `CREATE TABLE yes(id PRIMARY KEY,v); CREATE TABLE no(id PRIMARY KEY); CREATE TABLE nopk(v); CREATE TABLE nullable(id PRIMARY KEY,v)`)
		s := sessionTest(t, c)
		second := sessionTest(t, c)
		if err := c.Close(); !errors.Is(err, CodeBusy) {
			t.Fatal(err)
		}
		if err := s.Attach(""); !errors.Is(err, ErrInvalidArgument) {
			t.Fatal(err)
		}
		for _, name := range []string{"x\x00y"} {
			if err := s.Attach(name); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal(err)
			}
			if _, err := c.NewSession(name); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal(err)
			}
			if err := s.Diff(name, "yes"); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal(err)
			}
			if err := s.Diff("main", name); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal(err)
			}
		}
		calls := 0
		must(t, s.SetTableFilter(func(table string) bool { calls++; return table == "yes" }))
		enabled, err := s.Enabled()
		must(t, err)
		if !enabled {
			t.Fatal("disabled default")
		}
		empty, err := s.Empty()
		must(t, err)
		if !empty {
			t.Fatal("new session not empty")
		}
		must(t, s.SetEnabled(false))
		enabled, err = s.Enabled()
		must(t, err)
		if enabled {
			t.Fatal("enabled")
		}
		script(t, c, "INSERT INTO yes VALUES(1,'not recorded')")
		must(t, s.SetEnabled(true))
		must(t, s.SetIndirect(true))
		indirect, err := s.Indirect()
		must(t, err)
		if !indirect {
			t.Fatal("not indirect")
		}
		script(t, c, "INSERT INTO yes VALUES(2,'recorded'); INSERT INTO no VALUES(1); INSERT INTO nopk VALUES(1); INSERT INTO nullable VALUES(NULL,1)")
		n := eachChange(t, sessionBytes(t, s, false), func(ch Change) {
			info, err := ch.Info()
			must(t, err)
			if info.Table != "yes" || !info.Indirect {
				t.Fatal(info)
			}
		})
		if n != 1 || calls == 0 {
			t.Fatalf("n=%d calls=%d", n, calls)
		}
		n = eachChange(t, sessionBytes(t, second, false), func(Change) {})
		if n != 3 {
			t.Fatalf("native PK restrictions: n=%d want 3", n)
		}
		if !bytes.Equal(sessionBytes(t, s, false), sessionBytes(t, s, false)) {
			t.Fatal("write reset session")
		}
		must(t, s.SetIndirect(false))
		must(t, s.SetTableFilter(nil))
		must(t, s.Close())
		must(t, s.Close())
		if _, err := s.Empty(); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		must(t, second.Close())
		must(t, c.Close())
	})
	t.Run("diff", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, `CREATE TABLE t(id PRIMARY KEY,v); INSERT INTO t VALUES(1,'new'),(2,'added'); ATTACH ':memory:' AS old; CREATE TABLE old.t(id PRIMARY KEY,v); INSERT INTO old.t VALUES(1,'old'),(3,'removed')`)
		s := sessionTest(t, c)
		must(t, s.Diff("old", "t"))
		data := sessionBytes(t, s, false)
		target := openTest(t, ":memory:")
		script(t, target, `CREATE TABLE t(id PRIMARY KEY,v); INSERT INTO t VALUES(1,'old'),(3,'removed')`)
		must(t, target.ApplyChangeset(bytes.NewReader(data), ApplyOptions{}))
		requireRows(t, target, "SELECT id||':'||v FROM t ORDER BY id", []string{"1:new", "2:added"})
		_, _, priorErr := c.Prepare("SELECT )", 0)
		var prior *Error
		if !errors.As(priorErr, &prior) || prior.Offset < 0 {
			t.Fatalf("missing prior SQL diagnostic: %v", priorErr)
		}
		err := s.Diff("absent", "t")
		var diagnostic *Error
		if !errors.As(err, &diagnostic) || diagnostic.Code != CodeSchema || !strings.Contains(diagnostic.Message, "absent.t") || diagnostic.Offset != -1 {
			t.Fatalf("direct Diff diagnostic: %v", err)
		}
	})
	t.Run("zero-handles", func(t *testing.T) {
		var s Session
		must(t, s.Close())
		if err := s.Attach("t"); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		var it ChangesetIter
		must(t, it.Close())
		if _, err := it.Next(); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		var g Changegroup
		must(t, g.Close())
		if err := g.Add(bytes.NewReader(nil)); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	})
}
