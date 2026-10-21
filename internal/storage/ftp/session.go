package ftp

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"net"
	"sync"
	"time"

	client "github.com/jlaffaye/ftp"
)

// A session has exactly one SDK caller. Cancellation touches only net.Conn,
// never ServerConn or Response, which the SDK does not make concurrent-safe.
type session struct {
	repo        *Repository
	ctx         context.Context
	cancel      context.CancelFunc
	stopParent  func() bool
	stopIO      func() bool
	conn        *client.ServerConn
	mu          sync.Mutex
	sockets     map[*deadlineConn]struct{}
	interrupted bool
	closeErr    error
	once        sync.Once
}

type deadlineConn struct {
	net.Conn
	owner    *session
	once     sync.Once
	closeErr error
}

func (c *deadlineConn) Read(p []byte) (int, error) {
	if err := c.SetReadDeadline(time.Now().Add(c.owner.repo.cfg.IdleTimeout)); err != nil {
		return 0, err
	}
	return c.Conn.Read(p)
}

func (c *deadlineConn) Write(p []byte) (int, error) {
	if err := c.SetWriteDeadline(time.Now().Add(c.owner.repo.cfg.IdleTimeout)); err != nil {
		return 0, err
	}
	n, err := c.Conn.Write(p)
	if n < len(p) && err == nil {
		err = io.ErrShortWrite
	}
	return n, err
}

func (c *deadlineConn) Close() error {
	c.once.Do(func() {
		c.closeErr = c.Conn.Close()
		c.owner.mu.Lock()
		delete(c.owner.sockets, c)
		c.owner.mu.Unlock()
	})
	return c.closeErr
}

func (r *Repository) connect(ctx context.Context) (*session, error) {
	r.mu.Lock()
	if r.closed {
		r.mu.Unlock()
		return nil, fs.ErrClosed
	}
	r.active.Add(1)
	r.mu.Unlock()
	ctx, cancel := context.WithCancel(ctx)
	s := &session{repo: r, ctx: ctx, cancel: cancel, sockets: make(map[*deadlineConn]struct{})}
	s.stopParent = context.AfterFunc(r.ctx, cancel)
	s.stopIO = context.AfterFunc(ctx, s.interrupt)
	// The SDK's MLSD parser treats unrecognized link types as regular files.
	// LIST retains link identity; -a also exposes our private ownership marker.
	conn, err := client.Dial(r.cfg.Address, client.DialWithDialFunc(s.dial),
		client.DialWithDisabledMLSD(true), client.DialWithForceListHidden(true))
	if err == nil {
		s.conn = conn
		err = conn.Login(r.cfg.Username, r.cfg.Password)
	}
	if err != nil {
		err = s.result(err)
		s.finish()
		return nil, err
	}
	return s, nil
}

func (s *session) dial(network, address string) (net.Conn, error) {
	ctx, cancel := context.WithTimeout(s.ctx, s.repo.cfg.DialTimeout)
	defer cancel()
	conn, err := s.repo.dialContext(ctx, network, address)
	if err != nil {
		return nil, err
	}
	wrapped := &deadlineConn{Conn: conn, owner: s}
	s.mu.Lock()
	if s.interrupted || s.ctx.Err() != nil {
		s.mu.Unlock()
		return nil, errors.Join(s.ctx.Err(), conn.Close())
	}
	s.sockets[wrapped] = struct{}{}
	s.mu.Unlock()
	return wrapped, nil
}

func (s *session) interrupt() {
	s.mu.Lock()
	s.interrupted = true
	sockets := make([]*deadlineConn, 0, len(s.sockets))
	for conn := range s.sockets {
		sockets = append(sockets, conn)
	}
	s.mu.Unlock()
	for _, conn := range sockets {
		if err := conn.Close(); err != nil {
			s.mu.Lock()
			s.closeErr = errors.Join(s.closeErr, err)
			s.mu.Unlock()
		}
	}
}

func (s *session) finish() {
	s.once.Do(func() {
		s.stopParent()
		s.stopIO()
		s.interrupt()
		s.cancel()
		// The transport owner closes all sockets directly. Sending QUIT is not
		// needed to release them and cannot roll back an acknowledged RNTO.
		s.repo.active.Done()
	})
}

func (s *session) result(err error) error {
	if err == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	return errors.Join(err, s.ctx.Err(), s.closeErr)
}
