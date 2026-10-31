package sqlite

/*
#include "bridge.h"
*/
import "C"

import (
	"runtime/cgo"
	"unsafe"
)

// BusyHandler returns true to retry a locked operation. Attempt starts at zero.
type BusyHandler func(attempt int) bool

// CommitHook returns true to veto a commit.
type CommitHook func() bool

// RollbackHook observes a native transaction rollback.
type RollbackHook func()

// Operation identifies a native row insertion, update, or deletion.
type Operation int32

// UpdateEvent describes a changed row. Its strings are Go-owned copies.
type UpdateEvent struct {
	Operation Operation
	Schema    string
	Table     string
	RowID     int64
}

// UpdateHook observes native row changes, including changes later rolled back.
type UpdateHook func(UpdateEvent)

// OptionalText distinguishes a native NULL pointer from an empty string.
type OptionalText struct {
	Text  string
	Valid bool
}

// AuthorizerAction identifies the operation SQLite is authorizing.
type AuthorizerAction int32

// AuthorizerResult allows, denies, or ignores a native authorization request.
type AuthorizerResult int32

// Authorization describes a native authorization request with copied strings.
type Authorization struct {
	Action                     AuthorizerAction
	Arg1, Arg2, Schema, Origin OptionalText
}

// Authorizer runs during statement preparation, including automatic reprepare.
type Authorizer func(Authorization) AuthorizerResult

const (
	busyCallback = iota
	commitCallback
	rollbackCallback
	updateCallback
	authorizerCallback
	callbackCount
)

type callback struct {
	conn *Conn
	fn   any
}

func (c *Conn) setCallback(kind int, fn any) error {
	if err := c.check(); err != nil {
		return err
	}
	var next *C.sg_callback
	if fn != nil {
		h := cgo.NewHandle(&callback{conn: c, fn: fn})
		next = C.sg_callback_new(C.uintptr_t(h))
		if next == nil {
			h.Delete()
			return codeError(C.SQLITE_NOMEM, "SetCallback")
		}
	}
	rc := C.sg_set_callback(c.db, C.int(kind), next)
	if rc != C.SQLITE_OK {
		freeCallback(next)
		return codeError(rc, "SetCallback")
	}
	old := c.callbacks[kind]
	c.callbacks[kind] = next
	freeCallback(old)
	return nil
}

func freeCallback(ctx *C.sg_callback) {
	if ctx != nil {
		cgo.Handle(ctx.handle).Delete()
		C.sqlite3_free(unsafe.Pointer(ctx))
	}
}

// releaseCallbacks runs only after native unregistration or successful close.
func (c *Conn) releaseCallbacks() {
	for i, ctx := range c.callbacks {
		freeCallback(ctx)
		c.callbacks[i] = nil
	}
}

func (c *Conn) clearCallbacks() {
	for kind, ctx := range c.callbacks {
		if ctx != nil {
			C.sg_set_callback(c.db, C.int(kind), nil)
		}
	}
}

// SetBusyHandler installs f; nil removes it and disables any native timeout.
func (c *Conn) SetBusyHandler(f BusyHandler) error {
	if f == nil {
		return c.setCallback(busyCallback, nil)
	}
	return c.setCallback(busyCallback, f)
}

// SetCommitHook installs f; nil removes it. True vetoes the commit.
func (c *Conn) SetCommitHook(f CommitHook) error {
	if f == nil {
		return c.setCallback(commitCallback, nil)
	}
	return c.setCallback(commitCallback, f)
}

// SetRollbackHook installs f; nil removes it.
func (c *Conn) SetRollbackHook(f RollbackHook) error {
	if f == nil {
		return c.setCallback(rollbackCallback, nil)
	}
	return c.setCallback(rollbackCallback, f)
}

// SetUpdateHook installs f; nil removes it. Events do not imply a commit.
func (c *Conn) SetUpdateHook(f UpdateHook) error {
	if f == nil {
		return c.setCallback(updateCallback, nil)
	}
	return c.setCallback(updateCallback, f)
}

// SetAuthorizer installs f; nil removes it. Return AuthorizerOK, AuthorizerDeny,
// or AuthorizerIgnore according to SQLite's authorization rules.
func (c *Conn) SetAuthorizer(f Authorizer) error {
	if f == nil {
		return c.setCallback(authorizerCallback, nil)
	}
	return c.setCallback(authorizerCallback, f)
}

func optionalText(p *C.char) OptionalText {
	if p == nil {
		return OptionalText{}
	}
	return OptionalText{Text: C.GoString(p), Valid: true}
}

// capturePanic is deferred by every trampoline. The completion flag also
// catches panic(nil) when a caller has enabled GODEBUG=panicnil=1.
func (c *Conn) capturePanic(returned *bool) {
	value := recover()
	if !*returned && !c.panicked {
		c.recordPanic(value)
	}
}

// recordPanic suppresses both owners of a backup before native cleanup can
// invoke another callback. Calls sharing a participating owner cannot nest.
func (c *Conn) recordPanic(value any) {
	c.panicked = true
	c.panicValue = value
	if peer := c.backupPeer; peer != nil && !peer.panicked {
		peer.panicked = true
		peer.panicValue = value
	}
}

//export sg_go_busy
func sg_go_busy(token C.uintptr_t, attempt C.int) (retry C.int) {
	cb := cgo.Handle(token).Value().(*callback)
	returned := false
	defer cb.conn.capturePanic(&returned)
	if !cb.conn.panicked && cb.fn.(BusyHandler)(int(attempt)) {
		retry = 1
	}
	returned = true
	return retry
}

//export sg_go_commit
func sg_go_commit(token C.uintptr_t) (veto C.int) {
	veto = 1
	cb := cgo.Handle(token).Value().(*callback)
	returned := false
	defer cb.conn.capturePanic(&returned)
	if !cb.conn.panicked && !cb.fn.(CommitHook)() {
		veto = 0
	}
	returned = true
	return veto
}

//export sg_go_rollback
func sg_go_rollback(token C.uintptr_t) {
	cb := cgo.Handle(token).Value().(*callback)
	returned := false
	defer cb.conn.capturePanic(&returned)
	if !cb.conn.panicked {
		cb.fn.(RollbackHook)()
	}
	returned = true
}

//export sg_go_update
func sg_go_update(token C.uintptr_t, op C.int, schema, table *C.char, rowid C.sqlite3_int64) {
	cb := cgo.Handle(token).Value().(*callback)
	returned := false
	defer cb.conn.capturePanic(&returned)
	if !cb.conn.panicked {
		cb.fn.(UpdateHook)(UpdateEvent{Operation: Operation(op), Schema: C.GoString(schema), Table: C.GoString(table), RowID: int64(rowid)})
	}
	returned = true
}

//export sg_go_authorize
func sg_go_authorize(token C.uintptr_t, action C.int, arg1, arg2, schema, origin *C.char) (result C.int) {
	result = C.SQLITE_DENY
	cb := cgo.Handle(token).Value().(*callback)
	returned := false
	defer cb.conn.capturePanic(&returned)
	if !cb.conn.panicked {
		result = C.int(cb.fn.(Authorizer)(Authorization{Action: AuthorizerAction(action), Arg1: optionalText(arg1), Arg2: optionalText(arg2), Schema: optionalText(schema), Origin: optionalText(origin)}))
	}
	returned = true
	return result
}
