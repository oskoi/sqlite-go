// Copyright (c) 2026 oskoi and sqlite-go contributors.
// SPDX-License-Identifier: Apache-2.0

// Package sqlite provides typed access to a vendored native SQLite engine.
// It requires Go 1.27, cgo and a C compiler. It does not use database/sql.
//
// Open returns a pointer-owned Conn. Do not copy native handles after
// construction. A connection and its children have one simultaneous owner;
// ownership may move between goroutines without runtime.LockOSThread.
// Independent owners may run concurrently. Close children before their
// parent. Conn.Close returns CodeBusy while children remain and leaves the
// connection usable. Close is idempotent; no finalizer releases resources.
//
// OpenMultiwriter requires the sqlite_multiwriter build tag and a POSIX target
// (Go's unix constraint); otherwise, including on Windows with the tag, it
// returns ErrMultiwriterUnavailable without touching the pathname. Ordinary
// SQLite remains available on Windows. The explicit opener uses writable
// on-disk databases with independent private lanes, read validation, and no rebase.
// Open returns a standard main database, rejecting actual private or tracking
// lanes. Both constructors validate main once after native opening; rejection
// can follow filesystem changes. There is no mode query or cached wrapper mode.
// These guarantees do not cover databases later selected through ATTACH.
// BEGIN IMMEDIATE does not serialize writers under Multiwriter. Preserve its
// sidecars and reopen through OpenMultiwriter: successful Close does not make
// standard-SQLite reopening or main-file-only backup safe.
// SQLite/VFS bootstrap failures are process-sticky. Upstream connection-hook
// registration happens during private opening; its failure rejects that open
// and can be retried by a later open, without undoing all filesystem effects.
//
// Prepare compiles the first SQL statement and returns its remaining tail.
// ExecScript executes complete scripts, including triggers, without adding a
// transaction. Both apply SQL length limits per statement, not to the full input.
// Bind parameters only in Ready, Step through rows until DONE,
// then Reset before reuse. Step after DONE does not restart. Reset retains
// bindings and restores Ready even when reporting an earlier native error.
// Stmt.Exec drains rows and resets in one native call, retaining bindings.
// ResetClearBindings also clears parameters and returns the two operation errors
// separately. A reset callback panic propagates before bindings are cleared.
//
// BindText and BindBlob copy inputs during the call. Nil blobs mean SQL NULL;
// non-nil empty blobs remain BLOB. ColumnText and ColumnBlob copy output into
// Go-owned memory. ColumnTextView and ColumnBlobView borrow read-only SQLite
// memory. Do not mutate a blob view or retain either view after Step, Reset,
// Close, or conversion of that column's representation. ColumnType preserves
// the row's original storage class even after conversion.
// ColumnValue returns that original kind and an owned value in one native call;
// like Value.Copy, earlier coercion can affect the copied representation.
//
// SetBusyHandler, SetCommitHook, SetRollbackHook, SetUpdateHook and SetAuthorizer
// install synchronous native hooks; nil removes them. Busy handlers return true
// to retry, commit hooks return true to veto. Callback event strings are copied.
// Callbacks, Readers and Writers cannot re-enter participating owners or
// children (ErrReentrant); borrowed conflict inspection and independent owners
// remain usable. After a callback panic the wrapper suppresses subsequent user
// callbacks, finishes native and Go cleanup, unregisters all managed connection
// hooks and session filters, then re-panics with the same value. Sessions remain
// open; reinstall registrations explicitly. Ordinary errors retain them.
// Notification/filter panics do not promise rollback. An enclosing caller
// transaction can provide that policy.
//
// Interrupt is the cross-goroutine exception to single-owner access. It protects
// native lifetime against Close but does not identify an application request or
// promise to wake busy sleeps or Go callbacks. Stop and join external cancellation
// watchers before reusing the connection. The library starts no watcher.
// BusyTimeout replaces the managed busy handler and rounds positive durations up
// to milliseconds; nonpositive durations disable waiting.
//
// Sessions record native changes without owning transactions. Attach names must
// be nonempty; AttachAll selects SQLite's automatic attachment. Tables need
// primary keys; SQLite omits rows with NULL key components. WriteChangeset and
// WritePatchset stream without resetting the session. ApplyChangeset uses the
// native savepoint, main schema and compatibility/filter semantics. A nil
// conflict handler explicitly aborts conflicts.
//
// ChangesetIter, transforms and change groups stream caller I/O without closing
// it or copying every chunk through Go buffers. Readers/Writers must not retain
// callback buffers. Invalid counts, short writes and 100 consecutive no-progress
// reads produce errors. Original Go errors remain discoverable with errors.Is.
// Only exact io.EOF means clean termination; wrapped or joined EOF remains a
// failure even when errors.Is reports EOF. Apply rejects its paired bytes too.
// Iterator/transform paired data arrives before its error; buffered rows remain
// usable, and completion/Close surfaces pending non-EOF errors. Apply rejects
// non-EOF errors and their paired bytes immediately so the native savepoint
// rolls back before return. Output can contain an unusable prefix on failure.
//
// Change, Conflict and Value are copyable borrowed references checked against
// their owner's generation. Next, Close and conflict return expire them with
// ErrExpired. Value distinguishes Missing from NULL and shares original-kind
// caching across conversions and refetches. Text, Blob and Copy own results.
// Copy uses the original kind and current native converted value.
//
// Ordinary iterator failures permit only Close. Reader panic consumes/finalizes
// an iterator before re-panic; Close afterward is harmless. Native Changegroup
// Add failure/panic leaves only Close valid, while preentry rejection does not.
// Failed WriteTo/panic preserves a group and counts partial accepted output.
//
// NewBackup copies an explicit schema between different open connections and
// retains both parents until Close. Its destination is reserved, including old
// children and Interrupt, until native finish; other access returns CodeBusy.
// Serialize backup calls with source operations; the source is usable between
// steps. Negative page counts copy all, zero follows SQLite, and DONE is sticky.
// Progress remains available after DONE/fatal failure. BUSY/LOCKED are retryable
// without wrapper retries; fatal native errors leave only Progress and Close.
// Close consumes even on error and preserves native early-abort rollback.
// A callback panic finishes/consumes the backup before unregistering hooks and
// session filters from both owners and re-panicking; already committed native
// effects are not undone. A later Close is harmless.
//
// OpenBlob retains its parent for fixed-size incremental BLOB/TEXT I/O. Read
// advances sequential position and returns EOF only when no bytes remain;
// ReadAt returns EOF on a short read. Positional I/O preserves that position.
// Zero reads succeed on an open, non-aborted handle. Seek stays within the cell;
// oversized writes return ErrBlobFull without changing a prefix. Caller buffers
// are borrowed only synchronously; ReadAt/WriteAt do not permit concurrent use.
// Reopen refreshes size and resets position. Failed native Reopen or observed
// read/write SQLITE_ABORT permanently aborts: only Size (zero), RowID (last
// successful row), and Close remain usable. Reopen before observed I/O failure
// can recover an unseen row-update expiration. Close is explicit and consumes
// even on commit failure or callback panic. Constructor panic closes any native
// blob before callback cleanup and re-panic; no Go panic crosses C.
// Successful sequential I/O advances position before callback cleanup re-panics;
// recovering callers can continue at the next byte. Positional I/O keeps position
// unchanged, including after a callback panic. Recovery adds no rollback policy.
//
// Native failures retain *Error with the returned Code and SQL byte Offset
// (-1 when unavailable). Messages use operation-owned native diagnostics when
// available, otherwise the code's static description. Stmt.Reset and Stmt.Close
// report saved result codes with static messages and offset -1; retain the
// original Prepare/Step/Exec error for detailed diagnostics.
// errors.Is(err, CodeConstraint) matches a whole primary family;
// errors.Is(err, CodeConstraintUnique) matches only that extended result.
// Wrapper misuse returns distinct ErrClosed, ErrState, ErrRange and related
// sentinels. Error snapshots do not append SQL or bound values.
package sqlite
