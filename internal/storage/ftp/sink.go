package ftp

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net/textproto"
	"path"
	"strings"
	"sync"

	client "github.com/jlaffaye/ftp"

	"github.com/flyingEirc/Intern/internal/pipeline"
)

type sinkState uint8

const (
	sinkOpen sinkState = iota
	sinkFailed
	sinkCommitted
	sinkAborted
)

// Sink streams exactly one object. TemporaryPath is available before any STOR,
// so the caller can persist cleanup responsibility before sending payload bytes.
type Sink struct {
	repo      *Repository
	key       string
	part      string
	ctx       context.Context
	cancel    context.CancelFunc
	stopRepo  func() bool
	stopPipe  func() bool
	mu        sync.Mutex
	state     sinkState
	size      int64
	session   *session
	writer    *io.PipeWriter
	done      chan struct{}
	uploadErr error
	writeErr  error
	abortErr  error
}

var _ pipeline.Sink = (*Sink)(nil)

func (s *Sink) TemporaryPath() string { return s.part }

func (r *Repository) parents(s *session, key string) error {
	current := r.cfg.InstanceID
	for part := range strings.SplitSeq(strings.TrimPrefix(path.Dir(key), r.cfg.InstanceID), "/") {
		if part == "" {
			continue
		}
		current = path.Join(current, part)
		if err := ensureDirectory(s.conn, r.absolute(current)); err != nil {
			return err
		}
	}
	return nil
}

func ensureDirectory(conn *client.ServerConn, name string) error {
	entry, err := lookup(conn, name)
	if err != nil {
		return err
	}
	if entry == nil {
		makeErr := conn.MakeDir(name)
		entry, err = lookup(conn, name)
		if err != nil || makeErr != nil && entry == nil {
			return errors.Join(makeErr, err)
		}
		if entry == nil {
			return fs.ErrNotExist
		}
	}
	if entry.Type != client.EntryTypeFolder {
		return ErrOwnership
	}
	return nil
}

func (r *Repository) absent(s *session, key string) error {
	entry, err := lookup(s.conn, r.absolute(key))
	if err != nil {
		return err
	}
	if entry != nil {
		return fs.ErrExist
	}
	return nil
}

// Create checks ownership and the destination but does not open a payload
// connection. Empty objects start their STOR only when Commit is called.
// ctx owns the entire sink lifetime, including Write and Commit; callers must
// use their run/transfer context, not a short-lived setup-only context.
func (r *Repository) Create(ctx context.Context, key string) (*Sink, error) {
	if err := r.key(key, false, false); err != nil {
		return nil, failure("create", err)
	}
	err := r.operation(ctx, "create", func(session *session) error {
		if err := r.parents(session, key); err != nil {
			return err
		}
		return r.absent(session, key)
	})
	if err != nil {
		return nil, err
	}
	ctx, cancel := context.WithCancel(ctx)
	sink := &Sink{repo: r, key: key, part: temporaryKey(path.Dir(key)), ctx: ctx, cancel: cancel}
	sink.stopRepo = context.AfterFunc(r.ctx, cancel)
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.closed || ctx.Err() != nil {
		sink.stopRepo()
		cancel()
		if r.closed {
			return nil, failure("create", fs.ErrClosed)
		}
		return nil, failure("create", ctx.Err())
	}
	r.sinks[sink.part] = sink
	r.parts[sink.part] = true
	return sink, nil
}

func (s *Sink) start() error {
	if s.session != nil {
		return nil
	}
	session, err := s.repo.connect(s.ctx)
	if err != nil {
		return err
	}
	if err = s.repo.owner(session); err == nil {
		err = directory(session.conn, path.Dir(s.repo.absolute(s.part)))
	}
	if err == nil {
		err = s.repo.absent(session, s.part)
	}
	if err != nil {
		err = session.result(err)
		session.finish()
		return err
	}
	s.session = session
	reader, writer := io.Pipe()
	s.writer = writer
	s.done = make(chan struct{})
	s.stopPipe = context.AfterFunc(session.ctx, func() {
		err := session.ctx.Err()
		_ = writer.CloseWithError(err) //nolint:errcheck // io.Pipe.CloseWithError always returns nil.
		_ = reader.CloseWithError(err) //nolint:errcheck // io.Pipe.CloseWithError always returns nil.
	})
	go func() {
		err := session.conn.Stor(s.repo.absolute(s.part), reader)
		s.uploadErr = session.result(err)
		_ = reader.CloseWithError(failure("upload", s.uploadErr)) //nolint:errcheck // io.Pipe.CloseWithError always returns nil.
		if s.uploadErr != nil {
			session.finish()
		}
		close(s.done)
	}()
	return nil
}

func (s *Sink) Write(p []byte) (int, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != sinkOpen {
		return 0, failure("write", fs.ErrClosed)
	}
	if err := s.start(); err != nil {
		s.state = sinkFailed
		s.writeErr = err
		return 0, failure("write", err)
	}
	n, err := s.writer.Write(p)
	s.size += int64(n)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	if err != nil {
		// Waiting for Stor also retains a quota/permission final reply which
		// might arrive after the data socket fails.
		<-s.done
		err = errors.Join(err, s.uploadErr, s.ctx.Err())
		s.state = sinkFailed
		s.writeErr = err
	}
	return n, failure("write", err)
}

// Commit waits for data Close and the final STOR reply, checks exact SIZE,
// rechecks ownership/target and serializes publication. An acknowledged rename
// is final even if either context is canceled immediately afterwards.
func (s *Sink) Commit(ctx context.Context) (pipeline.Receipt, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state != sinkOpen {
		return pipeline.Receipt{}, failure("commit", errors.Join(fs.ErrClosed, s.writeErr))
	}
	stopCommit := context.AfterFunc(ctx, s.cancel)
	defer stopCommit()
	s.state = sinkFailed
	if err := errors.Join(ctx.Err(), s.ctx.Err()); err != nil {
		return pipeline.Receipt{}, failure("commit", err)
	}
	if err := s.start(); err != nil {
		return pipeline.Receipt{}, failure("commit", err)
	}
	defer s.finishSession()
	closeErr := s.writer.Close()
	<-s.done
	if err := errors.Join(closeErr, s.uploadErr, ctx.Err(), s.ctx.Err()); err != nil {
		return pipeline.Receipt{}, failure("commit", err)
	}
	s.repo.publication.Lock()
	defer s.repo.publication.Unlock()
	if err := s.publish(ctx); err != nil {
		return pipeline.Receipt{}, failure("commit", err)
	}
	s.state = sinkCommitted
	s.release()
	return pipeline.Receipt{Path: s.key, Size: s.size}, nil
}

func (s *Sink) publish(ctx context.Context) error {
	if err := s.repo.owner(s.session); err != nil {
		return err
	}
	if err := directory(s.session.conn, path.Dir(s.repo.absolute(s.key))); err != nil {
		return err
	}
	if err := s.repo.absent(s.session, s.key); err != nil {
		return err
	}
	size, err := s.session.conn.FileSize(s.repo.absolute(s.part))
	if err != nil {
		return err
	}
	if size != s.size {
		return io.ErrShortWrite
	}
	if err = errors.Join(ctx.Err(), s.ctx.Err()); err != nil {
		return err
	}
	err = s.session.conn.Rename(s.repo.absolute(s.part), s.repo.absolute(s.key))
	if err == nil {
		return nil
	}
	var reply *textproto.Error
	if !errors.As(err, &reply) {
		// Rename combines RNFR and RNTO. Without a definitive reply we
		// conservatively retain the possible final object for readback.
		err = errors.Join(ErrCommitUncertain, err)
	}
	return s.session.result(err)
}

func (s *Sink) finishSession() {
	if s.stopPipe != nil {
		s.stopPipe()
	}
	if s.session != nil {
		s.session.finish()
	}
}

func (s *Sink) release() {
	s.stopRepo()
	s.cancel()
	s.repo.mu.Lock()
	delete(s.repo.sinks, s.part)
	delete(s.repo.parts, s.part)
	s.repo.mu.Unlock()
}

// Abort interrupts a blocked Write before acquiring its lock, drains Stor and
// deletes only its own temporary key. A failed cleanup is handed off once;
// repeated Abort calls return the same outcome without another network attempt.
func (s *Sink) Abort() error {
	s.cancel()
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.state == sinkCommitted {
		return nil
	}
	if s.state == sinkAborted {
		return s.abortErr
	}
	if s.done != nil {
		<-s.done
	}
	s.finishSession()
	s.state = sinkAborted
	s.release()
	if s.session != nil {
		s.abortErr = s.repo.CleanupPart(context.Background(), s.part)
	}
	return s.abortErr
}
