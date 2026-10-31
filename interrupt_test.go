// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"errors"
	"os"
	"os/exec"
	"runtime"
	"sync"
	"testing"
)

func TestInterruptLifetimeAndReuse(t *testing.T) {
	t.Run("watcher-stop-join", testInterruptWatcherContract)
	t.Run("failed-close-and-callback", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE x(a)")
		s := prepareTest(t, c, "SELECT 1")
		if err := c.Close(); !errors.Is(err, CodeBusy) {
			t.Fatal(err)
		}
		if err := c.Interrupt(); err != nil {
			t.Fatal(err)
		}
		rowTest(t, s)
		if err := s.Close(); err != nil {
			t.Fatal(err)
		}
		calls := 0
		active, interrupted := make(chan struct{}), make(chan error, 1)
		stop, joined := make(chan struct{}), make(chan struct{})
		if err := c.SetUpdateHook(func(UpdateEvent) {
			calls++
			if calls == 1 {
				close(active) // The INSERT VM is active inside its native hook.
				if err := <-interrupted; err != nil {
					t.Error(err)
				}
			}
		}); err != nil {
			t.Fatal(err)
		}
		go func() {
			defer close(joined)
			select {
			case <-active:
				interrupted <- c.Interrupt() // One externally issued interrupt.
			case <-stop:
				return
			}
			<-stop
		}()
		// A single-row insert can already be too near completion for SQLite
		// to observe the flag. This recursive insert must continue its VM loop.
		err := c.ExecScript("WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<100) INSERT INTO x SELECT x FROM n")
		close(stop)
		<-joined // No watcher survives into removal, inspection or reuse.
		if !errors.Is(err, CodeInterrupt) {
			t.Fatal(err)
		}
		if calls != 1 {
			t.Fatal(calls)
		}
		if err := c.SetUpdateHook(nil); err != nil {
			t.Fatal(err)
		}
		if got := scalarTest(t, c, "SELECT count(*) FROM x"); got != 0 {
			t.Fatal("interrupted INSERT effects:", got)
		}
		if got := scalarTest(t, c, "SELECT 42"); got != 42 {
			t.Fatal(got)
		}
		// Close invokes rollback with the interrupt gate disabled and unlocked.
		script(t, c, "BEGIN; INSERT INTO x VALUES(2)")
		if err := c.SetRollbackHook(func() {
			calls++
			if err := c.Interrupt(); !errors.Is(err, ErrClosed) {
				t.Error(err)
			}
		}); err != nil {
			t.Fatal(err)
		}
		if err := c.Close(); err != nil {
			t.Fatal(err)
		}
		if calls != 2 {
			t.Fatal(calls)
		}
		if err := c.Interrupt(); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		var zero Conn
		if err := zero.Interrupt(); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		if err := (*Conn)(nil).Interrupt(); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
	})
	t.Run("isolated-close-race", func(t *testing.T) {
		if os.Getenv("SQLITE_GO_INTERRUPT_PROBE") == "1" {
			for range 100 {
				c, err := Open(":memory:", 0)
				if err != nil {
					t.Fatal(err)
				}
				start, stop := make(chan struct{}), make(chan struct{})
				ready := make(chan struct{}, 4)
				var wg sync.WaitGroup
				for range 4 {
					wg.Go(func() {
						<-start
						if err := c.Interrupt(); err != nil {
							t.Error(err)
						}
						ready <- struct{}{}
						for {
							select {
							case <-stop:
								return
							default:
							}
							err := c.Interrupt()
							if err != nil && !errors.Is(err, ErrClosed) {
								t.Error(err)
								return
							}
							runtime.Gosched()
						}
					})
				}
				close(start)
				for range 4 {
					<-ready
				}
				if err := c.Close(); err != nil {
					t.Fatal(err)
				}
				close(stop)
				wg.Wait()
				if err := c.Interrupt(); !errors.Is(err, ErrClosed) {
					t.Fatal(err)
				}
			}
			return
		}
		bin, err := os.Executable()
		if err != nil {
			t.Fatal(err)
		}
		cmd := exec.Command(bin, "-test.run=^TestInterruptLifetimeAndReuse/isolated-close-race$", "-test.count=1")
		cmd.Env = append(os.Environ(), "SQLITE_GO_INTERRUPT_PROBE=1")
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("interrupt/close probe: %v\n%s", err, out)
		}
	})
}
