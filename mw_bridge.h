#ifndef SQLITE_GO_MULTIWRITER_BRIDGE_H
#define SQLITE_GO_MULTIWRITER_BRIDGE_H

#include "sqlite3.h"

int sg_multiwriter_bootstrap(void);
int sg_multiwriter_validate_standard(sqlite3 *db);
int sg_multiwriter_validate_writable_private(sqlite3 *db);

#endif
