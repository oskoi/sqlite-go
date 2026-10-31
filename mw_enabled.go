//go:build sqlite_multiwriter && unix

package sqlite

/*
#cgo CFLAGS: -I${SRCDIR} -I${SRCDIR}/libs/sqlite-multiwriter/src -pthread
#cgo linux CFLAGS: -D_GNU_SOURCE
#cgo LDFLAGS: -pthread
#include "mw_bridge.h"
*/
import "C"

import (
	"errors"
	"fmt"
	"net/url"
	"strings"
	"sync"
)

var multiwriterBootstrap struct {
	once sync.Once
	err  error
}

func bootstrapMultiwriter() error {
	multiwriterBootstrap.once.Do(func() {
		multiwriterBootstrap.err = codeError(C.sg_multiwriter_bootstrap(), "Multiwriter.Initialize")
	})
	return multiwriterBootstrap.err
}

// OpenMultiwriter opens a writable on-disk pathname in private-lane mode with
// single-process operation, read validation enabled, and rebase disabled.
// It requires the sqlite_multiwriter build tag and a Unix target. URI inputs,
// temporary databases, memory databases and read-only flags are rejected.
// Zero flags use Open's defaults. SQLite/VFS bootstrap failures are cached for
// the process; later private-open hook registration failures can be retried.
func OpenMultiwriter(path string, flags OpenFlags) (*Conn, error) {
	if err := validCString(path); err != nil {
		return nil, err
	}
	if path == "" || path == ":memory:" || strings.HasPrefix(path, "file:") {
		return nil, ErrInvalidArgument
	}
	flags, err := normalizeOpenFlags(flags)
	if err != nil {
		return nil, err
	}
	if flags&OpenReadWrite == 0 || flags&OpenMemory != 0 {
		return nil, ErrInvalidArgument
	}
	// Escaping happens only after pathname and flags validation. Keep literal
	// percent, question-mark and hash bytes out of the URI's option syntax.
	name := path
	if strings.HasPrefix(name, "//") {
		name = "//" + name // No URI authority: preserve a pathname's leading //.
	}
	u := url.URL{Path: name}
	c, err := openNative("file:"+u.EscapedPath()+"?vfs=multiwriter&mw=1&mw_mp=0&mw_readcheck=1&mw_rebase=0", flags|OpenURI)
	if err != nil {
		return nil, err
	}
	if err := validateMultiwriter(c); err != nil {
		return nil, err
	}
	return c, nil
}

// validateStandard closes c if main uses an actual private or tracking lane.
func validateStandard(c *Conn) error {
	rc := C.sg_multiwriter_validate_standard(c.db)
	var err error
	if rc == C.SQLITE_MISUSE {
		err = fmt.Errorf("sqlite Open: main database uses a private or tracking lane: %w", ErrInvalidArgument)
	} else {
		err = codeError(rc, "Open.Validate")
	}
	if err != nil {
		return errors.Join(err, c.Close())
	}
	return nil
}

// validateMultiwriter closes c unless main is a writable private lane.
func validateMultiwriter(c *Conn) error {
	rc := C.sg_multiwriter_validate_writable_private(c.db)
	var err error
	if rc == C.SQLITE_MISUSE {
		err = fmt.Errorf("sqlite OpenMultiwriter: main database has no private lane: %w", ErrState)
	} else {
		err = codeError(rc, "OpenMultiwriter.Validate")
	}
	if err != nil {
		return errors.Join(err, c.Close())
	}
	return nil
}
