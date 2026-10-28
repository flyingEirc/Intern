package restore

import (
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
)

// CheckExportRoot admits an export root only when it is an existing absolute
// non-symlink directory outside every forbidden root and outside the KEK. It
// returns a fixed diagnostic without echoing any path.
func CheckExportRoot(root string, forbidden ...string) error {
	resolved, err := resolveExportRoot(root)
	if err != nil {
		return err
	}
	return checkExportOutside(resolved, forbidden)
}

func resolveExportRoot(root string) (string, error) {
	if !filepath.IsAbs(root) || strings.ContainsAny(root, "\x00\r\n") || filepath.Clean(root) != root {
		return "", errors.New("restore: export root must be a clean absolute path")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return "", errors.New("restore: export root must exist")
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return "", errors.New("restore: export root must be a non-symlink directory")
	}
	resolved, err := canonicalPath(root)
	if err != nil {
		return "", errors.New("restore: export root cannot be resolved")
	}
	return resolved, nil
}

func checkExportOutside(resolved string, forbidden []string) error {
	for _, other := range forbidden {
		if other == "" {
			continue
		}
		candidate, err := canonicalPath(other)
		if err != nil {
			return errors.New("restore: a restricted root cannot be resolved")
		}
		if pathWithin(resolved, candidate) {
			return errors.New("restore: export root must be outside protected directories")
		}
	}
	return nil
}

// validateKeyOutsideExport ensures the KEK file is not below the export root.
func validateKeyOutsideExport(keyFile, exportRoot string) error {
	if keyFile == "" {
		return nil
	}
	key, err := canonicalPath(keyFile)
	if err != nil {
		return errors.New("restore: the KEK cannot be resolved")
	}
	root, err := canonicalPath(exportRoot)
	if err != nil {
		return errors.New("restore: export root cannot be resolved")
	}
	if pathWithin(key, root) {
		return errors.New("restore: export root must not contain the KEK")
	}
	return nil
}

// pathWithin reports whether child is parent or below it.
func pathWithin(child, parent string) bool {
	relative, err := filepath.Rel(parent, child)
	if err != nil {
		return false
	}
	return relative == "." || relative != ".." && !strings.HasPrefix(relative, ".."+string(os.PathSeparator))
}

// canonicalPath resolves symlinks in the longest existing prefix and appends the
// remaining components, matching how the backup paths are compared.
func canonicalPath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	current := absolute
	var suffix []string
	for {
		resolved, err := filepath.EvalSymlinks(current)
		if err == nil {
			for index := len(suffix) - 1; index >= 0; index-- {
				resolved = filepath.Join(resolved, suffix[index])
			}
			return resolved, nil
		}
		if !errors.Is(err, fs.ErrNotExist) {
			return "", err
		}
		parent := filepath.Dir(current)
		if parent == current {
			return "", err
		}
		suffix = append(suffix, filepath.Base(current))
		current = parent
	}
}

// validatePrivateWorkRoot requires an existing 0700 non-symlink directory.
func validatePrivateWorkRoot(root string) error {
	if !filepath.IsAbs(root) || strings.ContainsAny(root, "\x00\r\n") || filepath.Clean(root) != root {
		return errors.New("restore: work root must be a clean absolute path")
	}
	info, err := os.Lstat(root)
	if err != nil {
		return errors.New("restore: work root must exist")
	}
	if info.Mode() != os.ModeDir|0o700 {
		return errors.New("restore: work root must be a private directory with mode 0700")
	}
	return nil
}

// validateExecutable requires an absolute regular executable file.
func validateExecutable(path, label string) error {
	if path == "" || !filepath.IsAbs(path) || strings.ContainsAny(path, "\x00\r\n") {
		return errors.New("restore: " + label + " must be an absolute path")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return errors.New("restore: " + label + " is unavailable")
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return errors.New("restore: " + label + " must be an executable file")
	}
	return nil
}
