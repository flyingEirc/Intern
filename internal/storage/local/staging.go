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
	mu sync.Mutex
}

// Begin creates a 0700 pending directory on the repository filesystem.
// dir is the clean repository-relative directory to be created by Publish.
func (r *Repository) Begin(dir string) (*Staging, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if err := r.checkPath(dir, true); err != nil {
		return nil, err
	}
	pending := ".pending-" + rand.Text()
	if err := r.root.Mkdir(pending, 0700); err != nil {
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
	file, err := s.repo.root.OpenFile(path.Join(s.pending, name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return nil, err
	}
	sink := &fileSink{staging: s, file: file, name: name}
	s.sinks = append(s.sinks, sink)
	return sink, nil
}

// Publish makes the entire staging directory visible without replacing a target.
// Every created sink must have committed; cancellation or failure leaves Abort safe.
func (s *Staging) Publish(ctx context.Context) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.repo.mu.Lock()
	defer s.repo.mu.Unlock()
	if err := s.ready(ctx); err != nil {
		return err
	}
	if err := syncDirectory(s.repo.root, s.pending); err != nil {
		return err
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
	if err = s.syncRename(ctx, rootDir, parent); err != nil {
		rollbackErr := renameExclusive(parent, path.Base(s.dir), rootDir, s.pending)
		if rollbackErr == nil {
			rollbackErr = errors.Join(parent.Sync(), rootDir.Sync())
			return false, errors.Join(err, rollbackErr)
		}
		return true, errors.Join(err, fmt.Errorf("restore pending directory: %w", rollbackErr))
	}
	return true, nil
}

func (s *Staging) syncRename(ctx context.Context, rootDir, parent *os.File) error {
	err := parent.Sync()
	if path.Dir(s.dir) != "." {
		err = errors.Join(err, rootDir.Sync())
	}
	return errors.Join(err, ctx.Err())
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
