package main

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"io"
	"net/http"
	"path"
	"strings"
)

const multiwriterDirectory = "libs/sqlite-multiwriter/"

func (pin multiwriterPin) validate() error {
	if strings.TrimSpace(pin.Version) == "" {
		return fmt.Errorf("Multiwriter version label must not be blank")
	}
	if !isLowerHex(pin.Commit, 40) {
		return fmt.Errorf("invalid Multiwriter commit %q: want 40 lowercase hexadecimal characters", pin.Commit)
	}
	return nil
}

func fetchMultiwriter(ctx context.Context, client *http.Client, pin multiwriterPin) (map[string][]byte, error) {
	archiveURL := "https://codeload.github.com/sqliteai/sqlite-multiwriter/tar.gz/" + pin.Commit
	archive, err := download(ctx, client, archiveURL)
	if err != nil {
		return nil, err
	}
	return importMultiwriterArchive(archive, pin.Commit)
}

func importMultiwriterArchive(archive []byte, commit string) (map[string][]byte, error) {
	compressed, err := gzip.NewReader(bytes.NewReader(archive))
	if err != nil {
		return nil, fmt.Errorf("open Multiwriter archive: %w", err)
	}
	defer compressed.Close()
	limited := &io.LimitedReader{R: compressed, N: maxExtractedBytes + 1}
	reader := tar.NewReader(limited)
	root := "sqlite-multiwriter-" + commit
	files := make(map[string][]byte)
	var importedBytes int64
	for {
		header, err := reader.Next()
		if limited.N == 0 {
			return nil, fmt.Errorf("Multiwriter archive exceeds %d decompressed bytes", maxExtractedBytes)
		}
		if err == io.EOF {
			break
		}
		if err != nil {
			return nil, fmt.Errorf("read Multiwriter archive: %w", err)
		}
		// GitHub's git archives start with global PAX metadata, not a file.
		if header.Typeflag == tar.TypeXGlobalHeader {
			continue
		}
		name := header.Name
		if header.Typeflag == tar.TypeDir {
			name = strings.TrimSuffix(name, "/")
		}
		if name == "" || strings.ContainsAny(name, "\\:\x00") || strings.HasPrefix(name, "/") || path.Clean(name) != name || (name != root && !strings.HasPrefix(name, root+"/")) {
			return nil, fmt.Errorf("unsafe Multiwriter archive path %q", header.Name)
		}
		relative := strings.TrimPrefix(name, root+"/")
		relevant := relative == "LICENSE" || relative == "NOTICE" || strings.HasPrefix(relative, "src/") && (path.Ext(relative) == ".c" || path.Ext(relative) == ".h")
		if !relevant {
			continue
		}
		if header.Typeflag != tar.TypeReg && header.Typeflag != tar.TypeRegA {
			return nil, fmt.Errorf("nonregular Multiwriter archive member %q", header.Name)
		}
		destination := multiwriterDirectory + relative
		if _, exists := files[destination]; exists {
			return nil, fmt.Errorf("duplicate Multiwriter archive path %q", header.Name)
		}
		if header.Size < 0 || header.Size > maxExtractedBytes-importedBytes {
			return nil, fmt.Errorf("Multiwriter source files exceed %d bytes", maxExtractedBytes)
		}
		data, err := io.ReadAll(reader)
		if err != nil {
			return nil, fmt.Errorf("read Multiwriter source %s: %w", relative, err)
		}
		importedBytes += int64(len(data))
		files[destination] = data
	}
	// Consume the gzip trailer too: tar EOF alone does not validate its checksum.
	if _, err := io.Copy(io.Discard, limited); err != nil {
		return nil, fmt.Errorf("finish Multiwriter archive: %w", err)
	}
	if limited.N == 0 {
		return nil, fmt.Errorf("Multiwriter archive exceeds %d decompressed bytes", maxExtractedBytes)
	}
	for _, required := range []string{"LICENSE", "NOTICE", "src/multiwriter.h", "src/multiwriter_internal.h", "src/multiwriter_vfs.c"} {
		if len(files[multiwriterDirectory+required]) == 0 {
			return nil, fmt.Errorf("Multiwriter archive is missing required file %s", required)
		}
	}
	return files, nil
}
