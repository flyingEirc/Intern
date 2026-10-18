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
	// Sinks in one staging share that mutex and are written one at a time.
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
	// Sync and Close run to completion. Cancellation before Sync fails the commit.
	// After both succeed, the receipt is returned even if the context ended during
	// the syscall. A real Sync or Close error leaves the file uncommitted.
	s.staging.mu.Lock()
	defer s.staging.mu.Unlock()
	if s.state != fileOpen || s.staging.state != stagingOpen {
		return pipeline.Receipt{}, fs.ErrClosed
	}
	if err := ctx.Err(); err != nil {
		s.state = fileFailed
		return pipeline.Receipt{}, errors.Join(err, s.closeFile())
	}
	s.state = fileFailed
	err := errors.Join(s.file.Sync(), s.closeFile())
	if err != nil {
		return pipeline.Receipt{}, err
	}
	s.state = fileCommitted
	return pipeline.Receipt{Path: path.Join(s.staging.dir, s.name), Size: s.size}, nil
}

func (s *fileSink) Abort() error {
	// Staging.mu first: Abort waits out Write or Commit without holding the
	// repository lock, so List, Open, and Size stay available.
	// The file is closed only after that Write or Sync returns. A syscall
	// blocked in the kernel, including a stuck NFS RPC, runs to completion.
	// Callers write one pipeline stream, or commit several sinks one after
	// another. A per-sink lock becomes relevant only when one staging directory
	// has parallel streams; that caller does not exist. The wait is cancellation
	// latency, and the bytes already given to the kernel stay on the sink.
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
