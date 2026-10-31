// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"bytes"
	"errors"
	"testing"
)

func testSessionRoundTripContract(t *testing.T) {
	const table = "lantern_stock"
	type stockRow struct {
		id    int64
		label string
		units int64
	}

	c := openTest(t, ":memory:")
	script(t, c, `
		CREATE TABLE lantern_stock (
			stock_id INTEGER PRIMARY KEY,
			label TEXT NOT NULL,
			units INTEGER NOT NULL
		);
		INSERT INTO lantern_stock VALUES
			(17, 'wick', 6),
			(38, 'lens', 11),
			(64, 'oil', 8),
			(92, 'mantle', 3);
	`)

	assertRows := func(stage string, want []stockRow) {
		t.Helper()
		stmt := prepareTest(t, c, "SELECT stock_id, label, units FROM lantern_stock ORDER BY stock_id")
		for i, expected := range want {
			rowTest(t, stmt)
			label, nonNull, err := stmt.ColumnText(1)
			must(t, err)
			if !nonNull {
				t.Fatalf("%s: row %d has a NULL label", stage, i)
			}
			got := stockRow{id: intTest(t, stmt, 0), label: label, units: intTest(t, stmt, 2)}
			if got != expected {
				t.Fatalf("%s: row %d = %+v, want %+v", stage, i, got, expected)
			}
		}
		more, err := stmt.Step()
		must(t, err)
		if more {
			t.Fatalf("%s: unexpected row after %d expected rows", stage, len(want))
		}
		must(t, stmt.Close())
	}

	original := []stockRow{
		{17, "wick", 6},
		{38, "lens", 11},
		{64, "oil", 8},
		{92, "mantle", 3},
	}
	assertRows("initial contents", original)

	session, err := c.NewSession("main")
	must(t, err)
	t.Cleanup(func() { must(t, session.Close()) })
	must(t, session.AttachAll())
	script(t, c, `
		INSERT INTO lantern_stock VALUES (206, 'reflector', 2);
		DELETE FROM lantern_stock WHERE stock_id = 64;
		UPDATE lantern_stock SET label = 'wick bundle', units = 12 WHERE stock_id = 17;
		INSERT INTO lantern_stock VALUES (125, 'bracket', 4);
		UPDATE lantern_stock SET stock_id = 171 WHERE stock_id = 17;
		INSERT INTO lantern_stock VALUES (83, 'chimney', 5);
		UPDATE lantern_stock SET label = 'corner bracket', units = 9 WHERE stock_id = 125;
	`)
	forward := []stockRow{
		{38, "lens", 11},
		{83, "chimney", 5},
		{92, "mantle", 3},
		{125, "corner bracket", 9},
		{171, "wick bundle", 12},
		{206, "reflector", 2},
	}
	assertRows("forward contents", forward)
	script(t, c, "UPDATE lantern_stock SET label = 'unreachable', units = -1 WHERE stock_id = 999")
	if affected := scalarTest(t, c, "SELECT changes()"); affected != 0 {
		t.Fatalf("unmatched UPDATE affected %d rows, want 0", affected)
	}
	assertRows("after unmatched update", forward)
	forwardCount := scalarTest(t, c, "SELECT count(*) FROM lantern_stock")
	// Four original rows, three new rows, and one deletion leave six rows.
	if forwardCount != 6 {
		t.Fatalf("forward row count = %d, want 6", forwardCount)
	}

	var changeset, patchset bytes.Buffer
	must(t, session.WriteChangeset(&changeset))
	must(t, session.WritePatchset(&patchset))
	must(t, session.Close())

	iter, err := NewChangesetIter(bytes.NewReader(changeset.Bytes()))
	must(t, err)
	t.Cleanup(func() { must(t, iter.Close()) })
	remaining := map[Operation]map[int64]stockRow{
		OperationDelete: {
			17: {17, "wick", 6},
			64: {64, "oil", 8},
		},
		OperationInsert: {
			83:  {83, "chimney", 5},
			125: {125, "corner bracket", 9},
			171: {171, "wick bundle", 12},
			206: {206, "reflector", 2},
		},
	}
	counts := make(map[Operation]int)
	for {
		more, err := iter.Next()
		must(t, err)
		if !more {
			break
		}
		change, err := iter.Change()
		must(t, err)
		info, err := change.Info()
		must(t, err)
		if info.Table != table || info.Columns != 3 || info.Indirect {
			t.Fatalf("change metadata = %+v, want table %q, 3 columns, direct", info, table)
		}
		counts[info.Operation]++
		get := change.New
		switch info.Operation {
		case OperationInsert:
		case OperationDelete:
			get = change.Old
		default:
			t.Fatalf("unexpected changeset operation %v", info.Operation)
		}

		keyValue, err := get(0)
		must(t, err)
		id, keyPresent, err := keyValue.Int64()
		must(t, err)
		labelValue, err := get(1)
		must(t, err)
		label, labelPresent, err := labelValue.Text()
		must(t, err)
		unitsValue, err := get(2)
		must(t, err)
		units, unitsPresent, err := unitsValue.Int64()
		must(t, err)
		if !keyPresent || !labelPresent || !unitsPresent {
			t.Fatalf("operation %v contains a NULL field", info.Operation)
		}
		expected, ok := remaining[info.Operation][id]
		if !ok {
			t.Fatalf("unexpected or repeated operation %v for stock_id %d", info.Operation, id)
		}
		if got := (stockRow{id, label, units}); got != expected {
			t.Fatalf("operation %v row = %+v, want %+v", info.Operation, got, expected)
		}
		delete(remaining[info.Operation], id)
	}
	must(t, iter.Close())
	// The moved key contributes one DELETE and one INSERT. The explicit
	// deletion and three new keys contribute the other four operations.
	if counts[OperationDelete] != 2 || counts[OperationInsert] != 4 || counts[OperationUpdate] != 0 {
		t.Fatalf("changeset counts: DELETE=%d INSERT=%d UPDATE=%d, want 2, 4, 0",
			counts[OperationDelete], counts[OperationInsert], counts[OperationUpdate])
	}
	if len(remaining[OperationDelete]) != 0 || len(remaining[OperationInsert]) != 0 {
		t.Fatalf("changeset omitted expected rows: %v", remaining)
	}

	var inverse, reinverted bytes.Buffer
	must(t, InvertChangeset(&inverse, bytes.NewReader(changeset.Bytes())))
	must(t, InvertChangeset(&reinverted, bytes.NewReader(inverse.Bytes())))
	if !bytes.Equal(reinverted.Bytes(), changeset.Bytes()) {
		t.Fatal("inverting twice did not reproduce the original changeset bytes")
	}
	must(t, c.ApplyChangeset(bytes.NewReader(inverse.Bytes()), ApplyOptions{}))
	assertRows("inverse restoration", original)

	filterCalls, replayConflicts := 0, 0
	must(t, c.ApplyChangeset(bytes.NewReader(patchset.Bytes()), ApplyOptions{
		Filter: func(name string) bool {
			filterCalls++
			if name != table {
				t.Errorf("patchset filter received table %q, want %q", name, table)
			}
			return name == table
		},
		Conflict: func(Conflict) (ConflictAction, error) {
			replayConflicts++
			t.Error("first patchset replay unexpectedly encountered a conflict")
			return Abort, nil
		},
	}))
	if filterCalls != 1 || replayConflicts != 0 {
		t.Fatalf("patchset callback counts: filter=%d conflict=%d, want 1, 0", filterCalls, replayConflicts)
	}
	if got := scalarTest(t, c, "SELECT count(*) FROM lantern_stock"); got != forwardCount {
		t.Fatalf("patchset row count = %d, want captured forward count %d", got, forwardCount)
	}
	assertRows("patchset replay contents", forward)

	// Within-table change order is unspecified. Restore the deleted keys so
	// replay reaches an INSERT collision rather than a missing-row DELETE,
	// regardless of which operation SQLite visits first.
	script(t, c, "INSERT INTO lantern_stock VALUES (17, 'wick', 6), (64, 'oil', 8)")
	beforeAbort := []stockRow{
		{17, "wick", 6},
		{38, "lens", 11},
		{64, "oil", 8},
		{83, "chimney", 5},
		{92, "mantle", 3},
		{125, "corner bracket", 9},
		{171, "wick bundle", 12},
		{206, "reflector", 2},
	}
	assertRows("before repeat abort", beforeAbort)
	conflictCalls := 0
	err = c.ApplyChangeset(bytes.NewReader(patchset.Bytes()), ApplyOptions{
		Conflict: func(conflict Conflict) (ConflictAction, error) {
			conflictCalls++
			kind, err := conflict.Type()
			if err != nil {
				t.Errorf("repeat conflict type: %v", err)
				return Abort, err
			}
			if kind != ConflictConflict {
				t.Errorf("repeat conflict type = %v, want %v", kind, ConflictConflict)
			}
			change, err := conflict.Change()
			if err != nil {
				t.Errorf("repeat incoming change: %v", err)
				return Abort, err
			}
			info, err := change.Info()
			if err != nil {
				t.Errorf("repeat incoming metadata: %v", err)
				return Abort, err
			}
			if info.Operation != OperationInsert || info.Table != table || info.Columns != 3 {
				t.Errorf("repeat incoming metadata = %+v, want INSERT into %q with 3 columns", info, table)
			}
			return Abort, nil
		},
	})
	if !errors.Is(err, CodeAbort) {
		t.Fatalf("repeat patchset error = %v, want CodeAbort", err)
	}
	if conflictCalls != 1 {
		t.Fatalf("repeat conflict callback count = %d, want 1", conflictCalls)
	}
	assertRows("repeat abort rollback", beforeAbort)
}
