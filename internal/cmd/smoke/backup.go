// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

package main

import (
	"bytes"
	"errors"
	"fmt"
	"path/filepath"

	sqlite "github.com/oskoi/sqlite-go"
)

func backups(dir string) {
	src, err := sqlite.Open(filepath.Join(dir, "backup-source.db"), 0)
	check(err)
	dst, err := sqlite.Open(filepath.Join(dir, "backup-destination.db"), 0)
	check(err)
	check(src.ExecScript(`CREATE TABLE x(id INTEGER PRIMARY KEY,v BLOB);
WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<128)
INSERT INTO x SELECT x,zeroblob(1000) FROM n`))
	b, err := dst.NewBackup("main", src, "main")
	check(err)
	srcClose, dstClose := src.Close(), dst.Close()
	if !errors.Is(srcClose, sqlite.CodeBusy) || !errors.Is(dstClose, sqlite.CodeBusy) {
		panic("backup parent-close protection")
	}
	done, err := b.Step(1)
	check(err)
	remaining, total, err := b.Progress()
	check(err)
	if done || total < 3 || remaining != total-1 {
		panic("incremental backup progress")
	}
	check(src.ExecScript("UPDATE x SET v=x'010002' WHERE id=1; INSERT INTO x VALUES(129,x'ff00')"))
	steps := 1
	for !done {
		done, err = b.Step(4)
		check(err)
		steps++
	}
	if remaining, _, err := b.Progress(); err != nil || remaining != 0 {
		panic("backup completion progress")
	}
	check(b.Close())
	a, _, err := src.Prepare("SELECT id,v FROM x ORDER BY id", 0)
	check(err)
	z, _, err := dst.Prepare("SELECT id,v FROM x ORDER BY id", 0)
	check(err)
	for id := int64(1); id <= 129; id++ {
		for _, s := range []*sqlite.Stmt{a, z} {
			row, err := s.Step()
			check(err)
			if !row {
				panic("backup missing row")
			}
			got, ok, err := s.ColumnInt64(0)
			check(err)
			if !ok || got != id {
				panic("backup row identity")
			}
		}
		left, err := a.ColumnBlob(1)
		check(err)
		right, err := z.ColumnBlob(1)
		check(err)
		if !bytes.Equal(left, right) {
			panic("backup payload mismatch")
		}
	}
	for _, s := range []*sqlite.Stmt{a, z} {
		row, err := s.Step()
		check(err)
		if row {
			panic("backup extra row")
		}
		check(s.Close())
	}
	fmt.Printf("backup: pages=%d steps=%d; source changed between steps; exact rows=129; parent Close=BUSY/BUSY; finish restores destination\n", total, steps)

	check(src.ExecScript("CREATE TABLE not_copied(v); INSERT INTO x VALUES(130,x'01')"))
	b, err = dst.NewBackup("main", src, "main")
	check(err)
	done, err = b.Step(1)
	check(err)
	if done {
		panic("early backup unexpectedly complete")
	}
	check(b.Close())
	if scalar(dst, "SELECT count(*) FROM x") != 129 || scalar(dst, "SELECT count(*) FROM sqlite_schema WHERE name='not_copied'") != 0 {
		panic("early backup rollback")
	}
	check(dst.ExecScript("CREATE TABLE reused(v); INSERT INTO reused VALUES(42)"))
	if scalar(dst, "SELECT v FROM reused") != 42 {
		panic("destination reuse")
	}
	fmt.Println("backup early abort: prior 129 rows/schema restored; destination reused=42")

	backupAttachedSchema(src, dst)
	check(src.Close())
	check(dst.Close())
}
