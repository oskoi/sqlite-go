package sqlite

import (
	"fmt"
	"io"
	"runtime"
	"testing"
)

func BenchmarkBlob(b *testing.B) {
	for _, size := range []int{16, 4096} {
		for _, mode := range []string{"ReadAt", "WriteAt", "ReadSeek", "WriteSeek"} {
			b.Run(fmt.Sprintf("%s/bytes%d", mode, size), func(b *testing.B) {
				c := openTest(b, ":memory:")
				script(b, c, fmt.Sprintf("CREATE TABLE x(a); INSERT INTO x VALUES(zeroblob(%d))", size))
				blob := openBlobTest(b, c, 1, true)
				buf := make([]byte, size)
				for i := range buf {
					buf[i] = byte(i)
				}
				if n, err := blob.WriteAt(buf, 0); err != nil || n != size {
					b.Fatal(n, err)
				}
				if n, err := blob.ReadAt(buf, 0); err != nil || n != size {
					b.Fatal(n, err)
				}
				b.ReportAllocs()
				b.SetBytes(int64(size))
				before := runtime.NumCgoCall()
				b.ResetTimer()
				for range b.N {
					var n int
					var err error
					switch mode {
					case "ReadAt":
						n, err = blob.ReadAt(buf, 0)
					case "WriteAt":
						n, err = blob.WriteAt(buf, 0)
					case "ReadSeek":
						if _, e := blob.Seek(0, io.SeekStart); e != nil {
							b.Fatal(e)
						}
						n, err = blob.Read(buf)
					case "WriteSeek":
						if _, e := blob.Seek(0, io.SeekStart); e != nil {
							b.Fatal(e)
						}
						n, err = blob.Write(buf)
					}
					if n != size || err != nil {
						b.Fatal(n, err)
					}
				}
				b.StopTimer()
				b.ReportMetric(float64(runtime.NumCgoCall()-before)/float64(b.N), "cgo/op")
			})
		}
	}
}
