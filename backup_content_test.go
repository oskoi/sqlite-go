// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"errors"
	"testing"
)

func testBackupContentAndProgress(t *testing.T) {
	src := openTest(t, ":memory:")
	dst := openTest(t, ":memory:")
	script(t, src, `
		PRAGMA page_size = 1024;
		CREATE TABLE gauge_packets (
			packet INTEGER PRIMARY KEY,
			caption TEXT NOT NULL,
			waveform BLOB NOT NULL
		);
	`)
	const caption = "upstream\x00night watch"
	insert := prepareTest(t, src, `INSERT INTO gauge_packets VALUES (43, ?, zeroblob(131072))`)
	must(t, insert.BindText(1, caption))
	_, err := insert.Exec()
	must(t, err)
	must(t, insert.Close())

	backup, err := dst.NewBackup("main", src, "main")
	must(t, err)
	t.Cleanup(func() { must(t, backup.Close()) })
	remaining, total, err := backup.Progress()
	must(t, err)
	if remaining != 0 || total != 0 {
		t.Fatalf("unstarted progress = (%d, %d), want (0, 0)", remaining, total)
	}
	done, err := backup.Step(0)
	must(t, err)
	if done {
		t.Fatal("zero-page step completed the copy")
	}
	remaining, total, err = backup.Progress()
	must(t, err)
	if total <= 1 || remaining != total {
		t.Fatalf("zero-page progress = (%d, %d), want all of multiple pages remaining", remaining, total)
	}
	initialTotal := total

	done, err = backup.Step(1)
	must(t, err)
	if done {
		t.Fatal("one-page step completed the multi-page copy")
	}
	remaining, total, err = backup.Progress()
	must(t, err)
	if total != initialTotal || remaining <= 0 || remaining >= initialTotal {
		t.Fatalf("partial progress = (%d, %d), original total %d", remaining, total, initialTotal)
	}

	done, err = backup.Step(-7)
	must(t, err)
	if !done {
		t.Fatal("negative-page step did not complete the copy")
	}
	remaining, total, err = backup.Progress()
	must(t, err)
	if remaining != 0 || total != initialTotal {
		t.Fatalf("completed progress = (%d, %d), want (0, %d)", remaining, total, initialTotal)
	}

	script(t, src, `
		UPDATE gauge_packets SET caption = 'daylight revision' WHERE packet = 43;
		INSERT INTO gauge_packets VALUES (808, 'later dispatch', zeroblob(3));
	`)
	done, err = backup.Step(3)
	must(t, err)
	if !done {
		t.Fatal("completed backup lost its done state after source mutation")
	}
	remaining, total, err = backup.Progress()
	must(t, err)
	if remaining != 0 || total != initialTotal {
		t.Fatalf("sticky-done progress = (%d, %d), want (0, %d)", remaining, total, initialTotal)
	}
	must(t, backup.Close())
	must(t, backup.Close())
	if _, err := backup.Step(1); !errors.Is(err, ErrClosed) {
		t.Fatalf("step after close = %v, want ErrClosed", err)
	}
	if _, _, err := backup.Progress(); !errors.Is(err, ErrClosed) {
		t.Fatalf("progress after close = %v, want ErrClosed", err)
	}

	copied := prepareTest(t, dst, `SELECT caption, length(waveform) FROM gauge_packets WHERE packet = 43`)
	rowTest(t, copied)
	text, valid, err := copied.ColumnText(0)
	must(t, err)
	if !valid || text != caption {
		t.Fatalf("copied caption = (%q, %t), want complete %q", text, valid, caption)
	}
	if got := intTest(t, copied, 1); got != 131072 {
		t.Fatalf("copied waveform length = %d, want 131072", got)
	}
	must(t, copied.Close())
	if got := scalarTest(t, dst, `SELECT count(*) FROM gauge_packets WHERE packet = 808`); got != 0 {
		t.Fatalf("destination contains %d later dispatches, want none", got)
	}
	if got := scalarTest(t, src, `SELECT count(*) FROM gauge_packets WHERE packet = 808`); got != 1 {
		t.Fatalf("source mutation was not retained: later dispatch count = %d", got)
	}
}
