package sqlite

/*
#include "bridge.h"
#include <stdlib.h>
*/
import "C"

import (
	"math"
	"unsafe"
)

// Backup incrementally copies a schema between two different connections.
// Both parents must outlive it. The destination and its other children are
// unavailable until Close, including after Step reports done. Do not copy it.
type Backup struct {
	_            noCopy
	backup       *C.sqlite3_backup
	dst, src     *Conn
	done, failed bool
}

func backupBusy() error {
	return &Error{Code: CodeBusy, Message: "reserved backup destination", Offset: -1, Op: "Backup"}
}

// NewBackup reserves dst and initializes a native backup from srcSchema to
// dstSchema. Schema names are explicit. SQLite decides transaction legality.
// Serialize source operations with backup calls; source use between calls is OK.
func (dst *Conn) NewBackup(dstSchema string, src *Conn, srcSchema string) (*Backup, error) {
	if err := dst.check(); err != nil {
		return nil, err
	}
	if err := src.check(); err != nil {
		return nil, err
	}
	if dst == src {
		return nil, ErrInvalidArgument
	}
	if err := validCString(dstSchema); err != nil {
		return nil, err
	}
	if err := validCString(srcSchema); err != nil {
		return nil, err
	}
	d, s := C.CString(dstSchema), C.CString(srcSchema)
	defer C.free(unsafe.Pointer(d))
	defer C.free(unsafe.Pointer(s))
	b := &Backup{dst: dst, src: src}
	// Taking the same gate as Interrupt drains in-flight native interrupts.
	// Publish Reserving before releasing it; no gate is held across callbacks.
	dst.lifetime.Lock()
	dst.backup = b
	dst.lifetime.Unlock()
	b.beginCall()
	defer b.endCall(dst, src)
	r := C.sg_backup_init(dst.db, d, src.db, s)
	b.backup = r.backup
	if err := takeError(r.error, "NewBackup"); err != nil {
		b.releaseReservation()
		return nil, err
	}
	// A native handle changes Reserving to Reserved. Interrupt only needs the
	// reservation identity, never this owner-only handle or its state.
	dst.children++
	src.children++
	return b, nil
}

func (b *Backup) check() error {
	if b == nil || b.backup == nil {
		return ErrClosed
	}
	// Check both active guards before reservations, retaining reentry precedence.
	if b.dst.active || b.src.active {
		return ErrReentrant
	}
	// An older backup's source may have become another backup's destination.
	// Even Progress/Close must not bypass that newer destination reservation.
	if b.src.backup != nil || b.dst.backup != b {
		return backupBusy()
	}
	return nil
}
func (b *Backup) beginCall() {
	b.dst.active, b.src.active = true, true
	b.dst.backupPeer, b.src.backupPeer = b.src, b.dst
}
func (b *Backup) releaseReservation() {
	dst := b.dst
	dst.lifetime.Lock()
	dst.backup = nil
	dst.lifetime.Unlock()
}
func (b *Backup) finish() error {
	dst, src := b.dst, b.src
	err := takeError(C.sg_backup_finish(dst.db, b.backup), "Backup.Close")
	b.backup = nil
	dst.children--
	src.children--
	b.releaseReservation()
	b.dst, b.src = nil, nil
	return err
}
func (b *Backup) endCall(dst, src *Conn) {
	if dst.panicked || src.panicked {
		// Destination APIs, including hook unregistration, are forbidden until
		// native finish. Consume the backup before cleaning up either owner.
		if b.backup != nil {
			b.finish()
		}
		value := dst.panicValue
		for _, c := range []*Conn{dst, src} {
			c.clearCallbacks()
			c.clearSessionFilters()
			c.releaseCallbacks()
			c.panicValue, c.panicked = nil, false
		}
		dst.backupPeer, src.backupPeer = nil, nil
		dst.active, src.active = false, false
		panic(value)
	}
	dst.backupPeer, src.backupPeer = nil, nil
	dst.active, src.active = false, false
}

// Step copies up to pages pages. Any negative value copies all remaining pages;
// zero retains SQLite's zero-page behavior. DONE is sticky. BUSY and LOCKED are
// returned without retries; after other native failures only Progress/Close work.
// A callback panic finishes and consumes the backup before re-panicking. Native
// effects already committed by DONE are not undone by panic cleanup.
func (b *Backup) Step(pages int) (done bool, err error) {
	if err := b.check(); err != nil {
		return false, err
	}
	if pages > math.MaxInt32 {
		return false, ErrRange
	}
	if b.failed {
		return false, ErrState
	}
	if b.done {
		return true, nil
	}
	if pages < 0 {
		pages = -1
	}
	dst, src := b.dst, b.src
	b.beginCall()
	defer b.endCall(dst, src)
	rc := C.sqlite3_backup_step(b.backup, C.int(pages))
	if rc == C.SQLITE_DONE {
		b.done = true
		return true, nil
	}
	if rc == C.SQLITE_OK {
		return false, nil
	}
	if Code(rc).Primary() != CodeBusy && Code(rc).Primary() != CodeLocked {
		b.failed = true
	}
	// The destination may not be passed to ANY other SQLite API while the
	// backup exists. Step returns its exact code (including extended codes);
	// connection diagnostics are only available after failed init or finish.
	return false, codeError(rc, "Backup.Step")
}

// Progress reports the page counts from the most recent native Step, including
// after DONE or fatal failure. It does not refresh counts after source mutation.
func (b *Backup) Progress() (remaining, total int, err error) {
	if err := b.check(); err != nil {
		return 0, 0, err
	}
	r := C.sg_backup_status(b.backup)
	return int(r.remaining), int(r.total), nil
}

// Close finishes once and releases both parents and the destination reservation,
// even on native failure. An incomplete backup has SQLite's rollback semantics.
// Repeated Close, including after a callback panic consumed it, is harmless.
func (b *Backup) Close() error {
	if b == nil || b.backup == nil {
		return nil
	}
	if err := b.check(); err != nil {
		return err
	}
	dst, src := b.dst, b.src
	b.beginCall()
	defer b.endCall(dst, src)
	return b.finish()
}
