package ftp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/textproto"
	"strings"
	"testing"
	"time"
)

func validConfig() Config {
	return Config{Address: "127.0.0.1:21", Username: "intern", Password: "private", BasePath: "/backups", InstanceID: "instance-a", TLSMode: "plain"}
}

func TestConfigurationRejectsBeforeDial(t *testing.T) {
	cases := map[string]func(*Config){
		"implicit_tls":      func(c *Config) { c.TLSMode = "implicit" },
		"explicit_tls":      func(c *Config) { c.TLSMode = "explicit" },
		"unspecified_tls":   func(c *Config) { c.TLSMode = "" },
		"url_address":       func(c *Config) { c.Address = "ftp://intern:private@localhost:21" },
		"missing_port":      func(c *Config) { c.Address = "localhost" },
		"negative_port":     func(c *Config) { c.Address = "localhost:-1" },
		"empty_host":        func(c *Config) { c.Address = ":21" },
		"password_command":  func(c *Config) { c.Password = "private\r\nDELE /file" },
		"username_command":  func(c *Config) { c.Username = "intern\nPASS private" },
		"password_control":  func(c *Config) { c.Password = "private\tvalue" },
		"username_control":  func(c *Config) { c.Username = "intern\x7f" },
		"base_control":      func(c *Config) { c.BasePath = "/backups/\x01name" },
		"base_space":        func(c *Config) { c.BasePath = "/backups /child" },
		"instance_space":    func(c *Config) { c.InstanceID = " instance" },
		"relative_base":     func(c *Config) { c.BasePath = "backups" },
		"unclean_base":      func(c *Config) { c.BasePath = "/backups/../foreign" },
		"backslash_base":    func(c *Config) { c.BasePath = "/backups\\foreign" },
		"nested_instance":   func(c *Config) { c.InstanceID = "instance/foreign" },
		"reserved_instance": func(c *Config) { c.InstanceID = ".INTERN-instance" },
		"negative_dial":     func(c *Config) { c.DialTimeout = -time.Second },
		"negative_idle":     func(c *Config) { c.IdleTimeout = -time.Second },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			mutate(&cfg)
			repo, err := Open(context.Background(), cfg)
			if repo != nil || !errors.Is(err, fs.ErrInvalid) {
				t.Fatalf("invalid configuration admitted: repository=%v error=%v", repo != nil, err)
			}
			if strings.Contains(fmt.Sprintf("%+v", err), "private") {
				t.Fatal("configuration error disclosed credentials")
			}
		})
	}
}

func TestInstanceKeysAndCleanupReferences(t *testing.T) {
	r := &Repository{cfg: validConfig()}
	for _, key := range []string{"", ".", "/instance-a/file", "instance-a/../foreign/file", "instance-a//file", "instance-a/file/", "instance-a/file\r\nDELE other", "instance-a/dir\\file", "instance-a/.intern-instance", "instance-a/.pending-old/file", "instance-a/file.PART", "instance-a/.INTERN-probe-x"} {
		if err := r.key(key, false, false); err == nil {
			t.Errorf("unsafe key admitted: %q", key)
		}
	}
	for _, key := range []string{"other/file", "instance-ab/file"} {
		if !errors.Is(r.key(key, false, false), ErrOwnership) {
			t.Errorf("foreign key not rejected: %q", key)
		}
	}
	if err := r.key("instance-a/source/chain/001-backup/payload", false, false); err != nil {
		t.Fatal(err)
	}
	if err := r.key("instance-a", true, false); err != nil {
		t.Fatal(err)
	}
	if !errors.Is(r.key("instance-a", false, false), fs.ErrInvalid) {
		t.Fatal("instance root accepted as object")
	}
}

func TestTemporaryCleanupReferences(t *testing.T) {
	r := &Repository{cfg: validConfig()}
	part := temporaryKey("instance-a/source/chain")
	if err := r.key(part, false, true); err != nil {
		t.Fatal(err)
	}
	if err := r.key(part, false, false); err == nil {
		t.Fatal("temporary object admitted as published")
	}
	if err := r.key("instance-a/payload.part", false, true); err == nil {
		t.Fatal("non-owned part naming admitted for cleanup")
	}
	if next := temporaryKey("instance-a/source/chain"); next == part {
		t.Fatal("temporary key collision")
	}
}

func TestErrorsKeepCodesAndSafeIdentity(t *testing.T) {
	for _, code := range []int{550, 552} {
		raw := &textproto.Error{Code: code, Msg: "password=private ftp://intern:private@host/path"}
		err := failure("upload", errors.Join(raw, io.ErrShortWrite, context.Canceled))
		var typed *Error
		if !errors.As(err, &typed) || typed.ReplyCode != code {
			t.Fatalf("reply code lost: %v", err)
		}
		if !errors.Is(err, io.ErrShortWrite) || !errors.Is(err, context.Canceled) {
			t.Fatal("safe error identity lost")
		}
		var leaked *textproto.Error
		if errors.As(err, &leaked) || strings.Contains(fmt.Sprintf("%+v", err), "private") {
			t.Fatal("raw server reply escaped")
		}
	}
}

func TestSafeSentinelsAndCleanupHandoff(t *testing.T) {
	for _, sentinel := range []error{fs.ErrClosed, fs.ErrInvalid, fs.ErrExist, context.DeadlineExceeded, ErrCommitUncertain, ErrOwnership} {
		if !errors.Is(failure("operation", sentinel), sentinel) {
			t.Fatalf("sentinel lost: %v", sentinel)
		}
	}
	cleanup := &CleanupError{Path: "instance-a/.intern-upload-00000000000000000000000000000000.part", cause: failure("cleanup", context.DeadlineExceeded)}
	if !errors.Is(cleanup, ErrCleanupPending) || !errors.Is(cleanup, context.DeadlineExceeded) {
		t.Fatal("cleanup handoff identity lost")
	}
	var retained *CleanupError
	if !errors.As(failure("initialize", cleanup), &retained) || retained.Path != cleanup.Path {
		t.Fatal("outer error discarded cleanup reference")
	}
}

func tcpPair(t *testing.T) (net.Conn, net.Conn) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { listener.Close() })
	peer := make(chan net.Conn, 1)
	acceptErr := make(chan error, 1)
	go func() {
		conn, acceptFailure := listener.Accept()
		if acceptFailure != nil {
			acceptErr <- acceptFailure
			return
		}
		peer <- conn
	}()
	conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.Close() })
	select {
	case other := <-peer:
		t.Cleanup(func() { other.Close() })
		return conn, other
	case err := <-acceptErr:
		t.Fatal(err)
	case <-time.After(2 * time.Second):
		t.Fatal("TCP accept stalled")
	}
	return nil, nil
}

func TestRollingIdleDeadlineOnRealTCP(t *testing.T) {
	conn, peer := tcpPair(t)
	s := &session{repo: &Repository{cfg: Config{IdleTimeout: 300 * time.Millisecond}}, sockets: make(map[*deadlineConn]struct{})}
	wrapped := &deadlineConn{Conn: conn, owner: s}
	s.sockets[wrapped] = struct{}{}
	for range 5 {
		time.Sleep(100 * time.Millisecond)
		if _, err := peer.Write([]byte("x")); err != nil {
			t.Fatal(err)
		}
		var b [1]byte
		if _, err := io.ReadFull(wrapped, b[:]); err != nil {
			t.Fatalf("rolling deadline became whole-transfer timeout: %v", err)
		}
	}
	var b [1]byte
	_, err := wrapped.Read(b[:])
	var networkError net.Error
	if !errors.As(err, &networkError) || !networkError.Timeout() {
		t.Fatalf("idle read did not time out: %v", err)
	}
	if err = wrapped.Close(); err != nil {
		t.Fatal(err)
	}
	if err = wrapped.Close(); err != nil || len(s.sockets) != 0 {
		t.Fatal("socket close was not idempotent or did not unregister")
	}
}

func TestSessionInterruptClosesRealSocket(t *testing.T) {
	conn, _ := tcpPair(t)
	s := &session{repo: &Repository{cfg: Config{IdleTimeout: time.Minute}}, sockets: make(map[*deadlineConn]struct{})}
	wrapped := &deadlineConn{Conn: conn, owner: s}
	s.sockets[wrapped] = struct{}{}
	done := make(chan error, 1)
	go func() {
		var b [1]byte
		_, err := wrapped.Read(b[:])
		done <- err
	}()
	s.interrupt()
	select {
	case err := <-done:
		if err == nil {
			t.Fatal("interrupted read falsely succeeded")
		}
	case <-time.After(time.Second):
		t.Fatal("cancellation failed to release socket")
	}
	s.interrupt()
	if len(s.sockets) != 0 {
		t.Fatal("interrupted socket remained registered")
	}
}
