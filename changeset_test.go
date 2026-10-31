package sqlite

import (
	"bytes"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func recorded(t testing.TB, schema, before, change string) []byte {
	t.Helper()
	c := openTest(t, ":memory:")
	script(t, c, schema+";"+before)
	s := sessionTest(t, c)
	script(t, c, change)
	return sessionBytes(t, s, false)
}
func valueKind(t testing.TB, v Value, want Kind) {
	t.Helper()
	got, err := v.Kind()
	must(t, err)
	if got != want {
		t.Fatalf("kind=%v want=%v", got, want)
	}
}
func TestChangesetValuesAndConflicts(t *testing.T) {
	t.Run("values-expiry-cache", func(t *testing.T) {
		data := recorded(t, `CREATE TABLE t(id PRIMARY KEY,a,b,c,d,e,f)`, `INSERT INTO t VALUES(1,'before',2,3,4,5,6),(3,1,2,3,4,5,6)`, `UPDATE t SET a='after' WHERE id=1; DELETE FROM t WHERE id=3; INSERT INTO t VALUES(2,NULL,42,2.5,'a'||char(0)||'b',x'3432',x'')`)
		it, err := NewChangesetIter(bytes.NewReader(data))
		must(t, err)
		if _, err := it.Change(); !errors.Is(err, ErrState) {
			t.Fatal(err)
		}
		var previous Change
		var oldValue Value
		owned := []ValueCopy{}
		ops := map[Operation]int{}
		for {
			row, err := it.Next()
			must(t, err)
			if _, err := previous.Info(); !errors.Is(err, ErrExpired) {
				t.Fatal(err)
			}
			if _, err := oldValue.Kind(); !errors.Is(err, ErrExpired) {
				t.Fatal(err)
			}
			if got, err := oldValue.Copy(); !errors.Is(err, ErrExpired) || !reflect.DeepEqual(got, ValueCopy{}) {
				t.Fatalf("expired copy: value=%#v err=%v", got, err)
			}
			if !row {
				break
			}
			ch, err := it.Change()
			must(t, err)
			previous = ch
			info, err := ch.Info()
			must(t, err)
			ops[info.Operation]++
			pk, err := ch.PrimaryKey()
			must(t, err)
			if !reflect.DeepEqual(pk, []bool{true, false, false, false, false, false, false}) {
				t.Fatal(pk)
			}
			if _, err := ch.New(-1); info.Operation != OperationDelete && !errors.Is(err, ErrRange) {
				t.Fatal(err)
			}
			switch info.Operation {
			case OperationInsert:
				if _, err := ch.Old(0); !errors.Is(err, ErrState) {
					t.Fatal(err)
				}
				wants := []ValueCopy{{Kind: KindInteger, Int64: 2}, {Kind: KindNull}, {Kind: KindInteger, Int64: 42}, {Kind: KindFloat, Float64: 2.5}, {Kind: KindText, Text: "a\x00b"}, {Kind: KindBlob, Blob: []byte("42")}, {Kind: KindBlob, Blob: []byte{}}}
				for i, want := range wants {
					v, err := ch.New(i)
					must(t, err)
					got, err := v.Copy()
					must(t, err)
					if !reflect.DeepEqual(got, want) {
						t.Fatalf("column=%d copy=%#v want=%#v", i, got, want)
					}
					owned = append(owned, got)
					oldValue = v
				}
				null, err := ch.New(1)
				must(t, err)
				n, ok, err := null.Int64()
				must(t, err)
				if n != 0 || ok {
					t.Fatal("NULL integer")
				}
				f, ok, err := null.Float64()
				must(t, err)
				if f != 0 || ok {
					t.Fatal("NULL float")
				}
				text, ok, err := null.Text()
				must(t, err)
				if text != "" || ok {
					t.Fatal("NULL text")
				}
				blob, err := null.Blob()
				must(t, err)
				if blob != nil {
					t.Fatal("NULL blob")
				}
				v, err := ch.New(5)
				must(t, err)
				copyV := v
				text, ok, err = copyV.Text()
				must(t, err)
				if text != "42" || !ok {
					t.Fatal(text)
				}
				again, err := ch.New(5)
				must(t, err)
				valueKind(t, again, KindBlob)
				valueKind(t, v, KindBlob)
				copiedBlob, err := again.Copy()
				must(t, err)
				if !reflect.DeepEqual(copiedBlob, ValueCopy{Kind: KindBlob, Blob: []byte("42")}) {
					t.Fatal(copiedBlob)
				}
				number, err := ch.New(2)
				must(t, err)
				_, _, err = number.Text()
				must(t, err)
				refetched, err := ch.New(2)
				must(t, err)
				valueKind(t, refetched, KindInteger)
				copied, err := refetched.Copy()
				must(t, err)
				if !reflect.DeepEqual(copied, ValueCopy{Kind: KindInteger, Int64: 42}) {
					t.Fatal(copied)
				}
			case OperationUpdate:
				missing, err := ch.New(2)
				must(t, err)
				cp, err := missing.Copy()
				must(t, err)
				if !reflect.DeepEqual(cp, ValueCopy{Kind: KindMissing}) {
					t.Fatal(cp)
				}
				valueKind(t, missing, KindMissing)
				if _, _, err := missing.Int64(); !errors.Is(err, ErrState) {
					t.Fatal(err)
				}
				if _, _, err := missing.Float64(); !errors.Is(err, ErrState) {
					t.Fatal(err)
				}
				if _, _, err := missing.Text(); !errors.Is(err, ErrState) {
					t.Fatal(err)
				}
				if _, err := missing.Blob(); !errors.Is(err, ErrState) {
					t.Fatal(err)
				}
				cp, err = missing.Copy()
				must(t, err)
				if !reflect.DeepEqual(cp, ValueCopy{Kind: KindMissing}) {
					t.Fatal(cp)
				}
				oldValue = missing
				old, err := ch.Old(1)
				must(t, err)
				text, _, err := old.Text()
				must(t, err)
				if text != "before" {
					t.Fatal(text)
				}
			case OperationDelete:
				if _, err := ch.New(0); !errors.Is(err, ErrState) {
					t.Fatal(err)
				}
				oldValue, err = ch.Old(0)
				must(t, err)
			}
		}
		if len(ops) != 3 {
			t.Fatal(ops)
		}
		must(t, it.Close())
		must(t, it.Close())
		if owned[4].Text != "a\x00b" || string(owned[5].Blob) != "42" || owned[6].Blob == nil {
			t.Fatal(owned)
		}
		// Close also expires an otherwise valid current row.
		it, err = NewChangesetIter(bytes.NewReader(data))
		must(t, err)
		row, err := it.Next()
		must(t, err)
		if !row {
			t.Fatal("no row")
		}
		ch, err := it.Change()
		must(t, err)
		must(t, it.Close())
		if _, err := ch.PrimaryKey(); !errors.Is(err, ErrExpired) {
			t.Fatal(err)
		}
	})
	t.Run("wide-primary-key", func(t *testing.T) {
		cols, values := make([]string, 133), make([]string, 133)
		for i := range cols {
			cols[i] = fmt.Sprintf("c%d", i)
			values[i] = fmt.Sprint(i)
		}
		schema := "CREATE TABLE wide(" + strings.Join(cols, ",") + ",PRIMARY KEY(c0,c132))"
		data := recorded(t, schema, "", "INSERT INTO wide VALUES("+strings.Join(values, ",")+")")
		eachChange(t, data, func(ch Change) {
			pk, err := ch.PrimaryKey()
			must(t, err)
			if len(pk) != 133 || !pk[0] || !pk[132] {
				t.Fatal(pk)
			}
			for i := 1; i < 132; i++ {
				if pk[i] {
					t.Fatal(i)
				}
			}
			pk[0] = false
			again, err := ch.PrimaryKey()
			must(t, err)
			if !again[0] {
				t.Fatal("borrowed mask")
			}
		})
	})
	t.Run("native-conflicts", func(t *testing.T) {
		schema := "CREATE TABLE t(id PRIMARY KEY,v UNIQUE)"
		cases := []struct {
			name, before, change, target string
			typ                          ConflictType
			action                       ConflictAction
			want                         []string
		}{
			{"data", `INSERT INTO t VALUES(1,'old')`, `UPDATE t SET v='new' WHERE id=1`, `INSERT INTO t VALUES(1,'other')`, ConflictData, Replace, []string{"1:new"}},
			{"notfound", `INSERT INTO t VALUES(1,'old')`, `DELETE FROM t WHERE id=1`, "", ConflictNotFound, Omit, nil},
			{"conflict", "", `INSERT INTO t VALUES(1,'new')`, `INSERT INTO t VALUES(1,'other')`, ConflictConflict, Replace, []string{"1:new"}},
			{"constraint", "", `INSERT INTO t VALUES(2,'same')`, `INSERT INTO t VALUES(1,'same')`, ConflictConstraint, Omit, []string{"1:same"}},
		}
		for _, tc := range cases {
			t.Run(tc.name, func(t *testing.T) {
				data := recorded(t, schema, tc.before, tc.change)
				c := openTest(t, ":memory:")
				script(t, c, schema+";"+tc.target)
				var retained Conflict
				var value Value
				calls := 0
				must(t, c.ApplyChangeset(bytes.NewReader(data), ApplyOptions{Conflict: func(cf Conflict) (ConflictAction, error) {
					retained = cf
					calls++
					typ, err := cf.Type()
					must(t, err)
					if typ != tc.typ {
						t.Fatal(typ)
					}
					ch, err := cf.Change()
					must(t, err)
					info, err := ch.Info()
					must(t, err)
					if info.Table != "t" {
						t.Fatal(info)
					}
					if _, err := cf.ForeignKeyConflicts(); !errors.Is(err, ErrState) {
						t.Fatal(err)
					}
					if typ == ConflictData || typ == ConflictConflict {
						value, err = cf.Current(1)
						must(t, err)
						text, _, err := value.Text()
						must(t, err)
						if text != "other" {
							t.Fatal(text)
						}
					} else if _, err := cf.Current(0); !errors.Is(err, ErrState) {
						t.Fatal(err)
					}
					if err := c.Close(); !errors.Is(err, ErrReentrant) {
						t.Fatal(err)
					}
					if _, err := c.Changes(); !errors.Is(err, ErrReentrant) {
						t.Fatal(err)
					}
					return tc.action, nil
				}}))
				if calls != 1 {
					t.Fatal(calls)
				}
				if _, err := retained.Type(); !errors.Is(err, ErrExpired) {
					t.Fatal(err)
				}
				if _, err := value.Kind(); !errors.Is(err, ErrExpired) {
					t.Fatal(err)
				}
				requireRows(t, c, "SELECT id||':'||v FROM t ORDER BY id", tc.want)
			})
		}
		data := recorded(t, schema, "", `INSERT INTO t VALUES(1,'new')`)
		for _, mode := range []string{"nil", "error", "invalid", "abort"} {
			t.Run(mode, func(t *testing.T) {
				c := openTest(t, ":memory:")
				script(t, c, schema+`; INSERT INTO t VALUES(1,'old')`)
				options := ApplyOptions{}
				sentinel := errors.New("handler error")
				if mode != "nil" {
					options.Conflict = func(Conflict) (ConflictAction, error) {
						if mode == "error" {
							return Omit, sentinel
						}
						if mode == "invalid" {
							return ConflictAction(99), nil
						}
						return Abort, nil
					}
				}
				err := c.ApplyChangeset(bytes.NewReader(data), options)
				want := error(CodeAbort)
				if mode == "error" {
					want = sentinel
				}
				if mode == "invalid" {
					want = ErrInvalidArgument
				}
				if !errors.Is(err, want) {
					t.Fatal(err)
				}
				requireRows(t, c, "SELECT v FROM t", []string{"old"})
			})
		}
		// REPLACE is not valid for NOTFOUND or CONSTRAINT.
		for _, tc := range cases[1:] {
			if tc.typ == ConflictConflict {
				continue
			}
			t.Run("invalid-replace-"+tc.name, func(t *testing.T) {
				c := openTest(t, ":memory:")
				script(t, c, schema+";"+tc.target)
				data := recorded(t, schema, tc.before, tc.change)
				err := c.ApplyChangeset(bytes.NewReader(data), ApplyOptions{Conflict: func(Conflict) (ConflictAction, error) { return Replace, nil }})
				if !errors.Is(err, ErrInvalidArgument) {
					t.Fatal(err)
				}
			})
		}
	})
	t.Run("foreign-key", func(t *testing.T) {
		schema := `CREATE TABLE p(id PRIMARY KEY); CREATE TABLE child(id PRIMARY KEY,p REFERENCES p(id))`
		data := recorded(t, schema, "", "INSERT INTO child VALUES(1,42)")
		for _, action := range []ConflictAction{Abort, Omit, Replace} {
			t.Run(fmt.Sprint(action), func(t *testing.T) {
				c := openTest(t, ":memory:")
				script(t, c, "PRAGMA foreign_keys=ON;"+schema)
				var retained Conflict
				calls := 0
				err := c.ApplyChangeset(bytes.NewReader(data), ApplyOptions{Conflict: func(cf Conflict) (ConflictAction, error) {
					retained = cf
					calls++
					typ, err := cf.Type()
					must(t, err)
					if typ != ConflictForeignKey {
						t.Fatal(typ)
					}
					n, err := cf.ForeignKeyConflicts()
					must(t, err)
					if n != 1 {
						t.Fatal(n)
					}
					if _, err := cf.Change(); !errors.Is(err, ErrState) {
						t.Fatal(err)
					}
					if _, err := cf.Current(0); !errors.Is(err, ErrState) {
						t.Fatal(err)
					}
					return action, nil
				}})
				if action == Omit {
					must(t, err)
					if scalarTest(t, c, "SELECT count(*) FROM child") != 1 {
						t.Fatal("omitted FK")
					}
				} else {
					want := error(CodeConstraint)
					if action == Replace {
						want = ErrInvalidArgument
					}
					if !errors.Is(err, want) {
						t.Fatal(err)
					}
					if scalarTest(t, c, "SELECT count(*) FROM child") != 0 {
						t.Fatal("FK rollback")
					}
				}
				if calls != 1 {
					t.Fatal(calls)
				}
				if _, err := retained.ForeignKeyConflicts(); !errors.Is(err, ErrExpired) {
					t.Fatal(err)
				}
			})
		}
	})
	t.Run("filter-and-native-compatibility", func(t *testing.T) {
		data := recorded(t, "CREATE TABLE t(id PRIMARY KEY,v)", "", "INSERT INTO t VALUES(1,2)")
		c := openTest(t, ":memory:")
		script(t, c, "CREATE TABLE t(id PRIMARY KEY,v)")
		must(t, c.ApplyChangeset(bytes.NewReader(data), ApplyOptions{Filter: func(string) bool { return false }}))
		if scalarTest(t, c, "SELECT count(*) FROM t") != 0 {
			t.Fatal("filter ignored")
		}
		incompatible := openTest(t, ":memory:")
		script(t, incompatible, "CREATE TABLE t(id PRIMARY KEY)")
		must(t, incompatible.ApplyChangeset(bytes.NewReader(data), ApplyOptions{}))
		if scalarTest(t, incompatible, "SELECT count(*) FROM t") != 0 {
			t.Fatal("compatibility ignored")
		}
	})
}
func TestChangesetLazyValueCache(t *testing.T) {
	schema := `CREATE TABLE small(id PRIMARY KEY,v); CREATE TABLE wide(id PRIMARY KEY,a,b,c)`
	insert := `INSERT INTO small VALUES(1,x'3432'),(2,NULL); INSERT INTO wide VALUES(1,1,2,42),(2,1,2,x'3432')`
	data := recorded(t, schema, "", insert)
	checkValues := func(t *testing.T, info ChangeInfo, get func(int) (Value, error)) {
		t.Helper()
		id, err := get(0)
		must(t, err)
		n, nonNull, err := id.Int64()
		must(t, err)
		if !nonNull {
			t.Fatal("NULL primary key")
		}
		want := ValueCopy{Kind: KindBlob, Blob: []byte("42")}
		if info.Table == "small" && n == 2 {
			want = ValueCopy{Kind: KindNull}
		} else if info.Table == "wide" && n == 1 {
			want = ValueCopy{Kind: KindInteger, Int64: 42}
		}
		v, err := get(info.Columns - 1)
		must(t, err)
		copied := v
		text, nonNull, err := copied.Text()
		must(t, err)
		if want.Kind == KindNull {
			if text != "" || nonNull {
				t.Fatalf("NULL conversion: text=%q nonNull=%v", text, nonNull)
			}
		} else if text != "42" || !nonNull {
			t.Fatalf("text=%q nonNull=%v", text, nonNull)
		}
		refetched, err := get(info.Columns - 1)
		must(t, err)
		for _, value := range []Value{v, copied, refetched} {
			valueKind(t, value, want.Kind)
			got, err := value.Copy()
			must(t, err)
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("%s row %d: copy=%#v want=%#v", info.Table, n, got, want)
			}
		}
	}
	t.Run("iterator-width-and-generation", func(t *testing.T) {
		it, err := NewChangesetIter(bytes.NewReader(data))
		must(t, err)
		defer it.Close()
		for {
			row, err := it.Next()
			must(t, err)
			if !row {
				break
			}
			ch, err := it.Change()
			must(t, err)
			info, err := ch.Info()
			must(t, err)
			checkValues(t, info, ch.New)
		}
	})
	t.Run("current-width-and-generation", func(t *testing.T) {
		c := openTest(t, ":memory:")
		script(t, c, schema+";"+insert)
		conflicts := 0
		must(t, c.ApplyChangeset(bytes.NewReader(data), ApplyOptions{Conflict: func(cf Conflict) (ConflictAction, error) {
			ch, err := cf.Change()
			must(t, err)
			info, err := ch.Info()
			must(t, err)
			conflicts++
			// Current must initialize the cache without fetching an incoming Value.
			checkValues(t, info, cf.Current)
			checkValues(t, info, ch.New)
			return Omit, nil
		}}))
		if conflicts != 4 {
			t.Fatalf("conflicts=%d want=4", conflicts)
		}
	})
}

func TestChangesetNullAndMissingValueExpiry(t *testing.T) {
	schema := `CREATE TABLE t(id PRIMARY KEY,a,b)`
	data := recorded(t, schema, `INSERT INTO t VALUES(1,'old',NULL),(2,'old',NULL)`, `UPDATE t SET a=NULL`)
	for _, expireBy := range []string{"next", "close"} {
		t.Run(expireBy, func(t *testing.T) {
			it, err := NewChangesetIter(bytes.NewReader(data))
			must(t, err)
			defer it.Close()
			row, err := it.Next()
			must(t, err)
			if !row {
				t.Fatal("no row")
			}
			ch, err := it.Change()
			must(t, err)
			var retained []Value
			for column, kind := range map[int]Kind{1: KindNull, 2: KindMissing} {
				v, err := ch.New(column)
				must(t, err)
				valueKind(t, v, kind)
				copied := v
				_, _, err = copied.Text()
				if kind == KindMissing {
					if !errors.Is(err, ErrState) {
						t.Fatal(err)
					}
				} else {
					must(t, err)
				}
				refetched, err := ch.New(column)
				must(t, err)
				valueKind(t, refetched, kind)
				retained = append(retained, v, copied, refetched)
			}
			if expireBy == "next" {
				row, err = it.Next()
				must(t, err)
				if !row {
					t.Fatal("no second row")
				}
			} else {
				must(t, it.Close())
			}
			for _, v := range retained {
				if _, err := v.Kind(); !errors.Is(err, ErrExpired) {
					t.Fatalf("Kind after %s: %v", expireBy, err)
				}
				if got, err := v.Copy(); !errors.Is(err, ErrExpired) || !reflect.DeepEqual(got, ValueCopy{}) {
					t.Fatalf("Copy after %s: value=%#v err=%v", expireBy, got, err)
				}
				if _, _, err := v.Int64(); !errors.Is(err, ErrExpired) {
					t.Fatalf("Int64 after %s: %v", expireBy, err)
				}
				if _, _, err := v.Float64(); !errors.Is(err, ErrExpired) {
					t.Fatalf("Float64 after %s: %v", expireBy, err)
				}
				if _, _, err := v.Text(); !errors.Is(err, ErrExpired) {
					t.Fatalf("Text after %s: %v", expireBy, err)
				}
				if _, err := v.Blob(); !errors.Is(err, ErrExpired) {
					t.Fatalf("Blob after %s: %v", expireBy, err)
				}
			}
		})
	}
}

func TestChangesetTransformsAndGroups(t *testing.T) {
	schema := "CREATE TABLE t(id PRIMARY KEY,v)"
	a := recorded(t, schema, `INSERT INTO t VALUES(1,'old')`, `UPDATE t SET v='new'; INSERT INTO t VALUES(2,'two')`)
	b := recorded(t, schema, `INSERT INTO t VALUES(1,'new'),(2,'two')`, `DELETE FROM t WHERE id=1; UPDATE t SET v='final' WHERE id=2`)
	var concat bytes.Buffer
	must(t, ConcatChangesets(&concat, bytes.NewReader(a), bytes.NewReader(b)))
	g, err := NewChangegroup()
	must(t, err)
	t.Cleanup(func() { must(t, g.Close()) })
	if err := g.Add(nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal(err)
	}
	must(t, g.Add(bytes.NewReader(a)))
	must(t, g.Add(bytes.NewReader(b)))
	var grouped bytes.Buffer
	n, err := g.WriteTo(&grouped)
	must(t, err)
	if n != int64(grouped.Len()) {
		t.Fatal(n)
	}
	for name, data := range map[string][]byte{"concat": concat.Bytes(), "group": grouped.Bytes()} {
		t.Run(name, func(t *testing.T) {
			c := openTest(t, ":memory:")
			script(t, c, schema+`; INSERT INTO t VALUES(1,'old')`)
			must(t, c.ApplyChangeset(bytes.NewReader(data), ApplyOptions{}))
			requireRows(t, c, "SELECT id||':'||v FROM t", []string{"2:final"})
			var inverse bytes.Buffer
			must(t, InvertChangeset(&inverse, bytes.NewReader(data)))
			must(t, c.ApplyChangeset(bytes.NewReader(inverse.Bytes()), ApplyOptions{}))
			requireRows(t, c, "SELECT id||':'||v FROM t", []string{"1:old"})
		})
	}
	sentinel := errors.New("output failed")
	partial := &failureWriter{err: sentinel, accept: 3}
	n, err = g.WriteTo(partial)
	if n != 3 || !errors.Is(err, sentinel) || len(partial.prefix) != 3 {
		t.Fatalf("n=%d err=%v prefix=%x", n, err, partial.prefix)
	}
	var recovered bytes.Buffer
	n, err = g.WriteTo(&recovered)
	must(t, err)
	if !bytes.Equal(recovered.Bytes(), grouped.Bytes()) || n != int64(recovered.Len()) {
		t.Fatal("output failure changed group")
	}
	must(t, g.Close())
	must(t, g.Close())
	for _, tc := range streamInputFailures(sentinel) {
		t.Run("failed-add-"+tc.name, func(t *testing.T) {
			failed, err := NewChangegroup()
			must(t, err)
			err = failed.Add(&pairedReader{data: a, err: tc.err})
			if !errors.Is(err, tc.err) {
				t.Errorf("Add error=%v want %v", err, tc.err)
			}
			if err := failed.Add(bytes.NewReader(a)); !errors.Is(err, ErrState) {
				t.Errorf("Add after failed input: %v", err)
			}
			if _, err := failed.WriteTo(&bytes.Buffer{}); !errors.Is(err, ErrState) {
				t.Errorf("WriteTo after failed input: %v", err)
			}
			must(t, failed.Close())
		})
	}
	corrupt, err := NewChangegroup()
	must(t, err)
	if err := corrupt.Add(strings.NewReader("bad changeset")); err == nil {
		t.Fatal("corrupt add accepted")
	}
	if _, err := corrupt.WriteTo(&bytes.Buffer{}); !errors.Is(err, ErrState) {
		t.Fatal(err)
	}
	must(t, corrupt.Close())
}
