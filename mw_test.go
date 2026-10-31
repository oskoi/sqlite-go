//go:build sqlite_multiwriter && unix

package sqlite

import (
	"context"
	"errors"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func openMultiwriterTest(t testing.TB, path string) *Conn {
	t.Helper()
	c, err := OpenMultiwriter(path, 0)
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

func multiwriterTestURI(path, options string) string {
	name := path
	if strings.HasPrefix(name, "//") {
		name = "//" + name
	}
	u := url.URL{Path: name}
	return "file:" + u.EscapedPath() + "?" + options
}

func TestMultiwriterPathsAndPersistence(t *testing.T) {
	for _, path := range []string{"", ":memory:", "file:literal.db", "file:memory?mode=memory", "nul\x00.db"} {
		if c, err := OpenMultiwriter(path, 0); c != nil || !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("path %q: %v %v", path, c, err)
		}
	}
	for _, flags := range []OpenFlags{OpenReadOnly, OpenCreate, OpenReadOnly | OpenCreate, OpenReadOnly | OpenReadWrite, OpenReadWrite | OpenMemory, OpenReadWrite | OpenNoMutex | OpenFullMutex, 0x20000} {
		path := filepath.Join(t.TempDir(), "invalid.db")
		if c, err := OpenMultiwriter(path, flags); c != nil || !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("flags %x: %v %v", flags, c, err)
		}
		if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("invalid flags created file: %v", err)
		}
	}
	missing := filepath.Join(t.TempDir(), "no-create.db")
	if c, err := OpenMultiwriter(missing, OpenReadWrite|OpenFullMutex); c != nil || !errors.Is(err, CodeCantOpen) {
		t.Fatalf("explicit no-create: %v %v", c, err)
	}
	if _, err := os.Stat(missing); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("explicit no-create touched file: %v", err)
	}
	// An ordinary per-file failure must not poison the successful bootstrap.
	created := openMultiwriterTest(t, missing)
	script(t, created, "CREATE TABLE explicit_flags(v); INSERT INTO explicit_flags VALUES(9)")
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	explicit, err := OpenMultiwriter(missing, OpenReadWrite|OpenFullMutex)
	if err != nil {
		t.Fatal(err)
	}
	if got := scalarTest(t, explicit, "SELECT v FROM explicit_flags"); got != 9 {
		t.Fatalf("explicit flags payload %d", got)
	}
	if err := explicit.Close(); err != nil {
		t.Fatal(err)
	}

	const base = "literal%00?#.db"
	for _, relative := range []bool{false, true} {
		t.Run(fmt.Sprintf("relative=%v", relative), func(t *testing.T) {
			actual := filepath.Join(t.TempDir(), base)
			path := actual
			if relative {
				cwd, err := os.Getwd()
				if err != nil {
					t.Fatal(err)
				}
				path, err = filepath.Rel(cwd, actual)
				if err != nil {
					t.Fatal(err)
				}
			}
			c := openMultiwriterTest(t, path)
			script(t, c, "CREATE TABLE payload(v BLOB); INSERT INTO payload VALUES(x'00253f23ff')")
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			if _, err := os.Stat(actual); err != nil {
				t.Fatalf("literal pathname not created: %v", err)
			}
			c = openMultiwriterTest(t, path)
			if got := scalarTest(t, c, "SELECT v = x'00253f23ff' FROM payload"); got != 1 {
				t.Fatal("escaped pathname payload changed")
			}
		})
	}
}

func TestOpenStandardMain(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options string
	}{
		{"ordinary", ""},
		{"passthrough", "vfs=multiwriter&mw=0"},
		{"ignored-stock-option", "mw=1"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "standard.db")
			name := path
			if tt.options != "" {
				name = multiwriterTestURI(path, tt.options)
			}
			c := openTest(t, name)
			script(t, c, "CREATE TABLE payload(v); INSERT INTO payload VALUES(7)")
			if got := scalarTest(t, c, "SELECT v FROM payload"); got != 7 {
				t.Fatalf("standard payload %d", got)
			}
			if err := validateMultiwriter(c); !errors.Is(err, ErrState) {
				t.Fatalf("writable non-private database accepted: %v", err)
			}
			if _, err := c.AutoCommit(); !errors.Is(err, ErrClosed) {
				t.Fatalf("rejected non-private connection not closed: %v", err)
			}
		})
	}
	for _, name := range []string{":memory:", "", "file:private?mode=memory&vfs=multiwriter&mw=1"} {
		c := openTest(t, name)
		script(t, c, "CREATE TABLE payload(v); INSERT INTO payload VALUES(11)")
		if got := scalarTest(t, c, "SELECT v FROM payload"); got != 11 {
			t.Fatalf("temporary/in-memory %q payload %d", name, got)
		}
	}
}

func TestOpenRejectsPrivateAndTrackingMain(t *testing.T) {
	for _, tt := range []struct {
		name    string
		options string
	}{
		{"private", "vfs=multiwriter&mw=1&mw_mp=0&mw_readcheck=1&mw_rebase=0"},
		{"tracking", "vfs=multiwriter&mw=2"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "rejected.db")
			name := multiwriterTestURI(path, tt.options)
			c, err := Open(name, 0)
			if c != nil {
				if closeErr := c.Close(); closeErr != nil {
					t.Error(closeErr)
				}
			}
			if c != nil || !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("standard opener accepted %s main: %v %v", tt.name, c, err)
			}
			// Exercise the same one-time rejection with a retained owner so an
			// ordinary lifecycle operation can prove that the handle was closed.
			raw, err := openNative(name, OpenReadWrite|OpenCreate|OpenURI|OpenNoMutex)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := raw.Close(); err != nil {
					t.Error(err)
				}
			})
			if err := validateStandard(raw); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("actual %s main accepted: %v", tt.name, err)
			}
			if _, err := raw.AutoCommit(); !errors.Is(err, ErrClosed) {
				t.Fatalf("rejected %s connection not closed: %v", tt.name, err)
			}
			reopened := openMultiwriterTest(t, path)
			if err := reopened.BeginImmediate(); err != nil {
				t.Fatal(err)
			}
			script(t, reopened, "CREATE TABLE payload(v); INSERT INTO payload VALUES(19)")
			if err := reopened.Commit(); err != nil {
				t.Fatal(err)
			}
			if err := reopened.Close(); err != nil {
				t.Fatal(err)
			}
			reopened = openMultiwriterTest(t, path)
			if got := scalarTest(t, reopened, "SELECT v FROM payload"); got != 19 {
				t.Fatalf("post-rejection reopened payload %d", got)
			}
		})
	}
}

func TestOpenReadOnlyStandardFallback(t *testing.T) {
	// Upstream deliberately falls back to passthrough for a read-only database
	// that cannot be converted to WAL. Open accepts that actual standard main.
	path := filepath.Join(t.TempDir(), "readonly-delete.db")
	stock := openTest(t, path)
	script(t, stock, "PRAGMA journal_mode=DELETE; CREATE TABLE ro(v); INSERT INTO ro VALUES(3)")
	if err := stock.Close(); err != nil {
		t.Fatal(err)
	}
	fallback, err := Open(multiwriterTestURI(path, "vfs=multiwriter&mw=1"), OpenReadOnly|OpenURI)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fallback.Close(); err != nil {
			t.Error(err)
		}
	})
	if got := scalarTest(t, fallback, "SELECT v FROM ro"); got != 3 {
		t.Fatalf("read-only standard payload %d", got)
	}
	if err := fallback.ExecScript("INSERT INTO ro VALUES(4)"); !errors.Is(err, CodeReadOnly) {
		t.Fatalf("standard fallback must actually be read-only: %v", err)
	}
	if err := validateMultiwriter(fallback); !errors.Is(err, CodeReadOnly) {
		t.Fatalf("read-only standard fallback accepted: %v", err)
	}
	if _, err := fallback.AutoCommit(); !errors.Is(err, ErrClosed) {
		t.Fatalf("rejected fallback not closed: %v", err)
	}
}

func TestMultiwriterReadOnlyFallback(t *testing.T) {
	path := filepath.Join(t.TempDir(), "readonly-wal.db")
	stock := openTest(t, path)
	script(t, stock, "PRAGMA journal_mode=WAL; CREATE TABLE ro(v); INSERT INTO ro VALUES(3)")
	if err := stock.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(path + "-wal"); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("fixture must have no outstanding WAL: %v", err)
	}
	if err := os.Chmod(path, 0400); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.Chmod(path, 0600); err != nil {
			t.Error(err)
		}
	})
	// Root and some filesystems can ignore these permission bits. Do not claim
	// a read-only fallback regression ran when this fixture is still writable.
	if f, err := os.OpenFile(path, os.O_RDWR, 0); err == nil {
		if err := f.Close(); err != nil {
			t.Fatal(err)
		}
		t.Skip("filesystem permits read-write access despite read-only permissions")
	} else if !errors.Is(err, os.ErrPermission) {
		t.Fatal(err)
	}
	for _, flags := range []OpenFlags{OpenReadWrite, 0} {
		c, err := OpenMultiwriter(path, flags)
		if c != nil {
			if closeErr := c.Close(); closeErr != nil {
				t.Error(closeErr)
			}
		}
		if c != nil || !errors.Is(err, CodeReadOnly) {
			t.Fatalf("read-only native fallback, flags %x: %v %v", flags, c, err)
		}
	}
	name := multiwriterTestURI(path, "vfs=multiwriter&mw=1&mw_mp=0&mw_readcheck=1&mw_rebase=0")
	c, err := Open(name, 0)
	if c != nil {
		if closeErr := c.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if c != nil || !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("standard opener accepted read-only private main: %v %v", c, err)
	}
	// Retain an owner only inside this test to assert that the explicit
	// constructor's validator closes a private lane that fell back to read-only.
	fallback, err := openNative(name, OpenReadWrite|OpenCreate|OpenURI|OpenNoMutex)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := fallback.Close(); err != nil {
			t.Error(err)
		}
	})
	if got := scalarTest(t, fallback, "SELECT v FROM ro"); got != 3 {
		t.Fatalf("read-only payload %d", got)
	}
	if err := fallback.ExecScript("INSERT INTO ro VALUES(4)"); !errors.Is(err, CodeReadOnly) {
		t.Fatalf("private fallback must actually be read-only: %v", err)
	}
	if err := validateMultiwriter(fallback); !errors.Is(err, CodeReadOnly) {
		t.Fatalf("private but read-only fallback accepted: %v", err)
	}
	if _, err := fallback.AutoCommit(); !errors.Is(err, ErrClosed) {
		t.Fatalf("rejected read-only private connection not closed: %v", err)
	}
	if err := os.Chmod(path, 0600); err != nil {
		t.Fatal(err)
	}
	reopened := openMultiwriterTest(t, path)
	script(t, reopened, "INSERT INTO ro VALUES(4)")
	if got := scalarTest(t, reopened, "SELECT sum(v) FROM ro"); got != 7 {
		t.Fatalf("post-rejection writable payload %d", got)
	}
}

func TestMultiwriterIndependentTransactions(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transactions.db")
	a := openMultiwriterTest(t, path)
	script(t, a, "CREATE TABLE a(id INTEGER PRIMARY KEY,v); CREATE TABLE b(id INTEGER PRIMARY KEY,v); INSERT INTO a VALUES(1,10); INSERT INTO b VALUES(1,20)")
	b := openMultiwriterTest(t, path)
	if err := a.BeginImmediate(); err != nil {
		t.Fatal(err)
	}
	if err := b.BeginImmediate(); err != nil {
		t.Fatalf("independent immediate transaction: %v", err)
	}
	script(t, a, "UPDATE a SET v=11 WHERE id=1")
	script(t, b, "UPDATE b SET v=21 WHERE id=1")
	if err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := b.Commit(); err != nil {
		t.Fatalf("disjoint existing-table commit: %v", err)
	}
	if got := scalarTest(t, b, "SELECT v FROM a WHERE id=1"); got != 11 {
		t.Fatalf("compatible commit a=%d", got)
	}
	if got := scalarTest(t, a, "SELECT v FROM b WHERE id=1"); got != 21 {
		t.Fatalf("compatible commit b=%d", got)
	}

	// Pin a read snapshot on b before a publishes. Reads on b must not see the
	// winner until that snapshot ends, even though a can commit independently.
	if err := b.Begin(); err != nil {
		t.Fatal(err)
	}
	if got := scalarTest(t, b, "SELECT v FROM a WHERE id=1"); got != 11 {
		t.Fatal(got)
	}
	if err := a.BeginImmediate(); err != nil {
		t.Fatal(err)
	}
	script(t, a, "UPDATE a SET v=12 WHERE id=1")
	if err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	if got := scalarTest(t, b, "SELECT v FROM a WHERE id=1"); got != 11 {
		t.Fatalf("snapshot saw later commit: %d", got)
	}
	if err := b.Rollback(); err != nil {
		t.Fatal(err)
	}
	if got := scalarTest(t, b, "SELECT v FROM a WHERE id=1"); got != 12 {
		t.Fatalf("new snapshot a=%d", got)
	}

	if err := a.BeginImmediate(); err != nil {
		t.Fatal(err)
	}
	if err := b.BeginImmediate(); err != nil {
		t.Fatal(err)
	}
	script(t, a, "UPDATE a SET v=13 WHERE id=1")
	script(t, b, "UPDATE a SET v=99 WHERE id=1")
	if err := a.Commit(); err != nil {
		t.Fatal(err)
	}
	err := b.Commit()
	var native *Error
	if !errors.As(err, &native) || native.Code != CodeBusySnapshot {
		t.Fatalf("same-snapshot conflict: %v", err)
	}
	ac, err := b.AutoCommit()
	if err != nil || !ac {
		t.Fatalf("snapshot conflict must roll back natively: autocommit=%v err=%v", ac, err)
	}
	t.Log("same-row COMMIT: BUSY_SNAPSHOT with native automatic rollback")
	if got := scalarTest(t, b, "SELECT v FROM a WHERE id=1"); got != 13 {
		t.Fatalf("losing write survived: %d", got)
	}
	if err := b.BeginImmediate(); err != nil {
		t.Fatal(err)
	}
	script(t, b, "UPDATE b SET v=22 WHERE id=1")
	if err := b.Rollback(); err != nil {
		t.Fatal(err)
	}
	if err := b.BeginImmediate(); err != nil {
		t.Fatal(err)
	}
	script(t, b, "UPDATE b SET v=23 WHERE id=1")
	if err := b.Commit(); err != nil {
		t.Fatal(err)
	}
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openMultiwriterTest(t, path)
	if got := scalarTest(t, reopened, "SELECT v FROM a WHERE id=1"); got != 13 {
		t.Fatalf("reopened a=%d", got)
	}
	if got := scalarTest(t, reopened, "SELECT v FROM b WHERE id=1"); got != 23 {
		t.Fatalf("reopened b=%d", got)
	}
}

func TestMultiwriterConcurrentFirstOpen(t *testing.T) {
	const childEnv = "SQLITE_GO_MULTIWRITER_FIRST_OPEN"
	if os.Getenv(childEnv) != "1" {
		executable, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, executable, "-test.run=^TestMultiwriterConcurrentFirstOpen$", "-test.count=1")
		cmd.Env = append(os.Environ(), childEnv+"=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("fresh concurrent-first-open process: %v\n%s", err, out)
		}
		return
	}
	// This process has not opened SQLite and never installs the fault allocator:
	// its counters are deliberately non-atomic and must not see concurrent opens.
	dir := t.TempDir()
	const owners = 6
	start := make(chan struct{})
	failures := make(chan error, owners)
	var ready sync.WaitGroup
	ready.Add(owners)
	for i := range owners {
		go func() {
			ready.Done()
			<-start
			failures <- multiwriterFirstOpen(filepath.Join(dir, fmt.Sprintf("owner-%d.db", i)), i)
		}()
	}
	ready.Wait()
	close(start)
	for range owners {
		if err := <-failures; err != nil {
			t.Error(err)
		}
	}
}

func multiwriterFirstOpen(path string, owner int) (err error) {
	opener := Open
	if owner%2 != 0 {
		opener = OpenMultiwriter
	}
	c, err := opener(path, 0)
	if err != nil {
		return fmt.Errorf("owner %d first open: %w", owner, err)
	}
	defer func() { err = errors.Join(err, c.Close()) }()
	if err := c.ExecScript(fmt.Sprintf("CREATE TABLE payload(v); INSERT INTO payload VALUES(%d)", owner+100)); err != nil {
		return err
	}
	if err := c.Close(); err != nil {
		return err
	}
	c, err = opener(path, 0)
	if err != nil {
		return err
	}
	s, _, err := c.Prepare("SELECT v FROM payload", 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	row, err := s.Step()
	if err != nil || !row {
		return fmt.Errorf("owner %d payload row=%v: %v", owner, row, err)
	}
	v, nonNull, err := s.ColumnInt64(0)
	if err != nil || !nonNull || v != int64(owner+100) {
		return fmt.Errorf("owner %d payload=%d nonNull=%v: %v", owner, v, nonNull, err)
	}
	return nil
}

func TestMultiwriterOpenAllocationFailure(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// Unlike a parent -tags flag, the go-run flag below always reaches the probe.
	cmd := exec.CommandContext(ctx, "go", "run", "-tags=sqlite_multiwriter", "./internal/cmd/faultprobe", "multiwriter-open")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("cold-open allocation fault probe: %v\n%s", err, out)
	}
	t.Log(strings.TrimSpace(string(out)))
}
