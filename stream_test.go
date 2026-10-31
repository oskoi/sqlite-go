package sqlite

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"testing"
	"time"
	"weak"
)

type readerFunc func([]byte) (int, error)

func (f readerFunc) Read(p []byte) (int, error) { return f(p) }

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

type callerReader struct {
	io.Reader
	closed *bool
}

func (r callerReader) Close() error { *r.closed = true; return nil }

type callerWriter struct {
	io.Writer
	closed *bool
}

func (w callerWriter) Close() error { *w.closed = true; return nil }

type pairedReader struct {
	data []byte
	err  error
}

func (r *pairedReader) Read(p []byte) (int, error) {
	if len(r.data) == 0 {
		return 0, r.err
	}
	n := copy(p, r.data)
	r.data = r.data[n:]
	if len(r.data) == 0 {
		return n, r.err
	}
	return n, nil
}

type failureWriter struct {
	err    error
	accept int
	prefix []byte
}

func (w *failureWriter) Write(p []byte) (int, error) {
	n := min(w.accept, len(p))
	w.prefix = append(w.prefix, p[:n]...)
	return n, w.err
}
func streamInputFailures(sentinel error) []struct {
	name string
	err  error
} {
	return []struct {
		name string
		err  error
	}{
		{"sentinel", sentinel},
		{"joined-eof", errors.Join(io.EOF, sentinel)},
		{"wrapped-eof", fmt.Errorf("reader failed: %w", io.EOF)},
		{"wrapped-joined-eof", fmt.Errorf("reader failed: %w", errors.Join(io.EOF, sentinel))},
	}
}
func TestStreamFailures(t *testing.T) {
	schema := "CREATE TABLE t(id PRIMARY KEY,v)"
	data := recorded(t, schema, "", "INSERT INTO t VALUES(1,'recorded')")
	sentinel := errors.New("stream sentinel")
	t.Run("paired-eof", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, schema)
		must(t, c.ApplyChangeset(&pairedReader{data: data, err: io.EOF}, ApplyOptions{}))
		requireRows(t, c, "SELECT v FROM t", []string{"recorded"})
		it, err := NewChangesetIter(&pairedReader{data: data, err: io.EOF})
		must(t, err)
		row, err := it.Next()
		must(t, err)
		if !row {
			t.Fatal("no row")
		}
		row, err = it.Next()
		must(t, err)
		if row {
			t.Fatal("extra row")
		}
		must(t, it.Close())
		must(t, InvertChangeset(io.Discard, &pairedReader{data: data, err: io.EOF}))
		must(t, ConcatChangesets(io.Discard, &pairedReader{data: data, err: io.EOF}, bytes.NewReader(nil)))
		group, err := NewChangegroup()
		must(t, err)
		must(t, group.Add(&pairedReader{data: data, err: io.EOF}))
		var output bytes.Buffer
		_, err = group.WriteTo(&output)
		must(t, err)
		if !bytes.Equal(output.Bytes(), data) {
			t.Fatal("exact EOF lost group input")
		}
		must(t, group.Close())
	})
	t.Run("paired-error-apply-native-rollback", func(t *testing.T) {
		for _, tc := range streamInputFailures(sentinel) {
			t.Run(tc.name, func(t *testing.T) {
				c := openTest(t, ":memory:")
				script(t, c, schema+`; INSERT INTO t VALUES(900,'unchanged')`)
				err := c.ApplyChangeset(&pairedReader{data: data, err: tc.err}, ApplyOptions{})
				if !errors.Is(err, tc.err) || errors.Is(err, CodeIOErr) {
					t.Errorf("paired input error lost: got %v, want %v", err, tc.err)
				}
				requireRows(t, c, "SELECT id||':'||v FROM t", []string{"900:unchanged"})
			})
		}
		c := openTest(t, ":memory:")
		script(t, c, schema+`; INSERT INTO t VALUES(900,'unchanged')`)
		// A later strict input error must also roll back previously accepted chunks.
		large := recorded(t, schema, "", "INSERT INTO t SELECT value,printf('%0100d',value) FROM json_each('[1,2,3,4,5,6,7,8,9,10,11,12,13,14,15,16,17,18,19,20]')")
		cut := len(large) / 2
		first := true
		err := c.ApplyChangeset(readerFunc(func(p []byte) (int, error) {
			if first {
				first = false
				n := copy(p, large[:cut])
				large = large[n:]
				return n, nil
			}
			n := copy(p, large)
			large = large[n:]
			return n, sentinel
		}), ApplyOptions{})
		if !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
		requireRows(t, c, "SELECT id||':'||v FROM t", []string{"900:unchanged"})
	})
	t.Run("paired-error-iterator-row-close-completion", func(t *testing.T) {
		// Native table-header read-ahead can request more input before ROW for
		// a tiny payload. Multiple rows leave a complete first row buffered.
		pairedData := recorded(t, schema, "", "INSERT INTO t SELECT value,'recorded' FROM json_each('[1,2,3,4,5,6,7,8,9,10,11,12]')")
		for _, tc := range streamInputFailures(sentinel) {
			t.Run(tc.name, func(t *testing.T) {
				for _, closeEarly := range []bool{true, false} {
					it, err := NewChangesetIter(&pairedReader{data: pairedData, err: tc.err})
					must(t, err)
					row, err := it.Next()
					must(t, err)
					if !row {
						t.Fatal("paired bytes lost")
					}
					ch, err := it.Change()
					must(t, err)
					v, err := ch.New(1)
					must(t, err)
					text, _, err := v.Text()
					must(t, err)
					if text != "recorded" {
						t.Fatal(text)
					}
					if !closeEarly {
						for {
							row, err = it.Next()
							if err != nil || !row {
								break
							}
						}
						if row || !errors.Is(err, tc.err) {
							t.Fatalf("row=%v err=%v", row, err)
						}
						if _, err := it.Next(); !errors.Is(err, ErrState) {
							t.Fatal(err)
						}
						if _, err := it.Change(); !errors.Is(err, ErrState) {
							t.Fatal(err)
						}
					}
					if err := it.Close(); !errors.Is(err, tc.err) {
						t.Fatal(err)
					}
					must(t, it.Close())
					if _, err := v.Kind(); !errors.Is(err, ErrExpired) {
						t.Fatal(err)
					}
				}
			})
		}
	})
	t.Run("transforms-preserve-paired-error", func(t *testing.T) {
		for _, tc := range streamInputFailures(sentinel) {
			t.Run(tc.name, func(t *testing.T) {
				if err := InvertChangeset(io.Discard, &pairedReader{data: data, err: tc.err}); !errors.Is(err, tc.err) {
					t.Fatalf("inverse error=%v want %v", err, tc.err)
				}
				if err := ConcatChangesets(io.Discard, bytes.NewReader(data), &pairedReader{err: tc.err}); !errors.Is(err, tc.err) {
					t.Fatalf("concat error=%v want %v", err, tc.err)
				}
			})
		}
	})
	t.Run("no-progress-reset-invalid-counts", func(t *testing.T) {
		reads := 0
		r := readerFunc(func([]byte) (int, error) { reads++; return 0, nil })
		it, err := NewChangesetIter(r)
		must(t, err)
		_, err = it.Next()
		if !errors.Is(err, io.ErrNoProgress) || reads != 100 {
			t.Fatalf("reads=%d err=%v", reads, err)
		}
		if err := it.Close(); !errors.Is(err, io.ErrNoProgress) {
			t.Fatal(err)
		}
		reads, offset := 0, 0
		r = readerFunc(func(p []byte) (int, error) {
			reads++
			if reads%100 != 0 {
				return 0, nil
			}
			if offset == len(data) {
				return 0, io.EOF
			}
			p[0] = data[offset]
			offset++
			return 1, nil
		})
		it, err = NewChangesetIter(r)
		must(t, err)
		row, err := it.Next()
		must(t, err)
		if !row {
			t.Fatal("progress reset failed")
		}
		_, err = it.Next()
		must(t, err)
		must(t, it.Close())
		for _, invalid := range []int{-1, 1 << 30} {
			it, err := NewChangesetIter(readerFunc(func([]byte) (int, error) { return invalid, nil }))
			must(t, err)
			_, err = it.Next()
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatal(err)
			}
			if err := it.Close(); !errors.Is(err, ErrInvalidArgument) {
				t.Fatal(err)
			}
		}
	})
	t.Run("output-failures-prefix-and-counts", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, schema)
		s := sessionTest(t, c)
		script(t, c, "INSERT INTO t VALUES(1,'recorded')")
		for _, write := range []func(io.Writer) error{s.WriteChangeset, s.WritePatchset, func(w io.Writer) error { return InvertChangeset(w, bytes.NewReader(data)) }, func(w io.Writer) error { return ConcatChangesets(w, bytes.NewReader(data), bytes.NewReader(nil)) }} {
			w := &failureWriter{err: sentinel, accept: 3}
			if err := write(w); !errors.Is(err, sentinel) || len(w.prefix) != 3 {
				t.Fatalf("prefix=%x err=%v", w.prefix, err)
			}
			short := &failureWriter{accept: 1}
			if err := write(short); !errors.Is(err, io.ErrShortWrite) {
				t.Fatal(err)
			}
			for _, n := range []int{-1, 1 << 30} {
				if err := write(writerFunc(func([]byte) (int, error) { return n, nil })); !errors.Is(err, ErrInvalidArgument) {
					t.Fatal(err)
				}
			}
		}
		if err := s.WriteChangeset(nil); !errors.Is(err, ErrInvalidArgument) {
			t.Fatal(err)
		}
		if _, err := NewChangesetIter(nil); !errors.Is(err, ErrInvalidArgument) {
			t.Fatal(err)
		}
	})
}
func TestStreamAndFilterReentryPanic(t *testing.T) {
	if os.Getenv("SQLITE_GO_STREAM_CHILD") == "" {
		ctx, cancel := context.WithTimeout(context.Background(), 45*time.Second)
		defer cancel()
		cmd := exec.CommandContext(ctx, os.Args[0], "-test.run=^TestStreamAndFilterReentryPanic$", "-test.v")
		cmd.Env = append(os.Environ(), "SQLITE_GO_STREAM_CHILD=1")
		out, err := cmd.CombinedOutput()
		if err != nil {
			t.Fatalf("isolated stream lifetime checks: %v\n%s", err, out)
		}
		return
	}
	schema := "CREATE TABLE t(id PRIMARY KEY,v)"
	data := recorded(t, schema, "", "INSERT INTO t VALUES(1,'new')")
	value := &struct{ name string }{"stream panic"}
	t.Run("iterator-context-after-return", func(t *testing.T) {
		it, readRef := func() (*ChangesetIter, weak.Pointer[callbackCapture]) {
			capture := new(callbackCapture)
			r := bytes.NewReader(data)
			it, err := NewChangesetIter(readerFunc(func(p []byte) (int, error) {
				capture.calls++
				return r.Read(p[:min(len(p), 1)])
			}))
			must(t, err)
			return it, weak.Make(capture)
		}()
		defer it.Close()
		// Reuse native streaming frames after construction. The iterator's
		// retained input context must not live on one of those frames.
		must(t, InvertChangeset(io.Discard, bytes.NewReader(data)))
		must(t, ConcatChangesets(io.Discard, bytes.NewReader(data), bytes.NewReader(nil)))
		runtime.GC()
		if readRef.Value() == nil {
			t.Fatal("iterator released its Reader before Close")
		}
		row, err := it.Next()
		must(t, err)
		if !row {
			t.Fatal("no row after constructor returned")
		}
		ch, err := it.Change()
		must(t, err)
		info, err := ch.Info()
		must(t, err)
		if info.Table != "t" || info.Columns != 2 || info.Operation != OperationInsert {
			t.Fatal(info)
		}
		must(t, InvertChangeset(io.Discard, bytes.NewReader(data)))
		row, err = it.Next()
		must(t, err)
		if row {
			t.Fatal("extra row")
		}
		must(t, it.Close())
		requireCollected(t, readRef)
	})
	t.Run("synchronous-handles-released", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, schema)
		s := sessionTest(t, c)
		script(t, c, "INSERT INTO t VALUES(1,'new')")
		sentinel := errors.New("synchronous stream failure")
		for _, operation := range []string{"changeset", "patchset", "apply", "invert", "concat", "group-add", "group-output"} {
			outcomes := []string{"success", "error", "panic"}
			if operation == "invert" || operation == "concat" {
				outcomes = append(outcomes, "writer-error", "writer-panic")
			}
			for _, outcome := range outcomes {
				t.Run(operation+"/"+outcome, func(t *testing.T) {
					var refs []weak.Pointer[callbackCapture]
					var closed bool
					writes := 0
					run := func() error {
						input := func() io.Reader {
							capture := new(callbackCapture)
							refs = append(refs, weak.Make(capture))
							r := bytes.NewReader(data)
							return callerReader{Reader: readerFunc(func(p []byte) (int, error) {
								capture.calls++
								if outcome == "panic" {
									panic(value)
								}
								if outcome == "error" {
									return 0, sentinel
								}
								return r.Read(p)
							}), closed: &closed}
						}
						output := func() io.Writer {
							capture := new(callbackCapture)
							refs = append(refs, weak.Make(capture))
							return callerWriter{Writer: writerFunc(func(p []byte) (int, error) {
								writes++
								capture.calls++
								if outcome == "panic" || outcome == "writer-panic" {
									panic(value)
								}
								if outcome == "error" || outcome == "writer-error" {
									return 0, sentinel
								}
								return len(p), nil
							}), closed: &closed}
						}
						switch operation {
						case "changeset":
							return s.WriteChangeset(output())
						case "patchset":
							return s.WritePatchset(output())
						case "apply":
							capture := new(callbackCapture)
							refs = append(refs, weak.Make(capture))
							return c.ApplyChangeset(input(), ApplyOptions{
								Filter: func(string) bool { capture.calls++; return true },
								Conflict: func(Conflict) (ConflictAction, error) {
									capture.calls++
									return Omit, nil
								},
							})
						case "invert":
							return InvertChangeset(output(), input())
						case "concat":
							return ConcatChangesets(output(), input(), input())
						default:
							g, err := NewChangegroup()
							if err != nil {
								return err
							}
							defer g.Close()
							if operation == "group-add" {
								return g.Add(input())
							}
							if err := g.Add(bytes.NewReader(data)); err != nil {
								return err
							}
							_, err = g.WriteTo(output())
							return err
						}
					}
					if outcome == "panic" || outcome == "writer-panic" {
						requirePanic(t, value, func() { run() })
					} else if err := run(); outcome == "error" || outcome == "writer-error" {
						if !errors.Is(err, sentinel) {
							t.Fatal(err)
						}
					} else {
						must(t, err)
					}
					if (outcome == "writer-error" || outcome == "writer-panic") && writes == 0 {
						t.Fatal("Writer failure was not reached")
					}
					if closed {
						t.Fatal("operation closed caller-owned I/O")
					}
					requireCollected(t, refs...)
				})
			}
		}
	})
	t.Run("apply-reader-released-with-retained-value", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, schema+"; INSERT INTO t VALUES(1,'existing')")
		var retained Conflict
		var borrowed Value
		readRef := func() weak.Pointer[callbackCapture] {
			capture := new(callbackCapture)
			r := bytes.NewReader(data)
			must(t, c.ApplyChangeset(readerFunc(func(p []byte) (int, error) {
				capture.calls++
				return r.Read(p)
			}), ApplyOptions{Conflict: func(cf Conflict) (ConflictAction, error) {
				retained = cf
				var err error
				borrowed, err = cf.Current(1)
				must(t, err)
				return Omit, nil
			}}))
			return weak.Make(capture)
		}()
		if _, err := borrowed.Kind(); !errors.Is(err, ErrExpired) {
			t.Fatal(err)
		}
		requireCollected(t, readRef)
		runtime.KeepAlive(retained)
		runtime.KeepAlive(borrowed)
	})
	t.Run("invalid-later-argument-handles-released", func(t *testing.T) {
		for _, operation := range []string{"invert-nil-writer", "concat-nil-writer", "concat-nil-second-reader"} {
			t.Run(operation, func(t *testing.T) {
				var refs []weak.Pointer[callbackCapture]
				var closed bool
				reads, writes := 0, 0
				err := func() error {
					input := func() io.Reader {
						capture := new(callbackCapture)
						refs = append(refs, weak.Make(capture))
						r := bytes.NewReader(data)
						return callerReader{Reader: readerFunc(func(p []byte) (int, error) {
							capture.calls++
							reads++
							return r.Read(p)
						}), closed: &closed}
					}
					output := func() io.Writer {
						capture := new(callbackCapture)
						refs = append(refs, weak.Make(capture))
						return callerWriter{Writer: writerFunc(func(p []byte) (int, error) {
							capture.calls++
							writes++
							return len(p), nil
						}), closed: &closed}
					}
					switch operation {
					case "invert-nil-writer":
						return InvertChangeset(nil, input())
					case "concat-nil-writer":
						return ConcatChangesets(nil, input(), input())
					default:
						return ConcatChangesets(output(), input(), nil)
					}
				}()
				if !errors.Is(err, ErrInvalidArgument) {
					t.Fatal(err)
				}
				if reads != 0 || writes != 0 || closed {
					t.Fatalf("invalid arguments used caller I/O: reads=%d writes=%d closed=%v", reads, writes, closed)
				}
				requireCollected(t, refs...)
			})
		}
	})
	t.Run("iterator-reader", func(t *testing.T) {
		var it *ChangesetIter
		var readRef weak.Pointer[callbackCapture]
		read := func() io.Reader {
			capture := new(callbackCapture)
			readRef = weak.Make(capture)
			return readerFunc(func([]byte) (int, error) {
				capture.calls++
				if err := it.Close(); !errors.Is(err, ErrReentrant) {
					t.Error(err)
				}
				if _, err := it.Next(); !errors.Is(err, ErrReentrant) {
					t.Error(err)
				}
				panic(value)
			})
		}()
		var err error
		it, err = NewChangesetIter(read)
		must(t, err)
		read = nil
		requirePanic(t, value, func() { it.Next() })
		must(t, it.Close())
		if _, err := it.Next(); !errors.Is(err, ErrClosed) {
			t.Fatal(err)
		}
		requireCollected(t, readRef)
	})
	t.Run("session-writer", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, schema)
		s := sessionTest(t, c)
		script(t, c, "INSERT INTO t VALUES(1,'new')")
		requirePanic(t, value, func() {
			s.WriteChangeset(writerFunc(func([]byte) (int, error) {
				if err := s.Close(); !errors.Is(err, ErrReentrant) {
					t.Error(err)
				}
				if err := c.Close(); !errors.Is(err, ErrReentrant) {
					t.Error(err)
				}
				panic(value)
			}))
		})
		if !bytes.Equal(sessionBytes(t, s, false), data) {
			t.Fatal("writer panic changed session")
		}
	})
	t.Run("all-session-filters-released", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, schema)
		a, b := sessionTest(t, c), sessionTest(t, c)
		refs := func() []weak.Pointer[callbackCapture] {
			first, second, hook := new(callbackCapture), new(callbackCapture), new(callbackCapture)
			must(t, a.SetTableFilter(func(string) bool {
				first.calls++
				if err := a.Close(); !errors.Is(err, ErrReentrant) {
					t.Error(err)
				}
				panic(value)
			}))
			must(t, b.SetTableFilter(func(string) bool {
				second.calls++
				if err := b.AttachAll(); !errors.Is(err, ErrReentrant) {
					t.Error(err)
				}
				return true
			}))
			must(t, c.SetUpdateHook(func(UpdateEvent) { hook.calls++ }))
			return []weak.Pointer[callbackCapture]{weak.Make(first), weak.Make(second), weak.Make(hook)}
		}()
		requirePanic(t, value, func() { c.ExecScript("INSERT INTO t VALUES(1,'one'),(2,'two')") })
		for _, ref := range refs {
			requireCollected(t, ref)
		}
		calls := 0
		must(t, a.SetTableFilter(func(string) bool { calls++; return true }))
		script(t, c, "CREATE TABLE other(id PRIMARY KEY); INSERT INTO other VALUES(1)")
		if calls != 1 {
			t.Fatal(calls)
		}
		must(t, a.Close())
		must(t, b.Close())
	})
	t.Run("apply-reader-filter-conflict", func(t *testing.T) {
		for _, mode := range []string{"reader", "filter", "conflict"} {
			t.Run(mode, func(t *testing.T) {
				c := openTest(t, ":memory:")
				script(t, c, schema)
				if mode == "conflict" {
					script(t, c, "INSERT INTO t VALUES(1,'old')")
				}
				must(t, c.Begin())
				var retained Conflict
				var retainedChange Change
				var retainedValue Value
				calls := 0
				reenter := func() {
					calls++
					if err := c.Close(); !errors.Is(err, ErrReentrant) {
						t.Error(err)
					}
					if err := c.ExecScript("SELECT 1"); !errors.Is(err, ErrReentrant) {
						t.Error(err)
					}
				}
				r := io.Reader(bytes.NewReader(data))
				options := ApplyOptions{}
				switch mode {
				case "reader":
					r = readerFunc(func([]byte) (int, error) { reenter(); panic(value) })
				case "filter":
					options.Filter = func(string) bool { reenter(); panic(value) }
				case "conflict":
					options.Conflict = func(cf Conflict) (ConflictAction, error) {
						reenter()
						retained = cf
						var err error
						retainedChange, err = cf.Change()
						must(t, err)
						retainedValue, err = cf.Current(1)
						must(t, err)
						panic(value)
					}
				}
				requirePanic(t, value, func() { c.ApplyChangeset(r, options) })
				if calls != 1 {
					t.Fatal(calls)
				}
				must(t, c.Rollback())
				want := int64(0)
				if mode == "conflict" {
					want = 1
				}
				if scalarTest(t, c, "SELECT count(*) FROM t") != want {
					t.Fatal("caller rollback failed")
				}
				if _, err := retained.Type(); !errors.Is(err, ErrExpired) {
					t.Fatal(err)
				}
				if _, err := retainedChange.Info(); !errors.Is(err, ErrExpired) {
					t.Fatal(err)
				}
				if _, err := retainedValue.Kind(); !errors.Is(err, ErrExpired) {
					t.Fatal(err)
				}
				must(t, c.ApplyChangeset(bytes.NewReader(data), ApplyOptions{Conflict: func(Conflict) (ConflictAction, error) { return Replace, nil }}))
				requireRows(t, c, "SELECT v FROM t", []string{"new"})
			})
		}
	})
	t.Run("group-add-output", func(t *testing.T) {
		g, err := NewChangegroup()
		must(t, err)
		requirePanic(t, value, func() {
			g.Add(readerFunc(func([]byte) (int, error) {
				if err := g.Close(); !errors.Is(err, ErrReentrant) {
					t.Error(err)
				}
				if err := g.Add(bytes.NewReader(nil)); !errors.Is(err, ErrReentrant) {
					t.Error(err)
				}
				panic(value)
			}))
		})
		if _, err := g.WriteTo(io.Discard); !errors.Is(err, ErrState) {
			t.Fatal(err)
		}
		must(t, g.Close())
		g, err = NewChangegroup()
		must(t, err)
		must(t, g.Add(bytes.NewReader(data)))
		requirePanic(t, value, func() {
			g.WriteTo(writerFunc(func([]byte) (int, error) {
				if err := g.Close(); !errors.Is(err, ErrReentrant) {
					t.Error(err)
				}
				if _, err := g.WriteTo(io.Discard); !errors.Is(err, ErrReentrant) {
					t.Error(err)
				}
				panic(value)
			}))
		})
		var out bytes.Buffer
		_, err = g.WriteTo(&out)
		must(t, err)
		if !bytes.Equal(out.Bytes(), data) {
			t.Fatal("output panic changed group")
		}
		must(t, g.Close())
	})
	t.Run("transforms", func(t *testing.T) {
		requirePanic(t, value, func() { InvertChangeset(io.Discard, readerFunc(func([]byte) (int, error) { panic(value) })) })
		requirePanic(t, value, func() {
			ConcatChangesets(writerFunc(func([]byte) (int, error) { panic(value) }), bytes.NewReader(data), bytes.NewReader(nil))
		})
	})
}

func TestSessionNativeLifetime(t *testing.T) {
	for _, mode := range []string{"session-stream-lifecycle", "session-stream-registration", "changeset-value-text", "changeset-value-blob"} {
		t.Run(mode, func(t *testing.T) {
			cmd := exec.Command("go", "run", "./internal/cmd/faultprobe", mode)
			out, err := cmd.CombinedOutput()
			if err != nil {
				t.Fatalf("native lifetime probe: %v\n%s", err, out)
			}
			t.Log(string(out))
		})
	}
}

type closeReader struct {
	*bytes.Reader
	closes int
}

func (r *closeReader) Close() error { r.closes++; return nil }

type closeWriter struct {
	bytes.Buffer
	closes int
}

func (w *closeWriter) Close() error { w.closes++; return nil }

func TestStreamOwnershipAndNativeErrors(t *testing.T) {
	data := recorded(t, "CREATE TABLE t(id PRIMARY KEY,v)", "", "INSERT INTO t VALUES(1,'new')")
	t.Run("caller-io-and-preentry-reentry", func(t *testing.T) {
		r := &closeReader{Reader: bytes.NewReader(data)}
		w := &closeWriter{}
		must(t, InvertChangeset(w, r))
		if r.closes != 0 || w.closes != 0 {
			t.Fatal("library closed caller I/O")
		}
		g, err := NewChangegroup()
		must(t, err)
		r.Reader.Reset(data)
		must(t, g.Add(readerFunc(func(p []byte) (int, error) {
			if err := g.Close(); !errors.Is(err, ErrReentrant) {
				t.Error(err)
			}
			if err := g.Add(nil); !errors.Is(err, ErrReentrant) {
				t.Error(err)
			}
			return r.Read(p)
		})))
		w.Reset()
		n, err := g.WriteTo(w)
		must(t, err)
		if n != int64(len(data)) || !bytes.Equal(w.Bytes(), data) || r.closes != 0 || w.closes != 0 {
			t.Fatal("preentry rejection poisoned group or closed caller I/O")
		}
		must(t, g.Close())
	})
	t.Run("distinct-native-error-joined", func(t *testing.T) {
		sentinel := errors.New("paired corrupt input")
		err := InvertChangeset(io.Discard, &pairedReader{data: []byte("invalid changeset"), err: sentinel})
		if !errors.Is(err, sentinel) || !errors.Is(err, CodeCorrupt) {
			t.Fatalf("missing Go/native error: %v", err)
		}
	})
	t.Run("ordinary-error-keeps-filter", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE t(id PRIMARY KEY,v)")
		s := sessionTest(t, c)
		calls := 0
		must(t, s.SetTableFilter(func(string) bool { calls++; return true }))
		script(t, c, "INSERT INTO t VALUES(1,'new')")
		sentinel := errors.New("writer error")
		if err := s.WriteChangeset(&failureWriter{err: sentinel}); !errors.Is(err, sentinel) {
			t.Fatal(err)
		}
		script(t, c, "CREATE TABLE other(id PRIMARY KEY); INSERT INTO other VALUES(1)")
		if calls != 2 {
			t.Fatalf("ordinary error removed filter: calls=%d", calls)
		}
	})
}
