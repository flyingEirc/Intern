package local

import (
	"context"
	"crypto/rand"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path"
	"sync"

	"github.com/flyingEirc/Intern/internal/fsutil"
	"github.com/flyingEirc/Intern/internal/pipeline"
)

type stagingState uint8

const (
	stagingOpen stagingState = iota
	stagingPublished
	stagingAborted
)

// Staging owns a private pending directory until Publish or Abort.
type Staging struct {
	repo    *Repository
	dir     string
	pending string
	sinks   []*fileSink
	state   stagingState
	// mu serializes this staging directory's sink list, lifecycle, and data-path
	// I/O (Write and Commit fsync). Methods that also need Repository.mu acquire
	// this mutex first, so a waiter does not hold the repository lock.
	//
	// Current callers write one pipeline stream, or commit several sinks one
	// after another. A stage may use worker goroutines only when those workers
	// serialize each writer's Write and join before Close. Sharing mu across
	// sinks is that serial contract. It does not make overlapping hash writes
	// safe, and it is not a per-sink lock. Abort waits until an in-flight Write
	// or Sync returns; a syscall stuck in the kernel keeps running. Parallel
	// streams in one staging directory are a later stage, not today's chain.
	mu sync.Mutex
}

// Begin creates a 0700 pending directory on the repository filesystem.
// dir is the clean repository-relative directory to be created by Publish.
// If that file or directory already exists, Begin returns fs.ErrExist and
// creates no pending directory. Publish still refuses a path that appears
// later, with a non-replacing rename.
func (r *Repository) Begin(dir string) (*Staging, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed {
		return nil, fs.ErrClosed
	}
	if err := r.checkPath(dir, true); err != nil {
		return nil, err
	}
	_, err := r.root.Lstat(dir)
	if err == nil {
		return nil, fmt.Errorf("publish target %s: %w", dir, fs.ErrExist)
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, err
	}
	pending := ".pending-" + rand.Text()
	if err := r.root.Mkdir(pending, 0o700); err != nil {
		return nil, err
	}
	return &Staging{repo: r, dir: dir, pending: pending}, nil
}

// Create exclusively creates a 0600 file in the pending directory.
// name must be one non-reserved path component.
func (s *Staging) Create(name string) (pipeline.Sink, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo.mu.Lock()
	defer s.repo.mu.Unlock()
	if err := validName(name); err != nil {
		return nil, err
	}
	if s.state != stagingOpen {
		return nil, fs.ErrClosed
	}
	file, err := s.repo.root.OpenFile(path.Join(s.pending, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return nil, err
	}
	sink := &fileSink{staging: s, file: file, name: name}
	s.sinks = append(s.sinks, sink)
	return sink, nil
}

// Publish makes the entire staging directory visible without replacing a target.
// Every created sink must have committed. Cancellation is observed before the
// exclusive rename. After that rename and its directory sync succeed, a later
// cancellation leaves the directory in place. A real directory sync error tries
// to restore the pending name. If that restore rename fails, the published
// directory stays where it is and Abort does not remove it.
//
// The private pending directory is synced before Repository.mu is taken. This
// staging lock stays held, and os.Root stays open until Publish returns.
// Parent creation, the rename, and the parent and root sync still hold
// Repository.mu, so readers block for that section. A slow disk sync runs to
// completion; the context does not cancel it.
func (s *Staging) Publish(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return err
	}
	if err := s.repo.pin(); err != nil {
		return err
	}
	defer s.repo.unpin()
	if err := syncDirectory(s.repo.root, s.pending); err != nil {
		return err
	}
	if err := ctx.Err(); err != nil {
		return err
	}

	s.repo.mu.Lock()
	defer s.repo.mu.Unlock()
	if s.repo.closed {
		return fs.ErrClosed
	}
	if err := s.repo.makeParents(s.dir); err != nil {
		return err
	}
	if err := s.repo.checkPath(s.dir, true); err != nil {
		return err
	}
	published, err := s.move(ctx)
	if published {
		s.state = stagingPublished
	}
	return err
}

func (s *Staging) ready(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	if s.state != stagingOpen {
		return fs.ErrClosed
	}
	for _, sink := range s.sinks {
		if sink.state != fileCommitted {
			return errors.New("all staging sinks must commit before publication")
		}
	}
	return nil
}

func (s *Staging) move(ctx context.Context) (published bool, result error) {
	rootDir, err := s.repo.root.Open(".")
	if err != nil {
		return false, err
	}
	defer fsutil.Close(&result, rootDir)
	parent, err := s.repo.root.Open(path.Dir(s.dir))
	if err != nil {
		return false, err
	}
	defer fsutil.Close(&result, parent)
	if err = ctx.Err(); err != nil {
		return false, err
	}
	if err = renameExclusive(rootDir, s.pending, parent, path.Base(s.dir)); err != nil {
		return false, fmt.Errorf("publish staging directory: %w", err)
	}
	if err = syncRenamed(s.dir, rootDir, parent); err != nil {
		return s.restorePending(parent, rootDir, err)
	}
	return true, nil
}

// syncRenamed durably records a rename that has already succeeded.
// The caller observes cancellation only before that rename.
func syncRenamed(dir string, rootDir, parent *os.File) error {
	err := parent.Sync()
	if path.Dir(dir) != "." {
		err = errors.Join(err, rootDir.Sync())
	}
	return err
}

// restorePending moves a renamed directory back to its private pending name.
// The bool is false when the pending name is restored. When the restore rename
// fails, the directory is already at its final path and the bool is true so
// the caller can keep Abort away from it.
func (s *Staging) restorePending(parent, rootDir *os.File, cause error) (bool, error) {
	rollbackErr := renameExclusive(parent, path.Base(s.dir), rootDir, s.pending)
	if rollbackErr == nil {
		rollbackErr = errors.Join(parent.Sync(), rootDir.Sync())
		return false, errors.Join(cause, rollbackErr)
	}
	return true, errors.Join(cause, fmt.Errorf("restore pending directory: %w", rollbackErr))
}

// Abort closes all sinks and recursively removes only the pending directory.
// It is idempotent and never changes an already published directory.
func (s *Staging) Abort() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo.mu.Lock()
	defer s.repo.mu.Unlock()
	if s.state != stagingOpen {
		return nil
	}
	var result error
	for _, sink := range s.sinks {
		result = errors.Join(result, sink.closeFile())
		sink.state = fileAborted
	}
	err := s.repo.root.RemoveAll(s.pending)
	if err == nil {
		s.state = stagingAborted
	}
	return errors.Join(result, err)
}
