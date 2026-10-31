package main

import (
	"context"
	"crypto/sha3"
	"fmt"
	"io"
	"net/http"
)

const maxDownloadBytes = 32 << 20
const maxExtractedBytes = 64 << 20

func digest(data []byte) string { return fmt.Sprintf("%x", sha3.Sum256(data)) }

func isLowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, c := range value {
		if !('0' <= c && c <= '9') && !('a' <= c && c <= 'f') {
			return false
		}
	}
	return true
}

func download(ctx context.Context, client *http.Client, url string) ([]byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	response, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("download %s: %w", url, err)
	}
	defer response.Body.Close()
	if response.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("download %s: %s", url, response.Status)
	}
	data, err := io.ReadAll(io.LimitReader(response.Body, maxDownloadBytes+1))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", url, err)
	}
	if len(data) > maxDownloadBytes {
		return nil, fmt.Errorf("download %s exceeds %d bytes", url, maxDownloadBytes)
	}
	return data, nil
}
