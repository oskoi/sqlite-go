package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
)

const multiwriterTestCommit = "e45f48d2ad7682b485f1637bd0249d8af5184372"

type multiwriterTestTransport func(*http.Request) (*http.Response, error)

func (transport multiwriterTestTransport) RoundTrip(request *http.Request) (*http.Response, error) {
	return transport(request)
}

type multiwriterTestMember struct {
	name string
	data []byte
	kind byte
}

func multiwriterTestInputs() []multiwriterTestMember {
	return []multiwriterTestMember{
		{name: "LICENSE", data: []byte("Apache License\n\x00exact license bytes\r\n")},
		{name: "NOTICE", data: []byte("Copyright upstream\r\n")},
		{name: "src/multiwriter.h", data: []byte("/* pristine public header */\r\n")},
		{name: "src/multiwriter_internal.h", data: []byte("#include \"multiwriter.h\"\n")},
		{name: "src/multiwriter_vfs.c", data: []byte("/* pristine VFS */\r\n")},
		{name: "src/multiwriter_os_win.c", data: []byte("/* Windows retained, not compiled */\n")},
		{name: "src/nested/helper.h", data: []byte("/* nested source header */\n")},
		{name: "third_party/sqlite/sqlite3.c", data: []byte("not imported")},
		{name: "src/README.md", data: []byte("not imported")},
		{name: "source.json", data: []byte(`{"commit":"not imported"}`)},
		{name: "ignored-link", kind: tar.TypeSymlink},
	}
}

func multiwriterTestArchive(t *testing.T, members []multiwriterTestMember) []byte {
	t.Helper()
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	// Match the global PAX header emitted by GitHub's commit archives.
	if err := writer.WriteHeader(&tar.Header{
		Name: "pax_global_header", Typeflag: tar.TypeXGlobalHeader,
		PAXRecords: map[string]string{"comment": multiwriterTestCommit},
	}); err != nil {
		t.Fatal(err)
	}
	for _, member := range members {
		kind := member.kind
		if kind == 0 {
			kind = tar.TypeReg
		}
		header := &tar.Header{
			Name: "sqlite-multiwriter-" + multiwriterTestCommit + "/" + member.name,
			Mode: 0o644, Typeflag: kind, Size: int64(len(member.data)),
		}
		if kind == tar.TypeSymlink || kind == tar.TypeLink {
			header.Linkname = "../../outside"
			header.Size = 0
		}
		if kind == tar.TypeDir {
			header.Size = 0
		}
		if err := writer.WriteHeader(header); err != nil {
			t.Fatal(err)
		}
		if kind == tar.TypeReg {
			if _, err := writer.Write(member.data); err != nil {
				t.Fatal(err)
			}
		}
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	return buffer.Bytes()
}

func TestFetchMultiwriterPinnedCommit(t *testing.T) {
	pin := multiwriterPin{Version: "human release label / not a tag", Commit: multiwriterTestCommit}
	if err := pin.validate(); err != nil {
		t.Fatal(err)
	}
	archive := multiwriterTestArchive(t, multiwriterTestInputs())
	archiveURL := "https://codeload.github.com/sqliteai/sqlite-multiwriter/tar.gz/" + pin.Commit
	requests := 0
	client := &http.Client{Transport: multiwriterTestTransport(func(request *http.Request) (*http.Response, error) {
		requests++
		if request.Method != http.MethodGet || request.URL.String() != archiveURL {
			t.Errorf("unexpected HTTP request: %s %s", request.Method, request.URL)
			return nil, fmt.Errorf("unexpected HTTP request: %s %s", request.Method, request.URL)
		}
		return &http.Response{StatusCode: http.StatusOK, Status: "200 OK", Body: io.NopCloser(bytes.NewReader(archive)), Header: make(http.Header)}, nil
	})}
	files, err := fetchMultiwriter(context.Background(), client, pin)
	if err != nil {
		t.Fatal(err)
	}
	if requests != 1 {
		t.Errorf("made %d HTTP requests; want one pinned archive download", requests)
	}
	for _, member := range multiwriterTestInputs()[:7] {
		if !bytes.Equal(files[multiwriterDirectory+member.name], member.data) {
			t.Errorf("upstream bytes changed for %s", member.name)
		}
	}
	if len(files) != 7 {
		t.Errorf("imported %d files; want seven native sources/licenses", len(files))
	}
	if _, exists := files[multiwriterDirectory+"source.json"]; exists {
		t.Error("import emitted source.json")
	}
}

func TestMultiwriterPinValidation(t *testing.T) {
	cases := []struct {
		name    string
		pin     multiwriterPin
		wantErr bool
	}{
		{name: "current release", pin: multiwriterPin{Version: "0.6.4", Commit: multiwriterTestCommit}},
		{name: "arbitrary label", pin: multiwriterPin{Version: "human release label / not a tag", Commit: multiwriterTestCommit}},
		{name: "empty label", pin: multiwriterPin{Commit: multiwriterTestCommit}, wantErr: true},
		{name: "blank label", pin: multiwriterPin{Version: " \t\r\n", Commit: multiwriterTestCommit}, wantErr: true},
		{name: "empty commit", pin: multiwriterPin{Version: "0.6.4"}, wantErr: true},
		{name: "short commit", pin: multiwriterPin{Version: "0.6.4", Commit: multiwriterTestCommit[:39]}, wantErr: true},
		{name: "long commit", pin: multiwriterPin{Version: "0.6.4", Commit: multiwriterTestCommit + "a"}, wantErr: true},
		{name: "uppercase commit", pin: multiwriterPin{Version: "0.6.4", Commit: strings.ToUpper(multiwriterTestCommit)}, wantErr: true},
		{name: "nonhex commit", pin: multiwriterPin{Version: "0.6.4", Commit: strings.Repeat("g", 40)}, wantErr: true},
		{name: "commit path", pin: multiwriterPin{Version: "0.6.4", Commit: "../" + multiwriterTestCommit}, wantErr: true},
		{name: "commit whitespace", pin: multiwriterPin{Version: "0.6.4", Commit: multiwriterTestCommit[:39] + " "}, wantErr: true},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := test.pin.validate(); (err != nil) != test.wantErr {
				t.Errorf("validate() error = %v, want error = %t", err, test.wantErr)
			}
		})
	}
}

func TestMultiwriterRejectsIncompleteArchives(t *testing.T) {
	for _, missing := range []string{"LICENSE", "NOTICE", "src/multiwriter.h", "src/multiwriter_internal.h", "src/multiwriter_vfs.c"} {
		t.Run("missing "+missing, func(t *testing.T) {
			var members []multiwriterTestMember
			for _, member := range multiwriterTestInputs() {
				if member.name != missing {
					members = append(members, member)
				}
			}
			if _, err := importMultiwriterArchive(multiwriterTestArchive(t, members), multiwriterTestCommit); err == nil {
				t.Fatal("accepted an incomplete source archive")
			}
		})
	}
}

func TestMultiwriterRejectsUnsafeArchiveMembers(t *testing.T) {
	cases := []multiwriterTestMember{
		{name: "../outside.c", data: []byte("escape")},
		{name: "src/../../outside.c", data: []byte("escape")},
		{name: "src\\outside.c", data: []byte("Windows separator")},
		{name: "src//duplicate.c", data: []byte("noncanonical path")},
		{name: "src/./duplicate.c", data: []byte("noncanonical path")},
		{name: "src/multiwriter_vfs.c", data: []byte("duplicate source")},
		{name: "NOTICE", data: []byte("duplicate notice")},
		{name: "src/link.h", kind: tar.TypeSymlink},
		{name: "src/link.c", kind: tar.TypeLink},
		{name: "src/pipe.c", kind: tar.TypeFifo},
		{name: "src/directory.h/", kind: tar.TypeDir},
	}
	for _, member := range cases {
		t.Run(member.name+fmt.Sprintf(" type=%d", member.kind), func(t *testing.T) {
			members := append(multiwriterTestInputs(), member)
			if _, err := importMultiwriterArchive(multiwriterTestArchive(t, members), multiwriterTestCommit); err == nil {
				t.Fatal("accepted an unsafe, duplicate or nonregular archive member")
			}
		})
	}
	archive := multiwriterTestArchive(t, multiwriterTestInputs())
	if _, err := importMultiwriterArchive(archive, strings.Repeat("a", 40)); err == nil {
		t.Fatal("accepted an archive rooted at the wrong commit")
	}
}

func TestMultiwriterRejectsDamagedArchive(t *testing.T) {
	archive := multiwriterTestArchive(t, multiwriterTestInputs())
	archive[len(archive)-8] ^= 0xff // Corrupt the gzip CRC after the tar EOF marker.
	if _, err := importMultiwriterArchive(archive, multiwriterTestCommit); err == nil {
		t.Fatal("accepted an archive with a bad gzip checksum")
	}
	if _, err := importMultiwriterArchive(archive[:len(archive)/2], multiwriterTestCommit); err == nil {
		t.Fatal("accepted a truncated archive")
	}
}

type multiwriterTestZeros struct{}

func (multiwriterTestZeros) Read(buffer []byte) (int, error) {
	clear(buffer)
	return len(buffer), nil
}

func TestMultiwriterBoundsDecompressionOfIgnoredFiles(t *testing.T) {
	var buffer bytes.Buffer
	compressed := gzip.NewWriter(&buffer)
	writer := tar.NewWriter(compressed)
	if err := writer.WriteHeader(&tar.Header{
		Name: "sqlite-multiwriter-" + multiwriterTestCommit + "/ignored.bin",
		Mode: 0o644, Typeflag: tar.TypeReg, Size: maxExtractedBytes + 1,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := io.CopyN(writer, multiwriterTestZeros{}, maxExtractedBytes+1); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	if err := compressed.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := importMultiwriterArchive(buffer.Bytes(), multiwriterTestCommit); err == nil {
		t.Fatalf("oversized decompression error = %v", err)
	}
}
