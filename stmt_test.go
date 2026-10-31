// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"errors"
	"testing"
)

func TestReprepareErrorProvenance(t *testing.T) {
	for _, tc := range []struct {
		mode    string
		expired bool
	}{
		{"reset", false}, {"close", false}, {"exec", false},
		{"reset", true}, {"close", true}, {"exec", true},
	} {
		name := tc.mode
		if tc.expired {
			name += "-expired"
		}
		t.Run(name, func(t *testing.T) {
			mode := tc.mode
			c := openTest(t, ":memory:")
			script(t, c, "CREATE TABLE t(wanted)")
			const query = "SELECT wanted FROM t"
			s := prepareTest(t, c, query)
			script(t, c, "ALTER TABLE t RENAME COLUMN wanted TO renamed")
			if tc.expired {
				must(t, c.SetAuthorizer(func(Authorization) AuthorizerResult { return AuthorizerOK }))
			}
			_, _, referenceErr := c.Prepare(query, 0)
			var reference *Error
			if !errors.As(referenceErr, &reference) || reference.Code != CodeError || reference.Offset != 7 {
				t.Fatalf("reference compilation: %v", referenceErr)
			}
			_, _, unrelatedErr := c.Prepare("SELECT 1, )", 0)
			var unrelated *Error
			if !errors.As(unrelatedErr, &unrelated) || unrelated.Offset != 10 || unrelated.Message == reference.Message {
				t.Fatalf("unrelated compilation: %v", unrelatedErr)
			}
			var err error
			if mode == "exec" {
				var result Result
				result, err = s.Exec()
				if result != (Result{}) {
					t.Fatalf("failed Exec result: %+v", result)
				}
			} else {
				var row bool
				row, err = s.Step()
				if row {
					t.Fatal("failed reprepare returned a row")
				}
			}
			var execution *Error
			if !errors.As(err, &execution) || execution.Code != reference.Code || execution.Message != reference.Message || execution.Offset != reference.Offset {
				t.Fatalf("reprepare lost its own diagnostic: %v; reference: %v", err, referenceErr)
			}
			c.Prepare("SELECT 1, )", 0)
			if mode == "exec" {
				if err := s.Close(); err != nil {
					t.Fatal("Exec did not reset after failure", err)
				}
				return
			}
			if mode == "reset" {
				err = s.Reset()
			} else {
				err = s.Close()
			}
			var cleanup *Error
			if !errors.As(err, &cleanup) || cleanup.Code != execution.Code || cleanup.Message != cleanup.Code.Error() || cleanup.Offset != -1 {
				t.Fatalf("cleanup lost the result code or inherited another operation's diagnostic: %v", err)
			}
			if err := s.Close(); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestResetClearBindings(t *testing.T) {
	for _, mode := range []string{"plain", "callback", "session"} {
		t.Run(mode, func(t *testing.T) {
			c := openTest(t, ":memory:")
			if mode == "callback" {
				if err := c.SetCommitHook(func() bool { return false }); err != nil {
					t.Fatal(err)
				}
			}
			if mode == "session" {
				session, err := c.NewSession("main")
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					if err := session.Close(); err != nil {
						t.Error(err)
					}
				})
			}
			for _, state := range []string{"ready", "row", "done"} {
				t.Run(state, func(t *testing.T) {
					s := prepareTest(t, c, "SELECT ?")
					if err := s.BindInt64(1, 9876); err != nil {
						t.Fatal(err)
					}
					if state != "ready" {
						rowTest(t, s)
					}
					if state == "done" {
						if row, err := s.Step(); row || err != nil {
							t.Fatal(row, err)
						}
					}
					if reset, clear := s.ResetClearBindings(); reset != nil || clear != nil {
						t.Fatal(reset, clear)
					}
					rowTest(t, s)
					if n, valid, err := s.ColumnInt64(0); err != nil || valid || n != 0 {
						t.Fatalf("stale binding: %d %v %v", n, valid, err)
					}
				})
			}
			script(t, c, "CREATE TABLE unique_value(n INTEGER PRIMARY KEY); INSERT INTO unique_value VALUES(7)")
			s := prepareTest(t, c, "INSERT INTO unique_value VALUES(?)")
			if err := s.BindInt64(1, 7); err != nil {
				t.Fatal(err)
			}
			if _, err := s.Step(); !errors.Is(err, CodeConstraintPrimaryKey) {
				t.Fatal(err)
			}
			reset, clear := s.ResetClearBindings()
			if !errors.Is(reset, CodeConstraintPrimaryKey) || clear != nil {
				t.Fatal(reset, clear)
			}
			if _, err := s.Exec(); err != nil {
				t.Fatal("reset error prevented clearing/reuse:", err)
			}
			if n := scalarTest(t, c, "SELECT max(n) FROM unique_value"); n != 8 {
				t.Fatal("NULL binding did not generate the next key:", n)
			}
		})
	}
	var closed *Stmt
	if reset, clear := closed.ResetClearBindings(); !errors.Is(reset, ErrClosed) || clear != nil {
		t.Fatal(reset, clear)
	}
}
