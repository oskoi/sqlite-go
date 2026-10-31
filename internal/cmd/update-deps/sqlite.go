package main

import (
	"archive/zip"
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strconv"
	"strings"
)

// sqliteArchiveVersion follows the 3XXYY00 / 3XXYYZZ download naming
// convention documented at https://www.sqlite.org/download.html#encoding.
func sqliteArchiveVersion(version string) (string, error) {
	parts := strings.Split(version, ".")
	if (len(parts) != 3 && len(parts) != 4) || parts[0] != "3" {
		return "", fmt.Errorf("SQLite version %q must be a canonical SQLite 3 release with three or four numeric components", version)
	}
	var components [4]int
	components[0] = 3
	for i := 1; i < len(parts); i++ {
		part := parts[i]
		if len(part) == 0 || len(part) > 2 || (len(part) > 1 && part[0] == '0') {
			return "", fmt.Errorf("SQLite version %q has a noncanonical component; download components must be 0..99 without leading zeroes", version)
		}
		for _, digit := range part {
			if digit < '0' || digit > '9' {
				return "", fmt.Errorf("SQLite version %q has a nonnumeric component", version)
			}
		}
		n, err := strconv.Atoi(part)
		if err != nil {
			return "", fmt.Errorf("SQLite version %q: %w", version, err)
		}
		components[i] = n
	}
	if len(parts) == 4 && components[3] == 0 {
		return "", fmt.Errorf("SQLite version %q must omit a zero fourth component", version)
	}
	return fmt.Sprintf("3%02d%02d%02d", components[1], components[2], components[3]), nil
}

func (pin sqlitePin) validate() error {
	if _, err := sqliteArchiveVersion(pin.Version); err != nil {
		return err
	}
	if pin.Year < 1000 || pin.Year > 9999 {
		return fmt.Errorf("SQLite year %d must have four digits", pin.Year)
	}
	if !isLowerHex(pin.SHA3, 64) {
		return fmt.Errorf("SQLite archive SHA3-256 must be 64 lowercase hexadecimal characters")
	}
	return nil
}

func fetchSQLite(ctx context.Context, client *http.Client, pin sqlitePin) (map[string][]byte, error) {
	encoded, err := sqliteArchiveVersion(pin.Version)
	if err != nil {
		return nil, err
	}
	root := "sqlite-amalgamation-" + encoded
	archiveURL := fmt.Sprintf("https://www.sqlite.org/%d/%s.zip", pin.Year, root)
	archive, err := download(ctx, client, archiveURL)
	if err != nil {
		return nil, err
	}
	if archiveHash := digest(archive); archiveHash != pin.SHA3 {
		return nil, fmt.Errorf("SQLite archive SHA3-256 mismatch: got %s, configured %s", archiveHash, pin.SHA3)
	}
	return sqliteExtract(archive, root)
}

func sqliteExtract(archive []byte, root string) (map[string][]byte, error) {
	reader, err := zip.NewReader(bytes.NewReader(archive), int64(len(archive)))
	if err != nil {
		return nil, fmt.Errorf("open SQLite amalgamation ZIP: %w", err)
	}
	files := make(map[string][]byte, 2)
	remaining := int64(maxExtractedBytes)
	for _, member := range reader.File {
		name := path.Base(strings.TrimSuffix(strings.ReplaceAll(member.Name, "\\", "/"), "/"))
		if name != "sqlite3.c" && name != "sqlite3.h" {
			continue
		}
		if member.Name != root+"/"+name || !member.Mode().IsRegular() {
			return nil, fmt.Errorf("invalid SQLite archive member %q: expected regular file %s/%s", member.Name, root, name)
		}
		if _, exists := files[name]; exists {
			return nil, fmt.Errorf("duplicate SQLite archive member %q", member.Name)
		}
		if member.UncompressedSize64 > uint64(remaining) {
			return nil, fmt.Errorf("SQLite archive member %q exceeds extraction limit", member.Name)
		}
		file, err := member.Open()
		if err != nil {
			return nil, fmt.Errorf("open SQLite archive member %q: %w", member.Name, err)
		}
		data, readErr := io.ReadAll(io.LimitReader(file, remaining+1))
		closeErr := file.Close()
		if readErr != nil {
			return nil, fmt.Errorf("read SQLite archive member %q: %w", member.Name, readErr)
		}
		if closeErr != nil {
			return nil, fmt.Errorf("close SQLite archive member %q: %w", member.Name, closeErr)
		}
		if int64(len(data)) > remaining {
			return nil, fmt.Errorf("SQLite archive member %q exceeds extraction limit", member.Name)
		}
		remaining -= int64(len(data))
		files[name] = data
	}
	for _, name := range []string{"sqlite3.c", "sqlite3.h"} {
		if _, exists := files[name]; !exists {
			return nil, fmt.Errorf("SQLite archive is missing %s", name)
		}
	}
	return files, nil
}
