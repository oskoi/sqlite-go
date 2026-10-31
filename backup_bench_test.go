package sqlite

import (
	"fmt"
	"testing"
)

func BenchmarkBackup(b *testing.B) {
	fixture := func(b *testing.B) (*Conn, *Conn, int64) {
		src, dst := openTest(b, ":memory:"), openTest(b, ":memory:")
		script(b, src, `PRAGMA page_size=4096; CREATE TABLE x(id INTEGER PRIMARY KEY,v BLOB);
WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<256)
INSERT INTO x SELECT x,zeroblob(1024) FROM n`)
		pages := scalarTest(b, src, "PRAGMA page_count")
		warm := newBackupTest(b, dst, src)
		backupDoneTest(b, warm, -1)
		if err := warm.Close(); err != nil {
			b.Fatal(err)
		}
		return src, dst, pages
	}
	b.Run("ConstructFinish/rows256/payload1024/page4096", func(b *testing.B) {
		src, dst, pages := fixture(b)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			backup, err := dst.NewBackup("main", src, "main")
			if err != nil {
				b.Fatal(err)
			}
			if err := backup.Close(); err != nil {
				b.Fatal(err)
			}
		}
		b.ReportMetric(float64(pages), "db-pages")
	})
	for _, perStep := range []int{1, 16, -1} {
		b.Run(fmt.Sprintf("StepProgress/rows256/payload1024/page4096/pages%d", perStep), func(b *testing.B) {
			src, dst, pages := fixture(b)
			b.ReportAllocs()
			b.SetBytes(pages * 4096)
			steps := 0
			b.ResetTimer()
			for range b.N {
				b.StopTimer()
				backup, err := dst.NewBackup("main", src, "main")
				if err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
				for {
					done, err := backup.Step(perStep)
					if err != nil {
						b.Fatal(err)
					}
					remaining, total, err := backup.Progress()
					if err != nil || total != int(pages) || remaining < 0 || done && remaining != 0 {
						b.Fatal(remaining, total, err)
					}
					steps++
					if done {
						break
					}
				}
				b.StopTimer()
				if err := backup.Close(); err != nil {
					b.Fatal(err)
				}
				b.StartTimer()
			}
			b.ReportMetric(float64(pages), "db-pages")
			b.ReportMetric(float64(steps)/float64(b.N), "steps/op")
		})
	}
	b.Run("ProgressDone/rows256/payload1024/page4096", func(b *testing.B) {
		src, dst, pages := fixture(b)
		backup := newBackupTest(b, dst, src)
		backupDoneTest(b, backup, -1)
		b.ReportAllocs()
		b.ResetTimer()
		for range b.N {
			remaining, total, err := backup.Progress()
			if err != nil || remaining != 0 || total != int(pages) {
				b.Fatal(remaining, total, err)
			}
		}
		b.ReportMetric(float64(pages), "db-pages")
	})
}
