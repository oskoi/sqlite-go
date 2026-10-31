package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"os"
	"strings"
	"testing"
)

type sqliteTestTransport func(*http.Request) (*http.Response, error)

func (transport sqliteTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type sqliteTestMember struct {
	name string
	data []byte
	mode os.FileMode
	size uint64
}

func sqliteTestZIP(t *testing.T, members []sqliteTestMember) []byte {
	t.Helper()
	var buffer bytes.Buffer
	writer := zip.NewWriter(&buffer)
	for _, member := range members {
		header := &zip.FileHeader{Name: member.name, Method: zip.Store}
		if member.mode != 0 {
			header.SetMode(member.mode)
		}
		var file io.Writer
		var err error
		if member.size != 0 {
			header.UncompressedSize64 = member.size
			file, err = writer.CreateRaw(header)
		} else {
			file, err = writer.CreateHeader(header)
		}
		if err != nil {
			t.Fatal(err)
		}
		if _, err := file.Write(member.data); err != nil {
			t.Fatal(err)
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

type sqliteTestArchive struct {
	version    string
	year       int
	root       string
	source     []byte
	header     []byte
	archive    []byte
	archiveURL string
}

func sqliteTestFixture(t *testing.T, version, encoded string, year int) sqliteTestArchive {
	t.Helper()
	fixture := sqliteTestArchive{
		version:    version,
		year:       year,
		root:       "sqlite-amalgamation-" + encoded,
		archiveURL: fmt.Sprintf("https://www.sqlite.org/%d/sqlite-amalgamation-%s.zip", year, encoded),
		source:     []byte("/* Original SQLite public-domain blessing. C source. */\nint sqlite3_fixture(void) { return 7; }\n"),
		header:     []byte("/* Original SQLite public-domain blessing. Header. */\nint sqlite3_fixture(void);\n"),
	}
	fixture.archive = sqliteTestZIP(t, fixture.members())
	return fixture
}

func (fixture sqliteTestArchive) members() []sqliteTestMember {
	return []sqliteTestMember{
		{name: fixture.root + "/sqlite3.c", data: fixture.source},
		{name: fixture.root + "/sqlite3.h", data: fixture.header},
		{name: fixture.root + "/shell.c", data: []byte("not imported\n")},
	}
}

func (fixture sqliteTestArchive) pin() sqlitePin {
	return sqlitePin{Version: fixture.version, Year: fixture.year, SHA3: digest(fixture.archive)}
}

func sqliteTestClient(t *testing.T, fixture sqliteTestArchive) *http.Client {
	t.Helper()
	requests := 0
	t.Cleanup(func() {
		if requests != 1 {
			t.Errorf("got %d requests, want one archive request", requests)
		}
	})
	return &http.Client{Transport: sqliteTestTransport(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.URL.String() != fixture.archiveURL {
			return nil, fmt.Errorf("unexpected URL %s; only %s is allowed", request.URL, fixture.archiveURL)
		}
		return &http.Response{StatusCode: http.StatusOK, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(fixture.archive)), Request: request}, nil
	})}
}

func TestSQLiteFetchPinnedArchive(t *testing.T) {
	for _, test := range []struct {
		name    string
		version string
		encoded string
		year    int
	}{
		{name: "configured year", version: "3.61.2", encoded: "3610200", year: 2031},
		{name: "fourth version component", version: "3.8.10.2", encoded: "3081002", year: 2015},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := sqliteTestFixture(t, test.version, test.encoded, test.year)
			pin := fixture.pin()
			if err := pin.validate(); err != nil {
				t.Fatal(err)
			}
			files, err := fetchSQLite(context.Background(), sqliteTestClient(t, fixture), pin)
			if err != nil {
				t.Fatal(err)
			}
			if len(files) != 2 || !bytes.Equal(files["sqlite3.c"], fixture.source) || !bytes.Equal(files["sqlite3.h"], fixture.header) {
				t.Fatal("import must contain only pristine sqlite3.c and sqlite3.h")
			}
		})
	}
}

func TestSQLitePinValidate(t *testing.T) {
	for _, test := range []struct {
		version string
		encoded string
		year    int
	}{
		{version: "3.53.4", encoded: "3530400", year: 2026},
		{version: "3.0.0", encoded: "3000000", year: 1000},
		{version: "3.99.99.99", encoded: "3999999", year: 9999},
	} {
		t.Run(test.version, func(t *testing.T) {
			pin := sqlitePin{Version: test.version, Year: test.year, SHA3: strings.Repeat("0123456789abcdef", 4)}
			if err := pin.validate(); err != nil {
				t.Fatal(err)
			}
			encoded, err := sqliteArchiveVersion(test.version)
			if err != nil || encoded != test.encoded {
				t.Fatalf("archive version = %q, error = %v; want %q", encoded, err, test.encoded)
			}
		})
	}
}

func TestSQLitePinRejectInvalidVersions(t *testing.T) {
	for _, version := range []string{"", "latest", "2.8.17", "4.0.0", "3.1", "3.1.2.3.4", "3.01.2", "3.1.02", "3.1.2.0", "3.1.2.03", "3.100.1", "3.1.100", "3.1.2.100", "3.-1.2", "3.+1.2", "3.1.2 ", "3.1.2-dev", "3..2"} {
		t.Run(version, func(t *testing.T) {
			pin := sqlitePin{Version: version, Year: 2026, SHA3: strings.Repeat("a", 64)}
			if err := pin.validate(); err == nil {
				t.Fatalf("invalid version %q was accepted", version)
			}
		})
	}
}

func TestSQLitePinRejectInvalidYears(t *testing.T) {
	for _, year := range []int{-2026, 0, 999, 10000} {
		t.Run(fmt.Sprint(year), func(t *testing.T) {
			pin := sqlitePin{Version: "3.53.4", Year: year, SHA3: strings.Repeat("a", 64)}
			if err := pin.validate(); err == nil {
				t.Fatalf("invalid year %d was accepted", year)
			}
		})
	}
}

func TestSQLitePinRejectInvalidHashes(t *testing.T) {
	for _, hash := range []string{"", strings.Repeat("a", 63), strings.Repeat("a", 65), strings.Repeat("A", 64), strings.Repeat("g", 64), strings.Repeat("a", 63) + " ", strings.Repeat("a", 63) + "\n"} {
		t.Run(fmt.Sprintf("%q", hash), func(t *testing.T) {
			pin := sqlitePin{Version: "3.53.4", Year: 2026, SHA3: hash}
			if err := pin.validate(); err == nil {
				t.Fatalf("invalid archive hash %q was accepted", hash)
			}
		})
	}
}

func TestSQLiteRejectArchiveHashMismatch(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, *sqliteTestArchive, *sqlitePin)
	}{
		{
			name: "different configured hash",
			mutate: func(t *testing.T, fixture *sqliteTestArchive, pin *sqlitePin) {
				pin.SHA3 = strings.Repeat("0", 64)
			},
		},
		{
			name: "tampered archive",
			mutate: func(t *testing.T, fixture *sqliteTestArchive, pin *sqlitePin) {
				members := fixture.members()
				members[0].data = []byte("changed source bytes\n")
				fixture.archive = sqliteTestZIP(t, members)
			},
		},
		{
			name: "hash checked before ZIP extraction",
			mutate: func(t *testing.T, fixture *sqliteTestArchive, pin *sqlitePin) {
				fixture.archive = []byte("not a ZIP archive")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := sqliteTestFixture(t, "3.61.2", "3610200", 2031)
			pin := fixture.pin()
			test.mutate(t, &fixture, &pin)
			files, err := fetchSQLite(context.Background(), sqliteTestClient(t, fixture), pin)
			if err == nil || files != nil {
				t.Fatalf("archive with mismatched hash returned files=%v, error=%v", files, err)
			}
			if !strings.Contains(err.Error(), pin.SHA3) || !strings.Contains(err.Error(), digest(fixture.archive)) {
				t.Fatalf("hash rejection must identify configured and actual digests: %v", err)
			}
		})
	}
}

func TestSQLiteRejectInvalidArchiveMembers(t *testing.T) {
	for _, test := range []struct {
		name   string
		mutate func(*testing.T, sqliteTestArchive) []byte
	}{
		{
			name: "missing header",
			mutate: func(t *testing.T, fixture sqliteTestArchive) []byte {
				return sqliteTestZIP(t, fixture.members()[:1])
			},
		},
		{
			name: "missing source",
			mutate: func(t *testing.T, fixture sqliteTestArchive) []byte {
				return sqliteTestZIP(t, fixture.members()[1:])
			},
		},
		{
			name: "duplicate source",
			mutate: func(t *testing.T, fixture sqliteTestArchive) []byte {
				return sqliteTestZIP(t, append(fixture.members(), fixture.members()[0]))
			},
		},
		{
			name: "duplicate header",
			mutate: func(t *testing.T, fixture sqliteTestArchive) []byte {
				return sqliteTestZIP(t, append(fixture.members(), fixture.members()[1]))
			},
		},
		{
			name: "path traversal",
			mutate: func(t *testing.T, fixture sqliteTestArchive) []byte {
				members := fixture.members()
				members[1].name = fixture.root + "/../sqlite3.h"
				return sqliteTestZIP(t, members)
			},
		},
		{
			name: "wrong archive root",
			mutate: func(t *testing.T, fixture sqliteTestArchive) []byte {
				members := fixture.members()
				members[1].name = "sqlite-amalgamation-3610300/sqlite3.h"
				return sqliteTestZIP(t, members)
			},
		},
		{
			name: "backslash path",
			mutate: func(t *testing.T, fixture sqliteTestArchive) []byte {
				members := fixture.members()
				members[1].name = fixture.root + "\\sqlite3.h"
				return sqliteTestZIP(t, members)
			},
		},
		{
			name: "header symlink",
			mutate: func(t *testing.T, fixture sqliteTestArchive) []byte {
				members := fixture.members()
				members[1].mode = os.ModeSymlink | 0777
				return sqliteTestZIP(t, members)
			},
		},
		{
			name: "header directory",
			mutate: func(t *testing.T, fixture sqliteTestArchive) []byte {
				members := fixture.members()
				members[1].name += "/"
				members[1].data = nil
				return sqliteTestZIP(t, members)
			},
		},
		{
			name: "oversized source",
			mutate: func(t *testing.T, fixture sqliteTestArchive) []byte {
				members := fixture.members()
				members[0].size = uint64(maxExtractedBytes) + 1
				members[0].data = nil
				return sqliteTestZIP(t, members)
			},
		},
		{
			name: "combined extraction limit",
			mutate: func(t *testing.T, fixture sqliteTestArchive) []byte {
				members := fixture.members()
				members[1].size = uint64(maxExtractedBytes)
				members[1].data = nil
				return sqliteTestZIP(t, members)
			},
		},
		{
			name: "malformed ZIP",
			mutate: func(t *testing.T, fixture sqliteTestArchive) []byte {
				return []byte("not a ZIP archive")
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			fixture := sqliteTestFixture(t, "3.61.2", "3610200", 2031)
			fixture.archive = test.mutate(t, fixture)
			files, err := fetchSQLite(context.Background(), sqliteTestClient(t, fixture), fixture.pin())
			if err == nil || files != nil {
				t.Fatalf("invalid archive returned files=%v, error=%v", files, err)
			}
		})
	}
}
