package local

import (
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
)

func validPendingToken(token string) error {
	if len(token) == 0 || len(token) > 64 {
		return fmt.Errorf("pending token must contain 1–64 lowercase letters, digits, or hyphens: %w", fs.ErrInvalid)
	}
	for i := range len(token) {
		c := token[i]
		if (c < 'a' || c > 'z') && (c < '0' || c > '9') && c != '-' {
			return fmt.Errorf("invalid pending token: %w", fs.ErrInvalid)
		}
	}
	return nil
}

// ReclaimPending removes only the pending directory belonging to token, without
// following symlinks. It is idempotent and never removes a published directory.
// The caller must establish from its durable ledger that the token belongs to
// an inactive run; tokens must not be reused while cleanup remains outstanding.
// Removal is synced before returning, including retries after a failed sync.
func (r *Repository) ReclaimPending(token string) error {
	if err := validPendingToken(token); err != nil {
		return err
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fs.ErrClosed
	}
	pending := pendingPrefix + token
	info, err := r.root.Lstat(pending)
	if errors.Is(err, fs.ErrNotExist) {
		return syncDirectory(r.root, ".")
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("pending entry must be a non-symlink directory: %w", fs.ErrPermission)
	}
	if err := r.root.RemoveAll(pending); err != nil {
		return err
	}
	return syncDirectory(r.root, ".")
}

// ReclaimPublished durably removes exactly one published directory tree.
// The caller must establish ownership and that the object is safe to reclaim
// from its durable cleanup ledger. Pending paths and symlink components are
// refused. An absent directory is still synced so cleanup retries are durable.
func (r *Repository) ReclaimPublished(dir string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return fs.ErrClosed
	}
	if err := r.checkPath(dir, true); err != nil {
		return err
	}
	info, err := r.root.Lstat(dir)
	if err != nil && !errors.Is(err, fs.ErrNotExist) {
		return err
	}
	if err == nil {
		if !info.IsDir() {
			return fmt.Errorf("published entry must be a directory: %w", fs.ErrInvalid)
		}
		if removeErr := r.root.RemoveAll(dir); removeErr != nil {
			return removeErr
		}
	}
	return r.syncReclaimedParent(path.Dir(dir))
}

func (r *Repository) syncReclaimedParent(parent string) error {
	// A missing ancestor cannot be synced. Sync its nearest existing parent
	// instead, so recovery also succeeds when the whole path never existed.
	for parent != "." {
		err := syncDirectory(r.root, parent)
		if !errors.Is(err, fs.ErrNotExist) {
			return errors.Join(err, syncDirectory(r.root, "."))
		}
		parent = path.Dir(parent)
	}
	return syncDirectory(r.root, ".")
}
