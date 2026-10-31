#include "bridge.h"

static sg_error code_error(int rc) {
    if (rc == SQLITE_OK || rc == SQLITE_ROW || rc == SQLITE_DONE)
        return (sg_error){0, -1, 0};
    return (sg_error){rc, -1, 0};
}
/* db must be non-NULL and the reported operation must have established its
   message. Matching connection/result codes do not prove that provenance. */
sg_error sg_error_db(sqlite3 *db, int rc) {
    sg_error e = code_error(rc);
    if (e.code) e.message = sqlite3_mprintf("%s", sqlite3_errmsg(db));
    return e;
}
/* v3 preparation and saved-SQL execution establish compiler diagnostics for
   the ERROR family. Early TOOBIG/LOCKED and API-exit NOMEM may retain an old
   offset; VM errors clear it in sqlite3VdbeTransferError. */
static sg_error sql_error(sqlite3 *db, int rc) {
    if (rc == SQLITE_MISUSE) return code_error(rc);
    int offset = (rc & 255) == SQLITE_ERROR ? sqlite3_error_offset(db) : -1;
    sg_error e = sg_error_db(db, rc);
    e.offset = offset;
    return e;
}
sg_prepare_result sg_prepare(sqlite3 *db, const char *sql, int n, unsigned int flags) {
    sg_prepare_result r = {0};
    const char *tail = sql;
    int rc = sqlite3_prepare_v3(db, sql, n+1, flags, &r.stmt, &tail);
    r.tail = (int)(tail - sql);
    r.error = sql_error(db, rc);
    if (rc == SQLITE_OK && r.stmt) {
        r.columns = sqlite3_column_count(r.stmt);
        r.parameters = sqlite3_bind_parameter_count(r.stmt);
    }
    return r;
}
sg_error sg_script(sqlite3 *db, const char *sql, int n) {
    const char *p = sql, *end = sql + n;
    sg_error e = {0, -1, 0};
    while (p < end) {
        sqlite3_stmt *stmt = 0;
        const char *tail = p;
        int rc = sqlite3_prepare_v3(db, p, (int)(end-p)+1, 0, &stmt, &tail);
        if (rc == SQLITE_OK && stmt) {
            do { rc = sqlite3_step(stmt); } while (rc == SQLITE_ROW);
            if (rc == SQLITE_DONE) rc = SQLITE_OK;
        }
        if (rc != SQLITE_OK) {
            e = sql_error(db, rc);
            if (e.offset >= 0) e.offset += (int)(p-sql);
            if (stmt) sqlite3_finalize(stmt);
            return e;
        }
        if (stmt) {
            rc = sqlite3_finalize(stmt);
            if (rc != SQLITE_OK) {
                e = code_error(rc);
                return e;
            }
        }
        if (tail <= p) break;
        p = tail;
    }
    return e;
}
sg_error sg_transaction(sqlite3 *db, int operation) {
    switch (operation) {
    case SG_TX_BEGIN: return sg_script(db, "BEGIN", sizeof("BEGIN") - 1);
    case SG_TX_IMMEDIATE: return sg_script(db, "BEGIN IMMEDIATE", sizeof("BEGIN IMMEDIATE") - 1);
    case SG_TX_EXCLUSIVE: return sg_script(db, "BEGIN EXCLUSIVE", sizeof("BEGIN EXCLUSIVE") - 1);
    case SG_TX_COMMIT: return sg_script(db, "COMMIT", sizeof("COMMIT") - 1);
    case SG_TX_ROLLBACK: return sg_script(db, "ROLLBACK", sizeof("ROLLBACK") - 1);
    default: return code_error(SQLITE_MISUSE);
    }
}
sg_step_result sg_step(sqlite3_stmt *stmt) {
    sg_step_result r = {0};
    r.code = sqlite3_step(stmt);
    r.columns = sqlite3_column_count(stmt);
    r.error = sql_error(sqlite3_db_handle(stmt), r.code);
    return r;
}
sg_exec_result sg_exec(sqlite3_stmt *stmt) {
    sg_exec_result r = {0};
    sqlite3 *db = sqlite3_db_handle(stmt);
    int rc;
    do { rc = sqlite3_step(stmt); } while (rc == SQLITE_ROW);
    r.error = sql_error(db, rc);
    int reset = sqlite3_reset(stmt);
    /* SQLite repeats the step result from reset. Do not duplicate it.
       A distinct cleanup failure has no independently captured message. */
    if (reset != SQLITE_OK && (r.error.code == 0 || reset != r.error.code))
        r.reset_error = code_error(reset);
    r.columns = sqlite3_column_count(stmt);
    if (r.error.code == 0 && r.reset_error.code == 0) {
        r.changes = sqlite3_changes64(db);
        r.rowid = sqlite3_last_insert_rowid(db);
    }
    return r;
}
/* Failed reprepare of an already-expired READY statement can leave pc<0.
   Reset/finalize then return its saved result without transferring its
   message (sqlite3VdbeReset). No fresh connection message is guaranteed. */
sg_error sg_reset(sqlite3_stmt *stmt) {
    return code_error(sqlite3_reset(stmt));
}
/* Only used when no Go callbacks or sessions are installed. Otherwise Go must
   observe a reset callback panic before deciding whether to clear bindings. */
sg_reset_result sg_reset_clear(sqlite3_stmt *stmt) {
    sg_reset_result r;
    r.reset_code = sg_reset(stmt).code;
    r.clear_code = sqlite3_clear_bindings(stmt);
    return r;
}
sg_error sg_finalize(sqlite3_stmt *stmt) {
    return code_error(sqlite3_finalize(stmt));
}
sg_error sg_close(sqlite3 *db) {
    int rc = sqlite3_close(db);
    return rc == SQLITE_BUSY ? sg_error_db(db, rc) : code_error(rc);
}
sg_backup_result sg_backup_init(sqlite3 *dst, const char *dst_schema,
                                sqlite3 *src, const char *src_schema) {
    sg_backup_result r = {0};
    r.backup = sqlite3_backup_init(dst, dst_schema, src, src_schema);
    if (!r.backup)
        r.error = sg_error_db(dst, sqlite3_extended_errcode(dst));
    return r;
}
sg_backup_progress sg_backup_status(sqlite3_backup *backup) {
    return (sg_backup_progress){sqlite3_backup_remaining(backup),
                               sqlite3_backup_pagecount(backup)};
}
sg_error sg_backup_finish(sqlite3 *dst, sqlite3_backup *backup) {
    int rc = sqlite3_backup_finish(backup);
    return sg_error_db(dst, rc);
}
sg_blob_result sg_blob_open(sqlite3 *db, const char *schema, const char *table,
                            const char *column, sqlite3_int64 rowid, int writable) {
    sg_blob_result r = {0};
    int rc = sqlite3_blob_open(db, schema, table, column, rowid, writable, &r.blob);
    r.error = rc == SQLITE_MISUSE ? code_error(rc) : sg_error_db(db, rc);
    if (rc == SQLITE_OK) r.size = sqlite3_blob_bytes(r.blob);
    return r;
}
sg_blob_result sg_blob_reopen(sqlite3 *db, sqlite3_blob *blob, sqlite3_int64 rowid) {
    sg_blob_result r = {0};
    int rc = sqlite3_blob_reopen(blob, rowid);
    /* An already-invalidated handle returns ABORT without refreshing db.
       Other failures install the message from blobSeekToRow. */
    r.error = rc == SQLITE_ABORT || rc == SQLITE_MISUSE
        ? code_error(rc) : sg_error_db(db, rc);
    if (rc == SQLITE_OK) r.size = sqlite3_blob_bytes(blob);
    return r;
}
sg_error sg_blob_io(sqlite3 *db, sqlite3_blob *blob, void *p, int n, int offset, int write) {
    int rc = write ? sqlite3_blob_write(blob, p, n, offset)
                   : sqlite3_blob_read(blob, p, n, offset);
    return rc == SQLITE_MISUSE ? code_error(rc) : sg_error_db(db, rc);
}
sg_error sg_blob_close(sqlite3 *db, sqlite3_blob *blob) {
    int rc = sqlite3_blob_close(blob);
    return rc == SQLITE_MISUSE ? code_error(rc) : sg_error_db(db, rc);
}
int sg_bind_text(sqlite3_stmt *stmt, int i, const char *p, sqlite3_uint64 n) {
    static const char empty = 0;
    return sqlite3_bind_text64(stmt, i, p ? p : &empty, n, SQLITE_TRANSIENT, SQLITE_UTF8);
}
int sg_bind_blob(sqlite3_stmt *stmt, int i, const void *p, sqlite3_uint64 n) {
    static const char empty = 0;
    return sqlite3_bind_blob64(stmt, i, p ? p : &empty, n, SQLITE_TRANSIENT);
}
sg_column_result sg_column(sqlite3_stmt *stmt, int i, int original, int mode) {
    sg_column_result r = {0};
    r.kind = original ? original : sqlite3_column_type(stmt, i);
    if (r.kind == SQLITE_NULL || mode == 0) return r;
    if (mode == SG_READ_ORIGINAL) mode = r.kind;
    switch (mode) {
    case 1: r.integer = sqlite3_column_int64(stmt, i); break;
    case 2: r.real = sqlite3_column_double(stmt, i); break;
    case 3:
        r.data = sqlite3_column_text(stmt, i);
        /* NULL from text conversion of any non-NULL value is OOM. */
        if (!r.data) { r.code = SQLITE_NOMEM; break; }
        r.length = sqlite3_column_bytes(stmt, i);
        break;
    case 4:
        r.data = sqlite3_column_blob(stmt, i);
        /* Empty TEXT/BLOB legitimately has a NULL blob pointer. Numeric
           stringification cannot be empty; failed expansion becomes NULL.
           Inspect the value before bytes can retry, not stale db errors. */
        if (!r.data && (r.kind == SQLITE_INTEGER || r.kind == SQLITE_FLOAT ||
                       sqlite3_column_type(stmt, i) == SQLITE_NULL)) {
            r.code = SQLITE_NOMEM;
            break;
        }
        r.length = sqlite3_column_bytes(stmt, i);
        break;
    }
    return r;
}

/* SQLite retains only C-owned contexts. The integer token names a cgo.Handle;
   no Go pointer is stored in SQLite or in this allocation. */
extern int sg_go_busy(uintptr_t, int);
extern int sg_go_commit(uintptr_t);
extern void sg_go_rollback(uintptr_t);
extern void sg_go_update(uintptr_t, int, char *, char *, sqlite3_int64);
extern int sg_go_authorize(uintptr_t, int, char *, char *, char *, char *);

sg_callback *sg_callback_new(uintptr_t handle) {
    sg_callback *ctx = sqlite3_malloc64(sizeof(*ctx));
    if (ctx) ctx->handle = handle;
    return ctx;
}
static int sg_busy(void *p, int attempt) {
    return sg_go_busy(((sg_callback *)p)->handle, attempt);
}
static int sg_commit(void *p) {
    return sg_go_commit(((sg_callback *)p)->handle);
}
static void sg_rollback(void *p) {
    sg_go_rollback(((sg_callback *)p)->handle);
}
static void sg_update(void *p, int op, const char *schema, const char *table, sqlite3_int64 rowid) {
    sg_go_update(((sg_callback *)p)->handle, op, (char *)schema, (char *)table, rowid);
}
static int sg_authorize(void *p, int action, const char *a, const char *b, const char *schema, const char *origin) {
    return sg_go_authorize(((sg_callback *)p)->handle, action, (char *)a, (char *)b, (char *)schema, (char *)origin);
}
int sg_set_callback(sqlite3 *db, int kind, sg_callback *ctx) {
    switch (kind) {
    case 0: return sqlite3_busy_handler(db, ctx ? sg_busy : 0, ctx);
    case 1: sqlite3_commit_hook(db, ctx ? sg_commit : 0, ctx); break;
    case 2: sqlite3_rollback_hook(db, ctx ? sg_rollback : 0, ctx); break;
    case 3: sqlite3_update_hook(db, ctx ? sg_update : 0, ctx); break;
    case 4: return sqlite3_set_authorizer(db, ctx ? sg_authorize : 0, ctx);
    default: return SQLITE_MISUSE;
    }
    return SQLITE_OK;
}

extern int sg_go_input(uintptr_t, void *, int *);
extern int sg_go_output(uintptr_t, void *, int);
extern int sg_go_filter(uintptr_t, char *);
extern int sg_go_conflict(uintptr_t, int, sqlite3_changeset_iter *);
static int sg_input(void *ctx, void *data, int *n) {
    return sg_go_input(((sg_callback *)ctx)->handle, data, n);
}
static int sg_output(void *ctx, const void *data, int n) {
    return sg_go_output(((sg_callback *)ctx)->handle, (void *)data, n);
}
static int sg_filter(void *ctx, const char *table) {
    return sg_go_filter(((sg_callback *)ctx)->handle, (char *)table);
}
static int sg_conflict(void *ctx, int kind, sqlite3_changeset_iter *it) {
    return sg_go_conflict(((sg_callback *)ctx)->handle, kind, it);
}
void sg_session_filter(sqlite3_session *s, sg_callback *ctx) {
    sqlite3session_table_filter(s, ctx ? sg_filter : 0, ctx);
}
int sg_session_output(sqlite3_session *s, uintptr_t token, int patch) {
    sg_callback out = {token};
    return patch ? sqlite3session_patchset_strm(s, sg_output, &out)
                 : sqlite3session_changeset_strm(s, sg_output, &out);
}
int sg_iter_start(sqlite3_changeset_iter **it, sg_callback *in) {
    return sqlite3changeset_start_strm(it, sg_input, in);
}
int sg_apply(sqlite3 *db, uintptr_t input, uintptr_t callback) {
    sg_callback in = {input}, cb = {callback};
    return sqlite3changeset_apply_strm(db, sg_input, &in, sg_filter, sg_conflict, &cb);
}
int sg_invert(uintptr_t input, uintptr_t output) {
    sg_callback in = {input}, out = {output};
    return sqlite3changeset_invert_strm(sg_input, &in, sg_output, &out);
}
int sg_concat(uintptr_t first, uintptr_t second, uintptr_t output) {
    sg_callback a = {first}, b = {second}, out = {output};
    return sqlite3changeset_concat_strm(sg_input, &a, sg_input, &b, sg_output, &out);
}
int sg_group_add(sqlite3_changegroup *g, uintptr_t input) {
    sg_callback in = {input};
    return sqlite3changegroup_add_strm(g, sg_input, &in);
}
int sg_group_output(sqlite3_changegroup *g, uintptr_t output) {
    sg_callback out = {output};
    return sqlite3changegroup_output_strm(g, sg_output, &out);
}
sg_change_info sg_change_op(sqlite3_changeset_iter *it) {
    sg_change_info r = {0};
    r.code = sqlite3changeset_op(it, &r.table, &r.columns, &r.operation, &r.indirect);
    return r;
}
sg_iter_result sg_iter_next(sqlite3_changeset_iter *it) {
    sg_iter_result r = {0};
    r.code = sqlite3changeset_next(it);
    if (r.code == SQLITE_ROW) r.info = sg_change_op(it);
    return r;
}
sg_change_value sg_change_get(sqlite3_changeset_iter *it, int column, int vector) {
    sg_change_value r = {0};
    if (vector == 0) r.code = sqlite3changeset_old(it, column, &r.value);
    else if (vector == 1) r.code = sqlite3changeset_new(it, column, &r.value);
    else r.code = sqlite3changeset_conflict(it, column, &r.value);
    return r;
}
sg_change_pk sg_change_primary_key(sqlite3_changeset_iter *it) {
    sg_change_pk r = {0};
    r.code = sqlite3changeset_pk(it, &r.mask, &r.columns);
    return r;
}
sg_column_result sg_value(sqlite3_value *v, int original, int mode) {
    sg_column_result r = {0};
    if (!v) return r;
    r.kind = original ? original : sqlite3_value_type(v);
    if (r.kind == SQLITE_NULL || mode == 0) return r;
    if (mode == SG_READ_ORIGINAL) mode = r.kind;
    switch (mode) {
    case 1: r.integer = sqlite3_value_int64(v); break;
    case 2: r.real = sqlite3_value_double(v); break;
    case 3:
        r.data = sqlite3_value_text(v);
        if (!r.data) { r.code = SQLITE_NOMEM; break; }
        r.length = sqlite3_value_bytes(v);
        break;
    case 4:
        r.data = sqlite3_value_blob(v);
        /* Empty TEXT/BLOB legitimately has a NULL blob pointer. Numeric
           stringification cannot be empty; failed expansion becomes NULL.
           Check before bytes, which can retry a failed conversion. */
        if (!r.data && (r.kind == SQLITE_INTEGER || r.kind == SQLITE_FLOAT ||
                       sqlite3_value_type(v) == SQLITE_NULL)) {
            r.code = SQLITE_NOMEM;
            break;
        }
        r.length = sqlite3_value_bytes(v);
        break;
    }
    return r;
}
