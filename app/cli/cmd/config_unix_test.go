//go:build unix

package cmd

import (
	"path/filepath"
	"syscall"
	"testing"
)

// configFIFO returns the path of a FIFO that has no write end.
func configFIFO(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "config.fifo")
	if err := syscall.Mkfifo(path, 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}
