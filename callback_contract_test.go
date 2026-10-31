// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package sqlite

import (
	"errors"
	"path/filepath"
	"runtime"
	"slices"
	"testing"
	"time"
)

func testCallbackTransactions(t *testing.T) {
	c := openTest(t, ":memory:")
	script(t, c, "CREATE TABLE settlement(entry INTEGER PRIMARY KEY, units INTEGER NOT NULL)")

	accept := true
	var history []string
	must(t, c.SetCommitHook(func() bool {
		if accept {
			history = append(history, "accepted commit")
		} else {
			history = append(history, "vetoed commit")
		}
		return !accept
	}))
	must(t, c.SetRollbackHook(func() {
		history = append(history, "rollback")
	}))

	must(t, c.Begin())
	script(t, c, "INSERT INTO settlement VALUES (13, 64), (29, 81)")
	must(t, c.Commit())
	if !slices.Equal(history, []string{"accepted commit"}) {
		t.Fatalf("accepted transaction hooks = %v", history)
	}

	accept = false
	must(t, c.Begin())
	script(t, c, "UPDATE settlement SET units = 400 WHERE entry = 13; INSERT INTO settlement VALUES (47, 125)")
	if err := c.Commit(); !errors.Is(err, CodeConstraintCommitHook) {
		t.Fatalf("vetoed Commit = %v, want %v", err, CodeConstraintCommitHook)
	}
	if want := []string{"accepted commit", "vetoed commit", "rollback"}; !slices.Equal(history, want) {
		t.Fatalf("transaction hook order = %v, want %v", history, want)
	}
	auto, err := c.AutoCommit()
	must(t, err)
	if !auto {
		t.Fatal("commit veto left a transaction active")
	}
	if got := scalarTest(t, c, "SELECT count(*) FROM settlement"); got != 2 {
		t.Fatalf("persisted row count = %d, want 2", got)
	}
	if got := scalarTest(t, c, "SELECT units FROM settlement WHERE entry = 13"); got != 64 {
		t.Fatalf("rolled-back update left units = %d, want 64", got)
	}
	if got := scalarTest(t, c, "SELECT units FROM settlement WHERE entry = 29"); got != 81 {
		t.Fatalf("accepted second row has units = %d, want 81", got)
	}
	if got := scalarTest(t, c, "SELECT count(*) FROM settlement WHERE entry = 47"); got != 0 {
		t.Fatalf("vetoed insert persisted %d rows", got)
	}
}

func testCallbackUpdates(t *testing.T) {
	c := openTest(t, ":memory:")
	script(t, c, "CREATE TABLE parcel_route(parcel INTEGER PRIMARY KEY, dock TEXT)")

	oldCalls := 0
	must(t, c.SetUpdateHook(func(UpdateEvent) { oldCalls++ }))
	var events []UpdateEvent
	must(t, c.SetUpdateHook(func(event UpdateEvent) {
		events = append(events, event)
	}))
	script(t, c, `
		INSERT INTO parcel_route VALUES (73, 'west'), (18, 'east');
		UPDATE parcel_route SET dock = 'north' WHERE parcel = 73;
		DELETE FROM parcel_route WHERE parcel = 18;
	`)
	want := []UpdateEvent{
		{Operation: OperationInsert, Schema: "main", Table: "parcel_route", RowID: 73},
		{Operation: OperationInsert, Schema: "main", Table: "parcel_route", RowID: 18},
		{Operation: OperationUpdate, Schema: "main", Table: "parcel_route", RowID: 73},
		{Operation: OperationDelete, Schema: "main", Table: "parcel_route", RowID: 18},
	}
	if oldCalls != 0 {
		t.Fatalf("replaced update hook ran %d times", oldCalls)
	}
	if !slices.Equal(events, want) {
		t.Fatalf("update events = %+v, want %+v", events, want)
	}

	must(t, c.SetUpdateHook(nil))
	script(t, c, "INSERT INTO parcel_route VALUES (96, 'south'); DROP TABLE parcel_route")
	if !slices.Equal(events, want) {
		t.Fatalf("unregistered update hook changed events: %+v", events)
	}
	must(t, c.Close())
	if !slices.Equal(events, want) {
		t.Fatalf("owned update events changed after Close: %+v", events)
	}
}

func testCallbackAuthorization(t *testing.T) {
	c := openTest(t, ":memory:")
	script(t, c, `
		CREATE TABLE gauge_samples(sample INTEGER PRIMARY KEY, ticks INTEGER NOT NULL);
		CREATE VIEW gauge_report AS SELECT ticks FROM gauge_samples;
	`)

	var events []Authorization
	must(t, c.SetAuthorizer(func(event Authorization) AuthorizerResult {
		events = append(events, event)
		return AuthorizerOK
	}))
	present := func(text string) OptionalText { return OptionalText{Text: text, Valid: true} }
	wantInsert := Authorization{
		Action: AuthorizerInsert,
		Arg1:   present("gauge_samples"), Schema: present("main"),
	}
	script(t, c, "INSERT INTO gauge_samples VALUES (37, 24)")
	if !slices.Equal(events, []Authorization{wantInsert}) {
		t.Fatalf("direct insert authorizations = %+v, want %+v", events, wantInsert)
	}
	ownedInsert := events[0]

	events = nil
	direct := prepareTest(t, c, "SELECT ticks FROM gauge_samples")
	rowTest(t, direct)
	if got := intTest(t, direct, 0); got != 24 {
		t.Fatalf("authorized direct read = %d, want 24", got)
	}
	if row, err := direct.Step(); err != nil || row {
		t.Fatalf("direct read completion = (%v, %v)", row, err)
	}
	must(t, direct.Close())
	wantRead := Authorization{
		Action: AuthorizerRead,
		Arg1:   present("gauge_samples"), Arg2: present("ticks"), Schema: present("main"),
	}
	wantDirect := []Authorization{{Action: AuthorizerSelect}, wantRead}
	if !slices.Equal(events, wantDirect) {
		t.Fatalf("direct read authorizations = %+v, want %+v", events, wantDirect)
	}
	ownedRead := events[1]

	events = nil
	if got := scalarTest(t, c, "SELECT ticks FROM gauge_report"); got != 24 {
		t.Fatalf("view read = %d, want 24", got)
	}
	wantView := wantRead
	wantView.Origin = present("gauge_report")
	var ownedView Authorization
	viewReads := 0
	for _, event := range events {
		if event.Action == AuthorizerRead && event.Arg1 == present("gauge_samples") {
			viewReads++
			ownedView = event
			if event != wantView {
				t.Fatalf("view-origin authorization = %+v, want %+v", event, wantView)
			}
		}
	}
	if viewReads != 1 {
		t.Fatalf("view authorized %d base-column reads, want 1", viewReads)
	}

	events = nil
	if got := scalarTest(t, c, "SELECT count(*) FROM gauge_samples"); got != 1 {
		t.Fatalf("authorized count = %d, want 1", got)
	}
	wantCount := Authorization{
		Action: AuthorizerRead,
		Arg1:   present("gauge_samples"), Arg2: present(""),
	}
	var ownedCount Authorization
	countReads := 0
	for _, event := range events {
		if event.Action == AuthorizerRead {
			countReads++
			ownedCount = event
			if event != wantCount {
				t.Fatalf("count authorization = %+v, want %+v", event, wantCount)
			}
		}
	}
	if countReads != 1 {
		t.Fatalf("count authorized %d reads, want 1", countReads)
	}

	oldEventCount := len(events)
	must(t, c.SetAuthorizer(func(event Authorization) AuthorizerResult {
		switch event.Action {
		case AuthorizerInsert:
			return AuthorizerDeny
		case AuthorizerRead:
			return AuthorizerIgnore
		default:
			return AuthorizerOK
		}
	}))
	if err := c.ExecScript("INSERT INTO gauge_samples VALUES (52, 99)"); !errors.Is(err, CodeAuth) {
		t.Fatalf("denied insert = %v, want %v", err, CodeAuth)
	}
	ignored := prepareTest(t, c, "SELECT ticks FROM gauge_samples")
	rowTest(t, ignored)
	value, nonNull, err := ignored.ColumnInt64(0)
	must(t, err)
	if value != 0 || nonNull {
		t.Fatalf("ignored read = (%d, non-NULL %v), want SQL NULL", value, nonNull)
	}
	if row, err := ignored.Step(); err != nil || row {
		t.Fatalf("ignored read completion = (%v, %v)", row, err)
	}
	must(t, ignored.Close())
	if len(events) != oldEventCount {
		t.Fatalf("replaced authorizer received %d additional events", len(events)-oldEventCount)
	}

	must(t, c.SetAuthorizer(nil))
	if got := scalarTest(t, c, "SELECT sum(ticks) FROM gauge_samples"); got != 24 {
		t.Fatalf("aggregate after removing authorizer = %d, want 24", got)
	}
	must(t, c.Close())
	if got, want := []Authorization{ownedInsert, ownedRead, ownedView, ownedCount}, []Authorization{wantInsert, wantRead, wantView, wantCount}; !slices.Equal(got, want) {
		t.Fatalf("owned authorizations changed after Close: %+v, want %+v", got, want)
	}
}

func TestBusyHandler(t *testing.T) {
	path := filepath.Join(t.TempDir(), "write-reservation.sqlite")
	holder := openTest(t, path)
	contender := openTest(t, path)
	script(t, holder, "CREATE TABLE loading_queue(batch INTEGER PRIMARY KEY, cost INTEGER)")
	must(t, holder.BeginImmediate())
	script(t, holder, "INSERT INTO loading_queue VALUES (101, 9)")
	const write = "INSERT INTO loading_queue VALUES (202, 16)"

	if err := contender.ExecScript(write); !errors.Is(err, CodeBusy) {
		t.Fatalf("write without busy handler = %v, want %v", err, CodeBusy)
	}
	must(t, contender.BusyTimeout(200*time.Microsecond))
	if err := contender.ExecScript(write); !errors.Is(err, CodeBusy) {
		t.Fatalf("write with sub-millisecond timeout = %v, want %v", err, CodeBusy)
	}

	var attempts []int
	must(t, contender.SetBusyHandler(func(attempt int) bool {
		attempts = append(attempts, attempt)
		return attempt < 3
	}))
	if err := contender.ExecScript(write); !errors.Is(err, CodeBusy) {
		t.Fatalf("write after retry budget = %v, want %v", err, CodeBusy)
	}
	wantAttempts := []int{0, 1, 2, 3}
	if !slices.Equal(attempts, wantAttempts) {
		t.Fatalf("busy attempts = %v, want %v", attempts, wantAttempts)
	}

	must(t, contender.BusyTimeout(0))
	if err := contender.ExecScript(write); !errors.Is(err, CodeBusy) {
		t.Fatalf("write after disabling timeout = %v, want %v", err, CodeBusy)
	}
	if !slices.Equal(attempts, wantAttempts) {
		t.Fatalf("timeout replacement called the old handler: %v", attempts)
	}

	var freshAttempts []int
	var releaseErr error
	must(t, contender.SetBusyHandler(func(attempt int) bool {
		freshAttempts = append(freshAttempts, attempt)
		if len(freshAttempts) != 1 {
			return false
		}
		releaseErr = holder.Rollback()
		return releaseErr == nil
	}))
	writeErr := contender.ExecScript(write)
	must(t, releaseErr)
	must(t, writeErr)
	if !slices.Equal(freshAttempts, []int{0}) {
		t.Fatalf("replacement handler attempts = %v, want [0]", freshAttempts)
	}
	if !slices.Equal(attempts, wantAttempts) {
		t.Fatalf("old handler ran after replacement: %v", attempts)
	}
	if got := scalarTest(t, contender, "SELECT count(*) FROM loading_queue"); got != 1 {
		t.Fatalf("row count after releasing reservation = %d, want 1", got)
	}
	if got := scalarTest(t, contender, "SELECT cost FROM loading_queue WHERE batch = 202"); got != 16 {
		t.Fatalf("retried write cost = %d, want 16", got)
	}
}

// The aggregate cannot emit a row until all 8^12 bounded combinations have
// been visited. Only the eight-row input needs materialization.
const interruptQuery = `
	WITH octet(v) AS (VALUES (0), (1), (2), (3), (4), (5), (6), (7))
	SELECT sum(a.v + b.v + c.v + d.v + e.v + f.v +
	           g.v + h.v + i.v + j.v + k.v + l.v)
	FROM octet AS a
	CROSS JOIN octet AS b
	CROSS JOIN octet AS c
	CROSS JOIN octet AS d
	CROSS JOIN octet AS e
	CROSS JOIN octet AS f
	CROSS JOIN octet AS g
	CROSS JOIN octet AS h
	CROSS JOIN octet AS i
	CROSS JOIN octet AS j
	CROSS JOIN octet AS k
	CROSS JOIN octet AS l
`

func testInterruptWatcherContract(t *testing.T) {
	c := openTest(t, ":memory:")
	s := prepareTest(t, c, interruptQuery)
	start := make(chan struct{})
	stop := make(chan struct{})
	joined := make(chan error, 1)
	go func() {
		<-start
		for {
			select {
			case <-stop:
				joined <- nil
				return
			default:
			}
			if err := c.Interrupt(); err != nil {
				joined <- err
				return
			}
			runtime.Gosched()
		}
	}()

	close(start)
	row, stepErr := s.Step()
	close(stop)
	watchErr := <-joined
	must(t, watchErr)
	if row || !errors.Is(stepErr, CodeInterrupt) {
		t.Fatalf("interrupted Step = (%v, %v), want (false, %v)", row, stepErr, CodeInterrupt)
	}
	if err := s.Reset(); !errors.Is(err, CodeInterrupt) {
		t.Fatalf("Reset after interruption = %v, want %v", err, CodeInterrupt)
	}
	must(t, s.Close())
	if got := scalarTest(t, c, "SELECT 11 * 23"); got != 253 {
		t.Fatalf("query after stopping watcher = %d, want 253", got)
	}
}
