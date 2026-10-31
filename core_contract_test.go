// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"bytes"
	"errors"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"
)

func coreContractError(t testing.TB, err, want error) {
	t.Helper()
	if !errors.Is(err, want) {
		t.Fatalf("error = %v, want %v", err, want)
	}
}

func coreContractValue(t testing.TB, got, want ValueCopy) {
	t.Helper()
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("value = %#v, want %#v", got, want)
	}
}

func TestOpenAndStrictClose(t *testing.T) {
	t.Run("configured-thread-model", func(t *testing.T) {
		if slices.Contains(CompileOptions(), "THREADSAFE=2") {
			return
		}
		t.Fatal("configured THREADSAFE=2 option is absent")
	})

	t.Run("zero-handles", func(t *testing.T) {
		var c Conn
		var s Stmt
		must(t, c.Close())
		must(t, s.Close())
		_, err := c.AutoCommit()
		coreContractError(t, err, ErrClosed)
		_, err = s.Step()
		coreContractError(t, err, ErrClosed)
	})

	t.Run("open-rejections", func(t *testing.T) {
		for _, tc := range []struct {
			name  string
			path  string
			flags OpenFlags
		}{
			{"nul-path", "port\x00ledger", 0},
			{"unknown-bit", ":memory:", OpenReadWrite | OpenFlags(1<<31)},
			{"missing-access", ":memory:", OpenCreate},
			{"two-access-modes", ":memory:", OpenReadOnly | OpenReadWrite},
			{"readonly-create", ":memory:", OpenReadOnly | OpenCreate},
			{"two-mutex-modes", ":memory:", OpenReadWrite | OpenNoMutex | OpenFullMutex},
		} {
			t.Run(tc.name, func(t *testing.T) {
				c, err := Open(tc.path, tc.flags)
				if c != nil {
					defer c.Close()
					t.Fatal("invalid open returned a connection")
				}
				coreContractError(t, err, ErrInvalidArgument)
			})
		}
		c, err := Open(filepath.Join(t.TempDir(), "uncreated.sqlite"), OpenReadOnly)
		if c != nil {
			defer c.Close()
			t.Fatal("read-only open created a missing database")
		}
		coreContractError(t, err, CodeCantOpen)
	})

	t.Run("file-ownership-and-access", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "navigation.sqlite")
		c := openTest(t, path)
		script(t, c, "CREATE TABLE harbors(id INTEGER PRIMARY KEY, name TEXT); INSERT INTO harbors VALUES(17, 'Sable Reach')")
		name, err := c.FileName("main")
		must(t, err)
		opened, err := os.Stat(path)
		must(t, err)
		reported, err := os.Stat(name)
		must(t, err)
		if !os.SameFile(opened, reported) {
			t.Fatalf("reported filename %q does not identify %q", name, path)
		}
		s := prepareTest(t, c, "SELECT id FROM harbors")
		coreContractError(t, c.Close(), CodeBusy)
		if got := scalarTest(t, c, "SELECT count(*) FROM harbors"); got != 1 {
			t.Fatalf("refused close damaged connection: count = %d", got)
		}
		rowTest(t, s)
		if got := intTest(t, s, 0); got != 17 {
			t.Fatalf("refused close damaged child: id = %d", got)
		}
		must(t, s.Close())
		must(t, c.Close())
		must(t, c.Close())
		_, err = c.AutoCommit()
		coreContractError(t, err, ErrClosed)

		ro, err := Open(path, OpenReadOnly)
		must(t, err)
		defer ro.Close()
		coreContractError(t, ro.ExecScript("INSERT INTO harbors VALUES(18, 'North Bell')"), CodeReadOnly)
		uri := (&url.URL{Scheme: "file", Path: filepath.ToSlash(path), RawQuery: "mode=ro"}).String()
		reader, err := Open(uri, OpenReadOnly|OpenURI)
		must(t, err)
		defer reader.Close()
		if got := scalarTest(t, reader, "SELECT id FROM harbors"); got != 17 {
			t.Fatalf("URI reader id = %d, want 17", got)
		}
		if runtime.GOOS != "windows" {
			link := filepath.Join(filepath.Dir(path), "navigation-link.sqlite")
			must(t, os.Symlink(path, link))
			linked, err := Open(link, OpenReadOnly|OpenNoFollow)
			if linked != nil {
				defer linked.Close()
				t.Fatal("no-follow open accepted a symlink")
			}
			coreContractError(t, err, CodeCantOpen)
		}
	})

	t.Run("anonymous-and-memory-databases", func(t *testing.T) {
		for _, name := range []string{"", ":memory:", "file:core-contract-chart?mode=memory&cache=shared"} {
			c := openTest(t, name)
			filename, err := c.FileName("main")
			must(t, err)
			if filename != "" {
				t.Fatalf("anonymous or memory database filename = %q", filename)
			}
			script(t, c, "CREATE TABLE readings(slot INTEGER PRIMARY KEY, weight INTEGER NOT NULL); INSERT INTO readings VALUES(31, 23)")
			if got := scalarTest(t, c, "SELECT weight FROM readings WHERE slot=31"); got != 23 {
				t.Fatalf("stored reading = %d, want 23", got)
			}
			_, err = c.FileName("ma\x00in")
			coreContractError(t, err, ErrInvalidArgument)
		}
	})

	t.Run("native-settings", func(t *testing.T) {
		c := openTest(t, ":memory:")
		original, err := c.Limit(LimitLength, -1)
		must(t, err)
		if original <= 1 {
			t.Fatalf("length limit = %d, want a positive usable limit", original)
		}
		previous, err := c.Limit(LimitLength, original/2)
		must(t, err)
		if previous != original {
			t.Fatalf("lowering limit returned %d, want previous %d", previous, original)
		}
		current, err := c.Limit(LimitLength, -7)
		must(t, err)
		if current != original/2 {
			t.Fatalf("queried lowered limit = %d", current)
		}
		_, err = c.Limit(LimitLength, original)
		must(t, err)
		if strconv.IntSize > 32 {
			for _, value := range []int64{int64(math.MinInt32) - 1, int64(math.MaxInt32) + 1} {
				_, err = c.Limit(LimitLength, int(value))
				coreContractError(t, err, ErrRange)
			}
		}
		_, _, err = c.Status(DBStatusCacheUsed, false)
		must(t, err)
		_, _, err = c.Status(DBStatus(-1), true)
		coreContractError(t, err, ErrRange)
		coreContractError(t, c.BusyTimeout(time.Duration(math.MaxInt32)*time.Millisecond+time.Nanosecond), ErrRange)
		for _, duration := range []time.Duration{-time.Second, 0, 3 * time.Millisecond} {
			must(t, c.BusyTimeout(duration))
		}
	})
}

func TestScriptTailAndOffsets(t *testing.T) {
	t.Run("empty-input-and-tail", func(t *testing.T) {
		c := openTest(t, ":memory:")
		for _, inTransaction := range []bool{false, true} {
			if inTransaction {
				must(t, c.Begin())
			}
			for _, sql := range []string{"", " \n-- waiting for a shipment;\n/* nothing to dispatch; */"} {
				s, tail, err := c.Prepare(sql, 0)
				must(t, err)
				if s != nil || tail != "" {
					t.Fatalf("empty prepare = (%v, %q), want (nil, empty)", s, tail)
				}
				must(t, c.ExecScript(sql))
				auto, err := c.AutoCommit()
				must(t, err)
				if auto == inTransaction {
					t.Fatalf("empty script changed transaction state: autocommit = %v", auto)
				}
			}
			if inTransaction {
				must(t, c.Rollback())
			}
		}
		const suffix = " \t/* unloaded; still unconsumed */\n-- manifest follows"
		s, tail, err := c.Prepare("SELECT 14;"+suffix, 0)
		must(t, err)
		if s == nil || tail != suffix {
			t.Fatalf("prepare returned statement %v and tail %q, want exact suffix %q", s, tail, suffix)
		}
		rowTest(t, s)
		if got := intTest(t, s, 0); got != 14 {
			t.Fatalf("first statement = %d", got)
		}
		must(t, s.Close())
	})

	t.Run("complete-sql-constructs", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, `
CREATE TABLE arrivals(id INTEGER PRIMARY KEY, note TEXT);
CREATE TABLE echoes(note TEXT);
CREATE TRIGGER remember_arrival AFTER INSERT ON arrivals BEGIN
  INSERT INTO echoes VALUES('seen;' || NEW.note);
  /* a semicolon ; belongs to this comment */
  INSERT INTO echoes VALUES('filed;');
END;
INSERT INTO arrivals VALUES(5, 'dock;west'); -- another ; in a comment
`)
		s := prepareTest(t, c, "SELECT note FROM echoes ORDER BY rowid")
		for _, want := range []string{"seen;dock;west", "filed;"} {
			rowTest(t, s)
			got, ok, err := s.ColumnText(0)
			must(t, err)
			if !ok || got != want {
				t.Fatalf("trigger note = %q, nonnull %v, want %q", got, ok, want)
			}
		}
		row, err := s.Step()
		must(t, err)
		if row {
			t.Fatal("trigger produced an unexpected extra row")
		}
		must(t, s.Close())
		if got := scalarTest(t, c, "SELECT note = 'dock;west' FROM arrivals WHERE id = 5"); got != 1 {
			t.Fatal("quoted semicolon was not retained")
		}
	})

	t.Run("failure-boundaries", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE waypoints(id INTEGER PRIMARY KEY, note TEXT)")
		const sql = "INSERT INTO waypoints VALUES(8, 'π');\nSELEKT 4; INSERT INTO waypoints VALUES(9, 'later');"
		err := c.ExecScript(sql)
		coreContractError(t, err, CodeError)
		var native *Error
		if !errors.As(err, &native) || native.Offset != strings.Index(sql, "SELEKT") {
			t.Fatalf("syntax error = %#v, want byte offset %d", native, strings.Index(sql, "SELEKT"))
		}
		if got := scalarTest(t, c, "SELECT count(*) FROM waypoints WHERE id = 8"); got != 1 {
			t.Fatal("the successful prefix was undone")
		}
		if got := scalarTest(t, c, "SELECT count(*) FROM waypoints WHERE id = 9"); got != 0 {
			t.Fatal("script continued past syntax failure")
		}
		coreContractError(t, c.ExecScript("BEGIN; INSERT INTO waypoints VALUES(12, 'pending'); SELEKT 5;"), CodeError)
		auto, err := c.AutoCommit()
		must(t, err)
		if auto || scalarTest(t, c, "SELECT count(*) FROM waypoints WHERE id = 12") != 1 {
			t.Fatal("script failure did not preserve the explicit transaction")
		}
		must(t, c.Rollback())
		if got := scalarTest(t, c, "SELECT count(*) FROM waypoints WHERE id = 12"); got != 0 {
			t.Fatal("explicit rollback did not remove pending data")
		}
		err = c.ExecScript("INSERT INTO waypoints VALUES(8, 'duplicate')")
		coreContractError(t, err, CodeConstraintPrimaryKey)
		coreContractError(t, err, CodeConstraint)
		if errors.Is(err, CodeConstraintUnique) {
			t.Fatal("primary-key error matched the unique extended code")
		}
		native = nil
		if !errors.As(err, &native) || native.Offset != -1 {
			t.Fatalf("runtime error = %#v, want unavailable offset", native)
		}
		_, _, err = c.Prepare("SELECT 1\x00; SELECT 2", 0)
		coreContractError(t, err, ErrInvalidArgument)
		coreContractError(t, c.ExecScript("SELECT 1;\x00SELECT 2"), ErrInvalidArgument)
	})

	t.Run("transaction-entry-points", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE dispatch(id INTEGER PRIMARY KEY)")
		for _, begin := range []func() error{c.Begin, c.BeginImmediate, c.BeginExclusive} {
			must(t, begin())
			script(t, c, "INSERT INTO dispatch VALUES(27)")
			must(t, c.Rollback())
			if got := scalarTest(t, c, "SELECT count(*) FROM dispatch"); got != 0 {
				t.Fatalf("rollback retained %d rows", got)
			}
		}
		must(t, c.Begin())
		script(t, c, "INSERT INTO dispatch VALUES(38)")
		must(t, c.Commit())
		if got := scalarTest(t, c, "SELECT id FROM dispatch"); got != 38 {
			t.Fatalf("committed id = %d", got)
		}
	})
}

func TestStatementLifecycle(t *testing.T) {
	t.Run("ready-done-failed-and-counters", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE ledger(id INTEGER PRIMARY KEY)")
		insert := prepareTest(t, c, "INSERT INTO ledger VALUES(?)")
		defer insert.Close()
		readOnly, err := insert.ReadOnly()
		must(t, err)
		if readOnly {
			t.Fatal("INSERT classified read-only")
		}
		must(t, insert.BindInt64(1, 41))
		result, err := insert.Exec()
		must(t, err)
		if result != (Result{Changes: 1, LastInsertRowID: 41}) {
			t.Fatalf("insert result = %+v", result)
		}
		result, err = insert.Exec()
		coreContractError(t, err, CodeConstraintPrimaryKey)
		if result != (Result{}) {
			t.Fatalf("failed Exec returned %+v", result)
		}
		if _, ok := errors.AsType[*Error](err); !ok {
			t.Fatalf("failed Exec is not a native error: %v", err)
		}
		if _, joined := err.(interface{ Unwrap() []error }); joined {
			t.Fatal("Exec joined its execution error with the saved reset error")
		}
		must(t, insert.BindInt64(1, 52))
		result, err = insert.Exec()
		must(t, err)
		if result != (Result{Changes: 1, LastInsertRowID: 52}) {
			t.Fatalf("reused Exec result = %+v", result)
		}
		must(t, insert.BindInt64(1, 63))
		for range 2 {
			row, err := insert.Step()
			must(t, err)
			if row {
				t.Fatal("INSERT returned a row")
			}
		}
		coreContractError(t, insert.BindInt64(1, 90), ErrState)
		must(t, insert.ClearBindings())
		must(t, insert.Reset())
		result, err = insert.Exec()
		must(t, err)
		if result != (Result{Changes: 1, LastInsertRowID: 64}) {
			t.Fatalf("cleared integer primary key did not allocate a rowid: %+v", result)
		}

		must(t, insert.BindInt64(1, 41))
		row, err := insert.Step()
		if row {
			t.Fatal("failed INSERT returned a row")
		}
		coreContractError(t, err, CodeConstraintPrimaryKey)
		_, err = insert.Step()
		coreContractError(t, err, ErrState)
		coreContractError(t, insert.ClearBindings(), ErrState)
		coreContractError(t, insert.Reset(), CodeConstraintPrimaryKey)
		must(t, insert.BindInt64(1, 75))
		_, err = insert.Exec()
		must(t, err)
		must(t, insert.BindInt64(1, 41))
		_, err = insert.Step()
		coreContractError(t, err, CodeConstraintPrimaryKey)
		coreContractError(t, insert.Close(), CodeConstraintPrimaryKey)
		must(t, insert.Close())
		_, err = insert.Step()
		coreContractError(t, err, ErrClosed)

		query := prepareTest(t, c, "SELECT id FROM ledger WHERE id + 0 >= ?1 ORDER BY id")
		defer query.Close()
		readOnly, err = query.ReadOnly()
		must(t, err)
		if !readOnly {
			t.Fatal("SELECT classified writable")
		}
		must(t, query.BindInt64(1, 0))
		result, err = query.Exec()
		must(t, err)
		// The immediately preceding failed INSERT leaves native changes at zero.
		if result != (Result{Changes: 0, LastInsertRowID: 75}) {
			t.Fatalf("draining SELECT result = %+v", result)
		}
		scanned, err := query.Status(StmtStatusFullScanStep, false)
		must(t, err)
		if scanned != 4 {
			t.Fatalf("Exec full-scan advances = %d, want all five rows visited", scanned)
		}
		changes, err := c.Changes()
		must(t, err)
		total, err := c.TotalChanges()
		must(t, err)
		last, err := c.LastInsertRowID()
		must(t, err)
		if changes != 0 || total != 5 || last != 75 {
			t.Fatalf("native counters = (%d, %d, %d), want (0, 5, 75)", changes, total, last)
		}
		rowTest(t, query)
		coreContractError(t, query.ClearBindings(), ErrState)
		coreContractError(t, query.BindInt64(1, 50), ErrState)
		result, err = query.Exec()
		coreContractError(t, err, ErrState)
		if result != (Result{}) {
			t.Fatalf("row-state Exec returned %+v", result)
		}
		steps, err := query.Status(StmtStatusVMStep, false)
		must(t, err)
		if steps <= 0 {
			t.Fatalf("executed query VM-step count = %d", steps)
		}
		must(t, query.Reset())
		rowTest(t, query)
		if got := intTest(t, query, 0); got != 41 {
			t.Fatalf("reset query first id = %d", got)
		}
	})

	t.Run("dependent-object-disappears", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE transient_manifest(item TEXT)")
		s := prepareTest(t, c, "SELECT item FROM transient_manifest")
		script(t, c, "DROP TABLE transient_manifest")
		result, err := s.Exec()
		coreContractError(t, err, CodeError)
		if result != (Result{}) {
			t.Fatalf("invalidated query result = %+v", result)
		}
		must(t, s.Close())
		must(t, c.Close())
	})
}

func TestParametersAndRetainedInputs(t *testing.T) {
	t.Run("sparse-slots-and-exact-names", func(t *testing.T) {
		c := openTest(t, ":memory:")
		s := prepareTest(t, c, "SELECT :Beacon, @Beacon, $Beacon, ?12, ?, :Beacon")
		defer s.Close()
		count, err := s.ParameterCount()
		must(t, err)
		if count != 13 {
			t.Fatalf("parameter count = %d, want highest slot 13", count)
		}
		for index := 1; index <= count; index++ {
			want := map[int]string{1: ":Beacon", 2: "@Beacon", 3: "$Beacon", 12: "?12"}[index]
			name, err := s.ParameterName(index)
			must(t, err)
			if name != want {
				t.Fatalf("slot %d name = %q, want %q", index, name, want)
			}
		}
		for name, want := range map[string]int{":Beacon": 1, "@Beacon": 2, "$Beacon": 3, "?12": 12, "Beacon": 0, ":beacon": 0, ":Unknown": 0} {
			index, err := s.ParameterIndex(name)
			must(t, err)
			if index != want {
				t.Fatalf("lookup %q = %d, want %d", name, index, want)
			}
		}
		for index, value := range map[int]int64{1: 11, 2: 22, 3: 33, 12: 44, 13: 55} {
			must(t, s.BindInt64(index, value))
		}
		rowTest(t, s)
		for index, want := range []int64{11, 22, 33, 44, 55, 11} {
			if got := intTest(t, s, index); got != want {
				t.Fatalf("selected parameter %d = %d, want %d", index, got, want)
			}
		}
	})

	t.Run("long-parameter-names", func(t *testing.T) {
		c := openTest(t, ":memory:")
		names := []string{":" + strings.Repeat("q", 254), ":" + strings.Repeat("r", 255), ":" + strings.Repeat("s", 388)}
		s := prepareTest(t, c, "SELECT "+strings.Join(names, ", "))
		defer s.Close()
		for i, name := range names {
			index, err := s.ParameterIndex(name)
			must(t, err)
			if index != i+1 {
				t.Fatalf("%d-byte name lookup = %d, want %d", len(name), index, i+1)
			}
			got, err := s.ParameterName(index)
			must(t, err)
			if got != name {
				t.Fatalf("%d-byte parameter name was truncated", len(name))
			}
			must(t, s.BindInt64(index, int64(207+i)))
		}
		rowTest(t, s)
		for i, want := range []int64{207, 208, 209} {
			if got := intTest(t, s, i); got != want {
				t.Fatalf("long-name value %d = %d, want %d", i, got, want)
			}
		}
	})

	t.Run("copied-inputs-and-empty-kinds", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE retained_input(label TEXT, payload BLOB, padding BLOB, blank BLOB, absent BLOB, empty BLOB)")
		insert := prepareTest(t, c, "INSERT INTO retained_input VALUES(?, ?, ?, ?, ?, ?)")
		const text = "red\x00kite"
		blob := []byte{'K', 0, 'L'}
		must(t, insert.BindText(1, text))
		must(t, insert.BindBlob(2, blob))
		must(t, insert.BindZeroBlob(3, 7))
		must(t, insert.BindZeroBlob(4, 0))
		must(t, insert.BindNull(5))
		must(t, insert.BindBlob(6, []byte{}))
		copy(blob, []byte{'x', 'y', 'z'})
		_, err := insert.Exec()
		must(t, err)
		must(t, insert.Close())
		query := prepareTest(t, c, "SELECT label, payload, padding, blank, absent, empty FROM retained_input")
		rowTest(t, query)
		for i, want := range []ValueCopy{
			{Kind: KindText, Text: text},
			{Kind: KindBlob, Blob: []byte{'K', 0, 'L'}},
			{Kind: KindBlob, Blob: []byte{0, 0, 0, 0, 0, 0, 0}},
			{Kind: KindBlob, Blob: []byte{}},
			{Kind: KindNull},
			{Kind: KindBlob, Blob: []byte{}},
		} {
			got, err := query.ColumnValue(i)
			must(t, err)
			coreContractValue(t, got, want)
		}
		must(t, query.Close())
		s := prepareTest(t, c, "SELECT ?")
		defer s.Close()
		must(t, s.BindBlob(1, []byte{}))
		rowTest(t, s)
		empty, err := s.ColumnBlob(0)
		must(t, err)
		if empty == nil || len(empty) != 0 {
			t.Fatalf("empty blob = %#v", empty)
		}
		must(t, s.Reset())
		must(t, s.ClearBindings())
		rowTest(t, s)
		null, err := s.ColumnBlob(0)
		must(t, err)
		kind, err := s.ColumnType(0)
		must(t, err)
		if null != nil || kind != KindNull {
			t.Fatalf("cleared binding = %#v, kind %v", null, kind)
		}
	})

	t.Run("parameter-range-errors", func(t *testing.T) {
		c := openTest(t, ":memory:")
		s := prepareTest(t, c, "SELECT ?")
		defer s.Close()
		maxInt := int(^uint(0) >> 1)
		for _, index := range []int{-3, 0, 2, maxInt} {
			for _, bind := range []func(int) error{
				s.BindNull,
				func(i int) error { return s.BindInt64(i, 6) },
				func(i int) error { return s.BindFloat64(i, 2.75) },
				func(i int) error { return s.BindText(i, "cargo") },
				func(i int) error { return s.BindBlob(i, []byte{4}) },
				func(i int) error { return s.BindZeroBlob(i, 2) },
			} {
				coreContractError(t, bind(index), ErrRange)
			}
			_, err := s.ParameterName(index)
			coreContractError(t, err, ErrRange)
		}
		coreContractError(t, s.BindZeroBlob(1, -1), ErrRange)
		_, err := s.ParameterIndex(":bad\x00name")
		coreContractError(t, err, ErrInvalidArgument)
	})
}

func TestNativeLocks(t *testing.T) {
	t.Run("same-owner-schema-lock", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE inventory(code INTEGER); INSERT INTO inventory VALUES(6), (15)")
		cursor := prepareTest(t, c, "SELECT code FROM inventory")
		rowTest(t, cursor)
		drop := prepareTest(t, c, "DROP TABLE inventory")
		row, err := drop.Step()
		if row {
			t.Fatal("locked DROP returned a row")
		}
		coreContractError(t, err, CodeLocked)
		coreContractError(t, drop.Close(), CodeLocked)
		must(t, cursor.Close())
		script(t, c, "DROP TABLE inventory")
	})

	t.Run("competing-file-writers", func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "freight.sqlite")
		first := openTest(t, path)
		script(t, first, "CREATE TABLE freight(id INTEGER PRIMARY KEY, units INTEGER); INSERT INTO freight VALUES(4, 10)")
		second := openTest(t, path)
		must(t, first.BeginImmediate())
		script(t, first, "UPDATE freight SET units = 11 WHERE id = 4")
		must(t, second.BusyTimeout(2*time.Millisecond))
		coreContractError(t, second.ExecScript("UPDATE freight SET units = 23 WHERE id = 4"), CodeBusy)
		must(t, first.Rollback())
		script(t, second, "UPDATE freight SET units = 23 WHERE id = 4")
		if got := scalarTest(t, second, "SELECT units FROM freight WHERE id = 4"); got != 23 {
			t.Fatalf("write after lock release stored %d", got)
		}
	})
}

func TestColumnValuesAndReprepare(t *testing.T) {
	t.Run("native-conversions-and-ranges", func(t *testing.T) {
		c := openTest(t, ":memory:")
		s := prepareTest(t, c, "SELECT NULL, '', X'', -37, 6.25, '81', X'3432'")
		defer s.Close()
		_, _, err := s.ColumnText(0)
		coreContractError(t, err, ErrState)
		rowTest(t, s)
		cases := []struct {
			original ValueCopy
			integer  int64
			real     float64
			text     string
		}{
			{ValueCopy{Kind: KindNull}, 0, 0, ""},
			{ValueCopy{Kind: KindText, Text: ""}, 0, 0, ""},
			{ValueCopy{Kind: KindBlob, Blob: []byte{}}, 0, 0, ""},
			{ValueCopy{Kind: KindInteger, Int64: -37}, -37, -37, "-37"},
			{ValueCopy{Kind: KindFloat, Float64: 6.25}, 6, 6.25, "6.25"},
			{ValueCopy{Kind: KindText, Text: "81"}, 81, 81, "81"},
			{ValueCopy{Kind: KindBlob, Blob: []byte("42")}, 42, 42, "42"},
		}
		for i, tc := range cases {
			original, err := s.ColumnValue(i)
			must(t, err)
			coreContractValue(t, original, tc.original)
			nonNull := tc.original.Kind != KindNull
			integer, ok, err := s.ColumnInt64(i)
			must(t, err)
			if integer != tc.integer || ok != nonNull {
				t.Fatalf("column %d integer = (%d, %v), want (%d, %v)", i, integer, ok, tc.integer, nonNull)
			}
			real, ok, err := s.ColumnFloat64(i)
			must(t, err)
			if real != tc.real || ok != nonNull {
				t.Fatalf("column %d real = (%g, %v), want (%g, %v)", i, real, ok, tc.real, nonNull)
			}
			text, ok, err := s.ColumnText(i)
			must(t, err)
			if text != tc.text || ok != nonNull {
				t.Fatalf("column %d text = (%q, %v), want (%q, %v)", i, text, ok, tc.text, nonNull)
			}
			view, ok, err := s.ColumnTextView(i)
			must(t, err)
			if view != tc.text || ok != nonNull {
				t.Fatalf("column %d text view = (%q, %v)", i, view, ok)
			}
			blob, err := s.ColumnBlob(i)
			must(t, err)
			if !bytes.Equal(blob, []byte(tc.text)) || (blob != nil) != nonNull {
				t.Fatalf("column %d blob = %#v, nonnull %v", i, blob, nonNull)
			}
			blobView, err := s.ColumnBlobView(i)
			must(t, err)
			if !bytes.Equal(blobView, []byte(tc.text)) || (blobView != nil) != nonNull {
				t.Fatalf("column %d blob view = %#v, nonnull %v", i, blobView, nonNull)
			}
			kind, err := s.ColumnType(i)
			must(t, err)
			if kind != tc.original.Kind {
				t.Fatalf("column %d kind after coercion = %v, want %v", i, kind, tc.original.Kind)
			}
		}
		for _, index := range []int{-1, len(cases), int(^uint(0) >> 1)} {
			_, err := s.ColumnType(index)
			coreContractError(t, err, ErrRange)
			value, err := s.ColumnValue(index)
			coreContractError(t, err, ErrRange)
			coreContractValue(t, value, ValueCopy{})
			_, _, err = s.ColumnInt64(index)
			coreContractError(t, err, ErrRange)
			_, _, err = s.ColumnFloat64(index)
			coreContractError(t, err, ErrRange)
			_, _, err = s.ColumnText(index)
			coreContractError(t, err, ErrRange)
			_, err = s.ColumnBlob(index)
			coreContractError(t, err, ErrRange)
			_, _, err = s.ColumnTextView(index)
			coreContractError(t, err, ErrRange)
			_, err = s.ColumnBlobView(index)
			coreContractError(t, err, ErrRange)
			_, err = s.ColumnName(index)
			coreContractError(t, err, ErrRange)
			_, err = s.ColumnDeclType(index)
			coreContractError(t, err, ErrRange)
		}
	})

	t.Run("bound-storage-representations", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE cells(id INTEGER PRIMARY KEY, value)")
		insert := prepareTest(t, c, "INSERT INTO cells VALUES(?, ?)")
		defer insert.Close()
		query := prepareTest(t, c, "SELECT value, typeof(value), value IS NULL, length(value), length(CAST(value AS BLOB)) FROM cells WHERE id = ?")
		defer query.Close()
		cases := []struct {
			name       string
			value      ValueCopy
			nilBlob    bool
			sqlType    string
			text       string
			length     int64
			byteLength int64
		}{
			{"empty-text", ValueCopy{Kind: KindText, Text: ""}, false, "text", "", 0, 0},
			{"explicit-null", ValueCopy{Kind: KindNull}, false, "null", "", 0, 0},
			{"empty-blob", ValueCopy{Kind: KindBlob, Blob: []byte{}}, false, "blob", "", 0, 0},
			{"nil-blob", ValueCopy{Kind: KindNull}, true, "null", "", 0, 0},
			{"nul-text", ValueCopy{Kind: KindText, Text: "az\x00by"}, false, "text", "az\x00by", 2, 5},
			{"nul-blob", ValueCopy{Kind: KindBlob, Blob: []byte{'d', 0, 'e'}}, false, "blob", "d\x00e", 3, 3},
		}
		for i, tc := range cases {
			must(t, insert.BindInt64(1, int64(i+1)))
			switch {
			case tc.nilBlob:
				must(t, insert.BindBlob(2, nil))
			case tc.value.Kind == KindNull:
				must(t, insert.BindNull(2))
			case tc.value.Kind == KindText:
				must(t, insert.BindText(2, tc.value.Text))
			case tc.value.Kind == KindBlob:
				must(t, insert.BindBlob(2, tc.value.Blob))
			}
			_, err := insert.Exec()
			must(t, err)
			must(t, query.BindInt64(1, int64(i+1)))
			rowTest(t, query)
			value, err := query.ColumnValue(0)
			must(t, err)
			coreContractValue(t, value, tc.value)
			nonNull := tc.value.Kind != KindNull
			for repetition := range 2 {
				text, ok, err := query.ColumnText(0)
				must(t, err)
				if text != tc.text || ok != nonNull {
					t.Fatalf("%s text round %d = (%q, %v)", tc.name, repetition, text, ok)
				}
				textView, ok, err := query.ColumnTextView(0)
				must(t, err)
				if textView != tc.text || ok != nonNull {
					t.Fatalf("%s text view round %d = (%q, %v)", tc.name, repetition, textView, ok)
				}
				blob, err := query.ColumnBlob(0)
				must(t, err)
				if !bytes.Equal(blob, []byte(tc.text)) || (blob != nil) != nonNull {
					t.Fatalf("%s blob round %d = %#v", tc.name, repetition, blob)
				}
				blobView, err := query.ColumnBlobView(0)
				must(t, err)
				if !bytes.Equal(blobView, []byte(tc.text)) || (blobView != nil) != nonNull {
					t.Fatalf("%s blob view round %d = %#v", tc.name, repetition, blobView)
				}
			}
			kind, err := query.ColumnType(0)
			must(t, err)
			if kind != tc.value.Kind {
				t.Fatalf("%s original kind after conversions = %v", tc.name, kind)
			}
			sqlType, ok, err := query.ColumnText(1)
			must(t, err)
			if !ok || sqlType != tc.sqlType {
				t.Fatalf("%s SQL typeof = (%q, %v)", tc.name, sqlType, ok)
			}
			wantNull := int64(0)
			if !nonNull {
				wantNull = 1
			}
			if got := intTest(t, query, 2); got != wantNull {
				t.Fatalf("%s SQL null predicate = %d", tc.name, got)
			}
			for column, want := range map[int]int64{3: tc.length, 4: tc.byteLength} {
				got, ok, err := query.ColumnInt64(column)
				must(t, err)
				if got != want || ok != nonNull {
					t.Fatalf("%s length column %d = (%d, %v), want (%d, %v)", tc.name, column, got, ok, want, nonNull)
				}
			}
			must(t, query.Reset())
		}
	})

	t.Run("numeric-binding-boundaries", func(t *testing.T) {
		c := openTest(t, ":memory:")
		s := prepareTest(t, c, "SELECT ?")
		defer s.Close()
		for _, want := range []int64{math.MinInt64, math.MaxInt64} {
			must(t, s.BindInt64(1, want))
			rowTest(t, s)
			copy, err := s.ColumnValue(0)
			must(t, err)
			coreContractValue(t, copy, ValueCopy{Kind: KindInteger, Int64: want})
			got, ok, err := s.ColumnInt64(0)
			must(t, err)
			if !ok || got != want {
				t.Fatalf("integer boundary = (%d, %v), want %d", got, ok, want)
			}
			must(t, s.Reset())
		}
		for _, want := range []float64{0, math.SmallestNonzeroFloat64, math.MaxFloat64, -11.375} {
			must(t, s.BindFloat64(1, want))
			rowTest(t, s)
			copy, err := s.ColumnValue(0)
			must(t, err)
			coreContractValue(t, copy, ValueCopy{Kind: KindFloat, Float64: want})
			got, ok, err := s.ColumnFloat64(0)
			must(t, err)
			if !ok || got != want {
				t.Fatalf("float boundary = (%g, %v), want %g", got, ok, want)
			}
			must(t, s.Reset())
		}
	})

	t.Run("row-kinds-and-owned-lifetimes", func(t *testing.T) {
		c := openTest(t, ":memory:")
		rows := prepareTest(t, c, "SELECT 'orchid' AS specimen UNION ALL SELECT NULL UNION ALL SELECT 29")
		for i, want := range []struct {
			kind    Kind
			text    string
			integer int64
		}{
			{KindText, "orchid", 0},
			{KindNull, "", 0},
			{KindInteger, "29", 29},
		} {
			rowTest(t, rows)
			integer, ok, err := rows.ColumnInt64(0)
			must(t, err)
			if integer != want.integer || ok != (want.kind != KindNull) {
				t.Fatalf("row %d integer = (%d, %v)", i, integer, ok)
			}
			text, ok, err := rows.ColumnText(0)
			must(t, err)
			if text != want.text || ok != (want.kind != KindNull) {
				t.Fatalf("row %d text = (%q, %v)", i, text, ok)
			}
			kind, err := rows.ColumnType(0)
			must(t, err)
			if kind != want.kind {
				t.Fatalf("row %d kind = %v, want %v", i, kind, want.kind)
			}
		}
		row, err := rows.Step()
		must(t, err)
		if row {
			t.Fatal("changing-kind query did not complete")
		}
		must(t, rows.Close())

		s := prepareTest(t, c, "SELECT ?1, ?2")
		const originalText = "paper\x00crane"
		originalBlob := []byte{0x71, 0, 0xa4, 0x19}
		must(t, s.BindText(1, originalText))
		must(t, s.BindBlob(2, originalBlob))
		rowTest(t, s)
		textValue, err := s.ColumnValue(0)
		must(t, err)
		blobValue, err := s.ColumnValue(1)
		must(t, err)
		text, ok, err := s.ColumnText(0)
		must(t, err)
		if !ok {
			t.Fatal("owned text unexpectedly NULL")
		}
		blob, err := s.ColumnBlob(1)
		must(t, err)
		must(t, s.Reset())
		must(t, s.BindText(1, "replacement fabric"))
		must(t, s.BindBlob(2, []byte{1, 2}))
		rowTest(t, s)
		must(t, s.Close())
		if text != originalText || !bytes.Equal(blob, originalBlob) {
			t.Fatalf("owned accessors changed after reuse and close: %q, %x", text, blob)
		}
		coreContractValue(t, textValue, ValueCopy{Kind: KindText, Text: originalText})
		coreContractValue(t, blobValue, ValueCopy{Kind: KindBlob, Blob: originalBlob})
	})

	t.Run("schema-refresh-with-active-cursor", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE specimens(code INTEGER); INSERT INTO specimens VALUES(3), (7)")
		s := prepareTest(t, c, "SELECT * FROM specimens ORDER BY code")
		defer s.Close()
		rowTest(t, s)
		script(t, c, "ALTER TABLE specimens ADD COLUMN mass REAL")
		for _, wantCode := range []int64{3, 7} {
			width, err := s.ColumnCount()
			must(t, err)
			if width != 1 || intTest(t, s, 0) != wantCode {
				t.Fatalf("active cursor shape/value = width %d, want original row %d", width, wantCode)
			}
			if wantCode == 3 {
				rowTest(t, s)
			}
		}
		row, err := s.Step()
		must(t, err)
		if row {
			t.Fatal("original cursor had an extra row")
		}
		must(t, s.Reset())
		rowTest(t, s)
		width, err := s.ColumnCount()
		must(t, err)
		if width != 2 {
			t.Fatalf("reprepared width = %d, want 2", width)
		}
		for i, want := range []struct{ name, declaration string }{{"code", "INTEGER"}, {"mass", "REAL"}} {
			name, err := s.ColumnName(i)
			must(t, err)
			declaration, err := s.ColumnDeclType(i)
			must(t, err)
			if name != want.name || declaration != want.declaration {
				t.Fatalf("column %d metadata = (%q, %q), want (%q, %q)", i, name, declaration, want.name, want.declaration)
			}
		}
		kind, err := s.ColumnType(1)
		must(t, err)
		if kind != KindNull {
			t.Fatalf("added column for existing row has kind %v", kind)
		}
		must(t, s.Reset())
		script(t, c, "ALTER TABLE specimens ADD COLUMN label TEXT")
		_, err = s.Exec()
		must(t, err)
		width, err = s.ColumnCount()
		must(t, err)
		name, err := s.ColumnName(2)
		must(t, err)
		declaration, err := s.ColumnDeclType(2)
		must(t, err)
		if width != 3 || name != "label" || declaration != "TEXT" {
			t.Fatalf("Exec reprepare metadata = (%d, %q, %q)", width, name, declaration)
		}
	})

	t.Run("wide-row", func(t *testing.T) {
		c := openTest(t, ":memory:")
		columns := make([]string, 193)
		for i := range columns {
			columns[i] = strconv.Itoa(i) + " AS c" + strconv.Itoa(i)
		}
		s := prepareTest(t, c, "SELECT "+strings.Join(columns, ", "))
		defer s.Close()
		rowTest(t, s)
		width, err := s.ColumnCount()
		must(t, err)
		name, err := s.ColumnName(192)
		must(t, err)
		if width != 193 || name != "c192" || intTest(t, s, 192) != 192 {
			t.Fatalf("wide-row final column: width %d, name %q", width, name)
		}
	})

	t.Run("native-affinity-matrix", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE affinity_samples(raw, signed INTEGER, fraction REAL, label TEXT)")
		insert := prepareTest(t, c, "INSERT INTO affinity_samples VALUES(?1, ?1, ?1, ?1)")
		defer insert.Close()
		cases := []struct {
			name  string
			input ValueCopy
			want  [4]ValueCopy
		}{
			{"null", ValueCopy{Kind: KindNull}, [4]ValueCopy{{Kind: KindNull}, {Kind: KindNull}, {Kind: KindNull}, {Kind: KindNull}}},
			{"empty-text", ValueCopy{Kind: KindText}, [4]ValueCopy{{Kind: KindText}, {Kind: KindText}, {Kind: KindText}, {Kind: KindText}}},
			{"empty-blob", ValueCopy{Kind: KindBlob, Blob: []byte{}}, [4]ValueCopy{{Kind: KindBlob, Blob: []byte{}}, {Kind: KindBlob, Blob: []byte{}}, {Kind: KindBlob, Blob: []byte{}}, {Kind: KindBlob, Blob: []byte{}}}},
			{"integer-zero", ValueCopy{Kind: KindInteger}, [4]ValueCopy{{Kind: KindInteger}, {Kind: KindInteger}, {Kind: KindFloat}, {Kind: KindText, Text: "0"}}},
			{"real-zero", ValueCopy{Kind: KindFloat}, [4]ValueCopy{{Kind: KindFloat}, {Kind: KindInteger}, {Kind: KindFloat}, {Kind: KindText, Text: "0.0"}}},
			{"fractional-real", ValueCopy{Kind: KindFloat, Float64: 6.75}, [4]ValueCopy{{Kind: KindFloat, Float64: 6.75}, {Kind: KindFloat, Float64: 6.75}, {Kind: KindFloat, Float64: 6.75}, {Kind: KindText, Text: "6.75"}}},
			{"integer", ValueCopy{Kind: KindInteger, Int64: -12}, [4]ValueCopy{{Kind: KindInteger, Int64: -12}, {Kind: KindInteger, Int64: -12}, {Kind: KindFloat, Float64: -12}, {Kind: KindText, Text: "-12"}}},
			{"numeric-text", ValueCopy{Kind: KindText, Text: "023"}, [4]ValueCopy{{Kind: KindText, Text: "023"}, {Kind: KindInteger, Int64: 23}, {Kind: KindFloat, Float64: 23}, {Kind: KindText, Text: "023"}}},
			{"numeric-blob", ValueCopy{Kind: KindBlob, Blob: []byte("023")}, [4]ValueCopy{{Kind: KindBlob, Blob: []byte("023")}, {Kind: KindBlob, Blob: []byte("023")}, {Kind: KindBlob, Blob: []byte("023")}, {Kind: KindBlob, Blob: []byte("023")}}},
		}
		for _, tc := range cases {
			switch tc.input.Kind {
			case KindNull:
				must(t, insert.BindNull(1))
			case KindInteger:
				must(t, insert.BindInt64(1, tc.input.Int64))
			case KindFloat:
				must(t, insert.BindFloat64(1, tc.input.Float64))
			case KindText:
				must(t, insert.BindText(1, tc.input.Text))
			case KindBlob:
				must(t, insert.BindBlob(1, tc.input.Blob))
			}
			_, err := insert.Exec()
			must(t, err)
		}
		query := prepareTest(t, c, "SELECT raw, signed, fraction, label FROM affinity_samples ORDER BY rowid")
		defer query.Close()
		for _, tc := range cases {
			rowTest(t, query)
			for column, want := range tc.want {
				got, err := query.ColumnValue(column)
				must(t, err)
				if !reflect.DeepEqual(got, want) {
					t.Fatalf("%s affinity column %d = %#v, want %#v", tc.name, column, got, want)
				}
				kind, err := query.ColumnType(column)
				must(t, err)
				if kind != want.Kind {
					t.Fatalf("%s affinity column %d kind = %v, want %v", tc.name, column, kind, want.Kind)
				}
			}
		}
		row, err := query.Step()
		must(t, err)
		if row {
			t.Fatal("affinity matrix produced extra rows")
		}
	})
}
