//go:build !sqlite_multiwriter || !unix

package sqlite

// OpenMultiwriter requires the sqlite_multiwriter build tag and a Unix target.
// On other builds it returns ErrMultiwriterUnavailable without touching path.
// It never falls back to a stock connection when the feature is unavailable.
func OpenMultiwriter(path string, flags OpenFlags) (*Conn, error) {
	return nil, ErrMultiwriterUnavailable
}

func bootstrapMultiwriter() error { return nil }

func validateStandard(c *Conn) error { return nil }
