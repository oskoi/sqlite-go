// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"
)

var (
	_ io.ReadWriteSeeker = (*Blob)(nil)
	_ io.ReaderAt        = (*Blob)(nil)
	_ io.WriterAt        = (*Blob)(nil)
	_ io.Closer          = (*Blob)(nil)
)

func openBlobTest(t testing.TB, c *Conn, row int64, writable bool) *Blob {
	t.Helper()
	b, err := c.OpenBlob("main", "x", "a", row, writable)
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
func blobPositionTest(t testing.TB, b *Blob, want int64) {
	t.Helper()
	if got, err := b.Seek(0, io.SeekCurrent); err != nil || got != want {
		t.Fatal(got, err, "want position", want)
	}
}
func blobMetadataTest(t testing.TB, b *Blob, size, row int64) {
	t.Helper()
	if got, err := b.Size(); err != nil || got != size {
		t.Fatal(got, err, "want size", size)
	}
	if got, err := b.RowID(); err != nil || got != row {
		t.Fatal(got, err, "want row", row)
	}
}
func blobBytesTest(t testing.TB, c *Conn, row int64, want []byte) {
	t.Helper()
	s := prepareTest(t, c, fmt.Sprintf("SELECT a FROM x WHERE rowid=%d", row))
	rowTest(t, s)
	got, err := s.ColumnBlob(0)
	if err != nil || !bytes.Equal(got, want) {
		t.Fatalf("bytes %q want %q: %v", got, want, err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
}
func blobCalls(b *Blob) map[string]func() error {
	return map[string]func() error{
		"read":       func() error { _, e := b.Read(make([]byte, 1)); return e },
		"read-at":    func() error { _, e := b.ReadAt(make([]byte, 1), 0); return e },
		"write":      func() error { _, e := b.Write([]byte{1}); return e },
		"write-at":   func() error { _, e := b.WriteAt([]byte{1}, 0); return e },
		"read-zero":  func() error { _, e := b.Read(nil); return e },
		"write-zero": func() error { _, e := b.Write(nil); return e },
		"seek":       func() error { _, e := b.Seek(0, io.SeekStart); return e },
		"reopen":     func() error { return b.Reopen(1) },
	}
}

func TestBlobNativeRestrictions(t *testing.T) {
	c := openTest(t, ":memory:")
	script(t, c, "CREATE TABLE x(a); INSERT INTO x VALUES(x'010203'),(1),(1.5),(NULL); CREATE TABLE nr(id PRIMARY KEY,a) WITHOUT ROWID; INSERT INTO nr VALUES(1,'abc'); CREATE INDEX indexed ON x(a)")
	for _, input := range []struct {
		table, column string
		row           int64
		write         bool
	}{
		{"x", "a", 1, true}, {"x", "a", 2, false}, {"x", "a", 3, false}, {"x", "a", 4, false}, {"x", "a", 99, false}, {"nr", "a", 1, false}, {"missing", "a", 1, false}, {"x", "missing", 1, false},
	} {
		if _, _, err := c.Prepare("SELECT )", 0); !errors.Is(err, CodeError) {
			t.Fatal(err)
		}
		b, err := c.OpenBlob("main", input.table, input.column, input.row, input.write)
		var e *Error
		if b != nil || !errors.Is(err, CodeError) || !errors.As(err, &e) || e.Offset != -1 {
			t.Fatal(input, b, err)
		}
	}
	for _, names := range [][3]string{{"ma\x00in", "x", "a"}, {"main", "x\x00", "a"}, {"main", "x", "a\x00"}} {
		if _, err := c.OpenBlob(names[0], names[1], names[2], 1, false); !errors.Is(err, ErrInvalidArgument) {
			t.Fatal(err)
		}
	}
	b := openBlobTest(t, c, 1, false)
	if _, _, err := c.Prepare("SELECT )", 0); !errors.Is(err, CodeError) {
		t.Fatal(err)
	}
	if n, err := b.WriteAt([]byte{9}, 0); n != 0 || !errors.Is(err, CodeReadOnly) {
		t.Fatal(n, err)
	}
	out := make([]byte, 3)
	if n, err := b.Read(out); n != 3 || err != nil || !bytes.Equal(out, []byte{1, 2, 3}) {
		t.Fatal(n, err, out)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	blobBytesTest(t, c, 1, []byte{1, 2, 3})
	path := filepath.Join(t.TempDir(), "readonly.db")
	seed := openTest(t, path)
	script(t, seed, "CREATE TABLE x(a); INSERT INTO x VALUES(x'010203')")
	if err := seed.Close(); err != nil {
		t.Fatal(err)
	}
	readonly, err := Open(path, OpenReadOnly)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := readonly.Close(); err != nil {
			t.Error(err)
		}
	})
	if b, err := readonly.OpenBlob("main", "x", "a", 1, true); b != nil || !errors.Is(err, CodeReadOnly) {
		t.Fatal(b, err)
	}
	rb := openBlobTest(t, readonly, 1, false)
	if n, err := rb.Read(make([]byte, 3)); n != 3 || err != nil {
		t.Fatal(n, err)
	}
}

func TestBlobBackupExclusion(t *testing.T) {
	src, dst := openTest(t, ":memory:"), openTest(t, ":memory:")
	script(t, src, "CREATE TABLE copied(v); INSERT INTO copied VALUES(42)")
	script(t, dst, "ATTACH ':memory:' AS cells; CREATE TABLE cells.x(a); INSERT INTO cells.x VALUES(x'010203')")
	blob, err := dst.OpenBlob("cells", "x", "a", 1, false)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := blob.Close(); err != nil {
			t.Error(err)
		}
	})
	backup := newBackupTest(t, dst, src)
	calls := blobCalls(blob)
	calls["size"] = func() error { _, e := blob.Size(); return e }
	calls["rowid"] = func() error { _, e := blob.RowID(); return e }
	calls["close"] = blob.Close
	calls["open"] = func() error { _, e := dst.OpenBlob("main", "x", "a", 1, false); return e }
	for name, fn := range calls {
		if err := fn(); !errors.Is(err, CodeBusy) {
			t.Error(name, err)
		}
	}
	// Keep a blob in an attached schema: SQLite itself refuses backup init
	// with an active cursor on its target btree. Reservation covers all schemas.
	if err := backup.Close(); err != nil {
		t.Fatal(err)
	}
	blobMetadataTest(t, blob, 3, 1)
	blobPositionTest(t, blob, 0)
	if err := blob.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestBlobCallbacksAndPanic(t *testing.T) {
	for _, mode := range []string{"open-busy", "open-busy-panic", "close-commit", "close-commit-panic", "write-filter", "write-filter-panic"} {
		t.Run(mode, func(t *testing.T) {
			if os.Getenv("SQLITE_GO_BLOB_CALLBACK_PROBE") != "1" {
				ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
				defer cancel()
				bin, err := os.Executable()
				if err != nil {
					t.Fatal(err)
				}
				cmd := exec.CommandContext(ctx, bin, "-test.run=^TestBlobCallbacksAndPanic/"+mode+"$", "-test.count=1")
				cmd.Env = append(os.Environ(), "SQLITE_GO_BLOB_CALLBACK_PROBE=1")
				if out, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("callback probe %v\n%s", err, out)
				}
				return
			}
			path := filepath.Join(t.TempDir(), "blob.db")
			c := openTest(t, path)
			script(t, c, "CREATE TABLE x(id INTEGER PRIMARY KEY,a); INSERT INTO x VALUES(1,x'010203')")
			old := prepareTest(t, c, "SELECT 42")
			independent := openTest(t, ":memory:")
			var b *Blob
			calls, later := 0, 0
			value := &struct{ mode string }{mode}
			panics := mode == "open-busy-panic" || mode == "close-commit-panic" || mode == "write-filter-panic"
			callback := func() {
				calls++
				checks := blobCalls(b)
				checks["size"] = func() error { _, e := b.Size(); return e }
				checks["rowid"] = func() error { _, e := b.RowID(); return e }
				checks["close-parent"], checks["statement"] = c.Close, old.Reset
				checks["open"] = func() error { _, e := c.OpenBlob("main", "x", "a", 1, true); return e }
				checks["blob-close"] = b.Close
				for name, fn := range checks {
					// During construction no blob has escaped yet.
					if b == nil && name != "close-parent" && name != "statement" && name != "open" {
						continue
					}
					if err := fn(); !errors.Is(err, ErrReentrant) {
						t.Error(name, err)
					}
				}
				if scalarTest(t, independent, "SELECT 42") != 42 {
					t.Error("independent owner")
				}
				if panics {
					panic(value)
				}
			}
			var session *Session
			var invoke func()
			if mode == "open-busy" || mode == "open-busy-panic" {
				locker := openTest(t, path)
				script(t, locker, "BEGIN EXCLUSIVE")
				if err := c.SetBusyHandler(func(int) bool { callback(); return false }); err != nil {
					t.Fatal(err)
				}
				invoke = func() {
					got, err := c.OpenBlob("main", "x", "a", 1, true)
					if got != nil || !errors.Is(err, CodeBusy) {
						t.Fatal(got, err)
					}
				}
				t.Cleanup(func() {
					if err := locker.Rollback(); err != nil {
						t.Error(err)
					}
				})
			} else {
				b = openBlobTest(t, c, 1, true)
				if mode == "close-commit" || mode == "close-commit-panic" {
					if n, err := b.Write([]byte{9}); n != 1 || err != nil {
						t.Fatal(n, err)
					}
					if err := c.SetCommitHook(func() bool { callback(); return false }); err != nil {
						t.Fatal(err)
					}
					if err := c.SetRollbackHook(func() { later++ }); err != nil {
						t.Fatal(err)
					}
					invoke = func() {
						if err := b.Close(); err != nil {
							t.Fatal(err)
						}
					}
				} else {
					var err error
					session, err = c.NewSession("main")
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						if err := session.Close(); err != nil {
							t.Error(err)
						}
					})
					if err := session.SetTableFilter(func(string) bool { callback(); return true }); err != nil {
						t.Fatal(err)
					}
					if err := session.AttachAll(); err != nil {
						t.Fatal(err)
					}
					invoke = func() {
						if n, err := b.WriteAt([]byte{9}, 0); n != 1 || err != nil {
							t.Fatal(n, err)
						}
					}
				}
			}
			if panics {
				requirePanic(t, value, invoke)
			} else {
				invoke()
			}
			if calls != 1 || later != 0 {
				t.Fatal("callbacks", calls, later)
			}
			if mode == "write-filter" || mode == "write-filter-panic" {
				blobPositionTest(t, b, 0)
				buf := make([]byte, 3)
				if n, err := b.ReadAt(buf, 0); n != 3 || err != nil || !bytes.Equal(buf, []byte{9, 2, 3}) {
					t.Fatal("positional native bytes", n, err, buf)
				}
				blobPositionTest(t, b, 0)
			}
			if mode == "close-commit" || mode == "close-commit-panic" {
				if _, err := b.Size(); !errors.Is(err, ErrClosed) {
					t.Fatal(err)
				}
				if err := b.Close(); err != nil {
					t.Fatal(err)
				}
				want := []byte{9, 2, 3}
				if panics {
					want[0] = 1
				}
				blobBytesTest(t, c, 1, want)
			}
			if panics {
				// Recovered owners are reusable; all registrations were removed.
				if err := old.Reset(); err != nil {
					t.Fatal(err)
				}
				if b != nil {
					if err := b.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if session != nil {
					if err := session.Close(); err != nil {
						t.Fatal(err)
					}
				}
				if mode != "open-busy-panic" {
					script(t, c, "UPDATE x SET a=x'040506'")
					if calls != 1 || later != 0 {
						t.Fatal("registration survived panic")
					}
				}
			}
		})
	}
}

func TestBlobSequentialWritePanicPosition(t *testing.T) {
	if os.Getenv("SQLITE_GO_BLOB_SEQUENTIAL_PANIC_PROBE") != "1" {
		ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
		defer cancel()
		bin, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.CommandContext(ctx, bin, "-test.run=^TestBlobSequentialWritePanicPosition$", "-test.count=1")
		cmd.Env = append(os.Environ(), "SQLITE_GO_BLOB_SEQUENTIAL_PANIC_PROBE=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("sequential panic probe: %v\n%s", err, out)
		}
		return
	}
	c := openTest(t, ":memory:")
	script(t, c, "CREATE TABLE x(id INTEGER PRIMARY KEY,a); INSERT INTO x VALUES(1,x'010203')")
	b := openBlobTest(t, c, 1, true)
	session, err := c.NewSession("main")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := session.Close(); err != nil {
			t.Error(err)
		}
	})
	value := &struct{ message string }{"sequential blob filter panic"}
	calls := 0
	if err := session.SetTableFilter(func(string) bool {
		calls++
		if _, err := b.Seek(0, io.SeekCurrent); !errors.Is(err, ErrReentrant) {
			t.Error(err)
		}
		panic(value)
	}); err != nil {
		t.Fatal(err)
	}
	if err := session.AttachAll(); err != nil {
		t.Fatal(err)
	}
	requirePanic(t, value, func() { b.Write([]byte{9}) })
	if calls != 1 {
		t.Fatal("filter calls", calls)
	}
	buf := make([]byte, 3)
	if n, err := b.ReadAt(buf, 0); n != 3 || err != nil || !bytes.Equal(buf, []byte{9, 2, 3}) {
		t.Fatal("native bytes after panic", n, err, buf)
	}
	blobPositionTest(t, b, 1)
	if n, err := b.Write([]byte{8}); n != 1 || err != nil {
		t.Fatal(n, err)
	}
	blobPositionTest(t, b, 2)
	if calls != 1 {
		t.Fatal("filter survived panic", calls)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	blobBytesTest(t, c, 1, []byte{9, 8, 3})
}
