package sqlite

import (
	"math"
	"strings"
)

type noCopy struct{}

func (*noCopy) Lock()   {}
func (*noCopy) Unlock() {}

func validCString(s string) error {
	if strings.IndexByte(s, 0) >= 0 {
		return ErrInvalidArgument
	}
	if len(s) > math.MaxInt32-1 {
		return ErrRange
	}
	return nil
}
func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// check is intentionally lock-free under the single-owner contract. Only cold
// lifetime transitions use the connection's lifetime gate.
func (c *Conn) check() error {
	if c == nil || c.db == nil {
		return ErrClosed
	}
	if c.active {
		return ErrReentrant
	}
	if c.backup != nil {
		return backupBusy()
	}
	return nil
}

// endCall runs after native and Go handle/error cleanup. User callbacks remain
// suppressed until all managed registrations have been detached.
func (c *Conn) endCall() {
	if !c.panicked {
		c.active = false
		return
	}
	if c.db != nil {
		// All five unregister operations accept a valid connection and cannot
		// invoke a callback or fail. Keep contexts live until all detach.
		c.clearCallbacks()
	}
	c.clearSessionFilters()
	c.releaseCallbacks()
	value := c.panicValue
	c.panicValue = nil
	c.panicked = false
	c.active = false
	panic(value)
}
func (s *Stmt) check() error {
	if s == nil || s.stmt == nil {
		return ErrClosed
	}
	return s.conn.check()
}
func (s *Stmt) ready() error {
	if err := s.check(); err != nil {
		return err
	}
	if s.state != stmtReady {
		return ErrState
	}
	return nil
}
func (s *Stmt) parameter(index int) error {
	if err := s.ready(); err != nil {
		return err
	}
	if index < 1 || index > s.parameters {
		return ErrRange
	}
	return nil
}
func (s *Stmt) column(index int, row bool) error {
	if err := s.check(); err != nil {
		return err
	}
	if row && s.state != stmtRow {
		return ErrState
	}
	if index < 0 || index >= s.columns {
		return ErrRange
	}
	return nil
}
