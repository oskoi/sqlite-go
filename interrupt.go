package sqlite

/*
#include "bridge.h"
*/
import "C"

// Interrupt requests cancellation of currently running SQLite operations. It is
// safe to call from another goroutine, including while Close runs. It does not
// wake arbitrary Go callbacks or promise to wake a native busy sleep. Stop and
// join any cancellation watcher before reusing the connection for another task.
func (c *Conn) Interrupt() error {
	if c == nil {
		return ErrClosed
	}
	c.lifetime.Lock()
	defer c.lifetime.Unlock()
	if c.backup != nil {
		return backupBusy()
	}
	if c.interruptDB == nil {
		return ErrClosed
	}
	C.sqlite3_interrupt(c.interruptDB)
	return nil
}
