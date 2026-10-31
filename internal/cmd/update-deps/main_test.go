package main

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestTranslationUnitsRequireIntegrationForChangedSources(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "mw_vfs.c"), nil, 0644); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		name    string
		files   []string
		invalid bool
	}{
		{name: "POSIX source and retained Windows source", files: []string{"multiwriter_vfs.c", "multiwriter_os_win.c"}},
		{name: "new unwrapped translation unit", files: []string{"multiwriter_vfs.c", "new_engine.c"}, invalid: true},
		{name: "removed wrapped translation unit", files: []string{"replacement_vfs.c"}, invalid: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			files := make(map[string][]byte)
			for _, name := range test.files {
				files[multiwriterDirectory+"src/"+name] = []byte("source")
			}
			err := checkTranslationUnits(root, files)
			if (err != nil) != test.invalid {
				t.Fatalf("invalid=%v, error=%v", test.invalid, err)
			}
		})
	}
}

type pinTestTransport func(*http.Request) (*http.Response, error)

func (f pinTestTransport) RoundTrip(r *http.Request) (*http.Response, error) { return f(r) }

func TestUpdateRejectsInvalidConfigBeforeDownload(t *testing.T) {
	checksum := strings.Repeat("a", 64)
	commit := strings.Repeat("b", 40)
	valid := fmt.Sprintf(`{"sqlite":{"version":"3.61.2","year":2031,"sha3_256":%q},"sqlite_multiwriter":{"version":"0.8.1","commit":%q}}`, checksum, commit)
	for _, test := range []struct {
		name, config string
	}{
		{"missing pins", `{}`},
		{"old schema", `{"sqlite":"3.61.2","sqlite_multiwriter":"0.8.1"}`},
		{"misspelled checksum key", strings.Replace(valid, "sha3_256", "sha256", 1)},
		{"invalid SQLite checksum", strings.Replace(valid, checksum, strings.Repeat("g", 64), 1)},
		{"invalid Multiwriter commit", strings.Replace(valid, commit, "not-a-commit", 1)},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.WriteFile(filepath.Join(root, "versions.json"), []byte(test.config), 0644); err != nil {
				t.Fatal(err)
			}
			source := filepath.Join(root, "sqlite3.c")
			if err := os.WriteFile(source, []byte("original"), 0644); err != nil {
				t.Fatal(err)
			}
			client := &http.Client{Transport: pinTestTransport(func(r *http.Request) (*http.Response, error) {
				t.Fatalf("invalid config requested %s", r.URL)
				return nil, nil
			})}
			if err := update(context.Background(), root, client); err == nil {
				t.Fatal("accepted invalid configuration")
			}
			got, err := os.ReadFile(source)
			if err != nil || string(got) != "original" {
				t.Fatalf("source changed: %q, %v", got, err)
			}
		})
	}
}
