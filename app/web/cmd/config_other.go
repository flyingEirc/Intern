//go:build !unix

package cmd

import (
	"os"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

func openConfig(path string) (*os.File, error) {
	return fsutil.Open(path)
}

func openDatabaseSecret(path string) (*os.File, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, &os.PathError{Op: "open", Path: path, Err: os.ErrPermission}
	}
	return fsutil.Open(path)
}

func setConfigBlocking(*os.File) error { return nil }
