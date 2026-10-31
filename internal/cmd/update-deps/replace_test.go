package main

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestReplaceFilesRejectsInvalidDestination(t *testing.T) {
	dir := t.TempDir()
	original := []byte("previous source\n")
	if err := os.WriteFile(filepath.Join(dir, "sqlite3.c"), original, 0644); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(filepath.Join(dir, "sqlite3.h"), 0755); err != nil {
		t.Fatal(err)
	}
	if err := replaceFiles(dir, replacementInputs(), os.Rename, func() error { return nil }); err == nil {
		t.Fatal("accepted directory as header destination")
	}
	got, err := os.ReadFile(filepath.Join(dir, "sqlite3.c"))
	if err != nil || string(got) != string(original) {
		t.Fatalf("source changed after failed update: got %q, error %v", got, err)
	}
}

func replacementInputs() map[string][]byte {
	return map[string][]byte{
		"sqlite3.c": []byte("new source\n"),
		"sqlite3.h": []byte("new header\n"),
	}
}

func seedReplacementFiles(t *testing.T, dir string, files map[string][]byte) {
	t.Helper()
	for name, data := range files {
		target := filepath.Join(dir, name)
		if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(target, data, 0640); err != nil {
			t.Fatal(err)
		}
	}
}

func seedOriginals(t *testing.T, dir string, missingHeader bool) map[string][]byte {
	t.Helper()
	old := map[string][]byte{
		"sqlite3.c": []byte("previous source\n"),
		"sqlite3.h": []byte("previous header\n"),
	}
	if missingHeader {
		delete(old, "sqlite3.h")
	}
	seedReplacementFiles(t, dir, old)
	return old
}

func assertReplacementFiles(t *testing.T, dir string, want map[string][]byte) {
	t.Helper()
	wantDirs := map[string]bool{".": true}
	for name := range want {
		for parent := filepath.Dir(filepath.FromSlash(name)); parent != "."; parent = filepath.Dir(parent) {
			wantDirs[filepath.ToSlash(parent)] = true
		}
	}
	found := 0
	err := filepath.WalkDir(dir, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return err
		}
		relative, err := filepath.Rel(dir, path)
		if err != nil {
			return err
		}
		name := filepath.ToSlash(relative)
		if entry.IsDir() {
			if !wantDirs[name] {
				t.Errorf("unexpected directory %s", name)
			}
			return nil
		}
		data, exists := want[name]
		if !exists {
			t.Errorf("unexpected file %s", name)
			return nil
		}
		found++
		got, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if string(got) != string(data) {
			t.Errorf("%s = %q, want %q", name, got, data)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if found != len(want) {
		t.Errorf("found %d expected files, want %d", found, len(want))
	}
}

func TestReplaceFilesSuccess(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
			dir := t.TempDir()
			if existing {
				seedOriginals(t, dir, false)
			}
			files := replacementInputs()
			if err := replaceFiles(dir, files, os.Rename, func() error { return nil }); err != nil {
				t.Fatal(err)
			}
			assertReplacementFiles(t, dir, files)
			if existing && runtime.GOOS != "windows" {
				info, err := os.Stat(filepath.Join(dir, "sqlite3.c"))
				if err != nil {
					t.Fatal(err)
				}
				if info.Mode().Perm() != 0640 {
					t.Fatalf("source permissions = %o, want 640", info.Mode().Perm())
				}
			}
		})
	}
}

func TestReplaceFilesRollsBackRenameFailures(t *testing.T) {
	for _, name := range []string{"sqlite3.c", "sqlite3.h"} {
		for _, boundary := range []string{"save", "install"} {
			t.Run(name+"/"+boundary, func(t *testing.T) {
				dir := t.TempDir()
				old := seedOriginals(t, dir, false)
				failure := errors.New("replacement unavailable")
				target := filepath.Join(dir, name)
				rename := func(from, to string) error {
					if boundary == "save" && from == target ||
						boundary == "install" && to == target && strings.HasSuffix(from, ".new") {
						return failure
					}
					return os.Rename(from, to)
				}
				if err := replaceFiles(dir, replacementInputs(), rename, func() error { return nil }); !errors.Is(err, failure) {
					t.Fatalf("replacement error = %v, want injected failure", err)
				}
				assertReplacementFiles(t, dir, old)
			})
		}
	}
}

func TestReplaceFilesRollbackRemovesNewDestinations(t *testing.T) {
	dir := t.TempDir()
	old := seedOriginals(t, dir, true)
	failure := errors.New("verification failed")
	if err := replaceFiles(dir, replacementInputs(), os.Rename, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("replacement error = %v, want injected failure", err)
	}
	assertReplacementFiles(t, dir, old)
}

func TestReplaceFilesRollbackRemovesNewSourceDirectories(t *testing.T) {
	dir := t.TempDir()
	failure := errors.New("verification failed")
	if err := replaceFiles(dir, sourceTreeReplacementInputs(), os.Rename, func() error { return failure }); !errors.Is(err, failure) {
		t.Fatalf("replacement error = %v, want verification failure", err)
	}
	assertReplacementFiles(t, dir, nil)
}

func replacementRecoveryDirectory(t *testing.T, dir string, err error) string {
	t.Helper()
	stages, globErr := filepath.Glob(filepath.Join(dir, ".deps-update-*"))
	if globErr != nil || len(stages) != 1 {
		t.Fatalf("recovery directories = %v, error %v", stages, globErr)
	}
	if !strings.Contains(err.Error(), stages[0]) {
		t.Fatalf("error does not identify recovery directory: %v", err)
	}
	return stages[0]
}

func TestReplaceFilesPreservesRecoveryFiles(t *testing.T) {
	dir := t.TempDir()
	old := seedOriginals(t, dir, false)
	installFailure := errors.New("header replacement unavailable")
	restoreFailure := errors.New("source restore unavailable")
	rename := func(from, to string) error {
		switch filepath.Base(from) {
		case "sqlite3.h.new":
			return installFailure
		case "sqlite3.c.old":
			return restoreFailure
		default:
			return os.Rename(from, to)
		}
	}
	err := replaceFiles(dir, replacementInputs(), rename, func() error { return nil })
	if !errors.Is(err, installFailure) || !errors.Is(err, restoreFailure) {
		t.Fatalf("replacement error = %v, want install and restore failures", err)
	}
	stage := replacementRecoveryDirectory(t, dir, err)
	got, readErr := os.ReadFile(filepath.Join(stage, "sqlite3.c.old"))
	if readErr != nil || string(got) != string(old["sqlite3.c"]) {
		t.Fatalf("saved original = %q, error %v", got, readErr)
	}
	got, readErr = os.ReadFile(filepath.Join(dir, "sqlite3.h"))
	if readErr != nil || string(got) != string(old["sqlite3.h"]) {
		t.Fatalf("restored header = %q, error %v", got, readErr)
	}
	if err := os.Rename(filepath.Join(stage, "sqlite3.c.old"), filepath.Join(dir, "sqlite3.c")); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(stage); err != nil {
		t.Fatal(err)
	}
	assertReplacementFiles(t, dir, old)
}

func seedOriginalSourceTree(t *testing.T, dir string) map[string][]byte {
	t.Helper()
	old := map[string][]byte{
		"sqlite3.c":                                          []byte("old SQLite"),
		multiwriterDirectory + "LICENSE":                     []byte("old license"),
		multiwriterDirectory + "NOTICE":                      []byte("old notice"),
		multiwriterDirectory + "custom.txt":                  []byte("custom sibling"),
		multiwriterDirectory + "src/engine.c":                []byte("old Multiwriter"),
		multiwriterDirectory + "src/stale/nested/obsolete.h": []byte("unlisted old header"),
	}
	seedReplacementFiles(t, dir, old)
	for name, mode := range map[string]os.FileMode{"src": 0750, "src/stale/nested": 0700} {
		if err := os.Chmod(filepath.Join(dir, multiwriterDirectory, name), mode); err != nil {
			t.Fatal(err)
		}
	}
	return old
}

func sourceTreeReplacementInputs() map[string][]byte {
	return map[string][]byte{
		"sqlite3.c":                               []byte("new SQLite"),
		multiwriterDirectory + "LICENSE":          []byte("new license"),
		multiwriterDirectory + "src/engine.c":     []byte("new Multiwriter"),
		multiwriterDirectory + "src/nested/new.h": []byte("new header"),
	}
}

func assertOriginalSourceTreePermissions(t *testing.T, tree string) {
	t.Helper()
	if runtime.GOOS == "windows" {
		return
	}
	for name, mode := range map[string]os.FileMode{".": 0750, "stale/nested": 0700, "engine.c": 0640} {
		info, err := os.Stat(filepath.Join(tree, name))
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != mode {
			t.Errorf("%s permissions = %o, want %o", name, info.Mode().Perm(), mode)
		}
	}
}

func TestReplaceFilesReplacesEntireSourceTree(t *testing.T) {
	for _, existing := range []bool{false, true} {
		t.Run(fmt.Sprintf("existing=%v", existing), func(t *testing.T) {
			dir := t.TempDir()
			want := make(map[string][]byte)
			if existing {
				old := seedOriginalSourceTree(t, dir)
				for _, name := range []string{multiwriterDirectory + "NOTICE", multiwriterDirectory + "custom.txt"} {
					want[name] = old[name]
				}
			}
			files := sourceTreeReplacementInputs()
			maps.Copy(want, files)
			verify := func() error {
				if _, err := os.Stat(filepath.Join(dir, multiwriterDirectory, "src/stale")); !os.IsNotExist(err) {
					t.Fatalf("verification sees stale source tree: %v", err)
				}
				for name, data := range files {
					got, err := os.ReadFile(filepath.Join(dir, name))
					if err != nil || string(got) != string(data) {
						t.Fatalf("verification sees %s = %q, error %v", name, got, err)
					}
				}
				return nil
			}
			if err := replaceFiles(dir, files, os.Rename, verify); err != nil {
				t.Fatal(err)
			}
			assertReplacementFiles(t, dir, want)
		})
	}
}

func TestReplaceFilesRestoresBothDependenciesAfterVerificationFailure(t *testing.T) {
	dir := t.TempDir()
	old := seedOriginalSourceTree(t, dir)
	files := sourceTreeReplacementInputs()
	failure := errors.New("native smoke failed")
	verified := false
	verify := func() error {
		verified = true
		for name, want := range files {
			got, err := os.ReadFile(filepath.Join(dir, name))
			if err != nil || string(got) != string(want) {
				t.Fatalf("verification sees %s = %q, error %v", name, got, err)
			}
		}
		if _, err := os.Stat(filepath.Join(dir, multiwriterDirectory, "src/stale")); !os.IsNotExist(err) {
			t.Fatalf("verification sees stale source tree: %v", err)
		}
		return failure
	}
	if err := replaceFiles(dir, files, os.Rename, verify); !errors.Is(err, failure) {
		t.Fatalf("replacement error = %v, want verification failure", err)
	}
	if !verified {
		t.Fatal("verification was not reached")
	}
	assertReplacementFiles(t, dir, old)
	assertOriginalSourceTreePermissions(t, filepath.Join(dir, multiwriterDirectory, "src"))
}

func TestReplaceFilesRollsBackSourceDirectorySwapFailures(t *testing.T) {
	for _, boundary := range []string{"save", "install"} {
		t.Run(boundary, func(t *testing.T) {
			dir := t.TempDir()
			old := seedOriginalSourceTree(t, dir)
			failure := errors.New("source directory rename failed")
			target := filepath.Join(dir, multiwriterDirectory, "src")
			rename := func(from, to string) error {
				if boundary == "save" && from == target ||
					boundary == "install" && to == target && strings.HasSuffix(from, ".new") {
					return failure
				}
				return os.Rename(from, to)
			}
			if err := replaceFiles(dir, sourceTreeReplacementInputs(), rename, func() error { return nil }); !errors.Is(err, failure) {
				t.Fatalf("replacement error = %v, want directory rename failure", err)
			}
			assertReplacementFiles(t, dir, old)
			assertOriginalSourceTreePermissions(t, target)
		})
	}
}

func TestReplaceFilesPreservesCompleteSourceTreeForRecovery(t *testing.T) {
	dir := t.TempDir()
	old := seedOriginalSourceTree(t, dir)
	verifyFailure := errors.New("native smoke failed")
	restoreFailure := errors.New("source directory restore failed")
	target := filepath.Join(dir, multiwriterDirectory, "src")
	rename := func(from, to string) error {
		if to == target && strings.HasSuffix(from, "src.old") {
			return restoreFailure
		}
		return os.Rename(from, to)
	}
	err := replaceFiles(dir, sourceTreeReplacementInputs(), rename, func() error { return verifyFailure })
	if !errors.Is(err, verifyFailure) || !errors.Is(err, restoreFailure) {
		t.Fatalf("replacement error = %v, want verification and restore failures", err)
	}
	stage := replacementRecoveryDirectory(t, dir, err)
	saved := filepath.Join(stage, multiwriterDirectory, "src.old")
	want := make(map[string][]byte)
	for name, data := range old {
		if after, ok := strings.CutPrefix(name, multiwriterDirectory+"src/"); ok {
			want[after] = data
		} else {
			got, readErr := os.ReadFile(filepath.Join(dir, name))
			if readErr != nil || string(got) != string(data) {
				t.Errorf("restored %s = %q, error %v", name, got, readErr)
			}
		}
	}
	assertReplacementFiles(t, saved, want)
	assertOriginalSourceTreePermissions(t, saved)
	if _, statErr := os.Lstat(target); !os.IsNotExist(statErr) {
		t.Fatalf("replacement source tree survived failed restoration: %v", statErr)
	}
	if err := os.Rename(saved, target); err != nil {
		t.Fatal(err)
	}
	if err := os.RemoveAll(stage); err != nil {
		t.Fatal(err)
	}
	assertReplacementFiles(t, dir, old)
}

func TestReplaceFilesRejectsInvalidPaths(t *testing.T) {
	for _, name := range []string{"", ".", "../escape.c", "/absolute.c", "./header.h", "nested/../header.h", "nested//header.h", "nested\\header.h"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			old := seedOriginals(t, dir, false)
			files := replacementInputs()
			files[name] = []byte("invalid")
			if err := replaceFiles(dir, files, os.Rename, func() error { return nil }); err == nil {
				t.Fatal("accepted noncanonical output path")
			}
			assertReplacementFiles(t, dir, old)
		})
	}
}

func TestReplaceFilesRejectsConflictingPaths(t *testing.T) {
	for _, name := range []string{"header.h", multiwriterDirectory + "src", multiwriterDirectory + "src/nested"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			files := map[string][]byte{name: []byte("file"), name + "/child.h": []byte("child")}
			if err := replaceFiles(dir, files, os.Rename, func() error { return nil }); err == nil {
				t.Fatal("accepted output file as another output's parent")
			}
			assertReplacementFiles(t, dir, nil)
		})
	}
}

func TestReplaceFilesRejectsFileAsDirectory(t *testing.T) {
	for _, name := range []string{"added", multiwriterDirectory + "src"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			old := map[string][]byte{name: []byte("not a directory")}
			seedReplacementFiles(t, dir, old)
			files := map[string][]byte{name + "/new.h": []byte("new header")}
			if err := replaceFiles(dir, files, os.Rename, func() error { return nil }); err == nil {
				t.Fatal("accepted regular file as output directory")
			}
			assertReplacementFiles(t, dir, old)
		})
	}
}

func TestReplaceFilesRejectsAliasedDirectories(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	root := t.TempDir()
	destination := filepath.Join(root, "destination")
	if err := os.Mkdir(destination, 0755); err != nil {
		t.Fatal(err)
	}
	original := filepath.Join(destination, "source.c")
	if err := os.WriteFile(original, []byte("original"), 0644); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(destination, alias); err != nil {
		t.Fatal(err)
	}
	for _, test := range []struct {
		dir, name string
	}{
		{root, "alias/source.c"},
		{alias, "source.c"},
	} {
		err := replaceFiles(test.dir, map[string][]byte{test.name: []byte("replacement")}, os.Rename, func() error { return nil })
		if err == nil {
			t.Fatal("accepted symlinked output directory")
		}
	}
	got, err := os.ReadFile(original)
	if err != nil || string(got) != "original" {
		t.Fatalf("aliased source changed: %q, %v", got, err)
	}
}

func TestReplaceFilesRejectsSymlinkedTargets(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	for _, name := range []string{"sqlite3.h", multiwriterDirectory + "src"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			outside := t.TempDir()
			seedReplacementFiles(t, outside, map[string][]byte{"original.h": []byte("original")})
			target := filepath.Join(dir, name)
			if err := os.MkdirAll(filepath.Dir(target), 0755); err != nil {
				t.Fatal(err)
			}
			link := outside
			candidate := name + "/new.h"
			if name == "sqlite3.h" {
				link = filepath.Join(outside, "original.h")
				candidate = name
			}
			if err := os.Symlink(link, target); err != nil {
				t.Fatal(err)
			}
			if err := replaceFiles(dir, map[string][]byte{candidate: []byte("new")}, os.Rename, func() error { return nil }); err == nil {
				t.Fatal("accepted symlinked output target")
			}
			if got, err := os.Readlink(target); err != nil || got != link {
				t.Fatalf("original symlink changed: %q, error %v", got, err)
			}
			assertReplacementFiles(t, outside, map[string][]byte{"original.h": []byte("original")})
		})
	}
}

func TestReplaceFilesNeverTraversesOldSourceSymlinks(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("symlink creation requires privileges on Windows")
	}
	for _, rollback := range []bool{false, true} {
		t.Run(fmt.Sprintf("rollback=%v", rollback), func(t *testing.T) {
			dir := t.TempDir()
			outside := t.TempDir()
			outsideFiles := map[string][]byte{"original.h": []byte("outside original")}
			seedReplacementFiles(t, outside, outsideFiles)
			tree := filepath.Join(dir, multiwriterDirectory, "src")
			if err := os.MkdirAll(tree, 0755); err != nil {
				t.Fatal(err)
			}
			link := filepath.Join(tree, "nested")
			if err := os.Symlink(outside, link); err != nil {
				t.Fatal(err)
			}
			files := map[string][]byte{multiwriterDirectory + "src/nested/new.h": []byte("new header")}
			failure := errors.New("verification failed")
			verify := func() error {
				if rollback {
					return failure
				}
				return nil
			}
			err := replaceFiles(dir, files, os.Rename, verify)
			if rollback {
				if !errors.Is(err, failure) {
					t.Fatalf("replacement error = %v, want verification failure", err)
				}
				if got, err := os.Readlink(link); err != nil || got != outside {
					t.Fatalf("source symlink not restored: %q, error %v", got, err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				assertReplacementFiles(t, dir, files)
			}
			assertReplacementFiles(t, outside, outsideFiles)
		})
	}
}
