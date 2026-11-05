package ftp

import (
	"bytes"
	"context"
	"crypto/tls"
	"errors"
	"io"
	"io/fs"
	"net"
	"net/textproto"
	"sync"
	"sync/atomic"
	"time"

	client "github.com/jlaffaye/ftp"
)

// A session has exactly one SDK caller. Cancellation touches only net.Conn,
// never ServerConn or Response, which the SDK does not make concurrent-safe.
type session struct {
	repo          *Repository
	ctx           context.Context
	cancel        context.CancelFunc
	stopParent    func() bool
	stopIO        func() bool
	conn          *client.ServerConn
	mu            sync.Mutex
	sockets       map[*deadlineConn]struct{}
	interrupted   bool
	closeErr      error
	once          sync.Once
	controlDialed bool
	// authAsked is set once the SDK has sent AUTH TLS on the explicit control
	// channel; see authProbe.
	authAsked atomic.Bool
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

// authCommand starts the explicit upgrade: the SDK writes it as its first
// command once it has accepted the greeting.
var authCommand = []byte("AUTH ")

// authProbe is the explicit control channel until the SDK upgrades it. It
// forwards every byte untouched and only records that AUTH went out. The SDK
// reports a refused greeting and a refused AUTH TLS as the same
// *textproto.Error, yet only the second is a server declining TLS, and whether
// AUTH was sent is the one fact that tells them apart.
type authProbe struct {
	net.Conn
	asked *atomic.Bool
}

func (p *authProbe) Write(b []byte) (int, error) {
	if !p.asked.Load() && bytes.HasPrefix(b, authCommand) {
		p.asked.Store(true)
	}
	return p.Conn.Write(b)
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
	options := []client.DialOption{
		client.DialWithDialFunc(s.dial),
		client.DialWithDisabledMLSD(true),
		client.DialWithForceListHidden(true),
	}
	switch r.cfg.TLSMode {
	case TLSModeExplicit:
		options = append(options, client.DialWithExplicitTLS(r.tlsConfig))
	case TLSModeImplicit:
		options = append(options, client.DialWithTLS(r.tlsConfig))
	}
	conn, err := client.Dial(r.cfg.Address, options...)
	if err == nil {
		s.conn = conn
		err = conn.Login(r.cfg.Username, r.cfg.Password)
	} else if s.upgradeRefused(err) {
		// AUTH TLS is the first command the SDK sends, before USER and PASS. A
		// server that answers at all but refuses the upgrade therefore fails
		// strictly before any credential left this process.
		err = errors.Join(err, ErrTLS)
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
	first := !s.controlDialed
	s.controlDialed = true
	s.sockets[wrapped] = struct{}{}
	s.mu.Unlock()
	// The SDK upgrades the control channel itself after AUTH TLS, so explicit
	// mode leaves that one connection alone apart from noting the command. Every
	// data channel, and both channels of an implicit server, are encrypted here:
	// the library skips its own wrapping whenever a custom dial function is
	// supplied.
	if s.repo.tlsConfig == nil {
		return wrapped, nil
	}
	if first && s.repo.cfg.TLSMode == TLSModeExplicit {
		return &authProbe{Conn: wrapped, asked: &s.authAsked}, nil
	}
	channel := tls.Client(wrapped, s.repo.tlsConfig)
	if !first {
		// A data channel must not handshake here: the SDK opens it before it
		// sends STOR/RETR/LIST, while Pure-FTPd only starts the server side of the
		// data-channel TLS after it has received that command. An eager handshake
		// would wait for a ServerHello that never comes until the dial timeout.
		// A tls.Conn writes no application byte before its handshake succeeds, so
		// the first transfer read or write performs and verifies the handshake and
		// there is still no path to an unencrypted FTPS transfer.
		return channel, nil
	}
	// The implicit control channel is encrypted before the SDK reads the greeting,
	// so a server that cannot be trusted fails here, strictly before any FTP byte
	// or credential exists.
	if err = channel.HandshakeContext(ctx); err != nil {
		// The refusal is joined with the close outcome: a channel that could not
		// be encrypted must also be provably gone before any FTP byte exists.
		return nil, errors.Join(err, ErrTLS, wrapped.Close())
	}
	return channel, nil
}

// upgradeRefused reports whether err is a server's answer to AUTH TLS, the only
// reply that is a TLS refusal. A reply that arrived before AUTH was sent is the
// greeting, and an implicit server's greeting always follows a completed
// handshake: a refusal there (421 Too many users) is a plain FTP reply and
// keeps the classification plaintext gives it.
func (s *session) upgradeRefused(err error) bool {
	var reply *textproto.Error
	return s.authAsked.Load() && errors.As(err, &reply)
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
