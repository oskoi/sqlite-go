// Command update-deps imports the releases selected in versions.json.
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"maps"
	"net/http"
	"os"
	"os/exec"
	"os/signal"
	"path"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"time"
)

type sqlitePin struct {
	Version string `json:"version"`
	Year    int    `json:"year"`
	SHA3    string `json:"sha3_256"`
}

type multiwriterPin struct {
	Version string `json:"version"`
	Commit  string `json:"commit"`
}

type versions struct {
	SQLite      sqlitePin      `json:"sqlite"`
	Multiwriter multiwriterPin `json:"sqlite_multiwriter"`
}

func main() {
	flag.Usage = func() {
		fmt.Fprintln(flag.CommandLine.Output(), "Usage: go run ./internal/cmd/update-deps\nEdit versions.json, then run from the repository root. Imports both releases and verifies them before leaving changes to commit.")
	}
	flag.Parse()
	if flag.NArg() != 0 {
		flag.Usage()
		os.Exit(2)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt)
	defer stop()
	if err := update(ctx, ".", &http.Client{Timeout: 2 * time.Minute}); err != nil {
		fmt.Fprintln(os.Stderr, "update-deps:", err)
		os.Exit(1)
	}
}

func update(ctx context.Context, root string, client *http.Client) error {
	data, err := os.ReadFile(filepath.Join(root, "versions.json"))
	if err != nil {
		return fmt.Errorf("read versions.json (run from the repository root): %w", err)
	}
	var requested versions
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&requested); err != nil {
		return fmt.Errorf("decode versions.json: %w", err)
	}
	if err := decoder.Decode(new(any)); err != io.EOF {
		return fmt.Errorf("versions.json must contain one JSON object")
	}
	if err := requested.SQLite.validate(); err != nil {
		return fmt.Errorf("sqlite pin: %w", err)
	}
	if err := requested.Multiwriter.validate(); err != nil {
		return fmt.Errorf("sqlite_multiwriter pin: %w", err)
	}
	fmt.Printf("Downloading SQLite %s and sqlite-multiwriter %s (%s)\n", requested.SQLite.Version, requested.Multiwriter.Version, requested.Multiwriter.Commit)
	files, err := fetchSQLite(ctx, client, requested.SQLite)
	if err != nil {
		return fmt.Errorf("import SQLite: %w", err)
	}
	mwFiles, err := fetchMultiwriter(ctx, client, requested.Multiwriter)
	if err != nil {
		return fmt.Errorf("import sqlite-multiwriter: %w", err)
	}
	if err := checkTranslationUnits(root, mwFiles); err != nil {
		return err
	}
	maps.Copy(files, mwFiles)
	if err := replaceFiles(root, files, os.Rename, func() error { return verify(ctx, root) }); err != nil {
		return err
	}
	fmt.Printf("Updated SQLite %s and sqlite-multiwriter %s (%s); review the source diff and commit.\n", requested.SQLite.Version, requested.Multiwriter.Version, requested.Multiwriter.Commit)
	return nil
}

func checkTranslationUnits(root string, files map[string][]byte) error {
	wrappers, err := filepath.Glob(filepath.Join(root, "mw_*.c"))
	if err != nil {
		return err
	}
	included := make(map[string]bool)
	for _, name := range wrappers {
		source := "multiwriter_" + strings.TrimPrefix(filepath.Base(name), "mw_")
		included[multiwriterDirectory+"src/"+source] = true
	}
	var added, missing []string
	for name := range included {
		if _, exists := files[name]; !exists {
			missing = append(missing, name)
		}
	}
	for name := range files {
		if path.Ext(name) == ".c" && name != multiwriterDirectory+"src/multiwriter_os_win.c" && !included[name] {
			added = append(added, name)
		}
	}
	if len(included) == 0 || len(added) != 0 || len(missing) != 0 {
		slices.Sort(added)
		slices.Sort(missing)
		return fmt.Errorf("Multiwriter translation units need integration review: unwrapped=%v missing=%v (no files changed)", added, missing)
	}
	return nil
}

func verify(ctx context.Context, root string) (err error) {
	cache, err := os.MkdirTemp("", "sqlite-go-update-cache-")
	if err != nil {
		return fmt.Errorf("create verification build cache: %w", err)
	}
	defer func() {
		if cleanupErr := os.RemoveAll(cache); cleanupErr != nil {
			err = errors.Join(err, fmt.Errorf("remove verification build cache %s: %w", cache, cleanupErr))
		}
	}()
	commands := [][]string{
		{"test", "-count=1", "./..."},
		{"test", "-tags=sqlite_multiwriter", "-count=1", "./..."},
		{"vet", "./..."},
		{"vet", "-tags=sqlite_multiwriter", "./..."},
		{"run", "./internal/cmd/smoke"},
		{"run", "-tags=sqlite_multiwriter", "./internal/cmd/smoke"},
	}
	for _, args := range commands {
		fmt.Printf("+ go %s\n", strings.Join(args, " "))
		checkCtx, cancel := context.WithTimeout(ctx, 10*time.Minute)
		cmd := exec.CommandContext(checkCtx, "go", args...)
		cmd.Dir = root
		cmd.Env = append(os.Environ(), "GOCACHE="+cache, "CGO_ENABLED=1", "GOFLAGS=", "GOWORK=off", "GOOS="+runtime.GOOS, "GOARCH="+runtime.GOARCH)
		cmd.Stdout, cmd.Stderr = os.Stdout, os.Stderr
		err := cmd.Run()
		cancel()
		if err != nil {
			return fmt.Errorf("go %s: %w", strings.Join(args, " "), err)
		}
	}
	if runtime.GOOS == "windows" {
		fmt.Println("Multiwriter is disabled on Windows; its POSIX runtime still requires qualification on Linux or Darwin.")
	}
	return nil
}
