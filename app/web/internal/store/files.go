package store

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/gofrs/flock"
)

func checkDirectory(dir string) error {
	if !filepath.IsAbs(dir) {
		return errors.New("metadata directory must be an absolute path")
	}
	st, err := os.Lstat(dir)
	if err != nil {
		return err
	}
	if !st.IsDir() || st.Mode().Perm()&0o077 != 0 {
		return errors.New("metadata directory must be a non-symlink private directory (chmod 700)")
	}
	return nil
}

func checkFile(path string) error {
	st, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !st.Mode().IsRegular() || st.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("metadata file %q must be a non-symlink private regular file (chmod 600)", filepath.Base(path))
	}
	return nil
}

func checkFileIfExists(path string) error {
	if err := checkFile(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func lockDirectory(dir string) (*flock.Flock, error) {
	if !filepath.IsAbs(dir) {
		return nil, errors.New("metadata directory must be an absolute path")
	}
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return nil, err
	}
	if err := checkDirectory(dir); err != nil {
		return nil, err
	}
	path := filepath.Join(dir, ".intern-web.lock")
	if err := checkFileIfExists(path); err != nil {
		return nil, err
	}
	lock := flock.New(path)
	ok, err := lock.TryLock()
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	if !ok {
		return nil, errors.Join(errors.New("metadata directory is already in use by another intern process"), lock.Close())
	}
	return lock, nil
}
