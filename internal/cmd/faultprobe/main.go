// Command faultprobe runs test-only native allocator failures in a fresh process.
// It deliberately exits without restoring SQLite's process-global allocator.
package main

/*
#include "../../../sqlite3.h"
static sqlite3_mem_methods original;
static int failing;
static int failed_allocations;
static int live_blocks;
static int pause_allocation;
extern void probe_go_allocation(void);
static void *probe_malloc(int n) {
 if(pause_allocation) { pause_allocation=0; probe_go_allocation(); }
 if(failing) failed_allocations++;
 void *p = failing ? 0 : original.xMalloc(n);
 if(p) live_blocks++;
 return p;
}
static void *probe_realloc(void *p, int n) {
 if(failing) failed_allocations++;
 void *q = failing ? 0 : original.xRealloc(p,n);
 if(q && !p) live_blocks++;
 return q;
}
static void probe_free(void *p) { if(p) live_blocks--; original.xFree(p); }
static int probe_live(void) { return live_blocks; }
static int probe_config(void) {
 int rc=sqlite3_config(SQLITE_CONFIG_GETMALLOC,&original);
 if(rc!=SQLITE_OK) return rc;
 sqlite3_mem_methods methods=original;
 methods.xMalloc=probe_malloc;
 methods.xRealloc=probe_realloc;
 methods.xFree=probe_free;
 rc=sqlite3_config(SQLITE_CONFIG_MALLOC,&methods);
 if(rc!=SQLITE_OK) return rc;
 // Ensure small conversion allocations reach the injected allocator.
 return sqlite3_config(SQLITE_CONFIG_LOOKASIDE,0,0);
}
static void probe_arm(void) { failed_allocations=0; failing=1; }
static void probe_disarm(void) { failing=0; }
static void probe_pause(void) { pause_allocation=1; }
static int probe_failures(void) { return failed_allocations; }
*/
import "C"

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"weak"

	sqlite "github.com/oskoi/sqlite-go"
)

func run(mode string) error {
	// These static accessors must not initialize SQLite or prevent configuration.
	sqlite.Version()
	sqlite.SourceID()
	sqlite.CompileOptions()
	if rc := C.probe_config(); rc != C.SQLITE_OK {
		return fmt.Errorf("allocator config: %d", rc)
	}
	if mode == "init" {
		C.probe_arm()
		c, err := sqlite.Open(":memory:", 0)
		if c != nil || !errors.Is(err, sqlite.CodeNoMem) {
			return fmt.Errorf("constructor: %v %v", c, err)
		}
		fmt.Printf("init: native NOMEM, no package-init panic\n")
		return nil
	}
	if mode == "multiwriter-open" {
		// Initialize SQLite without warming up the wrapper bootstrap. This is
		// a generic cold-open failure, not a targeted hook allocation fault.
		if rc := C.sqlite3_initialize(); rc != C.SQLITE_OK {
			return fmt.Errorf("SQLite initialization: %d", rc)
		}
		dir, err := os.MkdirTemp("", "sqlite-go-mw-open-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		path := filepath.Join(dir, "retry.db")
		C.probe_arm()
		failed, first := sqlite.OpenMultiwriter(path, 0)
		C.probe_disarm()
		if failed != nil || !errors.Is(first, sqlite.CodeNoMem) {
			return fmt.Errorf("cold Multiwriter open: %v %v (requires sqlite_multiwriter on Unix)", failed, first)
		}
		if C.probe_failures() == 0 {
			return fmt.Errorf("cold-open allocation failure was not exercised")
		}
		ordinary, err := sqlite.Open(filepath.Join(dir, "ordinary.db"), 0)
		if err != nil {
			return fmt.Errorf("ordinary open after allocator disarm: %w", err)
		}
		if err := ordinary.Close(); err != nil {
			return err
		}
		for pass := range 2 {
			c, err := sqlite.OpenMultiwriter(path, 0)
			if err != nil {
				return fmt.Errorf("Multiwriter retry/reopen %d: %w", pass, err)
			}
			if pass == 0 {
				if err := c.ExecScript("CREATE TABLE payload(id INTEGER PRIMARY KEY,v TEXT NOT NULL,b BLOB NOT NULL); INSERT INTO payload VALUES(1,'recovered 雪',x'00253f23ff'),(2,'second Å',x'')"); err != nil {
					return errors.Join(err, c.Close())
				}
			}
			if err := checkMultiwriterOpenPayload(c); err != nil {
				return errors.Join(err, c.Close())
			}
			if err := c.Close(); err != nil {
				return err
			}
		}
		fmt.Println("multiwriter-open: generic cold-open NOMEM; ordinary and explicit opens recover after allocator disarm; exact values survive close/reopen")
		return nil
	}
	c, err := sqlite.Open(":memory:", 0)
	if err != nil {
		return err
	}
	if mode == "snapshot-copy" {
		if err := c.ExecScript("CREATE TABLE snapshot(id PRIMARY KEY)"); err != nil {
			return err
		}
		s, _, err := c.Prepare("INSERT INTO snapshot VALUES(1)", 0)
		if err != nil {
			return err
		}
		if err := c.SetCommitHook(func() bool {
			C.probe_arm()
			return true
		}); err != nil {
			return err
		}
		_, err = s.Exec()
		C.probe_disarm()
		var snapshot *sqlite.Error
		if !errors.As(err, &snapshot) || snapshot.Code != sqlite.CodeConstraintCommitHook || snapshot.Message != snapshot.Code.Error() || snapshot.Offset != -1 {
			return fmt.Errorf("message-copy OOM replaced original extended error: %v", err)
		}
		if C.probe_failures() == 0 {
			return fmt.Errorf("message-copy allocation failure not exercised")
		}
		if err := c.SetCommitHook(nil); err != nil {
			return err
		}
		if _, err := s.Exec(); err != nil {
			return fmt.Errorf("veto rollback or statement reuse: %w", err)
		}
		if err := errors.Join(s.Close(), c.Close()); err != nil {
			return err
		}
		fmt.Println("snapshot-copy: extended commit-hook constraint survives message-copy OOM; static fallback, rollback, statement reuse")
		return nil
	}
	if mode == "backup-init" || mode == "backup-step" || mode == "backup-init-reservation" {
		return backupProbe(mode, c)
	}
	if mode == "changeset-value-text" || mode == "changeset-value-blob" {
		if err := c.ExecScript("CREATE TABLE t(id PRIMARY KEY,v)"); err != nil {
			return err
		}
		s, err := c.NewSession("main")
		if err != nil {
			return err
		}
		if err := s.AttachAll(); err != nil {
			return err
		}
		if err := c.ExecScript("INSERT INTO t VALUES(1,42)"); err != nil {
			return err
		}
		var payload bytes.Buffer
		if err := s.WriteChangeset(&payload); err != nil {
			return err
		}
		it, err := sqlite.NewChangesetIter(bytes.NewReader(payload.Bytes()))
		if err != nil {
			return err
		}
		row, err := it.Next()
		if err != nil || !row {
			return fmt.Errorf("iterator row: %v %v", row, err)
		}
		ch, err := it.Change()
		if err != nil {
			return err
		}
		v, err := ch.New(1)
		if err != nil {
			return err
		}
		C.probe_arm()
		if mode == "changeset-value-text" {
			_, _, err = v.Text()
		} else {
			_, err = v.Blob()
		}
		C.probe_disarm()
		if !errors.Is(err, sqlite.CodeNoMem) {
			return fmt.Errorf("changeset conversion: %v", err)
		}
		if err := it.Close(); err != nil {
			return err
		}
		if err := s.Close(); err != nil {
			return err
		}
		if err := c.Close(); err != nil {
			return err
		}
		fmt.Printf("%s: native NOMEM, not successful NULL/empty output\n", mode)
		return nil
	}
	if mode == "session-stream-lifecycle" {
		if err := c.ExecScript("CREATE TABLE t(id PRIMARY KEY,v)"); err != nil {
			return err
		}
		session, err := c.NewSession("main")
		if err != nil {
			return err
		}
		if err := session.AttachAll(); err != nil {
			return err
		}
		if err := c.ExecScript("INSERT INTO t VALUES(1,'payload')"); err != nil {
			return err
		}
		var payload bytes.Buffer
		if err := session.WriteChangeset(&payload); err != nil {
			return err
		}
		cycle := func() error {
			it, err := sqlite.NewChangesetIter(bytes.NewReader(payload.Bytes()))
			if err != nil {
				return err
			}
			for {
				row, err := it.Next()
				if err != nil {
					return err
				}
				if !row {
					break
				}
			}
			if err := it.Close(); err != nil {
				return err
			}
			if err := sqlite.InvertChangeset(io.Discard, bytes.NewReader(payload.Bytes())); err != nil {
				return err
			}
			group, err := sqlite.NewChangegroup()
			if err != nil {
				return err
			}
			if err := group.Add(bytes.NewReader(payload.Bytes())); err != nil {
				return err
			}
			if _, err := group.WriteTo(io.Discard); err != nil {
				return err
			}
			return group.Close()
		}
		if err := cycle(); err != nil {
			return err
		}
		baseline := C.probe_live()
		for range 100 {
			if err := cycle(); err != nil {
				return err
			}
			if delta := C.probe_live() - baseline; delta != 0 {
				return fmt.Errorf("stream cycle retained %d native blocks", delta)
			}
		}
		type capture struct{ value int }
		for range 20 {
			p := new(capture)
			if err := session.SetTableFilter(func(string) bool { p.value++; return true }); err != nil {
				return err
			}
		}
		installed := C.probe_live()
		for range 100 {
			if err := session.SetTableFilter(func(string) bool { return true }); err != nil {
				return err
			}
			if delta := C.probe_live() - installed; delta != 0 {
				return fmt.Errorf("filter replacement retained %d native blocks", delta)
			}
		}
		if err := session.SetTableFilter(nil); err != nil {
			return err
		}
		if delta := C.probe_live() - baseline; delta != 0 {
			return fmt.Errorf("filter removal retained %d native blocks", delta)
		}
		if err := session.Close(); err != nil {
			return err
		}
		if err := c.Close(); err != nil {
			return err
		}
		fmt.Println("session-stream-lifecycle: 100 iterator/inverse/group cycles and 100 filter replacements at stable native baselines; filter removal at prior baseline")
		return nil
	}
	if mode == "session-stream-registration" {
		if err := c.ExecScript("CREATE TABLE t(id PRIMARY KEY,v)"); err != nil {
			return err
		}
		s, err := c.NewSession("main")
		if err != nil {
			return err
		}
		if err := s.AttachAll(); err != nil {
			return err
		}
		calls := 0
		if err := s.SetTableFilter(func(string) bool { calls++; return true }); err != nil {
			return err
		}
		baseline := C.probe_live()
		// Tiny pointer-free captures can share a live allocation, keeping a
		// weak pointer non-nil after the closure itself has been released.
		type capture struct{ data [1024]byte }
		for range 10 {
			w, err := func() (weak.Pointer[capture], error) {
				p := new(capture)
				w := weak.Make(p)
				C.probe_arm()
				err := s.SetTableFilter(func(string) bool { p.data[0]++; return true })
				C.probe_disarm()
				return w, err
			}()
			if !errors.Is(err, sqlite.CodeNoMem) {
				return fmt.Errorf("filter registration: %v", err)
			}
			for range 10 {
				runtime.GC()
			}
			if w.Value() != nil {
				return fmt.Errorf("failed session filter retained capture")
			}
			C.probe_arm()
			it, iterErr := sqlite.NewChangesetIter(bytes.NewReader(nil))
			g, groupErr := sqlite.NewChangegroup()
			outputErr := s.WriteChangeset(io.Discard)
			C.probe_disarm()
			if it != nil || g != nil || !errors.Is(iterErr, sqlite.CodeNoMem) || !errors.Is(groupErr, sqlite.CodeNoMem) || !errors.Is(outputErr, sqlite.CodeNoMem) {
				return fmt.Errorf("stream constructors: iterator=%v group=%v output=%v", iterErr, groupErr, outputErr)
			}
			if delta := C.probe_live() - baseline; delta != 0 {
				return fmt.Errorf("failed registration retained %d native blocks", delta)
			}
		}
		if err := c.ExecScript("INSERT INTO t VALUES(1,'recorded')"); err != nil {
			return err
		}
		if calls != 1 {
			return fmt.Errorf("failed replacement lost filter: calls=%d", calls)
		}
		if err := s.Close(); err != nil {
			return err
		}
		if err := c.Close(); err != nil {
			return err
		}
		fmt.Println("session-stream-registration: ten NOMEM rounds; failed captures released, native baseline retained, previous filter usable")
		return nil
	}
	if mode == "callback-lifecycle" {
		if err := c.Close(); err != nil {
			return err
		}
		closedBaseline := C.probe_live()
		c, err = sqlite.Open(":memory:", 0)
		if err != nil {
			return err
		}
		baseline := C.probe_live()
		install := func() error {
			return errors.Join(
				c.SetBusyHandler(func(int) bool { return false }),
				c.SetCommitHook(func() bool { return false }),
				c.SetRollbackHook(func() {}),
				c.SetUpdateHook(func(sqlite.UpdateEvent) {}),
				c.SetAuthorizer(func(sqlite.Authorization) sqlite.AuthorizerResult { return sqlite.AuthorizerOK }),
			)
		}
		if err := install(); err != nil {
			return err
		}
		installed := C.probe_live()
		for range 100 {
			if err := install(); err != nil {
				return err
			}
			if blocks := C.probe_live() - installed; blocks != 0 {
				return fmt.Errorf("callback replacement grew native retention by %d blocks", blocks)
			}
		}
		if err := errors.Join(c.SetBusyHandler(nil), c.SetCommitHook(nil),
			c.SetRollbackHook(nil), c.SetUpdateHook(nil), c.SetAuthorizer(nil)); err != nil {
			return err
		}
		if blocks := C.probe_live() - baseline; blocks != 0 {
			return fmt.Errorf("removed callbacks retained %d native blocks", blocks)
		}
		if err := install(); err != nil {
			return err
		}
		// Closing with live registrations must return to the plain baseline.
		if err := c.Close(); err != nil {
			return err
		}
		if blocks := C.probe_live() - closedBaseline; blocks != 0 {
			return fmt.Errorf("hook close differs from plain close by %d native blocks", blocks)
		}
		fmt.Println("callback-lifecycle: 100 equivalent replacements retained a stable native allocation baseline; removal returned to baseline; hook/plain close at same native baseline")
		return nil
	}
	if mode == "callback-registration" {
		if err := c.ExecScript("CREATE TABLE x(a)"); err != nil {
			return err
		}
		calls := 0
		if err := c.SetUpdateHook(func(sqlite.UpdateEvent) { calls++ }); err != nil {
			return err
		}
		before := C.probe_live()
		// The failed temporary cgo.Handle must release the closure capture;
		// the old native registration must remain installed and usable.
		type capture struct{ data [1024]byte }
		failed := func() (weak.Pointer[capture], error) {
			p := new(capture)
			w := weak.Make(p)
			C.probe_arm()
			err := c.SetUpdateHook(func(sqlite.UpdateEvent) { p.data[0]++ })
			C.probe_disarm()
			return w, err
		}
		for range 10 {
			w, err := failed()
			if !errors.Is(err, sqlite.CodeNoMem) {
				return fmt.Errorf("callback registration: %v", err)
			}
			for range 10 {
				runtime.GC()
			}
			if w.Value() != nil {
				return fmt.Errorf("failed registration retained closure capture")
			}
		}
		if after := C.probe_live(); after != before {
			return fmt.Errorf("failed registration leaked native blocks: before=%d after=%d", before, after)
		}
		if err := c.ExecScript("INSERT INTO x VALUES(1)"); err != nil {
			return err
		}
		if calls != 1 {
			return fmt.Errorf("failed replacement lost previous hook: calls=%d", calls)
		}
		if err := c.Close(); err != nil {
			return err
		}
		fmt.Println("callback-registration: ten native NOMEM failures; captures released; native blocks at baseline; previous hook usable")
		return nil
	}
	if mode == "partial-open" {
		if err := c.Close(); err != nil {
			return err
		}
		dir, err := os.MkdirTemp("", "sqlite-go-failed-open-")
		if err != nil {
			return err
		}
		defer os.RemoveAll(dir)
		before := C.probe_live()
		for range 10 {
			failed, err := sqlite.Open(filepath.Join(dir, "missing", "db"), sqlite.OpenReadOnly)
			if failed != nil || !errors.Is(err, sqlite.CodeCantOpen) {
				return fmt.Errorf("partial constructor: %v %v", failed, err)
			}
		}
		if after := C.probe_live(); after != before {
			return fmt.Errorf("partial constructor leaked native blocks: before=%d after=%d", before, after)
		}
		fmt.Println("partial-open: ten CANTOPEN failures; native live blocks returned to baseline")
		return nil
	}
	sql := "SELECT zeroblob(4096)"
	if mode == "conversion-blob" {
		sql = "SELECT 123456789"
	} else if mode == "conversion-zeroblob" {
		sql = "SELECT ?"
	}
	s, _, err := c.Prepare(sql, 0)
	if err != nil {
		return err
	}
	if mode == "conversion-zeroblob" {
		// Keep expansion deferred until the getter runs under allocator failure.
		if err := s.BindZeroBlob(1, 4096); err != nil {
			return err
		}
	}
	if row, err := s.Step(); err != nil || !row {
		return fmt.Errorf("step: %v %v", row, err)
	}
	_, _, priorErr := c.Prepare("SELECT )", 0)
	var prior *sqlite.Error
	if !errors.As(priorErr, &prior) || prior.Offset < 0 {
		return fmt.Errorf("missing prior SQL offset: %v", priorErr)
	}
	C.probe_arm()
	if mode == "conversion-text" {
		_, _, err = s.ColumnText(0)
	} else if mode == "conversion-blob" || mode == "conversion-zeroblob" {
		_, err = s.ColumnBlob(0)
	} else {
		return fmt.Errorf("unknown mode: %s", mode)
	}
	if !errors.Is(err, sqlite.CodeNoMem) {
		return fmt.Errorf("conversion was not native NOMEM: %v", err)
	}
	var conversion *sqlite.Error
	if !errors.As(err, &conversion) || conversion.Offset != -1 {
		return fmt.Errorf("conversion inherited unrelated SQL offset: %v", err)
	}
	if closeErr := s.Close(); !errors.Is(closeErr, sqlite.CodeNoMem) {
		return fmt.Errorf("finalize did not preserve conversion NOMEM: %v", closeErr)
	}
	if closeErr := s.Close(); closeErr != nil {
		return fmt.Errorf("finalize did not consume handle: %v", closeErr)
	}
	if closeErr := c.Close(); closeErr != nil {
		return closeErr
	}
	fmt.Printf("%s: native NOMEM, not successful NULL/empty output\n", mode)
	return nil
}

func checkMultiwriterOpenPayload(c *sqlite.Conn) (err error) {
	s, _, err := c.Prepare("SELECT count(*)=2 AND count(CASE WHEN (id=1 AND v='recovered 雪' AND b=x'00253f23ff') OR (id=2 AND v='second Å' AND b=x'') THEN 1 END)=2 FROM payload", 0)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, s.Close()) }()
	if row, err := s.Step(); err != nil || !row {
		return fmt.Errorf("Multiwriter payload row=%v: %v", row, err)
	}
	if exact, nonNull, err := s.ColumnInt64(0); err != nil || !nonNull || exact != 1 {
		return fmt.Errorf("Multiwriter exact payload=%d nonNull=%v: %v", exact, nonNull, err)
	}
	return nil
}

func backupProbe(mode string, src *sqlite.Conn) error {
	dst, err := sqlite.Open(":memory:", 0)
	if err != nil {
		return err
	}
	if err := src.ExecScript("CREATE TABLE x(v); INSERT INTO x VALUES(zeroblob(64000))"); err != nil {
		return err
	}
	if err := dst.ExecScript("CREATE TABLE old(v); INSERT INTO old VALUES(7)"); err != nil {
		return err
	}
	if mode == "backup-init-reservation" {
		allocationEntered, allocationContinue = make(chan struct{}), make(chan struct{})
		C.probe_pause()
		type result struct {
			backup *sqlite.Backup
			err    error
		}
		constructed := make(chan result, 1)
		go func() {
			b, err := dst.NewBackup("main", src, "main")
			constructed <- result{b, err}
		}()
		<-allocationEntered
		// Native initialization has entered its allocator and cannot finish.
		// Interrupt must reject immediately, not block on a gate held across C.
		err := dst.Interrupt()
		close(allocationContinue)
		r := <-constructed
		if !errors.Is(err, sqlite.CodeBusy) {
			return fmt.Errorf("initializing destination Interrupt: %v", err)
		}
		if r.err != nil {
			return r.err
		}
		if err := r.backup.Close(); err != nil {
			return err
		}
		if err := dst.Interrupt(); err != nil {
			return err
		}
		if err := errors.Join(dst.Close(), src.Close()); err != nil {
			return err
		}
		fmt.Println("backup-init-reservation: allocator-blocked native init rejects Interrupt with BUSY; gate unlocked across C; finish restores access")
		return nil
	}
	if mode == "backup-init" {
		baseline := C.probe_live()
		for range 10 {
			C.probe_arm()
			b, err := dst.NewBackup("main", src, "main")
			C.probe_disarm()
			if b != nil || !errors.Is(err, sqlite.CodeNoMem) {
				return fmt.Errorf("backup init: %v %v", b, err)
			}
			if delta := C.probe_live() - baseline; delta != 0 {
				return fmt.Errorf("backup init retained %d native blocks", delta)
			}
			if err := dst.Interrupt(); err != nil {
				return err
			}
		}
	} else {
		b, err := dst.NewBackup("main", src, "main")
		if err != nil {
			return err
		}
		C.probe_arm()
		done, stepErr := b.Step(-1)
		C.probe_disarm()
		if done || !errors.Is(stepErr, sqlite.CodeNoMem) {
			return fmt.Errorf("backup step: %v %v", done, stepErr)
		}
		if _, err := b.Step(1); !errors.Is(err, sqlite.ErrState) {
			return fmt.Errorf("fatal retry: %v", err)
		}
		if _, _, err := b.Progress(); err != nil {
			return err
		}
		if err := b.Close(); !errors.Is(err, sqlite.CodeNoMem) {
			return fmt.Errorf("backup finish: %v", err)
		}
		if err := b.Close(); err != nil {
			return err
		}
		if _, err := b.Step(1); !errors.Is(err, sqlite.ErrClosed) {
			return fmt.Errorf("consumed backup: %v", err)
		}
		s, _, err := dst.Prepare("SELECT v FROM old", 0)
		if err != nil {
			return err
		}
		if row, err := s.Step(); err != nil || !row {
			return fmt.Errorf("old row: %v %v", row, err)
		}
		if value, ok, err := s.ColumnInt64(0); err != nil || !ok || value != 7 {
			return fmt.Errorf("rollback value: %d %v %v", value, ok, err)
		}
		if err := s.Close(); err != nil {
			return err
		}
	}
	b, err := dst.NewBackup("main", src, "main")
	if err != nil {
		return err
	}
	if done, err := b.Step(-1); err != nil || !done {
		return fmt.Errorf("backup reuse: %v %v", done, err)
	}
	if err := b.Close(); err != nil {
		return err
	}
	if err := errors.Join(dst.Close(), src.Close()); err != nil {
		return err
	}
	if mode == "backup-init" {
		fmt.Println("backup-init: ten native NOMEM failures at stable allocation baseline; reservation/parents released; destination reused")
	} else {
		fmt.Println("backup-step: native NOMEM preserved through consuming finish; prior destination restored; parents released; destination reused")
	}
	return nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintln(os.Stderr, "usage: faultprobe <mode> (init, multiwriter-open, partial-open, conversion-*, snapshot-copy, callback-*, session-stream-*, changeset-value-*, backup-init, backup-step, backup-init-reservation)")
		os.Exit(2)
	}
	if err := run(os.Args[1]); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}
