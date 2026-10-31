#ifndef SQLITE_GO_BRIDGE_H
#define SQLITE_GO_BRIDGE_H
#include "sqlite3.h"
#include <stdint.h>

typedef struct { uintptr_t handle; } sg_callback;
sg_callback *sg_callback_new(uintptr_t);
int sg_set_callback(sqlite3 *, int, sg_callback *);

typedef struct { int code; int offset; char *message; } sg_error;
typedef struct { int code; int columns; sg_error error; } sg_step_result;
typedef struct { sqlite3_stmt *stmt; int tail; int columns; int parameters; sg_error error; } sg_prepare_result;
typedef struct { sqlite3_int64 changes; sqlite3_int64 rowid; int columns; sg_error error; sg_error reset_error; } sg_exec_result;
typedef struct { int kind; int code; sqlite3_int64 integer; double real; const void *data; int length; } sg_column_result;
enum { SG_READ_ORIGINAL = 5 };
enum { SG_TX_BEGIN, SG_TX_IMMEDIATE, SG_TX_EXCLUSIVE, SG_TX_COMMIT, SG_TX_ROLLBACK };
typedef struct { int reset_code; int clear_code; } sg_reset_result;
/* Requires a non-NULL db and a fresh message from the reported operation.
   The supplied result code is preserved; SQL offsets are not captured. */
sg_error sg_error_db(sqlite3 *, int);
sg_prepare_result sg_prepare(sqlite3 *, const char *, int, unsigned int);
sg_error sg_script(sqlite3 *, const char *, int);
sg_error sg_transaction(sqlite3 *, int);
sg_step_result sg_step(sqlite3_stmt *);
sg_exec_result sg_exec(sqlite3_stmt *);
sg_error sg_reset(sqlite3_stmt *);
sg_reset_result sg_reset_clear(sqlite3_stmt *);
sg_error sg_finalize(sqlite3_stmt *);
sg_error sg_close(sqlite3 *);
int sg_bind_text(sqlite3_stmt *, int, const char *, sqlite3_uint64);
int sg_bind_blob(sqlite3_stmt *, int, const void *, sqlite3_uint64);
sg_column_result sg_column(sqlite3_stmt *, int, int, int);
void sg_session_filter(sqlite3_session *, sg_callback *);
int sg_session_output(sqlite3_session *, uintptr_t, int);
int sg_iter_start(sqlite3_changeset_iter **, sg_callback *);
int sg_apply(sqlite3 *, uintptr_t, uintptr_t);
int sg_invert(uintptr_t, uintptr_t);
int sg_concat(uintptr_t, uintptr_t, uintptr_t);
int sg_group_add(sqlite3_changegroup *, uintptr_t);
int sg_group_output(sqlite3_changegroup *, uintptr_t);
typedef struct { int code; const char *table; int columns; int operation; int indirect; } sg_change_info;
sg_change_info sg_change_op(sqlite3_changeset_iter *);
typedef struct { int code; sg_change_info info; } sg_iter_result;
sg_iter_result sg_iter_next(sqlite3_changeset_iter *);
typedef struct { int code; sqlite3_value *value; } sg_change_value;
sg_change_value sg_change_get(sqlite3_changeset_iter *, int, int);
typedef struct { int code; unsigned char *mask; int columns; } sg_change_pk;
sg_change_pk sg_change_primary_key(sqlite3_changeset_iter *);
sg_column_result sg_value(sqlite3_value *, int, int);
typedef struct { sqlite3_backup *backup; sg_error error; } sg_backup_result;
typedef struct { int remaining; int total; } sg_backup_progress;
sg_backup_result sg_backup_init(sqlite3 *, const char *, sqlite3 *, const char *);
sg_backup_progress sg_backup_status(sqlite3_backup *);
sg_error sg_backup_finish(sqlite3 *, sqlite3_backup *);
typedef struct { sqlite3_blob *blob; int size; sg_error error; } sg_blob_result;
sg_blob_result sg_blob_open(sqlite3 *, const char *, const char *, const char *, sqlite3_int64, int);
sg_blob_result sg_blob_reopen(sqlite3 *, sqlite3_blob *, sqlite3_int64);
sg_error sg_blob_io(sqlite3 *, sqlite3_blob *, void *, int, int, int);
sg_error sg_blob_close(sqlite3 *, sqlite3_blob *);
#endif
