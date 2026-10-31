// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"fmt"

	"github.com/oskoi/sqlite-go"
)

func backupAttachedSchema(src, dst *sqlite.Conn) {
	check(src.ExecScript(`
		ATTACH ':memory:' AS "survey source";
		CREATE TABLE "survey source".instrument_codes (
			code TEXT PRIMARY KEY,
			display_name TEXT NOT NULL
		);
		CREATE TABLE "survey source".station_notes (
			station INTEGER PRIMARY KEY,
			code TEXT NOT NULL,
			reading INTEGER NOT NULL
		);
		CREATE INDEX "survey source".station_notes_by_code ON station_notes(code);
		CREATE VIEW "survey source".station_panel AS
			SELECT station, code, reading FROM station_notes;
		CREATE TRIGGER "survey source".station_panel_adjust
			INSTEAD OF UPDATE OF reading ON station_panel
			BEGIN
				UPDATE station_notes SET reading = NEW.reading WHERE station = OLD.station;
			END;
		INSERT INTO "survey source".instrument_codes VALUES
			('altimeter', 'Pressure dial'), ('current', 'Rotor meter');
		INSERT INTO "survey source".station_notes VALUES
			(81, 'altimeter', 934), (206, 'current', 27);
	`))
	check(dst.ExecScript(`ATTACH ':memory:' AS "survey replica"`))
	copy, err := dst.NewBackup("survey replica", src, "survey source")
	check(err)
	done, err := copy.Step(-1)
	check(err)
	if !done {
		panic("attached-schema backup did not finish")
	}
	check(copy.Close())

	catalog, _, err := dst.Prepare(`
		SELECT type, name, tbl_name, sql
		FROM "survey replica".sqlite_schema
		ORDER BY name
	`, 0)
	check(err)
	defer func() { check(catalog.Close()) }()
	objects := []struct {
		kind, name, table string
		hasSQL            bool
	}{
		{"table", "instrument_codes", "instrument_codes", true},
		{"index", "sqlite_autoindex_instrument_codes_1", "instrument_codes", false},
		{"table", "station_notes", "station_notes", true},
		{"index", "station_notes_by_code", "station_notes", true},
		{"view", "station_panel", "station_panel", true},
		{"trigger", "station_panel_adjust", "station_panel", true},
	}
	for _, object := range objects {
		row, err := catalog.Step()
		check(err)
		if !row {
			panic(fmt.Sprintf("missing copied catalog object %q", object.name))
		}
		for column, want := range []string{object.kind, object.name, object.table} {
			value, valid, err := catalog.ColumnText(column)
			check(err)
			if !valid || value != want {
				panic(fmt.Sprintf("copied catalog column %d = (%q, %t), want %q", column, value, valid, want))
			}
		}
		definition, valid, err := catalog.ColumnText(3)
		check(err)
		if valid != object.hasSQL || object.hasSQL && definition == "" {
			panic(fmt.Sprintf("catalog SQL for %q = (%q, %t)", object.name, definition, valid))
		}
	}
	row, err := catalog.Step()
	check(err)
	if row {
		panic("attached-schema backup copied an unexpected catalog object")
	}

	dataChecks := []struct {
		query string
		want  int64
	}{
		{`SELECT count(*) FROM "survey replica".instrument_codes`, 2},
		{`SELECT count(*) FROM "survey replica".instrument_codes
			WHERE (code = 'altimeter' AND display_name = 'Pressure dial')
			   OR (code = 'current' AND display_name = 'Rotor meter')`, 2},
		{`SELECT count(*) FROM "survey replica".station_notes`, 2},
		{`SELECT count(*) FROM "survey replica".station_notes
			WHERE (station = 81 AND code = 'altimeter' AND reading = 934)
			   OR (station = 206 AND code = 'current' AND reading = 27)`, 2},
		{`SELECT reading FROM "survey replica".station_panel WHERE station = 81`, 934},
	}
	for _, assertion := range dataChecks {
		if got := scalar(dst, assertion.query); got != assertion.want {
			panic(fmt.Sprintf("copied survey data = %d, want %d: %s", got, assertion.want, assertion.query))
		}
	}
	check(dst.ExecScript(`UPDATE "survey replica".station_panel SET reading = 941 WHERE station = 81`))
	if got := scalar(dst, `SELECT reading FROM "survey replica".station_notes WHERE station = 81`); got != 941 {
		panic(fmt.Sprintf("copied view trigger left destination reading at %d, want 941", got))
	}
	if got := scalar(src, `SELECT reading FROM "survey source".station_notes WHERE station = 81`); got != 934 {
		panic(fmt.Sprintf("destination view update changed source reading to %d, want 934", got))
	}
	if got := scalar(dst, `SELECT reading FROM "survey replica".station_notes WHERE station = 206`); got != 27 {
		panic(fmt.Sprintf("copied trigger changed another station to %d, want 27", got))
	}
	fmt.Println("backup attached schemas: exact catalog/data; copied view trigger changes replica only")
}
