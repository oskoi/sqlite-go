package sqlite

import (
	"bytes"
	"io"
	"strings"
	"testing"
)

// BenchmarkStream holds the native fixture and consumer capacity fixed outside
// timing. Reused consumers do not allocate per chunk; discard isolates output.
func BenchmarkStream(b *testing.B) {
	for _, size := range []struct {
		name  string
		bytes int
	}{{"small", 64}, {"large", 1 << 20}} {
		b.Run(size.name, func(b *testing.B) {
			c := openTest(b, ":memory:")
			script(b, c, "CREATE TABLE t(id PRIMARY KEY,v)")
			s := sessionTest(b, c)
			insert := prepareTest(b, c, "INSERT INTO t VALUES(1,?)")
			must(b, insert.BindText(1, strings.Repeat("x", size.bytes)))
			_, err := insert.Exec()
			must(b, err)
			must(b, insert.Close())
			data := sessionBytes(b, s, false)
			g, err := NewChangegroup()
			must(b, err)
			must(b, g.Add(bytes.NewReader(data)))
			b.Cleanup(func() { must(b, g.Close()) })
			for _, consumer := range []string{"discard", "reuse"} {
				b.Run("session/"+consumer, func(b *testing.B) {
					var out bytes.Buffer
					out.Grow(len(data) + 1024)
					var writer io.Writer = io.Discard
					if consumer == "reuse" {
						writer = &out
					}
					must(b, s.WriteChangeset(writer))
					b.ReportAllocs()
					b.SetBytes(int64(len(data)))
					b.ResetTimer()
					for b.Loop() {
						out.Reset()
						must(b, s.WriteChangeset(writer))
					}
				})
				b.Run("inverse/"+consumer, func(b *testing.B) {
					var out bytes.Buffer
					out.Grow(len(data) + 1024)
					var writer io.Writer = io.Discard
					if consumer == "reuse" {
						writer = &out
					}
					var input bytes.Reader
					input.Reset(data)
					must(b, InvertChangeset(writer, &input))
					b.ReportAllocs()
					b.SetBytes(int64(len(data)))
					b.ResetTimer()
					for b.Loop() {
						out.Reset()
						input.Reset(data)
						must(b, InvertChangeset(writer, &input))
					}
				})
				b.Run("group/"+consumer, func(b *testing.B) {
					var out bytes.Buffer
					out.Grow(len(data) + 1024)
					var writer io.Writer = io.Discard
					if consumer == "reuse" {
						writer = &out
					}
					_, err := g.WriteTo(writer)
					must(b, err)
					b.ReportAllocs()
					b.SetBytes(int64(len(data)))
					b.ResetTimer()
					for b.Loop() {
						out.Reset()
						_, err := g.WriteTo(writer)
						must(b, err)
					}
				})
			}
		})
	}
}
