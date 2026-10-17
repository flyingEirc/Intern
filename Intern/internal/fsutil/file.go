// Package fsutil confines file opens to their selected directory and preserves cleanup errors.
package fsutil

import (
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
)

// Open opens a file without allowing its final component to escape through a symlink.
func Open(path string) (*os.File, error) { return OpenFile(path, os.O_RDONLY, 0) }

// OpenFile pins the parent directory before resolving the final path component.
func OpenFile(path string, flags int, mode fs.FileMode) (*os.File, error) {
	root, err := os.OpenRoot(filepath.Dir(path))
	if err != nil {
		return nil, err
	}
	f, openErr := root.OpenFile(filepath.Base(path), flags, mode)
	if err = errors.Join(openErr, root.Close()); err != nil {
		if f != nil {
			err = errors.Join(err, f.Close())
		}
		return nil, err
	}
	return f, nil
}

// Close appends every close error to the caller's named return value, in argument order.
func Close(result *error, closers ...io.Closer) {
	for _, closer := range closers {
		*result = errors.Join(*result, closer.Close())
	}
}
