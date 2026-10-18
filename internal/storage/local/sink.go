package local

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path"

	"github.com/flyingEirc/Intern/internal/pipeline"
)

type fileState uint8

const (
	fileOpen fileState = iota
	fileCommitted
	fileFailed
	fileAborted
)

type fileSink struct {
	staging *Staging
	file    *os.File
	name    string
	size    int64
	state   fileState
}

func (s *fileSink) Write(p []byte) (int, error) {
	// Data-path I/O uses Staging.mu only; Repository.mu stays free for readers.
	s.staging.mu.Lock()
	defer s.staging.mu.Unlock()
	if s.state != fileOpen || s.staging.state != stagingOpen {
		return 0, fs.ErrClosed
	}
	n, err := s.file.Write(p)
	s.size += int64(n)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	if err != nil {
		s.state = fileFailed
	}
	return n, err
}

func (s *fileSink) Commit(ctx context.Context) (pipeline.Receipt, error) {
	// fsync and close stay on Staging.mu so a multi-GB commit does not lock List.
	s.staging.mu.Lock()
	defer s.staging.mu.Unlock()
	if s.state != fileOpen || s.staging.state != stagingOpen {
		return pipeline.Receipt{}, fs.ErrClosed
	}
	s.state = fileFailed
	if err := ctx.Err(); err != nil {
		return pipeline.Receipt{}, errors.Join(err, s.closeFile())
	}
	err := s.file.Sync()
	err = errors.Join(err, s.closeFile(), ctx.Err())
	if err != nil {
		return pipeline.Receipt{}, err
	}
	s.state = fileCommitted
	return pipeline.Receipt{Path: path.Join(s.staging.dir, s.name), Size: s.size}, nil
}

func (s *fileSink) Abort() error {
	// Staging.mu first: Abort waits out Write or Commit without holding the
	// repository lock, so List, Open, and Size stay available.
	s.staging.mu.Lock()
	defer s.staging.mu.Unlock()
	s.staging.repo.mu.Lock()
	defer s.staging.repo.mu.Unlock()
	if s.staging.state != stagingOpen || s.state == fileAborted {
		return nil
	}
	closeErr := s.closeFile()
	err := s.staging.repo.root.Remove(path.Join(s.staging.pending, s.name))
	if errors.Is(err, fs.ErrNotExist) {
		err = nil
	}
	if err == nil {
		s.state = fileAborted
	}
	return errors.Join(closeErr, err)
}

func (s *fileSink) closeFile() error {
	if s.file == nil {
		return nil
	}
	err := s.file.Close()
	s.file = nil
	return err
}
