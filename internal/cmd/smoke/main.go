// Command smoke exercises the public interface against throwaway databases.
package main

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"runtime"

	sqlite "github.com/oskoi/sqlite-go"
)

func check(err error) {
	if err != nil {
		panic(err)
	}
}
func scalar(c *sqlite.Conn, sql string) int64 {
	s, _, err := c.Prepare(sql, 0)
	check(err)
	row, err := s.Step()
	check(err)
	if !row {
		panic("missing scalar row")
	}
	n, ok, err := s.ColumnInt64(0)
	check(err)
	if !ok {
		panic("NULL scalar")
	}
	check(s.Close())
	return n
}

func callbacksAndInterrupt(c *sqlite.Conn) {
	check(c.ExecScript("CREATE TABLE interrupt_demo(v)"))
	query, _, err := c.Prepare(`WITH RECURSIVE n(x) AS (
 SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<100
) INSERT INTO interrupt_demo SELECT x FROM n`, 0)
	check(err)
	active, interrupted := make(chan struct{}), make(chan error, 1)
	stop, joined := make(chan struct{}), make(chan struct{})
	calls, interrupts := 0, 0
	check(c.SetUpdateHook(func(e sqlite.UpdateEvent) {
		calls++
		if calls == 1 {
			if e.Operation != sqlite.OperationInsert || e.Schema != "main" || e.Table != "interrupt_demo" || e.RowID != 1 {
				panic("native-active event")
			}
			close(active) // SQLite's INSERT VM is already running.
			check(<-interrupted)
		}
	}))
	go func() {
		defer close(joined)
		select {
		case <-active:
			interrupts++
			interrupted <- c.Interrupt()
		case <-stop:
			return
		}
		<-stop
	}()
	row, err := query.Step()
	close(stop)
	<-joined // No late interrupt can reach the next borrower.
	if row || !errors.Is(err, sqlite.CodeInterrupt) {
		panic(fmt.Sprintf("interrupt: row=%v err=%v", row, err))
	}
	if err := query.Reset(); !errors.Is(err, sqlite.CodeInterrupt) {
		panic(err)
	}
	check(query.Close())
	check(c.SetUpdateHook(nil))
	remaining := scalar(c, "SELECT count(*) FROM interrupt_demo")
	if calls != 1 || interrupts != 1 || remaining != 0 {
		panic("interrupted INSERT effects")
	}
	reused := scalar(c, "SELECT 42")
	if reused != 42 {
		panic("connection reuse")
	}
	fmt.Printf("recursive insert: INTERRUPT; native-active handshake; external-interrupts=%d; watcher stopped/joined; rows=%d; reused=%d\n", interrupts, remaining, reused)

	check(c.ExecScript("CREATE TABLE callback_demo(v); INSERT INTO callback_demo VALUES(1)"))
	commits, rollbacks := 0, 0
	check(c.SetCommitHook(func() bool { commits++; return true }))
	check(c.SetRollbackHook(func() { rollbacks++ }))
	if err := c.ExecScript("INSERT INTO callback_demo VALUES(2)"); !errors.Is(err, sqlite.CodeConstraintCommitHook) {
		panic(err)
	}
	count, sum := scalar(c, "SELECT count(*) FROM callback_demo"), scalar(c, "SELECT sum(v) FROM callback_demo")
	if commits != 1 || rollbacks != 1 || count != 1 || sum != 1 {
		panic("commit veto effects")
	}
	fmt.Printf("commit veto: commits=%d rollbacks=%d rows=%d sum=%d\n", commits, rollbacks, count, sum)
	check(c.SetCommitHook(nil))
	check(c.SetRollbackHook(nil))
	value := &struct{ message string }{"hook panic"}
	calls = 0
	check(c.SetUpdateHook(func(sqlite.UpdateEvent) { calls++; panic(value) }))
	recovered := false
	func() {
		defer func() {
			if got := recover(); got != value {
				panic(fmt.Sprintf("unexpected hook panic: %#v", got))
			}
			recovered = true
		}()
		check(c.ExecScript("INSERT INTO callback_demo VALUES(3),(4)"))
	}()
	count, sum = scalar(c, "SELECT count(*) FROM callback_demo"), scalar(c, "SELECT sum(v) FROM callback_demo")
	if !recovered || calls != 1 || count != 3 || sum != 8 {
		panic("notification panic effects")
	}
	// Panic cleanup detached every managed hook; reinstall explicitly.
	check(c.SetUpdateHook(func(sqlite.UpdateEvent) { calls++ }))
	check(c.ExecScript("INSERT INTO callback_demo VALUES(5)"))
	check(c.SetUpdateHook(nil))
	if calls != 2 || scalar(c, "SELECT sum(v) FROM callback_demo") != 13 {
		panic("hook reinstall")
	}
	fmt.Printf("hook panic: same value recovered=%v calls-before-reinstall=1 rows=%d sum=%d; reinstall sum=13\n", recovered, count, sum)
}
func run() {
	dir, err := os.MkdirTemp("", "sqlite-go-smoke-")
	check(err)
	defer os.RemoveAll(dir)
	fmt.Printf("Go %s %s/%s\nSQLite %s\nSource ID %s\nOptions %v\n", runtime.Version(), runtime.GOOS, runtime.GOARCH, sqlite.Version(), sqlite.SourceID(), sqlite.CompileOptions())
	c, err := sqlite.Open(filepath.Join(dir, "fresh.db"), 0)
	check(err)
	check(c.ExecScript(`CREATE TABLE t(id INTEGER PRIMARY KEY,n INTEGER,f REAL,txt TEXT,b BLOB); CREATE TABLE audit(v); CREATE TRIGGER inserted AFTER INSERT ON t BEGIN INSERT INTO audit VALUES('insert;trigger'); INSERT INTO audit VALUES(new.id); END;`))
	insert, _, err := c.Prepare("INSERT INTO t(n,f,txt,b) VALUES(?,?,?,?)", sqlite.PreparePersistent)
	check(err)
	for i := range 2 {
		check(insert.BindInt64(1, int64(i+1)))
		check(insert.BindFloat64(2, float64(i)+0.5))
		if i == 0 {
			check(insert.BindText(3, "a\x00b"))
			check(insert.BindBlob(4, []byte{}))
		} else {
			check(insert.BindText(3, ""))
			check(insert.BindBlob(4, nil))
		}
		result, err := insert.Exec()
		check(err)
		if result.Changes != 1 || result.LastInsertRowID != int64(i+1) {
			panic(result)
		}
	}
	check(insert.Close())
	if scalar(c, "SELECT count(*) FROM audit") != 4 {
		panic("trigger rows")
	}
	query, _, err := c.Prepare("SELECT * FROM t ORDER BY id", 0)
	check(err)
	for i := range 2 {
		row, err := query.Step()
		check(err)
		if !row {
			panic("missing mixed row")
		}
		id, _, err := query.ColumnInt64(0)
		check(err)
		n, _, err := query.ColumnInt64(1)
		check(err)
		f, _, err := query.ColumnFloat64(2)
		check(err)
		text, nonNull, err := query.ColumnText(3)
		check(err)
		blob, err := query.ColumnBlob(4)
		check(err)
		wantText := "a\x00b"
		if i == 1 {
			wantText = ""
		}
		if id != int64(i+1) || n != int64(i+1) || f != float64(i)+0.5 || text != wantText || !nonNull || (blob == nil) != (i == 1) || len(blob) != 0 {
			panic("mixed row mismatch")
		}
		fmt.Printf("row id=%d n=%d f=%.1f text=%q textNonNull=%v blobNil=%v blobLen=%d\n", id, n, f, text, nonNull, blob == nil, len(blob))
	}
	if row, err := query.Step(); err != nil || row {
		panic("DONE")
	}
	if row, err := query.Step(); err != nil || row {
		panic("sticky DONE")
	}
	check(query.Reset())
	check(c.ExecScript("ALTER TABLE t ADD added TEXT DEFAULT 'new'"))
	row, err := query.Step()
	check(err)
	if !row {
		panic("reprepare row")
	}
	count, err := query.ColumnCount()
	check(err)
	names := make([]string, count)
	for i := range count {
		names[i], err = query.ColumnName(i)
		check(err)
	}
	if !reflect.DeepEqual(names, []string{"id", "n", "f", "txt", "b", "added"}) {
		panic(names)
	}
	fmt.Printf("reprepare names=%v\n", names)
	if err := c.Close(); !errors.Is(err, sqlite.CodeBusy) {
		panic("strict Close")
	}
	check(query.Close())
	for _, module := range []string{"fts3", "fts4", "fts5"} {
		check(c.ExecScript("CREATE VIRTUAL TABLE " + module + "_smoke USING " + module + "(body); INSERT INTO " + module + "_smoke VALUES('native sqlite')"))
		if scalar(c, "SELECT count(*) FROM "+module+"_smoke WHERE body MATCH 'sqlite'") != 1 {
			panic(module)
		}
	}
	check(c.ExecScript(`CREATE VIRTUAL TABLE spatial USING rtree(id,x1,x2,y1,y2); INSERT INTO spatial VALUES(1,0,1,0,1); CREATE VIRTUAL TABLE poly USING geopoly; INSERT INTO poly(_shape) VALUES('[[0,0],[1,0],[1,1],[0,1],[0,0]]')`))
	if scalar(c, `SELECT count(*) FROM spatial WHERE x1<=0.5 AND x2>=0.5`) != 1 || scalar(c, `SELECT count(*) FROM poly WHERE geopoly_overlap(_shape,'[[0,0],[1,0],[1,1],[0,1],[0,0]]')`) != 1 {
		panic("spatial")
	}
	if scalar(c, `SELECT soundex('Robert')='R163' AND typeof(jsonb('{"x":42}'))='blob' AND json_extract(jsonb('{"x":42}'),'$.x')=42`) != 1 {
		panic("soundex/jsonb")
	}
	callbacksAndInterrupt(c)
	sessionsAndChangesets()
	backups(dir)
	blobs(c)
	check(c.Close())
	utf, err := sqlite.Open(filepath.Join(dir, "utf16.db"), 0)
	check(err)
	check(utf.ExecScript(`PRAGMA encoding='UTF-16le'; CREATE TABLE t(v); INSERT INTO t VALUES('héllo 世界')`))
	s, _, err := utf.Prepare("SELECT v FROM t", 0)
	check(err)
	row, err = s.Step()
	check(err)
	if !row {
		panic("UTF16 row")
	}
	text, ok, err := s.ColumnText(0)
	check(err)
	if !ok || text != "héllo 世界" {
		panic("UTF16 conversion")
	}
	check(s.Close())
	if scalar(utf, `SELECT (SELECT encoding FROM pragma_encoding)='UTF-16le'`) != 1 {
		panic("UTF16 encoding")
	}
	check(utf.Close())
	fmt.Println("FTS3/4/5, RTree, Geopoly, SOUNDEX, JSONB, UTF-16: PASS; strict close: BUSY then closed")
}
func main() { run() }
