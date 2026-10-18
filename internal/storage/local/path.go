package local

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"path/filepath"
	"strings"
)

const pendingPrefix = ".pending-"

func isPendingName(name string) bool {
	return len(name) >= len(pendingPrefix) && strings.EqualFold(name[:len(pendingPrefix)], pendingPrefix)
}

func validPath(name string) error {
	if name == "" || name == "." || filepath.IsAbs(name) || path.Clean(name) != name {
		return fmt.Errorf("repository path must be clean, nonempty and relative: %w", fs.ErrInvalid)
	}
	if !fs.ValidPath(name) || strings.ContainsAny(name, "\\\x00") {
		return fmt.Errorf("invalid repository path: %w", fs.ErrInvalid)
	}
	for component := range strings.SplitSeq(name, "/") {
		if isPendingName(component) {
			return fmt.Errorf("reserved pending component: %w", fs.ErrInvalid)
		}
	}
	return nil
}

func validName(name string) error {
	if err := validPath(name); err != nil {
		return err
	}
	if strings.ContainsRune(name, '/') {
		return fmt.Errorf("object name must be one path component: %w", fs.ErrInvalid)
	}
	return nil
}

func (r *Repository) checkPath(name string, missingOK bool) error {
	if err := validPath(name); err != nil {
		return err
	}
	current := ""
	for component := range strings.SplitSeq(name, "/") {
		current = path.Join(current, component)
		info, err := r.root.Lstat(current)
		if missingOK && errors.Is(err, fs.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("repository path contains a symlink: %w", fs.ErrPermission)
		}
	}
	return nil
}

func (r *Repository) makeParents(dir string) error {
	current := ""
	for component := range strings.SplitSeq(path.Dir(dir), "/") {
		if component == "." {
			continue
		}
		current = path.Join(current, component)
		err := r.root.Mkdir(current, 0700)
		if err == nil {
			if err = syncDirectory(r.root, path.Dir(current)); err != nil {
				return err
			}
		} else if !errors.Is(err, fs.ErrExist) {
			return err
		}
		if err = r.privateDirectory(current); err != nil {
			return err
		}
	}
	return nil
}

func (r *Repository) privateDirectory(name string) error {
	info, err := r.root.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode() != os.ModeDir|0700 {
		return fmt.Errorf("repository parent must be a directory with mode 0700: %w", fs.ErrPermission)
	}
	return nil
}
