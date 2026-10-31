// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"
)

func newBackupTest(t testing.TB, dst, src *Conn) *Backup {
	t.Helper()
	b, err := dst.NewBackup("main", src, "main")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := b.Close(); err != nil {
			t.Error(err)
		}
	})
	return b
}
func backupDoneTest(t testing.TB, b *Backup, pages int) {
	t.Helper()
	for {
		done, err := b.Step(pages)
		if err != nil {
			t.Fatal(err)
		}
		if done {
			return
		}
	}
}

func TestBackupReservationAndLifetime(t *testing.T) {
	t.Run("existing-children-and-operations", func(t *testing.T) {
		src, dst := openTest(t, ":memory:"), openTest(t, ":memory:")
		script(t, src, "CREATE TABLE x(id PRIMARY KEY,v); INSERT INTO x VALUES(1,42)")
		old := prepareTest(t, dst, "SELECT ?1")
		if err := old.BindInt64(1, 7); err != nil {
			t.Fatal(err)
		}
		rowTest(t, old)
		s, err := dst.NewSession("main")
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := s.Close(); err != nil {
				t.Error(err)
			}
		})
		b := newBackupTest(t, dst, src)
		for name, fn := range map[string]func() error{
			"dst-close": dst.Close, "src-close": src.Close,
			"prepare":         func() error { _, _, e := dst.Prepare("SELECT 1", 0); return e },
			"script":          func() error { return dst.ExecScript("SELECT 1") },
			"autocommit":      func() error { _, e := dst.AutoCommit(); return e },
			"changes":         func() error { _, e := dst.Changes(); return e },
			"total":           func() error { _, e := dst.TotalChanges(); return e },
			"rowid":           func() error { _, e := dst.LastInsertRowID(); return e },
			"filename":        func() error { _, e := dst.FileName("main"); return e },
			"limit":           func() error { _, e := dst.Limit(LimitLength, -1); return e },
			"status":          func() error { _, _, e := dst.Status(DBStatusLookasideUsed, false); return e },
			"busy-timeout":    func() error { return dst.BusyTimeout(0) },
			"busy-hook":       func() error { return dst.SetBusyHandler(nil) },
			"commit-hook":     func() error { return dst.SetCommitHook(nil) },
			"rollback-hook":   func() error { return dst.SetRollbackHook(nil) },
			"update-hook":     func() error { return dst.SetUpdateHook(nil) },
			"authorizer":      func() error { return dst.SetAuthorizer(nil) },
			"interrupt":       dst.Interrupt,
			"statement-close": old.Close, "statement-reset": old.Reset,
			"statement-step":     func() error { _, e := old.Step(); return e },
			"statement-getter":   func() error { _, _, e := old.ColumnInt64(0); return e },
			"statement-metadata": func() error { _, e := old.ParameterCount(); return e },
			"session-close":      s.Close, "session-attach": s.AttachAll,
			"session-enabled": func() error { _, e := s.Enabled(); return e },
			"session-setter":  func() error { return s.SetEnabled(false) },
			"session-filter":  func() error { return s.SetTableFilter(nil) },
			"session-output":  func() error { return s.WriteChangeset(&bytes.Buffer{}) },
			"session-create":  func() error { _, e := dst.NewSession("main"); return e },
			"second-backup":   func() error { _, e := dst.NewBackup("main", src, "main"); return e },
			"reserved-source": func() error { _, e := src.NewBackup("main", dst, "main"); return e },
		} {
			if err := fn(); !errors.Is(err, CodeBusy) {
				t.Errorf("%s: %v", name, err)
			}
		}
		if scalarTest(t, src, "SELECT v FROM x") != 42 {
			t.Fatal("source unavailable")
		}
		backupDoneTest(t, b, -1)
		if err := dst.Interrupt(); !errors.Is(err, CodeBusy) {
			t.Fatal(err)
		}
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		if err := dst.Interrupt(); err != nil {
			t.Fatal(err)
		}
		if err := old.Reset(); err != nil {
			t.Fatal(err)
		}
		rowTest(t, old)
		if intTest(t, old, 0) != 7 {
			t.Fatal("old statement changed")
		}
		if err := s.AttachAll(); err != nil {
			t.Fatal(err)
		}
		if scalarTest(t, dst, "SELECT v FROM x") != 42 {
			t.Fatal("copy")
		}
	})
	t.Run("failed-init-restores-reservation", func(t *testing.T) {
		src, dst := openTest(t, ":memory:"), openTest(t, ":memory:")
		script(t, dst, "CREATE TABLE old(v); INSERT INTO old VALUES(9)")
		for _, schemas := range [][2]string{{"missing", "main"}, {"main", "missing"}, {"", "main"}, {"main", ""}} {
			if _, _, err := dst.Prepare("SELECT )", 0); !errors.Is(err, CodeError) {
				t.Fatal(err)
			}
			b, err := dst.NewBackup(schemas[0], src, schemas[1])
			if b != nil || !errors.Is(err, CodeError) {
				t.Fatal(b, err)
			}
			var e *Error
			if !errors.As(err, &e) || e.Code != CodeError || e.Offset != -1 {
				t.Fatal("stale init diagnostics", err)
			}
			if err := dst.Interrupt(); err != nil {
				t.Fatal(err)
			}
			if scalarTest(t, dst, "SELECT v FROM old") != 9 {
				t.Fatal("failed init changed data")
			}
		}
		for _, begin := range []string{"BEGIN; SELECT * FROM old", "BEGIN IMMEDIATE"} {
			script(t, dst, begin)
			b, err := dst.NewBackup("main", src, "main")
			if b != nil || !errors.Is(err, CodeError) {
				t.Fatal(b, err)
			}
			if ac, err := dst.AutoCommit(); err != nil || ac {
				t.Fatal(ac, err)
			}
			if err := dst.Rollback(); err != nil {
				t.Fatal(err)
			}
		}
		b := newBackupTest(t, dst, src)
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		if err := dst.Close(); err != nil {
			t.Fatal(err)
		}
		if err := src.Close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("boundaries", func(t *testing.T) {
		src, dst := openTest(t, ":memory:"), openTest(t, ":memory:")
		var zero Conn
		closed := openTest(t, ":memory:")
		if err := closed.Close(); err != nil {
			t.Fatal(err)
		}
		for _, bad := range []*Conn{nil, &zero, closed} {
			if _, err := dst.NewBackup("main", bad, "main"); !errors.Is(err, ErrClosed) {
				t.Fatal(err)
			}
			if _, err := bad.NewBackup("main", src, "main"); !errors.Is(err, ErrClosed) {
				t.Fatal(err)
			}
		}
		if _, err := dst.NewBackup("main", dst, "main"); !errors.Is(err, ErrInvalidArgument) {
			t.Fatal(err)
		}
		for _, names := range [][2]string{{"ma\x00in", "main"}, {"main", "ma\x00in"}} {
			if _, err := dst.NewBackup(names[0], src, names[1]); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal(err)
			}
		}
		for _, b := range []*Backup{nil, {}} {
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := b.Step(1); !errors.Is(err, ErrClosed) {
				t.Fatal(err)
			}
			if _, _, err := b.Progress(); !errors.Is(err, ErrClosed) {
				t.Fatal(err)
			}
		}
		b := newBackupTest(t, dst, src)
		if _, err := b.Step(math.MaxInt32 + 1); !errors.Is(err, ErrRange) {
			t.Fatal(err)
		}
		backupDoneTest(t, b, math.MinInt)
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Step(-1); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		if _, _, err := b.Progress(); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	})
	t.Run("nested-reservation", func(t *testing.T) {
		src, dst, third := openTest(t, ":memory:"), openTest(t, ":memory:"), openTest(t, ":memory:")
		script(t, third, "CREATE TABLE x(v); INSERT INTO x VALUES(42)")
		first := newBackupTest(t, dst, src)
		second := newBackupTest(t, src, third)
		if _, err := first.Step(1); !errors.Is(err, CodeBusy) {
			t.Fatal(err)
		}
		if _, _, err := first.Progress(); !errors.Is(err, CodeBusy) {
			t.Fatal(err)
		}
		if err := first.Close(); !errors.Is(err, CodeBusy) {
			t.Fatal(err)
		}
		backupDoneTest(t, second, -1)
		if err := second.Close(); err != nil {
			t.Fatal(err)
		}
		backupDoneTest(t, first, -1)
		if err := first.Close(); err != nil {
			t.Fatal(err)
		}
		if scalarTest(t, dst, "SELECT v FROM x") != 42 {
			t.Fatal("nested copy")
		}
	})
	t.Run("isolated-interrupt-reservation", func(t *testing.T) {
		if os.Getenv("SQLITE_GO_BACKUP_PROBE") != "1" {
			ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
			defer cancel()
			bin, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			cmd := exec.CommandContext(ctx, bin, "-test.run=^TestBackupReservationAndLifetime/isolated-interrupt-reservation$", "-test.count=1")
			cmd.Env = append(os.Environ(), "SQLITE_GO_BACKUP_PROBE=1")
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("backup/interrupt probe: %v\n%s", err, out)
			}
			return
		}
		for range 100 {
			src, dst := openTest(t, ":memory:"), openTest(t, ":memory:")
			ready, stop := make(chan struct{}, 4), make(chan struct{})
			var wg sync.WaitGroup
			for range 4 {
				wg.Go(func() {
					if err := dst.Interrupt(); err != nil {
						t.Error(err)
					}
					ready <- struct{}{}
					for {
						select {
						case <-stop:
							return
						default:
						}
						if err := dst.Interrupt(); err != nil && !errors.Is(err, CodeBusy) {
							t.Error(err)
							return
						}
						runtime.Gosched()
					}
				})
			}
			for range 4 {
				<-ready
			}
			b := newBackupTest(t, dst, src)
			if err := dst.Interrupt(); !errors.Is(err, CodeBusy) {
				t.Fatal(err)
			}
			close(stop)
			wg.Wait()
			backupDoneTest(t, b, -1)
			if err := b.Close(); err != nil {
				t.Fatal(err)
			}
			if err := dst.Interrupt(); err != nil {
				t.Fatal(err)
			}
			if err := dst.Close(); err != nil {
				t.Fatal(err)
			}
			if err := src.Close(); err != nil {
				t.Fatal(err)
			}
		}
	})
}

func TestBackupProgressAndRecovery(t *testing.T) {
	t.Run("content-and-progress", testBackupContentAndProgress)
	t.Run("source-and-external-mutation", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "source.db")
		src, dst := openTest(t, path), openTest(t, ":memory:")
		script(t, src, `CREATE TABLE x(id INTEGER PRIMARY KEY,v BLOB); WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<128) INSERT INTO x SELECT x,zeroblob(1000) FROM n`)
		external := openTest(t, path)
		b := newBackupTest(t, dst, src)
		if done, err := b.Step(1); done || err != nil {
			t.Fatal(done, err)
		}
		before, total, err := b.Progress()
		if err != nil || before <= 1 || total <= 2 {
			t.Fatal(before, total, err)
		}
		script(t, src, "UPDATE x SET v=x'000102' WHERE id=1; INSERT INTO x VALUES(129,x'ffff')")
		if remaining, count, err := b.Progress(); err != nil || remaining != before || count != total {
			t.Fatal(remaining, count, err)
		}
		if done, err := b.Step(2); done || err != nil {
			t.Fatal(done, err)
		}
		script(t, external, "UPDATE x SET v=x'030004' WHERE id=2; INSERT INTO x VALUES(130,x'ff00')")
		backupDoneTest(t, b, 3)
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		left, right := prepareTest(t, src, "SELECT id,v FROM x ORDER BY id"), prepareTest(t, dst, "SELECT id,v FROM x ORDER BY id")
		for id := int64(1); id <= 130; id++ {
			rowTest(t, left)
			rowTest(t, right)
			if intTest(t, left, 0) != id || intTest(t, right, 0) != id {
				t.Fatal("row id", id)
			}
			a, err := left.ColumnBlob(1)
			if err != nil {
				t.Fatal(err)
			}
			z, err := right.ColumnBlob(1)
			if err != nil || !bytes.Equal(a, z) {
				t.Fatal("row payload", id, err)
			}
		}
		for _, s := range []*Stmt{left, right} {
			if row, err := s.Step(); row || err != nil {
				t.Fatal(row, err)
			}
		}
	})
	t.Run("busy-retry-and-finish", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "busy.db")
		src, dst := openTest(t, path), openTest(t, ":memory:")
		script(t, src, "CREATE TABLE x(v); INSERT INTO x VALUES(42)")
		lock := openTest(t, path)
		calls := 0
		if err := src.SetBusyHandler(func(attempt int) bool {
			if attempt != 0 {
				t.Error(attempt)
			}
			calls++
			return false
		}); err != nil {
			t.Fatal(err)
		}
		script(t, lock, "BEGIN EXCLUSIVE")
		b := newBackupTest(t, dst, src)
		if done, err := b.Step(1); done || !errors.Is(err, CodeBusy) {
			t.Fatal(done, err)
		}
		if calls != 1 {
			t.Fatal("hidden retries", calls)
		}
		if _, _, err := b.Progress(); err != nil {
			t.Fatal(err)
		}
		if err := lock.Rollback(); err != nil {
			t.Fatal(err)
		}
		backupDoneTest(t, b, -1)
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		if scalarTest(t, dst, "SELECT v FROM x") != 42 {
			t.Fatal("retry copy")
		}
		// This pinned engine actually returns BUSY for an active source write
		// and echoes the final BUSY from finish, unlike its header's prose.
		script(t, src, "BEGIN IMMEDIATE; INSERT INTO x VALUES(9)")
		b = newBackupTest(t, dst, src)
		if _, err := b.Step(-1); !errors.Is(err, CodeBusy) {
			t.Fatal(err)
		}
		if err := b.Close(); !errors.Is(err, CodeBusy) {
			t.Fatal(err)
		}
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		if _, _, err := b.Progress(); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		if err := src.Rollback(); err != nil {
			t.Fatal(err)
		}
		if scalarTest(t, dst, "SELECT count(*) FROM x") != 1 {
			t.Fatal("busy close changed destination")
		}
		b = newBackupTest(t, dst, src)
		backupDoneTest(t, b, -1)
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("early-rollback", func(t *testing.T) {
		src, dst := openTest(t, ":memory:"), openTest(t, ":memory:")
		script(t, src, "CREATE TABLE large(v); INSERT INTO large VALUES(zeroblob(64000))")
		script(t, dst, "CREATE TABLE old(v); INSERT INTO old VALUES(7)")
		b := newBackupTest(t, dst, src)
		if done, err := b.Step(1); done || err != nil {
			t.Fatal(done, err)
		}
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		if scalarTest(t, dst, "SELECT v FROM old") != 7 || scalarTest(t, dst, "SELECT count(*) FROM sqlite_schema WHERE name='large'") != 0 {
			t.Fatal("early finish did not restore old schema/data")
		}
		b = newBackupTest(t, dst, src)
		backupDoneTest(t, b, -1)
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		if scalarTest(t, dst, "SELECT length(v) FROM large") != 64000 {
			t.Fatal("reuse")
		}
	})
	t.Run("fatal-readonly-consumption", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "readonly.db")
		seed := openTest(t, path)
		script(t, seed, "CREATE TABLE old(v); INSERT INTO old VALUES(7)")
		if err := seed.Close(); err != nil {
			t.Fatal(err)
		}
		dst, err := Open(path, OpenReadOnly)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			if err := dst.Close(); err != nil {
				t.Error(err)
			}
		})
		src := openTest(t, ":memory:")
		script(t, src, "CREATE TABLE x(v); INSERT INTO x VALUES(42)")
		_, _, stale := dst.Prepare("SELECT )", 0)
		var old *Error
		if !errors.As(stale, &old) || old.Offset < 0 {
			t.Fatal(stale)
		}
		b := newBackupTest(t, dst, src)
		_, stepErr := b.Step(-1)
		var e *Error
		if !errors.Is(stepErr, CodeReadOnly) || !errors.As(stepErr, &e) || e.Code != CodeReadOnly || e.Offset != -1 {
			t.Fatal(stepErr)
		}
		if _, err := b.Step(1); !errors.Is(err, ErrState) {
			t.Fatal(err)
		}
		if _, _, err := b.Progress(); err != nil {
			t.Fatal(err)
		}
		finishErr := b.Close()
		if !errors.Is(finishErr, CodeReadOnly) || !errors.As(finishErr, &e) || e.Code != CodeReadOnly || e.Offset != -1 {
			t.Fatal(finishErr)
		}
		if err := b.Close(); err != nil {
			t.Fatal(err)
		}
		if _, err := b.Step(1); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		if scalarTest(t, dst, "SELECT v FROM old") != 7 {
			t.Fatal("failed backup changed destination")
		}
		if err := dst.Close(); err != nil {
			t.Fatal(err)
		}
		if err := src.Close(); err != nil {
			t.Fatal(err)
		}
		if !errors.Is(stepErr, CodeReadOnly) || !errors.Is(finishErr, CodeReadOnly) {
			t.Fatal("lost snapshots")
		}
	})
	t.Run("native-allocation-failures", func(t *testing.T) {
		for _, mode := range []string{"backup-init", "backup-step", "backup-init-reservation"} {
			ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
			cmd := exec.CommandContext(ctx, "go", "run", "./internal/cmd/faultprobe", mode)
			out, err := cmd.CombinedOutput()
			cancel()
			if err != nil {
				t.Fatalf("%s: %v\n%s", mode, err, out)
			}
			t.Log(strings.TrimSpace(string(out)))
		}
	})
}

func TestBackupCallbacksAndPanic(t *testing.T) {
	// Native backup does not execute SQL, invoke authorizers, or fire commit,
	// update or session filters. Real source/destination lock contention does
	// invoke their busy handlers, so exercise only those actual callback paths.
	for _, owner := range []string{"source", "destination"} {
		for _, panics := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/panic%v", owner, panics), func(t *testing.T) {
				if os.Getenv("SQLITE_GO_BACKUP_CALLBACK_PROBE") != "1" {
					ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
					defer cancel()
					bin, err := os.Executable()
					if err != nil {
						t.Fatal(err)
					}
					cmd := exec.CommandContext(ctx, bin, "-test.run=^TestBackupCallbacksAndPanic/"+owner+"/panic"+fmt.Sprint(panics)+"$", "-test.count=1")
					cmd.Env = append(os.Environ(), "SQLITE_GO_BACKUP_CALLBACK_PROBE=1")
					if out, err := cmd.CombinedOutput(); err != nil {
						t.Fatalf("callback probe: %v\n%s", err, out)
					}
					return
				}
				dir := t.TempDir()
				src, dst := openTest(t, filepath.Join(dir, "src.db")), openTest(t, filepath.Join(dir, "dst.db"))
				script(t, src, "CREATE TABLE x(id PRIMARY KEY,v); INSERT INTO x VALUES(1,42)")
				script(t, dst, "CREATE TABLE x(id PRIMARY KEY,v); INSERT INTO x VALUES(1,7)")
				old := prepareTest(t, dst, "SELECT 1")
				var sessions []*Session
				updates, filters, calls := 0, 0, 0
				for _, c := range []*Conn{src, dst} {
					if err := c.SetUpdateHook(func(UpdateEvent) { updates++ }); err != nil {
						t.Fatal(err)
					}
					s, err := c.NewSession("main")
					if err != nil {
						t.Fatal(err)
					}
					sessions = append(sessions, s)
					t.Cleanup(func() {
						if err := s.Close(); err != nil {
							t.Error(err)
						}
					})
					if err := s.SetTableFilter(func(string) bool { filters++; return true }); err != nil {
						t.Fatal(err)
					}
					if err := s.AttachAll(); err != nil {
						t.Fatal(err)
					}
				}
				locked, path := src, filepath.Join(dir, "src.db")
				if owner == "destination" {
					locked, path = dst, filepath.Join(dir, "dst.db")
				}
				locker := openTest(t, path)
				script(t, locker, "BEGIN EXCLUSIVE")
				var b *Backup
				value := &struct{ value string }{"first backup panic"}
				independent := openTest(t, ":memory:")
				if err := locked.SetBusyHandler(func(attempt int) bool {
					calls++
					if attempt != 0 {
						t.Error(attempt)
					}
					for name, fn := range map[string]func() error{
						"source-close": src.Close, "destination-close": dst.Close,
						"source-sql":           func() error { return src.ExecScript("SELECT 1") },
						"destination-metadata": func() error { _, e := dst.AutoCommit(); return e },
						"old-child":            old.Reset,
						"session":              func() error { _, e := sessions[0].Enabled(); return e },
						"backup-close":         func() error { return b.Close() },
						"backup-step":          func() error { _, e := b.Step(1); return e },
						"backup-progress":      func() error { _, _, e := b.Progress(); return e },
						"set-hook":             func() error { return src.SetUpdateHook(nil) },
					} {
						if err := fn(); !errors.Is(err, ErrReentrant) {
							t.Errorf("%s: %v", name, err)
						}
					}
					// This would deadlock if the lifetime gate crossed native C.
					if err := dst.Interrupt(); !errors.Is(err, CodeBusy) {
						t.Error(err)
					}
					if scalarTest(t, independent, "SELECT 42") != 42 {
						t.Error("independent owner")
					}
					if panics {
						panic(value)
					}
					return false
				}); err != nil {
					t.Fatal(err)
				}
				b = newBackupTest(t, dst, src)
				if panics {
					func() {
						defer func() {
							if got := recover(); got != value {
								t.Errorf("panic=%#v", got)
							}
						}()
						b.Step(-1)
						t.Error("did not repanic")
					}()
					if _, _, err := b.Progress(); !errors.Is(err, ErrClosed) {
						t.Fatal(err)
					}
					if _, err := b.Step(1); !errors.Is(err, ErrClosed) {
						t.Fatal(err)
					}
					if err := b.Close(); err != nil {
						t.Fatal(err)
					}
				} else {
					if _, err := b.Step(-1); !errors.Is(err, CodeBusy) {
						t.Fatal(err)
					}
				}
				if calls != 1 {
					t.Fatal("callbacks/retries", calls)
				}
				if err := locker.Rollback(); err != nil {
					t.Fatal(err)
				}
				if !panics {
					backupDoneTest(t, b, -1)
					if err := b.Close(); err != nil {
						t.Fatal(err)
					}
				}
				for _, c := range []*Conn{src, dst} {
					script(t, c, "INSERT INTO x VALUES(2,9)")
				}
				if panics && (updates != 0 || filters != 0) {
					t.Fatal("both-owner panic cleanup", updates, filters)
				}
				if !panics && (updates != 2 || filters != 2) {
					t.Fatal("ordinary error lost registrations", updates, filters)
				}
				for _, s := range sessions {
					if enabled, err := s.Enabled(); err != nil || !enabled {
						t.Fatal(enabled, err)
					}
				}
				if err := old.Close(); err != nil {
					t.Fatal(err)
				}
				for _, s := range sessions {
					if err := s.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if err := src.Close(); err != nil {
					t.Fatal(err)
				}
				if err := dst.Close(); err != nil {
					t.Fatal(err)
				}
			})
		}
	}
}
