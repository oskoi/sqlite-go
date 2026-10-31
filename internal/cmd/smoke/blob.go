package main

import (
	"bytes"
	"errors"
	"fmt"
	"io"

	sqlite "github.com/oskoi/sqlite-go"
)

func blobs(c *sqlite.Conn) {
	check(c.ExecScript("CREATE TABLE blob_demo(v,other); INSERT INTO blob_demo VALUES(zeroblob(8192),0)"))
	b, err := c.OpenBlob("main", "blob_demo", "v", 1, true)
	check(err)
	want := make([]byte, 8192)
	for i := range want {
		want[i] = byte(i*31 + 7)
	}
	for start := 0; start < len(want); start += 1024 {
		n, err := b.Write(want[start : start+1024])
		check(err)
		if n != 1024 {
			panic("short blob write")
		}
	}
	_, err = b.Seek(0, io.SeekStart)
	check(err)
	got := make([]byte, len(want))
	n, err := b.Read(got[:16])
	check(err)
	if n != 16 {
		panic("short blob read")
	}
	probe := make([]byte, 32)
	n, err = b.ReadAt(probe, 4000)
	check(err)
	if n != len(probe) || !bytes.Equal(probe, want[4000:4032]) {
		panic("positional blob bytes")
	}
	pos, err := b.Seek(0, io.SeekCurrent)
	check(err)
	if pos != 16 {
		panic("ReadAt changed blob offset")
	}
	n, err = b.WriteAt(want[6000:6016], 6000)
	check(err)
	pos, seekErr := b.Seek(0, io.SeekCurrent)
	check(seekErr)
	if n != 16 || pos != 16 {
		panic("WriteAt changed blob offset")
	}
	n, err = b.Read(got[16:])
	check(err)
	if n != len(got)-16 || !bytes.Equal(got, want) {
		panic("mixed blob bytes")
	}
	if n, err = b.Read(probe); n != 0 || err != io.EOF {
		panic("blob EOF")
	}
	check(b.Close())
	check(b.Close())
	s, _, err := c.Prepare("SELECT v FROM blob_demo", 0)
	check(err)
	row, err := s.Step()
	check(err)
	if !row {
		panic("missing SQL blob row")
	}
	data, err := s.ColumnBlob(0)
	check(err)
	if !bytes.Equal(data, want) {
		panic("SQL-visible blob bytes")
	}
	row, err = s.Step()
	check(err)
	if row {
		panic("unexpected SQL blob row")
	}
	check(s.Close())
	fmt.Println("blob: 8192 exact SQL-visible bytes; chunked fill; mixed sequential/positional offset independence; explicit close")
	blobTextReopen(c)
	blobSequentialPanic(c)
}

func blobSequentialPanic(c *sqlite.Conn) {
	check(c.ExecScript("CREATE TABLE blob_panic_demo(id INTEGER PRIMARY KEY,v); INSERT INTO blob_panic_demo VALUES(1,x'010203')"))
	b, err := c.OpenBlob("main", "blob_panic_demo", "v", 1, true)
	check(err)
	session, err := c.NewSession("main")
	check(err)
	value := &struct{ message string }{"sequential blob filter panic"}
	calls := 0
	check(session.SetTableFilter(func(string) bool {
		calls++
		if _, err := b.Seek(0, io.SeekCurrent); !errors.Is(err, sqlite.ErrReentrant) {
			panic("blob filter reentry")
		}
		panic(value)
	}))
	check(session.AttachAll())
	recovered := false
	func() {
		defer func() {
			if got := recover(); got != value {
				panic(fmt.Sprintf("unexpected sequential blob panic: %#v", got))
			}
			recovered = true
		}()
		_, err := b.Write([]byte{9})
		check(err)
	}()
	first := make([]byte, 3)
	n, err := b.ReadAt(first, 0)
	check(err)
	pos, err := b.Seek(0, io.SeekCurrent)
	check(err)
	if !recovered || calls != 1 || n != 3 || !bytes.Equal(first, []byte{9, 2, 3}) || pos != 1 {
		panic("sequential blob completion before panic")
	}
	n, err = b.Write([]byte{8})
	check(err)
	pos, err = b.Seek(0, io.SeekCurrent)
	check(err)
	if n != 1 || pos != 2 || calls != 1 {
		panic("sequential blob resume after panic")
	}
	check(b.Close())
	check(session.Close())
	if scalar(c, "SELECT v=x'090803' FROM blob_panic_demo") != 1 {
		panic("resumed SQL-visible blob bytes")
	}
	fmt.Printf("blob sequential panic: same value recovered=%v; first bytes=%x; position=1; resumed SQL bytes=090803; position=%d; explicit close\n", recovered, first, pos)
}
