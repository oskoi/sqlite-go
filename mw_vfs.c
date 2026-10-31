//go:build sqlite_multiwriter && unix

#include "mw_bridge.h"
#include "libs/sqlite-multiwriter/src/multiwriter_vfs.c"

int sg_multiwriter_bootstrap(void) {
    int rc = sqlite3_initialize();
    if (rc != SQLITE_OK) return rc;
    return mw_vfs_register(0);
}

static int sg_multiwriter_main_lane(sqlite3 *db, mw_lane **lane) {
    sqlite3_vfs *vfs = NULL;
    sqlite3_file *file = NULL;
    *lane = NULL;
    int rc = sqlite3_file_control(db, "main", SQLITE_FCNTL_VFS_POINTER, &vfs);
    if (rc == SQLITE_NOTFOUND) return SQLITE_OK;
    // A filename or VFS name is not proof of this pinned file representation.
    if (rc != SQLITE_OK || vfs != &mw_vfs) return rc;
    rc = sqlite3_file_control(db, "main", SQLITE_FCNTL_FILE_POINTER, &file);
    if (rc == SQLITE_NOTFOUND) return SQLITE_OK;
    // In-memory and unopened temporary main files have no file methods.
    if (rc != SQLITE_OK || !file || !file->pMethods) return rc;
    *lane = ((mw_file *)file)->lane;
    return SQLITE_OK;
}

int sg_multiwriter_validate_standard(sqlite3 *db) {
    mw_lane *lane;
    int rc = sg_multiwriter_main_lane(db, &lane);
    if (rc != SQLITE_OK) return rc;
    return lane ? SQLITE_MISUSE : SQLITE_OK;
}

int sg_multiwriter_validate_writable_private(sqlite3 *db) {
    mw_lane *lane;
    int rc = sg_multiwriter_main_lane(db, &lane);
    if (rc != SQLITE_OK) return rc;
    // A read-write open can fall back to read-only while retaining its private
    // lane. The explicit constructor requires both the lane and writability.
    if (sqlite3_db_readonly(db, "main") == 1) return SQLITE_READONLY;
    return lane && lane->private_mode ? SQLITE_OK : SQLITE_MISUSE;
}
