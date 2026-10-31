// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"errors"
	"os/exec"
	"path/filepath"
	"runtime"
	"testing"
	"time"
	"weak"
)

func requirePanic(t testing.TB, want any, fn func()) {
	t.Helper()
	defer func() {
		if got := recover(); got != want {
			t.Fatalf("panic = %#v, want %#v", got, want)
		}
	}()
	fn()
	t.Fatal("missing panic")
}

type callbackCapture struct {
	calls int
	data  [1024]byte
}

func requireCollected(t testing.TB, refs ...weak.Pointer[callbackCapture]) {
	t.Helper()
	for range 10 {
		runtime.GC()
	}
	for i, ref := range refs {
		if ref.Value() != nil {
			t.Fatalf("callback capture %d still reachable", i)
		}
	}
}

func TestCallbackLifecycleAndReentry(t *testing.T) {
	t.Run("transactions", testCallbackTransactions)
	t.Run("updates-and-release", testCallbackUpdates)
	t.Run("authorization", testCallbackAuthorization)
	t.Run("owner-guard", func(t *testing.T) {
		c := openTest(t, ":memory:")
		other := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE x(a)")
		s := prepareTest(t, c, "SELECT ?1")
		calls := 0
		if err := c.SetUpdateHook(func(UpdateEvent) {
			calls++
			for name, fn := range map[string]func() error{
				"close": c.Close, "child-close": s.Close, "reset": s.Reset,
				"replace":    func() error { return c.SetUpdateHook(nil) },
				"timeout":    func() error { return c.BusyTimeout(time.Millisecond) },
				"prepare":    func() error { _, _, err := c.Prepare("SELECT 2", 0); return err },
				"script":     func() error { return c.ExecScript("SELECT 2") },
				"step":       func() error { _, err := s.Step(); return err },
				"exec":       func() error { _, err := s.Exec(); return err },
				"bind":       func() error { return s.BindInt64(1, 99) },
				"clear":      s.ClearBindings,
				"metadata":   func() error { _, err := s.ParameterCount(); return err },
				"column":     func() error { _, _, err := s.ColumnInt64(0); return err },
				"counter":    func() error { _, err := c.Changes(); return err },
				"autocommit": func() error { _, err := c.AutoCommit(); return err },
			} {
				if err := fn(); !errors.Is(err, ErrReentrant) {
					t.Errorf("%s: %v", name, err)
				}
			}
			if got := scalarTest(t, other, "SELECT 42"); got != 42 {
				t.Error(got)
			}
		}); err != nil {
			t.Fatal(err)
		}
		script(t, c, "INSERT INTO x VALUES(1)")
		if calls != 1 {
			t.Fatal(calls)
		}
		rowTest(t, s)
		if _, nonNull, err := s.ColumnInt64(0); nonNull || err != nil {
			t.Fatal(nonNull, err)
		}
		if err := c.Close(); !errors.Is(err, CodeBusy) {
			t.Fatal(err)
		}
		script(t, c, "INSERT INTO x VALUES(2)")
		if calls != 2 {
			t.Fatal("failed close lost hook")
		}
	})
	t.Run("captures-released", func(t *testing.T) {
		c := openTest(t, ":memory:")
		setters := []func(*callbackCapture) error{
			func(p *callbackCapture) error { return c.SetBusyHandler(func(int) bool { p.calls++; return false }) },
			func(p *callbackCapture) error { return c.SetCommitHook(func() bool { p.calls++; return false }) },
			func(p *callbackCapture) error { return c.SetRollbackHook(func() { p.calls++ }) },
			func(p *callbackCapture) error { return c.SetUpdateHook(func(UpdateEvent) { p.calls++ }) },
			func(p *callbackCapture) error {
				return c.SetAuthorizer(func(Authorization) AuthorizerResult { p.calls++; return AuthorizerOK })
			},
		}
		removers := []func() error{
			func() error { return c.SetBusyHandler(nil) },
			func() error { return c.SetCommitHook(nil) },
			func() error { return c.SetRollbackHook(nil) },
			func() error { return c.SetUpdateHook(nil) },
			func() error { return c.SetAuthorizer(nil) },
		}
		install := func() []weak.Pointer[callbackCapture] {
			var refs []weak.Pointer[callbackCapture]
			for _, set := range setters {
				p := new(callbackCapture)
				refs = append(refs, weak.Make(p))
				if err := set(p); err != nil {
					t.Fatal(err)
				}
			}
			return refs
		}
		replaced := install()
		removed := install()
		requireCollected(t, replaced...)
		for _, remove := range removers {
			if err := remove(); err != nil {
				t.Fatal(err)
			}
		}
		requireCollected(t, removed...)
		closed := install()
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		requireCollected(t, closed...)
		for _, set := range setters {
			if err := set(new(callbackCapture)); !errors.Is(err, ErrClosed) {
				t.Fatal(err)
			}
		}
	})
}

func TestBusyTimeout(t *testing.T) {
	path := filepath.Join(t.TempDir(), "timeout.db")
	c := openTest(t, path)
	for _, tc := range []struct {
		d  time.Duration
		ms int64
	}{
		{-time.Second, 0}, {0, 0}, {time.Nanosecond, 1}, {time.Millisecond, 1}, {time.Millisecond + 1, 2}, {2147483647 * time.Millisecond, 2147483647},
	} {
		if err := c.BusyTimeout(tc.d); err != nil {
			t.Fatal(err)
		}
		if got := scalarTest(t, c, "PRAGMA busy_timeout"); got != tc.ms {
			t.Fatalf("timeout %s = %d", tc.d, got)
		}
	}
	locker := openTest(t, path)
	script(t, locker, "CREATE TABLE x(a); BEGIN; INSERT INTO x VALUES(1)")
	calls := 0
	install := func() weak.Pointer[callbackCapture] {
		p := new(callbackCapture)
		if err := c.SetBusyHandler(func(int) bool { p.calls++; calls++; return false }); err != nil {
			t.Fatal(err)
		}
		return weak.Make(p)
	}
	tryBusy := func() {
		t.Helper()
		if err := c.ExecScript("INSERT INTO x VALUES(2)"); !errors.Is(err, CodeBusy) {
			t.Fatal(err)
		}
	}
	replaced := install()
	if err := c.BusyTimeout(2147483647*time.Millisecond + 1); !errors.Is(err, ErrRange) {
		t.Fatal(err)
	}
	tryBusy()
	if calls != 1 {
		t.Fatal("range failure changed handler")
	}
	if err := c.BusyTimeout(1); err != nil {
		t.Fatal(err)
	}
	requireCollected(t, replaced)
	tryBusy()
	if calls != 1 {
		t.Fatal("native timeout invoked old handler")
	}
	inactive := install()
	for range 10 {
		script(t, c, "PRAGMA busy_timeout=0")
	}
	tryBusy()
	if calls != 1 {
		t.Fatal("SQL timeout invoked old handler")
	}
	if err := c.SetBusyHandler(nil); err != nil {
		t.Fatal(err)
	}
	requireCollected(t, inactive)
}

func TestCallbackPanicCleanup(t *testing.T) {
	t.Run("nil-value", func(t *testing.T) {
		t.Setenv("GODEBUG", "panicnil=1")
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE x(a)")
		if err := c.SetCommitHook(func() bool { panic(nil) }); err != nil {
			t.Fatal(err)
		}
		requirePanic(t, nil, func() { c.ExecScript("INSERT INTO x VALUES(1)") })
		if got := scalarTest(t, c, "SELECT count(*) FROM x"); got != 0 {
			t.Fatal(got)
		}
		script(t, c, "INSERT INTO x VALUES(2)")
		if got := scalarTest(t, c, "SELECT a FROM x"); got != 2 {
			t.Fatal(got)
		}
	})
	for _, method := range []string{"script", "step", "exec"} {
		t.Run("update-"+method, func(t *testing.T) {
			c := openTest(t, ":memory:")
			script(t, c, "CREATE TABLE x(a)")
			s := prepareTest(t, c, "INSERT INTO x VALUES(1),(2)")
			value := &struct{ method string }{method}
			calls, commits, rollbacks := 0, 0, 0
			refs := func() []weak.Pointer[callbackCapture] {
				busy, commit, rollback, update, authorize := new(callbackCapture), new(callbackCapture), new(callbackCapture), new(callbackCapture), new(callbackCapture)
				if err := c.SetBusyHandler(func(int) bool { busy.calls++; return false }); err != nil {
					t.Fatal(err)
				}
				if err := c.SetCommitHook(func() bool { commit.calls++; commits++; return false }); err != nil {
					t.Fatal(err)
				}
				if err := c.SetRollbackHook(func() { rollback.calls++; rollbacks++ }); err != nil {
					t.Fatal(err)
				}
				if err := c.SetAuthorizer(func(Authorization) AuthorizerResult { authorize.calls++; return AuthorizerOK }); err != nil {
					t.Fatal(err)
				}
				if err := c.SetUpdateHook(func(UpdateEvent) {
					update.calls++
					calls++
					if err := c.Close(); !errors.Is(err, ErrReentrant) {
						t.Error(err)
					}
					panic(value)
				}); err != nil {
					t.Fatal(err)
				}
				return []weak.Pointer[callbackCapture]{weak.Make(busy), weak.Make(commit), weak.Make(rollback), weak.Make(update), weak.Make(authorize)}
			}()
			requirePanic(t, value, func() {
				switch method {
				case "script":
					c.ExecScript("INSERT INTO x VALUES(1),(2)")
				case "step":
					s.Step()
				case "exec":
					s.Exec()
				}
			})
			if calls != 1 || commits != 0 || rollbacks != 0 {
				t.Fatalf("callbacks %d/%d/%d", calls, commits, rollbacks)
			}
			requireCollected(t, refs...)
			// The update notification cannot abort; the suppressed commit hook
			// vetoes, so this particular operation rolls back both rows.
			if got := scalarTest(t, c, "SELECT count(*) FROM x"); got != 0 {
				t.Fatal(got)
			}
			if method == "step" {
				if _, err := s.Step(); !errors.Is(err, ErrState) {
					t.Fatal(err)
				}
				if err := s.Reset(); !errors.Is(err, CodeConstraintCommitHook) {
					t.Fatal(err)
				}
			}
			if err := c.SetUpdateHook(func(UpdateEvent) { calls++ }); err != nil {
				t.Fatal(err)
			}
			script(t, c, "INSERT INTO x VALUES(3)")
			if calls != 2 {
				t.Fatal("cannot reinstall hook")
			}
		})
	}
	t.Run("notification-effects", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE x(a)")
		value := &struct{}{}
		calls := 0
		if err := c.SetUpdateHook(func(UpdateEvent) { calls++; panic(value) }); err != nil {
			t.Fatal(err)
		}
		requirePanic(t, value, func() { c.ExecScript("INSERT INTO x VALUES(1),(2); INSERT INTO x VALUES(3)") })
		if calls != 1 || scalarTest(t, c, "SELECT sum(a) FROM x") != 6 {
			t.Fatal("notification panic effects or suppression")
		}
	})
	t.Run("returning-row-state", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE x(a)")
		s := prepareTest(t, c, "INSERT INTO x VALUES(7) RETURNING a")
		value := &struct{}{}
		if err := c.SetUpdateHook(func(UpdateEvent) { panic(value) }); err != nil {
			t.Fatal(err)
		}
		requirePanic(t, value, func() { s.Step() })
		if intTest(t, s, 0) != 7 {
			t.Fatal("panic lost native ROW state")
		}
		if err := s.Reset(); err != nil {
			t.Fatal(err)
		}
		if got := scalarTest(t, c, "SELECT a FROM x"); got != 7 {
			t.Fatal(got)
		}
	})
	t.Run("commit-and-rollback-suppression", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE x(a); BEGIN; INSERT INTO x VALUES(1)")
		value := &struct{}{}
		rollbacks := 0
		if err := c.SetCommitHook(func() bool { panic(value) }); err != nil {
			t.Fatal(err)
		}
		if err := c.SetRollbackHook(func() { rollbacks++ }); err != nil {
			t.Fatal(err)
		}
		requirePanic(t, value, func() { c.Commit() })
		if rollbacks != 0 || scalarTest(t, c, "SELECT count(*) FROM x") != 0 {
			t.Fatal("commit panic cleanup")
		}
	})
	t.Run("rollback", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE x(a); BEGIN; INSERT INTO x VALUES(1)")
		value := &struct{}{}
		if err := c.SetRollbackHook(func() { panic(value) }); err != nil {
			t.Fatal(err)
		}
		requirePanic(t, value, func() { c.Rollback() })
		if scalarTest(t, c, "SELECT count(*) FROM x") != 0 {
			t.Fatal("rollback effect")
		}
	})
	t.Run("authorizer-prepare-and-reprepare", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE x(a); INSERT INTO x VALUES(1)")
		s := prepareTest(t, c, "SELECT * FROM x")
		value := &struct{}{}
		calls := 0
		install := func() {
			if err := c.SetAuthorizer(func(Authorization) AuthorizerResult { calls++; panic(value) }); err != nil {
				t.Fatal(err)
			}
		}
		install()
		requirePanic(t, value, func() { c.Prepare("SELECT * FROM x", 0) })
		if calls != 1 {
			t.Fatal("repeated authorizer callback")
		}
		if err := c.Close(); !errors.Is(err, CodeBusy) {
			t.Fatal(err)
		}
		install()
		requirePanic(t, value, func() { s.Step() })
		if calls != 2 {
			t.Fatal("repeated reprepare callback")
		}
		if _, err := s.Step(); !errors.Is(err, ErrState) {
			t.Fatal(err)
		}
		if err := s.Reset(); !errors.Is(err, CodeAuth) {
			t.Fatal(err)
		}
		rowTest(t, s)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil {
			t.Fatal("prepare panic leaked a handle:", err)
		}
	})
	t.Run("busy", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "panic.db")
		c1, c2 := openTest(t, path), openTest(t, path)
		script(t, c1, "CREATE TABLE x(a); BEGIN; INSERT INTO x VALUES(1)")
		value := &struct{}{}
		if err := c2.SetBusyHandler(func(int) bool { panic(value) }); err != nil {
			t.Fatal(err)
		}
		requirePanic(t, value, func() { c2.ExecScript("INSERT INTO x VALUES(2)") })
		if err := c1.Rollback(); err != nil {
			t.Fatal(err)
		}
		script(t, c2, "INSERT INTO x VALUES(3)")
	})
	for _, method := range []string{"reset", "reset-clear", "finalize"} {
		t.Run("commit-"+method, func(t *testing.T) {
			c := openTest(t, ":memory:")
			script(t, c, "CREATE TABLE x(a)")
			s := prepareTest(t, c, "INSERT INTO x VALUES(?) RETURNING a")
			if err := s.BindInt64(1, 1); err != nil {
				t.Fatal(err)
			}
			rowTest(t, s)
			value := &struct{}{}
			if err := c.SetCommitHook(func() bool {
				if err := s.Close(); !errors.Is(err, ErrReentrant) {
					t.Error(err)
				}
				panic(value)
			}); err != nil {
				t.Fatal(err)
			}
			requirePanic(t, value, func() {
				if method == "reset" {
					s.Reset()
				} else if method == "reset-clear" {
					s.ResetClearBindings()
				} else {
					s.Close()
				}
			})
			if scalarTest(t, c, "SELECT count(*) FROM x") != 0 {
				t.Fatal("veto did not roll back")
			}
			if method != "finalize" {
				rowTest(t, s) // Reset restored Ready even though the hook panicked.
				if intTest(t, s, 0) != 1 {
					t.Fatal("reset reuse value")
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
				if scalarTest(t, c, "SELECT count(*) FROM x") != 1 {
					t.Fatal("reset reuse did not commit")
				}
			} else {
				if _, err := s.Step(); !errors.Is(err, ErrClosed) {
					t.Fatal(err)
				}
				if err := s.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if err := c.Close(); err != nil {
				t.Fatal("finalization retained child:", err)
			}
		})
	}
	t.Run("close", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE x(a); BEGIN; INSERT INTO x VALUES(1)")
		value := &struct{}{}
		if err := c.SetRollbackHook(func() {
			if err := c.Close(); !errors.Is(err, ErrReentrant) {
				t.Error(err)
			}
			if err := c.Interrupt(); !errors.Is(err, ErrClosed) {
				t.Error(err)
			}
			panic(value)
		}); err != nil {
			t.Fatal(err)
		}
		requirePanic(t, value, func() { c.Close() })
		if err := c.ExecScript("SELECT 1"); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		if err := c.Interrupt(); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
	})
}

func TestCallbackRegistrationFailure(t *testing.T) {
	for _, mode := range []string{"callback-registration", "callback-lifecycle"} {
		cmd := exec.Command("go", "run", "./internal/cmd/faultprobe", mode)
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("%s probe: %v\n%s", mode, err, out)
		}
		t.Logf("%s", out)
	}
}
