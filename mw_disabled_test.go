//go:build !sqlite_multiwriter || !unix

package sqlite

import (
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestMultiwriterUnavailable(t *testing.T) {
	path := filepath.Join(t.TempDir(), "unavailable.db")
	c, err := OpenMultiwriter(path, 0)
	if c != nil || !errors.Is(err, ErrMultiwriterUnavailable) {
		t.Fatalf("OpenMultiwriter: %v %v", c, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("unavailable opener touched pathname: %v", err)
	}
}
