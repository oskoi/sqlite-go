package sqlite

/*
#include "bridge.h"
#include <stdlib.h>
*/
import "C"

import (
	"errors"
	"math"
	"sync"
	"time"
	"unsafe"
)

// Conn owns one native connection. Do not copy it or operate on it and its
// children simultaneously. Close every child explicitly before closing Conn.
type Conn struct {
	_        noCopy
	db       *C.sqlite3
	children int
	active   bool
	lifetime sync.Mutex
	// interruptDB is protected by lifetime; ordinary owner operations use db.
	interruptDB *C.sqlite3
	callbacks   [callbackCount]*C.sg_callback
	panicked    bool
	panicValue  any
	sessions    *Session
	// backup is the destination reservation. Writes and Interrupt reads use
	// lifetime; ordinary owner checks stay lock-free.
	backup *Backup
	// backupPeer participates in the same native call and shares its first panic.
	backupPeer *Conn
}

// Open opens a standard main database or returns an error. Nonzero flags are
// explicit rather than additive. Attached databases are not covered.
func Open(name string, flags OpenFlags) (*Conn, error) {
	if err := validCString(name); err != nil {
		return nil, err
	}
	flags, err := normalizeOpenFlags(flags)
	if err != nil {
		return nil, err
	}
	c, err := openNative(name, flags)
	if err != nil {
		return nil, err
	}
	if err := validateStandard(c); err != nil {
		return nil, err
	}
	return c, nil
}

// openNative requires a validated name and normalized flags.
func openNative(name string, flags OpenFlags) (*Conn, error) {
	if err := bootstrapMultiwriter(); err != nil {
		return nil, err
	}
	z := C.CString(name)
	defer C.free(unsafe.Pointer(z))
	var db *C.sqlite3
	rc := C.sqlite3_open_v2(z, &db, C.int(flags)|C.SQLITE_OPEN_EXRESCODE, nil)
	if rc != C.SQLITE_OK {
		if db == nil {
			return nil, codeError(rc, "Open")
		}
		err := dbError(db, rc, "Open")
		return nil, errors.Join(err, codeError(C.sqlite3_close(db), "Open.Close"))
	}
	return &Conn{db: db, interruptDB: db}, nil
}

func normalizeOpenFlags(flags OpenFlags) (OpenFlags, error) {
	const allowed = OpenReadOnly | OpenReadWrite | OpenCreate | OpenURI | OpenMemory | OpenNoMutex | OpenFullMutex | OpenPrivateCache | OpenNoFollow
	if flags == 0 {
		flags = OpenReadWrite | OpenCreate | OpenURI | OpenNoMutex
	}
	if flags & ^allowed != 0 || flags&(OpenReadOnly|OpenReadWrite) == 0 || flags&(OpenReadOnly|OpenReadWrite) == OpenReadOnly|OpenReadWrite || flags&OpenReadOnly != 0 && flags&OpenCreate != 0 || flags&(OpenNoMutex|OpenFullMutex) == OpenNoMutex|OpenFullMutex {
		return 0, ErrInvalidArgument
	}
	return flags, nil
}

// Close strictly closes the connection, refusing while children remain.
// A refused close leaves the connection and its children usable.
func (c *Conn) Close() error {
	if c == nil {
		return nil
	}
	if c.active {
		return ErrReentrant
	}
	if c.db == nil {
		return nil
	}
	if c.backup != nil {
		return backupBusy()
	}
	if c.children != 0 {
		return &Error{Code: CodeBusy, Message: "live child handles", Offset: -1, Op: "Close"}
	}
	// Wait for in-flight interrupts, then disable them. Never hold this gate
	// across native close: rollback hooks may themselves call Interrupt.
	c.lifetime.Lock()
	c.interruptDB = nil
	c.lifetime.Unlock()
	c.active = true
	defer c.endCall()
	err := takeError(C.sg_close(c.db), "Close")
	if err == nil {
		c.db = nil
		c.releaseCallbacks()
	} else {
		c.lifetime.Lock()
		c.interruptDB = c.db
		c.lifetime.Unlock()
	}
	return err
}

// Prepare compiles the first statement and returns the unconsumed SQL tail.
func (c *Conn) Prepare(sql string, flags PrepareFlags) (*Stmt, string, error) {
	if err := c.check(); err != nil {
		return nil, "", err
	}
	if err := validCString(sql); err != nil {
		return nil, "", err
	}
	if flags & ^(PreparePersistent|PrepareNoVTab) != 0 {
		return nil, "", ErrInvalidArgument
	}
	z := C.CString(sql)
	defer C.free(unsafe.Pointer(z))
	c.active = true
	defer c.endCall()
	r := C.sg_prepare(c.db, z, C.int(len(sql)), C.uint(flags))
	if c.panicked {
		// A notification during preparation can leave a successful native
		// statement. It must not escape when this call is about to re-panic.
		if r.stmt != nil {
			takeError(C.sg_finalize(r.stmt), "Prepare.Close")
		}
		takeError(r.error, "Prepare")
		return nil, "", nil
	}
	if err := takeError(r.error, "Prepare"); err != nil {
		return nil, "", err
	}
	tail := sql[int(r.tail):]
	if r.stmt == nil {
		return nil, tail, nil
	}
	s := &Stmt{stmt: r.stmt, conn: c, columns: int(r.columns), parameters: int(r.parameters), generation: 1}
	s.kinds = make([]columnKind, s.columns)
	c.children++
	return s, tail, nil
}

// ExecScript executes all statements, discarding rows, without a transaction.
func (c *Conn) ExecScript(sql string) error {
	if err := c.check(); err != nil {
		return err
	}
	if err := validCString(sql); err != nil {
		return err
	}
	z := C.CString(sql)
	defer C.free(unsafe.Pointer(z))
	c.active = true
	defer c.endCall()
	e := C.sg_script(c.db, z, C.int(len(sql)))
	return takeError(e, "ExecScript")
}

// Begin executes BEGIN.
func (c *Conn) Begin() error { return c.transaction(C.SG_TX_BEGIN) }

// BeginImmediate executes BEGIN IMMEDIATE.
func (c *Conn) BeginImmediate() error { return c.transaction(C.SG_TX_IMMEDIATE) }

// BeginExclusive executes BEGIN EXCLUSIVE.
func (c *Conn) BeginExclusive() error { return c.transaction(C.SG_TX_EXCLUSIVE) }

// Commit executes COMMIT, preserving SQLite's failure/transaction semantics.
func (c *Conn) Commit() error { return c.transaction(C.SG_TX_COMMIT) }

// Rollback executes ROLLBACK.
func (c *Conn) Rollback() error { return c.transaction(C.SG_TX_ROLLBACK) }

func (c *Conn) transaction(operation C.int) error {
	if err := c.check(); err != nil {
		return err
	}
	c.active = true
	defer c.endCall()
	return takeError(C.sg_transaction(c.db, operation), "ExecScript")
}

// AutoCommit reports whether SQLite is outside an explicit transaction.
func (c *Conn) AutoCommit() (bool, error) {
	if err := c.check(); err != nil {
		return false, err
	}
	return C.sqlite3_get_autocommit(c.db) != 0, nil
}

// Changes returns SQLite's last direct change count, excluding trigger effects.
func (c *Conn) Changes() (int64, error) {
	if err := c.check(); err != nil {
		return 0, err
	}
	return int64(C.sqlite3_changes64(c.db)), nil
}

// TotalChanges returns the native cumulative change count.
func (c *Conn) TotalChanges() (int64, error) {
	if err := c.check(); err != nil {
		return 0, err
	}
	return int64(C.sqlite3_total_changes64(c.db)), nil
}

// LastInsertRowID returns the native last inserted rowid.
func (c *Conn) LastInsertRowID() (int64, error) {
	if err := c.check(); err != nil {
		return 0, err
	}
	return int64(C.sqlite3_last_insert_rowid(c.db)), nil
}

// FileName copies SQLite's filename for schema (empty for an unknown schema).
func (c *Conn) FileName(schema string) (string, error) {
	if err := c.check(); err != nil {
		return "", err
	}
	if err := validCString(schema); err != nil {
		return "", err
	}
	z := C.CString(schema)
	defer C.free(unsafe.Pointer(z))
	return C.GoString(C.sqlite3_db_filename(c.db, z)), nil
}

// Limit sets a native limit, or queries it when value is negative.
func (c *Conn) Limit(id Limit, value int) (int, error) {
	if err := c.check(); err != nil {
		return 0, err
	}
	if id < LimitLength || id > LimitParserDepth || value < math.MinInt32 || value > math.MaxInt32 {
		return 0, ErrRange
	}
	return int(C.sqlite3_limit(c.db, C.int(id), C.int(value))), nil
}

// Status reads a native connection counter and optionally resets its peak.
func (c *Conn) Status(op DBStatus, reset bool) (current, peak int, err error) {
	if err = c.check(); err != nil {
		return
	}
	if op < DBStatusLookasideUsed || op > DBStatusTempBufSpill {
		return 0, 0, ErrRange
	}
	var a, b C.int
	rc := C.sqlite3_db_status(c.db, C.int(op), &a, &b, C.int(boolInt(reset)))
	return int(a), int(b), codeError(rc, "Status")
}

// BusyTimeout replaces the managed busy handler with SQLite's timeout.
// Nonpositive durations disable waiting; positive durations round up to ms.
func (c *Conn) BusyTimeout(d time.Duration) error {
	if err := c.check(); err != nil {
		return err
	}
	var ms time.Duration
	if d > 0 {
		ms = d / time.Millisecond
		if d%time.Millisecond != 0 {
			ms++
		}
		if ms > math.MaxInt32 {
			return ErrRange
		}
	}
	if err := c.SetBusyHandler(nil); err != nil {
		return err
	}
	return codeError(C.sqlite3_busy_timeout(c.db, C.int(ms)), "BusyTimeout")
}
