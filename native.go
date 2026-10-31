package sqlite

/*
#cgo CFLAGS: -std=c11 -O2 -DNDEBUG=1 -DSQLITE_THREADSAFE=2 -DSQLITE_DEFAULT_MEMSTATUS=0 -DSQLITE_ENABLE_API_ARMOR=1
#cgo CFLAGS: -DSQLITE_ENABLE_SESSION=1 -DSQLITE_ENABLE_PREUPDATE_HOOK=1 -DSQLITE_ENABLE_FTS3=1 -DSQLITE_ENABLE_FTS3_PARENTHESIS=1 -DSQLITE_ENABLE_FTS4=1 -DSQLITE_ENABLE_FTS5=1 -DSQLITE_ENABLE_RTREE=1 -DSQLITE_ENABLE_GEOPOLY=1 -DSQLITE_ENABLE_STAT4=1 -DSQLITE_SOUNDEX=1
#cgo CFLAGS: -DSQLITE_OMIT_LOAD_EXTENSION=1 -DSQLITE_OMIT_DEPRECATED=1 -DSQLITE_OMIT_PROGRESS_CALLBACK=1 -DSQLITE_OMIT_SHARED_CACHE=1
#cgo linux darwin LDFLAGS: -lm
#include "bridge.h"
*/
import "C"

// Version reports the linked engine's version without initializing SQLite.
func Version() string { return C.GoString(C.sqlite3_libversion()) }

// SourceID reports the linked engine's source identity without initializing SQLite.
func SourceID() string { return C.GoString(C.sqlite3_sourceid()) }

// CompileOptions returns a fresh copy of the linked engine's actual options.
func CompileOptions() []string {
	var out []string
	for i := C.int(0); ; i++ {
		p := C.sqlite3_compileoption_get(i)
		if p == nil {
			return out
		}
		out = append(out, C.GoString(p))
	}
}
