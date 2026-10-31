package sqlite

import (
	"fmt"
	"runtime"
	"strings"
	"testing"
)

func BenchmarkTypedNumeric(b *testing.B) {
	c := openTest(b, ":memory:")
	s := prepareTest(b, c, "SELECT ?1,?2")
	s.BindInt64(1, 42)
	s.BindFloat64(2, 4.2)
	rowTest(b, s)
	s.ColumnInt64(0)
	s.ColumnFloat64(1)
	s.Reset()
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if err := s.BindInt64(1, int64(i)); err != nil {
			b.Fatal(err)
		}
		if err := s.BindFloat64(2, 4.2); err != nil {
			b.Fatal(err)
		}
		if row, err := s.Step(); err != nil || !row {
			b.Fatal(row, err)
		}
		if _, _, err := s.ColumnInt64(0); err != nil {
			b.Fatal(err)
		}
		if _, _, err := s.ColumnFloat64(1); err != nil {
			b.Fatal(err)
		}
		if err := s.Reset(); err != nil {
			b.Fatal(err)
		}
	}
}
func BenchmarkPreparedExec(b *testing.B) {
	c := openTest(b, ":memory:")
	script(b, c, "CREATE TABLE t(v)")
	s := prepareTest(b, c, "INSERT INTO t VALUES(?)")
	s.BindInt64(1, 42)
	s.Exec()
	script(b, c, "BEGIN")
	b.ReportAllocs()
	b.ResetTimer()
	for i := range b.N {
		if err := s.BindInt64(1, int64(i)); err != nil {
			b.Fatal(err)
		}
		if _, err := s.Exec(); err != nil {
			b.Fatal(err)
		}
	}
	b.StopTimer()
	if err := c.Rollback(); err != nil {
		b.Fatal(err)
	}
}
func BenchmarkColumnAccess(b *testing.B) {
	for _, width := range []int{1, 300} {
		for _, size := range []int{0, 16, 4096} {
			for _, view := range []bool{false, true} {
				for _, blob := range []bool{false, true} {
					name := fmt.Sprintf("width%d/bytes%d/view%v/blob%v", width, size, view, blob)
					b.Run(name, func(b *testing.B) {
						c := openTest(b, ":memory:")
						cols := make([]string, width)
						for i := range cols {
							cols[i] = "NULL"
						}
						cols[width-1] = "?"
						s := prepareTest(b, c, "SELECT "+strings.Join(cols, ","))
						if size == 0 {
							s.BindNull(1)
						} else if blob {
							s.BindBlob(1, []byte(strings.Repeat("x", size)))
						} else {
							s.BindText(1, strings.Repeat("x", size))
						}
						rowTest(b, s)
						// Warm native conversion before counting steady-state Go allocations.
						if blob {
							s.ColumnBlobView(width - 1)
						} else {
							s.ColumnTextView(width - 1)
						}
						b.ReportAllocs()
						b.SetBytes(int64(size))
						b.ResetTimer()
						for range b.N {
							if blob {
								var out []byte
								var err error
								if view {
									out, err = s.ColumnBlobView(width - 1)
								} else {
									out, err = s.ColumnBlob(width - 1)
								}
								if err != nil {
									b.Fatal(err)
								}
								runtime.KeepAlive(out)
							} else {
								var out string
								var err error
								if view {
									out, _, err = s.ColumnTextView(width - 1)
								} else {
									out, _, err = s.ColumnText(width - 1)
								}
								if err != nil {
									b.Fatal(err)
								}
								runtime.KeepAlive(out)
							}
						}
					})
				}
			}
		}
	}
	// Full scans and sparse scans must be distinguishable from getter-only costs.
	for _, width := range []int{1, 300} {
		for _, sparse := range []bool{false, true} {
			b.Run(fmt.Sprintf("scan/width%d/sparse%v", width, sparse), func(b *testing.B) {
				c := openTest(b, ":memory:")
				cols := make([]string, width)
				for i := range cols {
					if i%2 == 0 {
						cols[i] = "123"
					} else {
						cols[i] = "NULL"
					}
				}
				s := prepareTest(b, c, "SELECT "+strings.Join(cols, ","))
				rowTest(b, s)
				s.Reset()
				b.ReportAllocs()
				b.ResetTimer()
				for range b.N {
					if row, err := s.Step(); err != nil || !row {
						b.Fatal(row, err)
					}
					if sparse {
						if _, _, err := s.ColumnInt64(width - 1); err != nil {
							b.Fatal(err)
						}
					} else {
						for i := range width {
							if _, _, err := s.ColumnInt64(i); err != nil {
								b.Fatal(err)
							}
						}
					}
					if err := s.Reset(); err != nil {
						b.Fatal(err)
					}
				}
			})
		}
	}
}
func BenchmarkParameterLookup(b *testing.B) {
	for _, length := range []int{2, 255, 256, 301} {
		b.Run(fmt.Sprintf("bytes%d", length), func(b *testing.B) {
			c := openTest(b, ":memory:")
			name := ":" + strings.Repeat("x", length-1)
			s := prepareTest(b, c, "SELECT "+name)
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if i, err := s.ParameterIndex(name); err != nil || i != 1 {
					b.Fatal(i, err)
				}
			}
		})
	}
}

func BenchmarkCallbacks(b *testing.B) {
	for _, hook := range []string{"none", "commit", "update"} {
		b.Run("exec/"+hook, func(b *testing.B) {
			c := openTest(b, ":memory:")
			script(b, c, "CREATE TABLE t(v)")
			if hook == "commit" {
				if err := c.SetCommitHook(func() bool { return false }); err != nil {
					b.Fatal(err)
				}
			} else if hook == "update" {
				if err := c.SetUpdateHook(func(UpdateEvent) {}); err != nil {
					b.Fatal(err)
				}
			}
			s := prepareTest(b, c, "INSERT INTO t VALUES(?)")
			if err := s.BindInt64(1, 42); err != nil {
				b.Fatal(err)
			}
			if _, err := s.Exec(); err != nil {
				b.Fatal(err)
			}
			before := runtime.NumCgoCall()
			b.ReportAllocs()
			b.ResetTimer()
			for i := range b.N {
				if err := s.BindInt64(1, int64(i)); err != nil {
					b.Fatal(err)
				}
				if _, err := s.Exec(); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(runtime.NumCgoCall()-before)/float64(b.N), "cgo/op")
		})
	}
	for _, enabled := range []bool{false, true} {
		b.Run(fmt.Sprintf("numeric/hook%v", enabled), func(b *testing.B) {
			c := openTest(b, ":memory:")
			if enabled {
				if err := c.SetUpdateHook(func(UpdateEvent) {}); err != nil {
					b.Fatal(err)
				}
			}
			s := prepareTest(b, c, "SELECT 42, 'text', x'0102'")
			rowTest(b, s)
			s.ColumnInt64(0)
			s.ColumnTextView(1)
			s.ColumnBlobView(2)
			before := runtime.NumCgoCall()
			b.ReportAllocs()
			b.ResetTimer()
			for range b.N {
				if _, _, err := s.ColumnInt64(0); err != nil {
					b.Fatal(err)
				}
				if _, _, err := s.ColumnTextView(1); err != nil {
					b.Fatal(err)
				}
				if _, err := s.ColumnBlobView(2); err != nil {
					b.Fatal(err)
				}
			}
			b.StopTimer()
			b.ReportMetric(float64(runtime.NumCgoCall()-before)/float64(b.N), "cgo/op")
		})
	}
}
