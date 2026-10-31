package sqlite

/*
#include "bridge.h"
*/
import "C"

import "io"

// Changegroup combines changes using SQLite's native change-group operations.
// Do not copy it. After a failed native Add only Close is valid.
type Changegroup struct {
	_      noCopy
	group  *C.sqlite3_changegroup
	active bool
	failed bool
	panic  callPanic
}

// NewChangegroup creates an empty native group.
func NewChangegroup() (*Changegroup, error) {
	var ptr *C.sqlite3_changegroup
	rc := C.sqlite3changegroup_new(&ptr)
	if rc != C.SQLITE_OK {
		return nil, codeError(rc, "NewChangegroup")
	}
	return &Changegroup{group: ptr}, nil
}
func (g *Changegroup) check() error {
	if g == nil {
		return ErrClosed
	}
	if g.active {
		return ErrReentrant
	}
	if g.group == nil {
		return ErrClosed
	}
	if g.failed {
		return ErrState
	}
	return nil
}

// Add streams another changeset or patchset. Once native addition starts, any
// input/native error or panic leaves only Close valid. Preentry errors do not.
func (g *Changegroup) Add(r io.Reader) error {
	if err := g.check(); err != nil {
		return err
	}
	in, err := inputStream(r, &g.panic, false)
	if err != nil {
		return err
	}
	defer in.close()
	g.active = true
	rc := C.sg_group_add(g.group, C.uintptr_t(in.handle))
	err = streamResult(rc, "Changegroup.Add", in)
	g.failed = err != nil || g.panic.failed()
	g.active = false
	g.panic.finish()
	return err
}

// WriteTo streams the group's output and counts accepted bytes, including a
// partial write with an error. Failed output may be unusable; the group remains
// valid. It never closes w.
func (g *Changegroup) WriteTo(w io.Writer) (int64, error) {
	if err := g.check(); err != nil {
		return 0, err
	}
	out, err := outputStream(w, &g.panic)
	if err != nil {
		return 0, err
	}
	defer out.close()
	g.active = true
	rc := C.sg_group_output(g.group, C.uintptr_t(out.handle))
	err = streamResult(rc, "Changegroup.WriteTo", out)
	g.active = false
	g.panic.finish()
	return out.written, err
}

// Close consumes the group exactly once, including a Failed group.
func (g *Changegroup) Close() error {
	if g == nil {
		return nil
	}
	if g.active {
		return ErrReentrant
	}
	if g.group == nil {
		return nil
	}
	C.sqlite3changegroup_delete(g.group)
	g.group = nil
	return nil
}
