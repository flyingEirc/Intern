// Package local stores private files in a pending directory and publishes the
// entire directory with an atomic, non-replacing rename on Linux and Darwin.
//
// Open pins a 0700 repository root with os.Root. Repository paths use slash
// separators, are clean and relative, and cannot contain symlinks or reserved
// .pending- components. The reserved prefix is matched case-insensitively so
// unpublished staging directories stay hidden on case-insensitive filesystems.
// List returns sorted regular-file paths recursively under a directory prefix;
// an empty prefix lists the whole published repository.
//
// A caller must Commit every created sink before Publish, or Abort the staging
// directory on failure. Aborting a sink invalidates that staging publication.
// Close the repository only after its staging directories and readers are done.
// Close waits until an in-flight Publish has finished using os.Root.
package local

import (
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sync"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

// Repository confines all object access to a pinned private directory.
type Repository struct {
	// mu serializes namespace operations on the pinned root: Open, Size, List,
	// Delete, Begin, exclusive Create, Abort removal, and Close. Publish holds
	// mu for parent creation, the exclusive rename, and the parent/root
	// directory sync. Create and Abort acquire Staging.mu before mu and hold mu
	// until they return.
	//
	// Write and Commit fsync take only Staging.mu. They never acquire mu, so a
	// large object does not block readers of this Repository. While Create,
	// Publish, or Abort waits for Staging.mu it does not hold mu, and List,
	// Open, and Size proceed.
	//
	// Publish syncs its private pending directory before taking mu. That sync
	// holds Staging.mu and inflight. Close waits for inflight before it closes
	// os.Root, so List, Open, and Size do not wait for the pending sync. After
	// Publish takes mu, readers block until the namespace section returns.
	// Directory sync is a normal file syscall: canceling the caller context
	// does not interrupt it, and Close cannot cancel a syscall already inside
	// the kernel.
	mu        sync.Mutex
	root      *os.Root
	closed    bool
	inflight  sync.WaitGroup
	closeOnce sync.Once
	closeErr  error
}

// Open opens an absolute root, creating it with mode 0700 if necessary.
// Existing roots must be directories with exactly mode 0700, not symlinks.
// Platforms without exclusive directory rename fail closed with ErrUnsupported.
func Open(root string) (*Repository, error) {
	if err := unsupportedPlatform(); err != nil {
		return nil, err
	}
	if !filepath.IsAbs(root) {
		return nil, fmt.Errorf("repository root must be absolute: %w", fs.ErrInvalid)
	}
	root = filepath.Clean(root)
	if err := os.MkdirAll(root, 0700); err != nil {
		return nil, fmt.Errorf("create repository root: %w", err)
	}
	pinned, err := os.OpenRoot(root)
	if err != nil {
		return nil, fmt.Errorf("open repository root: %w", err)
	}
	if err = verifyRoot(root, pinned); err != nil {
		return nil, errors.Join(err, pinned.Close())
	}
	return &Repository{root: pinned}, nil
}

func verifyRoot(name string, root *os.Root) error {
	info, err := os.Lstat(name)
	if err != nil {
		return err
	}
	if info.Mode() != os.ModeDir|0700 {
		return fmt.Errorf("repository root must be a non-symlink directory with mode 0700: %w", fs.ErrPermission)
	}
	pinnedInfo, err := root.Stat(".")
	if err != nil {
		return err
	}
	if !os.SameFile(info, pinnedInfo) {
		return fmt.Errorf("repository root changed while opening: %w", fs.ErrPermission)
	}
	return nil
}

// Close releases the pinned repository directory after staging work has ended.
// It waits for a Publish that still holds a root pin, including the private
// pending-directory sync that runs without mu. A second Close returns the
// same result after the root has been closed.
func (r *Repository) Close() error {
	r.closeOnce.Do(func() {
		r.mu.Lock()
		r.closed = true
		r.mu.Unlock()
		r.inflight.Wait()
		r.mu.Lock()
		r.closeErr = r.root.Close()
		r.mu.Unlock()
	})
	return r.closeErr
}

// pin keeps os.Root open across work that does not hold mu.
// Close waits for every pin before closing the root.
func (r *Repository) pin() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fs.ErrClosed
	}
	r.inflight.Add(1)
	return nil
}

func (r *Repository) unpin() { r.inflight.Done() }

// Open opens a published regular file without following symlink components.
func (r *Repository) Open(name string) (file io.ReadCloser, result error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkPath(name, false); err != nil {
		return nil, err
	}
	f, err := r.root.Open(name)
	if err != nil {
		return nil, err
	}
	info, err := f.Stat()
	if err == nil && !info.Mode().IsRegular() {
		err = fmt.Errorf("object is not a regular file: %w", fs.ErrInvalid)
	}
	if err != nil {
		return nil, errors.Join(err, f.Close())
	}
	return f, nil
}

// Size returns the size of a published regular file in bytes.
func (r *Repository) Size(name string) (int64, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkPath(name, false); err != nil {
		return 0, err
	}
	info, err := r.root.Stat(name)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, fmt.Errorf("object is not a regular file: %w", fs.ErrInvalid)
	}
	return info.Size(), nil
}

// List returns sorted published regular-file paths beneath prefix.
// An empty prefix lists the entire repository; symlinks and pending entries are omitted.
func (r *Repository) List(prefix string) ([]string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	start := "."
	if prefix != "" {
		if err := r.checkPath(prefix, false); err != nil {
			return nil, err
		}
		start = prefix
	}
	files := make([]string, 0)
	err := fs.WalkDir(r.root.FS(), start, func(name string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if isPendingName(entry.Name()) {
			if entry.IsDir() {
				return fs.SkipDir
			}
			return nil
		}
		if entry.Type().IsRegular() {
			files = append(files, name)
		}
		return nil
	})
	return files, err
}

// Delete removes a published file or directory tree without following symlinks.
func (r *Repository) Delete(name string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkPath(name, false); err != nil {
		return err
	}
	return r.root.RemoveAll(name)
}

func syncDirectory(root *os.Root, name string) (result error) {
	dir, err := root.Open(name)
	if err != nil {
		return err
	}
	defer fsutil.Close(&result, dir)
	return dir.Sync()
}
