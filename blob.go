package sqlite

/*
#cgo noescape sg_blob_io
// Audited: blob I/O uses caller bytes only synchronously. Writes may invoke
// session filters, so this helper must not be marked nocallback. Open/reopen/
// close likewise remain callback-capable.
#include "bridge.h"
#include <stdlib.h>
*/
import "C"

import (
	"io"
	"runtime"
	"unsafe"
)

// Blob owns incremental I/O on one fixed-size BLOB or TEXT cell and retains its
// connection. Do not copy it or use it concurrently with its parent/children.
// Close explicitly, including after abort; no finalizer commits its transaction.
type Blob struct {
	_       noCopy
	blob    *C.sqlite3_blob
	conn    *Conn
	size    int64
	offset  int64
	rowID   int64
	aborted bool
}

// OpenBlob opens an existing rowid cell. SQLite enforces column/type/index and
// read-only restrictions. The handle cannot resize the cell.
func (c *Conn) OpenBlob(schema, table, column string, rowID int64, writable bool) (*Blob, error) {
	if err := c.check(); err != nil {
		return nil, err
	}
	for _, name := range []string{schema, table, column} {
		if err := validCString(name); err != nil {
			return nil, err
		}
	}
	db, tbl, col := C.CString(schema), C.CString(table), C.CString(column)
	defer C.free(unsafe.Pointer(db))
	defer C.free(unsafe.Pointer(tbl))
	defer C.free(unsafe.Pointer(col))
	c.active = true
	defer c.endCall()
	r := C.sg_blob_open(c.db, db, tbl, col, C.sqlite3_int64(rowID), C.int(boolInt(writable)))
	if c.panicked {
		if r.blob != nil {
			takeError(C.sg_blob_close(c.db, r.blob), "OpenBlob.Close")
		}
		takeError(r.error, "OpenBlob")
		return nil, nil
	}
	if err := takeError(r.error, "OpenBlob"); err != nil {
		return nil, err
	}
	b := &Blob{blob: r.blob, conn: c, size: int64(r.size), rowID: rowID}
	c.children++
	return b, nil
}

func (b *Blob) check() error {
	if b == nil || b.blob == nil {
		return ErrClosed
	}
	return b.conn.check()
}
func (b *Blob) usable(op string) error {
	if err := b.check(); err != nil {
		return err
	}
	if b.aborted {
		return &Error{Code: CodeAbort, Message: "blob aborted", Offset: -1, Op: op}
	}
	return nil
}

// Size returns the cell size, or zero after an observed abort.
func (b *Blob) Size() (int64, error) {
	if err := b.check(); err != nil {
		return 0, err
	}
	return b.size, nil
}

// RowID returns the last successfully opened row identifier, even after abort.
func (b *Blob) RowID() (int64, error) {
	if err := b.check(); err != nil {
		return 0, err
	}
	return b.rowID, nil
}

// Read advances the sequential offset by bytes read. A nonempty final read
// returns nil; EOF is returned only when no bytes remain.
func (b *Blob) Read(p []byte) (int, error) {
	if err := b.usable("Blob.Read"); err != nil {
		return 0, err
	}
	c := b.conn
	c.active = true
	defer c.endCall()
	n, err := b.readAt(p, b.offset)
	b.offset += int64(n)
	return n, err
}

// ReadAt reads without changing the sequential offset. Short reads return EOF.
// Negative offsets return ErrRange; nonempty reads at/past the end return EOF.
func (b *Blob) ReadAt(p []byte, offset int64) (int, error) {
	if err := b.usable("Blob.ReadAt"); err != nil {
		return 0, err
	}
	c := b.conn
	c.active = true
	defer c.endCall()
	n, err := b.readAt(p, offset)
	if err == nil && n < len(p) {
		err = io.EOF
	}
	return n, err
}
func (b *Blob) readAt(p []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, ErrRange
	}
	if len(p) == 0 {
		return 0, nil
	}
	if offset >= b.size {
		return 0, io.EOF
	}
	n := len(p)
	if int64(n) > b.size-offset {
		n = int(b.size - offset)
	}
	return b.io(p[:n], offset, false)
}

// Write writes all bytes and advances the sequential offset, or returns zero
// and an error. A request exceeding the fixed size returns ErrBlobFull without
// changing a prefix. Native errors do not promise transactional rollback.
func (b *Blob) Write(p []byte) (int, error) {
	if err := b.usable("Blob.Write"); err != nil {
		return 0, err
	}
	c := b.conn
	c.active = true
	defer c.endCall()
	n, err := b.writeAt(p, b.offset)
	b.offset += int64(n)
	return n, err
}

// WriteAt writes all bytes without changing the sequential offset. Negative
// offsets return ErrRange; requests beyond the fixed size return ErrBlobFull.
func (b *Blob) WriteAt(p []byte, offset int64) (int, error) {
	if err := b.usable("Blob.WriteAt"); err != nil {
		return 0, err
	}
	c := b.conn
	c.active = true
	defer c.endCall()
	return b.writeAt(p, offset)
}
func (b *Blob) writeAt(p []byte, offset int64) (int, error) {
	if offset < 0 {
		return 0, ErrRange
	}
	if offset > b.size || int64(len(p)) > b.size-offset {
		return 0, ErrBlobFull
	}
	if len(p) == 0 {
		return 0, nil
	}
	return b.io(p, offset, true)
}
func (b *Blob) io(p []byte, offset int64, write bool) (int, error) {
	// size comes from sqlite3_blob_bytes (C int). Bounds above establish that
	// both count and offset fit C int without adding potentially large inputs.
	// Public I/O methods retain the active guard until sequential position and
	// positional EOF handling finish, before endCall can re-panic.
	c := b.conn
	e := C.sg_blob_io(c.db, b.blob, unsafe.Pointer(unsafe.SliceData(p)), C.int(len(p)), C.int(offset), C.int(boolInt(write)))
	runtime.KeepAlive(p)
	if Code(e.code).Primary() == CodeAbort {
		b.aborted = true
		b.size = 0
	}
	if err := takeError(e, "Blob.IO"); err != nil {
		return 0, err
	}
	return len(p), nil
}

// Seek sets the sequential offset within [0, Size()]. Invalid whence returns
// ErrInvalidArgument; out-of-bounds or overflowing positions return ErrRange.
func (b *Blob) Seek(offset int64, whence int) (int64, error) {
	if err := b.usable("Blob.Seek"); err != nil {
		return 0, err
	}
	var base int64
	switch whence {
	case io.SeekStart:
	case io.SeekCurrent:
		base = b.offset
	case io.SeekEnd:
		base = b.size
	default:
		return 0, ErrInvalidArgument
	}
	if offset < -base || offset > b.size-base {
		return 0, ErrRange
	}
	b.offset = base + offset
	return b.offset, nil
}

// Reopen selects the same column of another row and resets offset on success.
// Any native failure permanently aborts the handle. Reopen before an observed
// I/O abort can recover an otherwise unobserved SQL row-update expiration.
func (b *Blob) Reopen(rowID int64) error {
	if err := b.usable("Blob.Reopen"); err != nil {
		return err
	}
	c := b.conn
	c.active = true
	defer c.endCall()
	r := C.sg_blob_reopen(c.db, b.blob, C.sqlite3_int64(rowID))
	if err := takeError(r.error, "Blob.Reopen"); err != nil {
		b.aborted = true
		b.size = 0
		return err
	}
	b.size, b.offset, b.rowID = int64(r.size), 0, rowID
	return nil
}

// Close consumes the blob even on native commit failure or callback panic,
// releases its parent, and is harmless when called again.
func (b *Blob) Close() error {
	if b == nil || b.blob == nil {
		return nil
	}
	if err := b.check(); err != nil {
		return err
	}
	c := b.conn
	c.active = true
	defer c.endCall()
	e := C.sg_blob_close(c.db, b.blob)
	b.blob, b.conn = nil, nil
	b.size, b.offset = 0, 0
	c.children--
	return takeError(e, "Blob.Close")
}
