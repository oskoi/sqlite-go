//go:build sqlite_multiwriter && unix

package sqlite

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

func TestMultiwriterUnicodeReopenNoCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "数据库-Å-雪-𐐷.db")
	c, err := OpenMultiwriter(path, OpenReadWrite)
	if c != nil {
		if closeErr := c.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if c != nil || !errors.Is(err, CodeCantOpen) {
		t.Fatalf("missing Unicode pathname without create: %v %v", c, err)
	}
	if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("no-create open touched Unicode pathname: %v", err)
	}

	created := openMultiwriterTest(t, path)
	script(t, created, "CREATE TABLE unicode_payload(v); INSERT INTO unicode_payload VALUES(731)")
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := OpenMultiwriter(path, OpenReadWrite)
	if err != nil {
		t.Fatalf("reopen existing Unicode pathname without create: %v", err)
	}
	t.Cleanup(func() {
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	if got := scalarTest(t, reopened, "SELECT v FROM unicode_payload"); got != 731 {
		t.Fatalf("reopened Unicode payload %d, want 731", got)
	}
}

func TestMultiwriterEmptyFileWithoutCreate(t *testing.T) {
	path := filepath.Join(t.TempDir(), "empty.db")
	if err := os.WriteFile(path, nil, 0600); err != nil {
		t.Fatal(err)
	}
	if info, err := os.Stat(path); err != nil || info.Size() != 0 {
		t.Fatalf("empty-file fixture: %v %v", info, err)
	}
	c, err := OpenMultiwriter(path, OpenReadWrite)
	if err != nil {
		t.Fatalf("open existing empty file without create: %v", err)
	}
	t.Cleanup(func() {
		if err := c.Close(); err != nil {
			t.Error(err)
		}
	})
	script(t, c, "CREATE TABLE empty_payload(v); INSERT INTO empty_payload VALUES(419)")
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	reopened := openMultiwriterTest(t, path)
	if got := scalarTest(t, reopened, "SELECT v FROM empty_payload"); got != 419 {
		t.Fatalf("initialized empty-file payload %d, want 419", got)
	}
}

func TestMultiwriterStockWALRecovery(t *testing.T) {
	const childEnv = "SQLITE_GO_MULTIWRITER_STOCK_WAL_PATH"
	if path := os.Getenv(childEnv); path != "" {
		// Exit without Close: a normal stock close checkpoints/deletes the WAL
		// and would silently stop this from testing crash recovery.
		seedMultiwriterStockWAL(t, path)
		os.Exit(0)
	}
	executable, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"stock.db", "恢复-Å-雪-𐐷.db"} {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), name)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			cmd := exec.CommandContext(ctx, executable, "-test.run=^TestMultiwriterStockWALRecovery$", "-test.count=1")
			cmd.Env = append(os.Environ(), childEnv+"="+path)
			if out, err := cmd.CombinedOutput(); err != nil {
				t.Fatalf("stock WAL fixture subprocess: %v\n%s", err, out)
			}
			if info, err := os.Stat(path + "-wal"); err != nil || info.Size() <= 32 {
				t.Fatalf("fixture must retain stock WAL frames before engine open: %v %v", info, err)
			}
			// Create-capable opening deliberately bypasses the no-create path
			// check: the Unicode case must independently reach WAL recovery.
			c := openMultiwriterTest(t, path)
			checkMultiwriterRecoveredPayload(t, c)
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			reopened := openMultiwriterTest(t, path)
			checkMultiwriterRecoveredPayload(t, reopened)
		})
	}
}

func seedMultiwriterStockWAL(t *testing.T, path string) {
	t.Helper()
	c, err := Open(path, OpenReadWrite|OpenCreate)
	if err != nil {
		t.Fatal(err)
	}
	script(t, c, "PRAGMA journal_mode=WAL; PRAGMA wal_autocheckpoint=0; CREATE TABLE recovery_payload(id INTEGER PRIMARY KEY,v TEXT NOT NULL,b BLOB NOT NULL); INSERT INTO recovery_payload VALUES(1,'old',x'01'),(99,'deleted',x'99'); PRAGMA wal_checkpoint(TRUNCATE)")
	if got := scalarTest(t, c, "SELECT journal_mode='wal' FROM pragma_journal_mode"); got != 1 {
		t.Fatal("stock fixture did not enter WAL mode")
	}
	if got := scalarTest(t, c, "PRAGMA wal_autocheckpoint"); got != 0 {
		t.Fatalf("stock fixture autocheckpoint %d, want disabled", got)
	}
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() != 0 {
		t.Fatalf("fixture checkpoint did not truncate the baseline WAL: %v %v", info, err)
	}
	script(t, c, "BEGIN; UPDATE recovery_payload SET v='recovered 雪',b=x'0025ff' WHERE id=1; DELETE FROM recovery_payload WHERE id=99; INSERT INTO recovery_payload VALUES(2,'second Å',x'102030'),(3,'𐐷',x''); COMMIT")
	checkMultiwriterRecoveredPayload(t, c)
	if info, err := os.Stat(path + "-wal"); err != nil || info.Size() <= 32 {
		t.Fatalf("committed fixture has no uncheckpointed WAL frames: %v %v", info, err)
	}
}

func checkMultiwriterRecoveredPayload(t *testing.T, c *Conn) {
	t.Helper()
	if got := scalarTest(t, c, "SELECT count(*) FROM recovery_payload"); got != 3 {
		t.Fatalf("recovered row count %d, want 3", got)
	}
	if got := scalarTest(t, c, "SELECT count(*) FROM recovery_payload WHERE (id=1 AND v='recovered 雪' AND b=x'0025ff') OR (id=2 AND v='second Å' AND b=x'102030') OR (id=3 AND v='𐐷' AND b=x'')"); got != 3 {
		t.Fatalf("exact recovered row payloads %d, want 3", got)
	}
}

func TestMultiwriterWALMetadataError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "metadata.db")
	stock := openTest(t, path)
	script(t, stock, "PRAGMA journal_mode=WAL; CREATE TABLE metadata_payload(v); INSERT INTO metadata_payload VALUES(827)")
	if err := stock.Close(); err != nil {
		t.Fatal(err)
	}
	wal := path + "-wal"
	if _, err := os.Lstat(wal); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("closed stock fixture must have no WAL: %v", err)
	}
	if err := os.Symlink(filepath.Base(wal), wal); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(wal); !errors.Is(err, syscall.ELOOP) {
		t.Fatalf("WAL fixture must fail metadata lookup with ELOOP: %v", err)
	}
	c, err := OpenMultiwriter(path, 0)
	if c != nil {
		if closeErr := c.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	}
	if c != nil || !errors.Is(err, CodeIOErrFstat) {
		t.Fatalf("WAL metadata failure must fail closed with native IOERR_FSTAT: %v %v", c, err)
	}
	if err := os.Remove(wal); err != nil {
		t.Fatal(err)
	}
	recovered := openMultiwriterTest(t, path)
	if got := scalarTest(t, recovered, "SELECT v FROM metadata_payload"); got != 827 {
		t.Fatalf("metadata error changed committed data: %d", got)
	}
}
