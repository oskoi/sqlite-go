// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"errors"
	"math"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func openTest(t testing.TB, name string) *Conn {
	t.Helper()
	c, err := Open(name, 0)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	return c
}
func script(t testing.TB, c *Conn, sql string) {
	t.Helper()
	if err := c.ExecScript(sql); err != nil {
		t.Fatal(err)
	}
}
func prepareTest(t testing.TB, c *Conn, sql string) *Stmt {
	t.Helper()
	s, tail, err := c.Prepare(sql, 0)
	if err != nil || s == nil || strings.TrimSpace(tail) != "" {
		t.Fatalf("Prepare: %v, stmt=%v tail=%q", err, s, tail)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s
}
func rowTest(t testing.TB, s *Stmt) {
	t.Helper()
	row, err := s.Step()
	if err != nil || !row {
		t.Fatalf("Step row=%v err=%v", row, err)
	}
}
func intTest(t testing.TB, s *Stmt, i int) int64 {
	t.Helper()
	n, ok, err := s.ColumnInt64(i)
	if err != nil || !ok {
		t.Fatalf("ColumnInt64: %v nonNull=%v", err, ok)
	}
	return n
}
func scalarTest(t testing.TB, c *Conn, sql string) int64 {
	t.Helper()
	s := prepareTest(t, c, sql)
	rowTest(t, s)
	n := intTest(t, s, 0)
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return n
}

func TestNativeAllocationFailures(t *testing.T) {
	for _, mode := range []string{"init", "partial-open", "conversion-text", "conversion-blob", "conversion-zeroblob", "snapshot-copy"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command("go", "run", "./internal/cmd/faultprobe", mode)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("fault probe %s: %v\n%s", mode, err, out)
			}
			t.Log(strings.TrimSpace(string(out)))
		})
	}
}

func TestNativeSmoke(t *testing.T) {
	options := CompileOptions()
	for _, opt := range []string{"THREADSAFE=2", "DEFAULT_MEMSTATUS=0", "ENABLE_API_ARMOR", "ENABLE_SESSION", "ENABLE_PREUPDATE_HOOK", "ENABLE_FTS3", "ENABLE_FTS3_PARENTHESIS", "ENABLE_FTS4", "ENABLE_FTS5", "ENABLE_RTREE", "ENABLE_GEOPOLY", "ENABLE_STAT4", "SOUNDEX", "OMIT_LOAD_EXTENSION", "OMIT_DEPRECATED", "OMIT_PROGRESS_CALLBACK", "OMIT_SHARED_CACHE"} {
		if !slices.Contains(options, opt) {
			t.Errorf("missing option %s", opt)
		}
	}
	c := openTest(t, ":memory:")
	for _, module := range []string{"fts3", "fts4", "fts5"} {
		script(t, c, "CREATE VIRTUAL TABLE "+module+"_test USING "+module+"(body); INSERT INTO "+module+"_test VALUES('native sqlite search')")
		if scalarTest(t, c, "SELECT count(*) FROM "+module+"_test WHERE body MATCH 'sqlite'") != 1 {
			t.Fatal(module)
		}
	}
	script(t, c, "CREATE VIRTUAL TABLE spatial USING rtree(id,minx,maxx,miny,maxy); INSERT INTO spatial VALUES(1,0,1,0,1)")
	if scalarTest(t, c, "SELECT count(*) FROM spatial WHERE minx<=0.5 AND maxx>=0.5") != 1 {
		t.Fatal("rtree")
	}
	script(t, c, `CREATE VIRTUAL TABLE poly USING geopoly; INSERT INTO poly(_shape) VALUES('[[0,0],[1,0],[1,1],[0,1],[0,0]]')`)
	if scalarTest(t, c, `SELECT count(*) FROM poly WHERE geopoly_overlap(_shape,'[[0,0],[1,0],[1,1],[0,1],[0,0]]')`) != 1 {
		t.Fatal("geopoly")
	}
	if scalarTest(t, c, `SELECT soundex('Robert')='R163' AND typeof(jsonb('{"x":42}'))='blob' AND json_extract(jsonb('{"x":42}'),'$.x')=42`) != 1 {
		t.Fatal("soundex/jsonb")
	}
	utf := openTest(t, filepath.Join(t.TempDir(), "utf16.db"))
	script(t, utf, `PRAGMA encoding='UTF-16le'; CREATE TABLE unicode(v); INSERT INTO unicode VALUES('héllo 世界')`)
	s := prepareTest(t, utf, "SELECT v FROM unicode")
	rowTest(t, s)
	v, ok, err := s.ColumnText(0)
	if err != nil || !ok || v != "héllo 世界" {
		t.Fatalf("UTF16 %q %v %v", v, ok, err)
	}
	s.Close()
	if scalarTest(t, utf, `SELECT (SELECT encoding FROM pragma_encoding)='UTF-16le'`) != 1 {
		t.Fatal("encoding")
	}
	t.Logf("%s %s; options=%v", Version(), SourceID(), options)
}

func TestResultCodes(t *testing.T) {
	for _, pair := range []struct{ code, primary, other Code }{
		{CodeBusySnapshot, CodeBusy, CodeBusyRecovery},
		{CodeConstraintUnique, CodeConstraint, CodeConstraintPrimaryKey},
		{CodeIOErrShortRead, CodeIOErr, CodeIOErrRead},
	} {
		err := &Error{Code: pair.code, Message: "snapshot", Offset: -1, Op: "test"}
		if pair.code.Primary() != pair.primary || !errors.Is(err, pair.primary) ||
			!errors.Is(err, pair.code) || errors.Is(err, pair.other) || errors.Is(err, ErrState) {
			t.Fatalf("result matching: code=%d primary=%d", pair.code, pair.primary)
		}
	}
}

func TestErrorSnapshotMatchesOperation(t *testing.T) {
	for _, priorFailure := range []string{"none", "syntax", "same-code"} {
		t.Run(priorFailure, func(t *testing.T) {
			c := openTest(t, ":memory:")
			s := prepareTest(t, c, "SELECT ?1")
			var prior *Error
			if priorFailure == "syntax" {
				_, _, err := c.Prepare("SELECT 1 + FROM x", 0)
				if !errors.As(err, &prior) || prior.Code != CodeError || prior.Offset < 0 {
					t.Fatalf("expected a prior SQL diagnostic with an offset: %v", err)
				}
			}
			if priorFailure == "same-code" {
				limit, err := c.Limit(LimitSQLLength, 1)
				if err != nil {
					t.Fatal(err)
				}
				_, _, err = c.Prepare("SELECT 1", 0)
				if !errors.As(err, &prior) || prior.Code != CodeTooBig {
					t.Fatalf("expected a prior TOOBIG preparation failure: %v", err)
				}
				if _, err := c.Limit(LimitSQLLength, limit); err != nil {
					t.Fatal(err)
				}
			}
			err := s.BindZeroBlob(1, math.MaxInt64)
			var snapshot *Error
			if !errors.As(err, &snapshot) || snapshot.Code != CodeTooBig || snapshot.Offset != -1 || snapshot.Op != "BindZeroBlob" {
				t.Fatalf("oversized zeroblob snapshot: %v", err)
			}
			// Compare diagnostics through the linked Code API rather than
			// asserting any particular version's native error wording.
			if snapshot.Message != CodeTooBig.Error() {
				t.Fatalf("diagnostic does not belong to returned code %d: %q", snapshot.Code, snapshot.Message)
			}
		})
	}
}

func TestSQLLengthErrorProvenance(t *testing.T) {
	for _, mode := range []string{"prepare", "script"} {
		t.Run(mode, func(t *testing.T) {
			c := openTest(t, ":memory:")
			_, _, priorErr := c.Prepare("SELECT )", 0)
			var prior *Error
			if !errors.As(priorErr, &prior) || prior.Offset != 7 {
				t.Fatalf("missing prior syntax offset: %v", priorErr)
			}
			if _, err := c.Limit(LimitSQLLength, 1); err != nil {
				t.Fatal(err)
			}
			var err error
			if mode == "prepare" {
				var s *Stmt
				s, _, err = c.Prepare("SELECT 1", 0)
				if s != nil {
					s.Close()
					t.Fatal("oversized SQL returned a statement")
				}
			} else {
				err = c.ExecScript("SELECT 1")
			}
			var limited *Error
			if !errors.As(err, &limited) || limited.Code != CodeTooBig || limited.Offset != -1 || limited.Message == prior.Message {
				t.Fatalf("SQL-length failure inherited unrelated diagnostics: %v", err)
			}
		})
	}
}

func TestSQLLengthLimitAppliesToEachStatement(t *testing.T) {
	const sqlLengthLimit = 32
	t.Run("script", func(t *testing.T) {
		c := openTest(t, ":memory:")
		// Load SQLite's internal schema before constraining application SQL.
		script(t, c, "CREATE TABLE t(v)")
		if _, err := c.Limit(LimitSQLLength, sqlLengthLimit); err != nil {
			t.Fatal(err)
		}
		script(t, c, "INSERT INTO t VALUES(1);INSERT INTO t VALUES(2);INSERT INTO t VALUES(3);")
		if got := scalarTest(t, c, "SELECT count(*) FROM t"); got != 3 {
			t.Fatalf("inserted rows = %d, want 3", got)
		}
		if got := scalarTest(t, c, "SELECT sum(v) FROM t"); got != 6 {
			t.Fatalf("inserted values sum = %d, want 6", got)
		}
	})
	t.Run("prepare", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "SELECT 1")
		if _, err := c.Limit(LimitSQLLength, sqlLengthLimit); err != nil {
			t.Fatal(err)
		}
		const tail = " SELECT 22; SELECT 33; SELECT 44;"
		s, gotTail, err := c.Prepare("SELECT 11;"+tail, 0)
		if err != nil {
			t.Fatal(err)
		}
		if s == nil {
			t.Fatal("missing first statement")
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		if gotTail != tail {
			t.Fatalf("tail = %q, want %q", gotTail, tail)
		}
		rowTest(t, s)
		if got := intTest(t, s, 0); got != 11 {
			t.Fatalf("first statement result = %d, want 11", got)
		}
	})
}

func TestTransactionHelpersPreserveAuthorization(t *testing.T) {
	c := openTest(t, ":memory:")
	for _, begin := range []func() error{c.Begin, c.BeginImmediate, c.BeginExclusive} {
		if err := begin(); err != nil {
			t.Fatal(err)
		}
		if err := c.Rollback(); err != nil {
			t.Fatal(err)
		}
	}
	if err := c.SetAuthorizer(func(a Authorization) AuthorizerResult {
		if a.Action == AuthorizerTransaction && a.Arg1.Text == "BEGIN" {
			return AuthorizerDeny
		}
		return AuthorizerOK
	}); err != nil {
		t.Fatal(err)
	}
	for _, begin := range []func() error{c.Begin, c.BeginImmediate, c.BeginExclusive} {
		if err := begin(); !errors.Is(err, CodeAuth) {
			t.Fatal("BEGIN bypassed current authorizer:", err)
		}
		if ac, err := c.AutoCommit(); err != nil || !ac {
			t.Fatal("denied BEGIN changed transaction state:", ac, err)
		}
	}
	if err := c.SetAuthorizer(func(a Authorization) AuthorizerResult {
		if a.Action == AuthorizerTransaction && a.Arg1.Text == "COMMIT" {
			return AuthorizerIgnore
		}
		return AuthorizerOK
	}); err != nil {
		t.Fatal(err)
	}
	if err := c.Begin(); err != nil {
		t.Fatal(err)
	}
	if err := c.Commit(); err != nil {
		t.Fatal(err)
	}
	if ac, err := c.AutoCommit(); err != nil || ac {
		t.Fatal("ignored COMMIT ended the transaction:", ac, err)
	}
	if err := c.Rollback(); err != nil {
		t.Fatal(err)
	}
}
