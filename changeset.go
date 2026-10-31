package sqlite

/*
#include "bridge.h"
*/
import "C"

import (
	"errors"
	"io"
	"runtime/cgo"
	"unsafe"
)

// ConflictType identifies why SQLite could not apply a change.
type ConflictType int32

// ConflictAction chooses native conflict resolution.
type ConflictAction int32

const (
	ConflictData       ConflictType   = C.SQLITE_CHANGESET_DATA
	ConflictNotFound   ConflictType   = C.SQLITE_CHANGESET_NOTFOUND
	ConflictConflict   ConflictType   = C.SQLITE_CHANGESET_CONFLICT
	ConflictConstraint ConflictType   = C.SQLITE_CHANGESET_CONSTRAINT
	ConflictForeignKey ConflictType   = C.SQLITE_CHANGESET_FOREIGN_KEY
	Abort              ConflictAction = C.SQLITE_CHANGESET_ABORT
	Omit               ConflictAction = C.SQLITE_CHANGESET_OMIT
	Replace            ConflictAction = C.SQLITE_CHANGESET_REPLACE
)

// ConflictHandler inspects a borrowed conflict and resolves it or returns an error.
type ConflictHandler func(Conflict) (ConflictAction, error)

// ApplyOptions supplies native table filtering and conflict resolution.
// Nil Filter includes all tables; nil Conflict aborts any conflict.
type ApplyOptions struct {
	Filter   func(table string) bool
	Conflict ConflictHandler
}

// ChangeInfo owns metadata for one changed row.
type ChangeInfo struct {
	Table     string
	Columns   int
	Operation Operation
	Indirect  bool
}

type changePosition struct {
	iter       *C.sqlite3_changeset_iter
	generation uint64
	valid      bool
	columns    int
	operation  Operation
	table      *C.char
	indirect   bool
	conflict   ConflictType
	kinds      []columnKind
}

func (p *changePosition) check(generation uint64) error {
	if p == nil || !p.valid || p.generation != generation {
		return ErrExpired
	}
	return nil
}
func (p *changePosition) invalidate() { p.valid = false; p.generation++; p.iter = nil; p.table = nil }
func (p *changePosition) start(iter *C.sqlite3_changeset_iter, conflict ConflictType, info C.sg_change_info) error {
	p.iter, p.conflict = iter, conflict
	if conflict != ConflictForeignKey {
		if info.code != C.SQLITE_OK {
			return codeError(info.code, "Change.Info")
		}
		p.columns, p.operation, p.table, p.indirect = int(info.columns), Operation(info.operation), (*C.char)(unsafe.Pointer(info.table)), info.indirect != 0
	}
	p.valid = true
	return nil
}

// Change is a copyable view of the current iterator or conflict position.
type Change struct {
	position   *changePosition
	generation uint64
}

// Info returns owned metadata. The table name is copied.
func (ch Change) Info() (ChangeInfo, error) {
	if err := ch.position.check(ch.generation); err != nil {
		return ChangeInfo{}, err
	}
	p := ch.position
	return ChangeInfo{Table: C.GoString(p.table), Columns: p.columns, Operation: p.operation, Indirect: p.indirect}, nil
}

// PrimaryKey returns an owned full-width primary-key mask.
func (ch Change) PrimaryKey() ([]bool, error) {
	if err := ch.position.check(ch.generation); err != nil {
		return nil, err
	}
	r := C.sg_change_primary_key(ch.position.iter)
	if r.code != C.SQLITE_OK {
		return nil, codeError(r.code, "Change.PrimaryKey")
	}
	out := make([]bool, int(r.columns))
	mask := unsafe.Slice((*byte)(unsafe.Pointer(r.mask)), int(r.columns))
	for i, b := range mask {
		out[i] = b != 0
	}
	return out, nil
}
func (ch Change) get(column, vector int) (Value, error) {
	if err := ch.position.check(ch.generation); err != nil {
		return Value{}, err
	}
	p := ch.position
	if vector == 0 && p.operation != OperationDelete && p.operation != OperationUpdate || vector == 1 && p.operation != OperationInsert && p.operation != OperationUpdate {
		return Value{}, ErrState
	}
	if column < 0 || column >= p.columns {
		return Value{}, ErrRange
	}
	r := C.sg_change_get(p.iter, C.int(column), C.int(vector))
	if r.code != C.SQLITE_OK {
		return Value{}, codeError(r.code, "Change.Value")
	}
	if len(p.kinds) < 3*p.columns {
		p.kinds = make([]columnKind, 3*p.columns)
	}
	return Value{position: p, generation: ch.generation, vector: vector, column: column, value: r.value}, nil
}

// Old returns an old value for UPDATE or DELETE, possibly KindMissing.
func (ch Change) Old(column int) (Value, error) { return ch.get(column, 0) }

// New returns a new value for INSERT or UPDATE, possibly KindMissing.
func (ch Change) New(column int) (Value, error) { return ch.get(column, 1) }

// Conflict is a copyable view that expires when the handler returns.
// Only Type and ForeignKeyConflicts are valid for foreign-key conflicts.
type Conflict struct {
	position   *changePosition
	generation uint64
}

// Type reports the native conflict type.
func (cf Conflict) Type() (ConflictType, error) {
	if err := cf.position.check(cf.generation); err != nil {
		return 0, err
	}
	return cf.position.conflict, nil
}

// Change returns the incoming change, except for foreign-key conflicts.
func (cf Conflict) Change() (Change, error) {
	if err := cf.position.check(cf.generation); err != nil {
		return Change{}, err
	}
	if cf.position.conflict == ConflictForeignKey {
		return Change{}, ErrState
	}
	return Change{cf.position, cf.generation}, nil
}

// Current returns a value from the conflicting row for DATA or CONFLICT only.
func (cf Conflict) Current(column int) (Value, error) {
	if err := cf.position.check(cf.generation); err != nil {
		return Value{}, err
	}
	if cf.position.conflict != ConflictData && cf.position.conflict != ConflictConflict {
		return Value{}, ErrState
	}
	return (Change{cf.position, cf.generation}).get(column, 2)
}

// ForeignKeyConflicts reports the violation count only for FOREIGN_KEY.
func (cf Conflict) ForeignKeyConflicts() (int, error) {
	if err := cf.position.check(cf.generation); err != nil {
		return 0, err
	}
	if cf.position.conflict != ConflictForeignKey {
		return 0, ErrState
	}
	var n C.int
	rc := C.sqlite3changeset_fk_conflicts(cf.position.iter, &n)
	return int(n), codeError(rc, "Conflict.ForeignKeyConflicts")
}

const (
	iterBefore = iota
	iterCurrent
	iterDone
	iterFailed
)

// ChangesetIter owns a streaming iterator, not its Reader. Close explicitly.
// Reader panics consume the iterator before propagating; ordinary failures do not.
type ChangesetIter struct {
	_        noCopy
	iter     *C.sqlite3_changeset_iter
	input    *stream
	panic    callPanic
	active   bool
	state    int
	position changePosition
}

// NewChangesetIter creates an iterator without materializing the Reader.
func NewChangesetIter(r io.Reader) (*ChangesetIter, error) {
	it := &ChangesetIter{}
	in, err := inputStream(r, &it.panic, false)
	if err != nil {
		return nil, err
	}
	in.ctx = C.sg_callback_new(C.uintptr_t(in.handle))
	if in.ctx == nil {
		in.close()
		return nil, codeError(C.SQLITE_NOMEM, "Stream.Context")
	}
	it.input = in
	it.active = true
	rc := C.sg_iter_start(&it.iter, in.ctx)
	it.active = false
	err = streamResult(rc, "NewChangesetIter", in)
	if it.panic.failed() || err != nil {
		if it.iter != nil {
			C.sqlite3changeset_finalize(it.iter)
			it.iter = nil
		}
		in.close()
		it.input = nil
		it.panic.finish()
		return nil, err
	}
	return it, nil
}
func (it *ChangesetIter) check() error {
	if it == nil {
		return ErrClosed
	}
	if it.active {
		return ErrReentrant
	}
	if it.iter == nil {
		return ErrClosed
	}
	return nil
}

// Next invalidates earlier views before advancing. DONE is sticky; failures
// permit only Close. A buffered row remains usable despite a pending read error.
func (it *ChangesetIter) Next() (bool, error) {
	if err := it.check(); err != nil {
		return false, err
	}
	if it.state == iterFailed {
		return false, ErrState
	}
	if it.state == iterDone {
		return false, nil
	}
	it.position.invalidate()
	it.active = true
	r := C.sg_iter_next(it.iter)
	if it.panic.failed() {
		C.sqlite3changeset_finalize(it.iter)
		it.iter = nil
		it.input.close()
		it.input = nil
		it.active = false
		it.panic.finish()
	}
	it.active = false
	if r.code == C.SQLITE_ROW {
		if err := it.position.start(it.iter, 0, r.info); err != nil {
			it.state = iterFailed
			return false, err
		}
		it.state = iterCurrent
		return true, nil
	}
	err := streamResult(r.code, "ChangesetIter.Next", it.input)
	if err != nil {
		it.state = iterFailed
		return false, err
	}
	it.state = iterDone
	return false, nil
}

// Change borrows the current position.
func (it *ChangesetIter) Change() (Change, error) {
	if err := it.check(); err != nil {
		return Change{}, err
	}
	if it.state != iterCurrent {
		return Change{}, ErrState
	}
	return Change{&it.position, it.position.generation}, nil
}

// Close finalizes once, expires all views, and reports pending non-EOF errors.
func (it *ChangesetIter) Close() error {
	if it == nil {
		return nil
	}
	if it.active {
		return ErrReentrant
	}
	if it.iter == nil {
		return nil
	}
	it.active = true
	it.position.invalidate()
	rc := C.sqlite3changeset_finalize(it.iter)
	it.iter = nil
	err := streamResult(rc, "ChangesetIter.Close", it.input)
	it.input.close()
	it.input = nil
	it.active = false
	it.panic.finish()
	return err
}

type applyState struct {
	panic    callPanic
	input    stream
	options  ApplyOptions
	position changePosition
	err      error
}

// ApplyChangeset applies to main using SQLite's default savepoint. Returned
// input/conflict errors abort natively. Filter/notification panics do not promise
// rollback: use an enclosing caller transaction when panic rollback is required.
func (c *Conn) ApplyChangeset(r io.Reader, options ApplyOptions) error {
	if err := c.check(); err != nil {
		return err
	}
	state := &applyState{panic: callPanic{conn: c}, options: options}
	in := &state.input
	if err := in.initInput(r, &state.panic, true); err != nil {
		return err
	}
	defer in.close()
	handle := cgo.NewHandle(state)
	defer handle.Delete()
	c.active = true
	defer c.endCall()
	rc := C.sg_apply(c.db, C.uintptr_t(in.handle), C.uintptr_t(handle))
	err := errors.Join(in.failure(), state.err)
	if rc == C.SQLITE_OK || rc == C.SQLITE_DONE || rc == C.SQLITE_ROW {
		return err
	}
	if err != nil && (Code(rc).Primary() == CodeIOErr || Code(rc).Primary() == CodeAbort) {
		return err
	}
	var native error
	if Code(rc).Primary() == CodeNoMem {
		// Iterator allocation can fail before Apply refreshes db diagnostics.
		native = codeError(rc, "ApplyChangeset")
	} else {
		// All other failures pass through Apply's final sqlite3_set_errmsg,
		// which restores its operation-owned message after internal cleanup.
		native = dbError(c.db, rc, "ApplyChangeset")
	}
	if err == nil {
		return native
	}
	return errors.Join(err, native)
}

//export sg_go_conflict
func sg_go_conflict(token C.uintptr_t, kind C.int, iter *C.sqlite3_changeset_iter) (action C.int) {
	action = C.SQLITE_CHANGESET_ABORT
	s := cgo.Handle(token).Value().(*applyState)
	returned := false
	defer s.panic.capture(&returned)
	if s.panic.failed() || s.err != nil || s.options.Conflict == nil {
		returned = true
		return action
	}
	p := &s.position
	p.invalidate()
	defer p.invalidate()
	var info C.sg_change_info
	if ConflictType(kind) != ConflictForeignKey {
		info = C.sg_change_op(iter)
	}
	if err := p.start(iter, ConflictType(kind), info); err != nil {
		s.err = err
		returned = true
		return action
	}
	result, err := s.options.Conflict(Conflict{p, p.generation})
	if err != nil {
		s.err = err
		returned = true
		return action
	}
	if result != Abort && result != Omit && result != Replace || result == Replace && ConflictType(kind) != ConflictData && ConflictType(kind) != ConflictConflict {
		s.err = ErrInvalidArgument
		returned = true
		return action
	}
	returned = true
	return C.int(result)
}

// InvertChangeset streams SQLite's inverse. Failed output may contain a prefix.
func InvertChangeset(w io.Writer, r io.Reader) error {
	op := &struct {
		panic   callPanic
		in, out stream
	}{}
	defer op.panic.finish()
	in, out := &op.in, &op.out
	if err := in.initInput(r, &op.panic, false); err != nil {
		return err
	}
	defer in.close()
	if err := out.initOutput(w, &op.panic); err != nil {
		return err
	}
	defer out.close()
	return streamResult(C.sg_invert(C.uintptr_t(in.handle), C.uintptr_t(out.handle)), "InvertChangeset", in, out)
}

// ConcatChangesets streams the native composition of first followed by second.
// Caller I/O is never closed. Failed output is not transactional.
func ConcatChangesets(w io.Writer, first, second io.Reader) error {
	op := &struct {
		panic     callPanic
		a, b, out stream
	}{}
	defer op.panic.finish()
	a, b, out := &op.a, &op.b, &op.out
	if err := a.initInput(first, &op.panic, false); err != nil {
		return err
	}
	defer a.close()
	if err := b.initInput(second, &op.panic, false); err != nil {
		return err
	}
	defer b.close()
	if err := out.initOutput(w, &op.panic); err != nil {
		return err
	}
	defer out.close()
	return streamResult(C.sg_concat(C.uintptr_t(a.handle), C.uintptr_t(b.handle), C.uintptr_t(out.handle)), "ConcatChangesets", a, b, out)
}
