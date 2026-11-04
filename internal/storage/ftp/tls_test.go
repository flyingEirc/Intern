package ftp

import (
	"bufio"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"io/fs"
	"math/big"
	"net"
	"strings"
	"sync"
	"testing"
	"time"
)

// authority is a throwaway test CA. Nothing it signs is trusted outside the
// process, and every certificate below is generated at run time.
type authority struct {
	ca      *x509.Certificate
	caKey   *ecdsa.PrivateKey
	caPEM   string
	foreign string
}

func newAuthority(t *testing.T) *authority {
	t.Helper()
	result := &authority{}
	result.ca, result.caKey = newRoot(t, "intern-test-ca")
	result.caPEM = encodePEM("CERTIFICATE", result.ca.Raw)
	foreign, _ := newRoot(t, "intern-foreign-ca")
	result.foreign = encodePEM("CERTIFICATE", foreign.Raw)
	return result
}

func newRoot(t *testing.T, common string) (*x509.Certificate, *ecdsa.PrivateKey) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: common},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certificate, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	return certificate, key
}

// leaf signs one server certificate with this authority. host is used as the
// subject, the only DNS name and the only IP address, so an address built from
// the same string matches or mismatches exactly as the case requires.
func (a *authority) leaf(t *testing.T, host string, signedByCA bool, lifetime time.Duration) tls.Certificate {
	t.Helper()
	issuer, issuerKey := a.ca, a.caKey
	if !signedByCA {
		issuer, issuerKey = newRoot(t, "intern-untrusted-ca")
	}
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	notBefore := time.Now().Add(-time.Minute)
	template := &x509.Certificate{
		SerialNumber: big.NewInt(2),
		Subject:      pkix.Name{CommonName: host},
		NotBefore:    notBefore,
		NotAfter:     notBefore.Add(lifetime),
		KeyUsage:     x509.KeyUsageDigitalSignature,
		ExtKeyUsage:  []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	if ip := net.ParseIP(host); ip != nil {
		template.IPAddresses = []net.IP{ip}
	} else {
		template.DNSNames = []string{host}
	}
	der, err := x509.CreateCertificate(rand.Reader, template, issuer, &key.PublicKey, issuerKey)
	if err != nil {
		t.Fatal(err)
	}
	return tls.Certificate{Certificate: [][]byte{der}, PrivateKey: key}
}

func encodePEM(kind string, der []byte) string {
	return string(pem.EncodeToMemory(&pem.Block{Type: kind, Bytes: der}))
}

// The fake server speaks just enough FTP to let the client reach its own
// decision, and records every command line it received in the clear. That
// record is the only oracle able to prove a refusal happened before any
// credential was transmitted, or that an upgrade was never actually downgraded.
type fakeServer struct {
	listener    net.Listener
	certificate tls.Certificate
	implicit    bool
	refuseTLS   bool
	// brokenUpgrade answers AUTH TLS with 234 and then stays in plaintext, the
	// worst possible server: a client that kept going would leak credentials.
	brokenUpgrade bool
	mu            sync.Mutex
	lines         []string
}

func startServer(t *testing.T, configure func(*fakeServer)) *fakeServer {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	server := &fakeServer{listener: listener}
	configure(server)
	t.Cleanup(func() { _ = listener.Close() })
	go func() {
		for {
			conn, err := listener.Accept()
			if err != nil {
				return
			}
			go server.serve(conn)
		}
	}()
	return server
}

func (s *fakeServer) serve(raw net.Conn) {
	defer raw.Close()
	conn, writer := net.Conn(raw), bufio.NewWriter(raw)
	if s.implicit && !s.handshake(&conn, &writer) {
		return
	}
	// A single-line greeting: textproto reads a "220-" banner as the start of a
	// multi-line reply and would wait for its terminator.
	if _, err := writer.WriteString("220 Fake Pure-FTPd ready\r\n"); err != nil {
		return
	}
	_ = writer.Flush()
	scanner := bufio.NewScanner(conn)
	for scanner.Scan() {
		line := scanner.Text()
		s.record(line)
		rescan, ok := s.answer(strings.ToUpper(line), &conn, &writer)
		if !ok {
			return
		}
		if rescan {
			scanner = bufio.NewScanner(conn)
		}
	}
}

// answer replies to one command. ok is false when the connection must end;
// rescan reports that the stream was upgraded and needs a fresh scanner.
func (s *fakeServer) answer(verb string, conn *net.Conn, writer **bufio.Writer) (rescan, ok bool) {
	if strings.HasPrefix(verb, "AUTH") {
		return s.authTLS(conn, writer)
	}
	replies, quit := plainReply(verb)
	if quit {
		_ = write(*writer, replies[0])
		return false, false
	}
	return false, writeAll(*writer, replies)
}

// authTLS answers AUTH TLS according to the configured server behaviour. ok
// is false when the connection must be dropped; upgraded reports that the
// stream now runs over TLS and needs a fresh scanner.
func (s *fakeServer) authTLS(conn *net.Conn, writer **bufio.Writer) (upgraded, ok bool) {
	if s.refuseTLS {
		return false, write(*writer, "530 Secured connections are disabled")
	}
	if !write(*writer, "234 AUTH TLS successful") {
		return false, false
	}
	if s.brokenUpgrade {
		return false, true
	}
	if !s.handshake(conn, writer) {
		return false, false
	}
	return true, true
}

// plainReply is the fixed answer to every command other than AUTH; quit asks
// the caller to send the single reply and close the connection.
func plainReply(verb string) (replies []string, quit bool) {
	switch {
	case strings.HasPrefix(verb, "USER"):
		return []string{"331 Password required"}, false
	case strings.HasPrefix(verb, "PASS"):
		return []string{"230 Logged in"}, false
	case strings.HasPrefix(verb, "FEAT"):
		return []string{"211-Features:\r\n TLS\r\n UTF8", "211 End"}, false
	case strings.HasPrefix(verb, "TYPE"), strings.HasPrefix(verb, "OPTS"),
		strings.HasPrefix(verb, "PBSZ"), strings.HasPrefix(verb, "PROT"):
		return []string{"200 Always OK"}, false
	case strings.HasPrefix(verb, "QUIT"):
		return []string{"221 Goodbye"}, true
	default:
		return []string{"500 Unrecognized command"}, false
	}
}

func writeAll(writer *bufio.Writer, lines []string) bool {
	for _, line := range lines {
		if !write(writer, line) {
			return false
		}
	}
	return true
}

func write(writer *bufio.Writer, line string) bool {
	if _, err := writer.WriteString(line + "\r\n"); err != nil {
		return false
	}
	return writer.Flush() == nil
}

// handshake switches the live connection to TLS. The caller keeps both the
// connection and its writer, because the upgraded stream replaces both.
func (s *fakeServer) handshake(conn *net.Conn, writer **bufio.Writer) bool {
	tlsConn := tls.Server(*conn, &tls.Config{Certificates: []tls.Certificate{s.certificate}})
	if err := tlsConn.Handshake(); err != nil {
		_ = (*conn).Close()
		return false
	}
	*conn, *writer = tlsConn, bufio.NewWriter(tlsConn)
	return true
}

func (s *fakeServer) record(line string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.lines = append(s.lines, line)
}

func (s *fakeServer) address() string { return s.listener.Addr().String() }

func (s *fakeServer) received(verb string) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	for _, line := range s.lines {
		if strings.HasPrefix(strings.ToUpper(line), verb+" ") || strings.EqualFold(line, verb) {
			return true
		}
	}
	return false
}

func tlsTestConfig(t *testing.T, mode, bundle string, server *fakeServer) Config {
	t.Helper()
	return Config{
		Address: server.address(), Username: "intern", Password: "unit-private",
		BasePath: "/backups", InstanceID: "instance-a",
		TLSMode: mode, CAPem: bundle, DialTimeout: 2 * time.Second, IdleTimeout: 2 * time.Second,
	}
}

func kindOf(t *testing.T, err error) string {
	t.Helper()
	var typed *Error
	if !errors.As(err, &typed) {
		t.Fatalf("error is not an FTP operation error: %v", err)
	}
	return typed.Kind
}

// TestFTPSRefusalsPrecedeCredentials is the unit half of S2-A2: a server that
// cannot serve the selected mode, an untrusted issuer, a wrong host name and an
// expired certificate must all fail as TLS refusals, before USER or PASS ever
// leaves the process, without the peer's message in the error.
func TestFTPSRefusalsPrecedeCredentials(t *testing.T) {
	authority := newAuthority(t)
	cases := []struct {
		name      string
		configure func(*fakeServer)
		kind      string
	}{
		{"no_tls_support", func(s *fakeServer) { s.refuseTLS = true }, "tls_negotiation"},
		{"untrusted_authority", func(s *fakeServer) {
			s.certificate = authority.leaf(t, "127.0.0.1", false, time.Hour)
		}, "tls_untrusted"},
		{"hostname_mismatch", func(s *fakeServer) {
			s.certificate = authority.leaf(t, "somewhere.else", true, time.Hour)
		}, "tls_hostname"},
		{"expired_certificate", func(s *fakeServer) {
			s.certificate = authority.leaf(t, "127.0.0.1", true, -time.Minute)
		}, "tls_expired"},
	}
	for _, mode := range []string{TLSModeExplicit, TLSModeImplicit} {
		for _, testCase := range cases {
			t.Run(mode+"_"+testCase.name, func(t *testing.T) {
				bundle := authority.caPEM
				if testCase.name == "untrusted_authority" {
					bundle = authority.foreign
				}
				requireRefusedBeforeCredentials(t, mode, bundle, testCase.kind, testCase.configure)
			})
		}
	}
}

// requireRefusedBeforeCredentials opens against a server configured to fail
// the TLS step and asserts the refusal kind, its TLS identity, a redacted
// message and that no credential reached the server.
func requireRefusedBeforeCredentials(t *testing.T, mode, bundle, kind string, configure func(*fakeServer)) {
	t.Helper()
	server := startServer(t, func(s *fakeServer) {
		s.implicit = mode == TLSModeImplicit
		configure(s)
	})
	repository, err := Open(t.Context(), tlsTestConfig(t, mode, bundle, server))
	if repository != nil {
		_ = repository.Close()
		t.Fatal("a refused FTPS server was accepted")
	}
	if got := kindOf(t, err); got != kind {
		t.Fatalf("kind = %q, want %q (error=%v)", got, kind, err)
	}
	if !errors.Is(err, ErrTLS) {
		t.Fatalf("TLS identity lost: %v", err)
	}
	if strings.Contains(err.Error(), "unit-private") || strings.Contains(err.Error(), "127.0.0.1") {
		t.Fatalf("error disclosed a credential or the peer: %q", err)
	}
	if server.received("USER") || server.received("PASS") {
		t.Fatal("credentials were transmitted after a TLS refusal")
	}
}

// TestFakeUpgradeNeverSendsCredentials covers the downgrade attempt: a server
// that claims TLS is ready but never completes the handshake must not receive a
// single credential byte.
func TestFakeUpgradeNeverSendsCredentials(t *testing.T) {
	authority := newAuthority(t)
	for _, mode := range []string{TLSModeExplicit, TLSModeImplicit} {
		t.Run(mode, func(t *testing.T) {
			server := startServer(t, func(s *fakeServer) {
				s.certificate = authority.leaf(t, "127.0.0.1", true, time.Hour)
				s.brokenUpgrade = true
			})
			repository, err := Open(t.Context(), tlsTestConfig(t, mode, authority.caPEM, server))
			if repository != nil {
				_ = repository.Close()
				t.Fatal("an unencrypted session was accepted")
			}
			if server.received("USER") || server.received("PASS") {
				t.Fatal("credentials were sent over a session without TLS")
			}
			if mode == TLSModeImplicit && !strings.Contains(fmtDetail(err), "tls_") {
				t.Fatalf("implicit refusal was not classified as TLS: %v", fmtDetail(err))
			}
		})
	}
}

// TestTrustedBundleOpensTheTLSSession proves the positive half of the same
// path: with the issuing CA supplied, the upgrade completes, the protected data
// channel is demanded, and the conversation continues over TLS. The server then
// refuses to serve the ownership marker, so the open still fails — but as a
// reply failure, never as a TLS one.
func TestTrustedBundleOpensTheTLSSession(t *testing.T) {
	authority := newAuthority(t)
	for _, mode := range []string{TLSModeExplicit, TLSModeImplicit} {
		t.Run(mode, func(t *testing.T) {
			server := startServer(t, func(s *fakeServer) {
				s.implicit = mode == TLSModeImplicit
				s.certificate = authority.leaf(t, "127.0.0.1", true, time.Hour)
			})
			repository, err := Open(t.Context(), tlsTestConfig(t, mode, authority.caPEM, server))
			if repository != nil {
				_ = repository.Close()
				t.Fatal("a server that refuses every transfer command completed initialization")
			}
			if strings.HasPrefix(kindOf(t, err), "tls_") {
				t.Fatalf("a trusted certificate was refused: %v (%s)", err, kindOf(t, err))
			}
			requireEncryptedConversation(t, mode, server)
			// A supplied bundle replaces the system roots, it never extends them.
			foreign := tlsTestConfig(t, mode, authority.foreign, server)
			if repository, err = Open(t.Context(), foreign); err == nil {
				_ = repository.Close()
				t.Fatal("a foreign bundle trusted the same certificate")
			} else if kindOf(t, err) != "tls_untrusted" {
				t.Fatalf("kind = %q, want tls_untrusted", kindOf(t, err))
			}
		})
	}
}

// requireEncryptedConversation asserts the commands a completed TLS session
// must have carried for the selected mode.
func requireEncryptedConversation(t *testing.T, mode string, server *fakeServer) {
	t.Helper()
	if mode == TLSModeExplicit && !server.received("AUTH") {
		t.Fatal("AUTH TLS was never issued")
	}
	if !server.received("USER") || !server.received("PROT") {
		t.Fatal("the encrypted control session never carried the FTP conversation")
	}
	if mode == TLSModeImplicit && !server.received("PBSZ") {
		t.Fatal("PBSZ was not issued for implicit TLS")
	}
}

func TestConfigurationRejectsTLSBeforeDial(t *testing.T) {
	authority := newAuthority(t)
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	privatePEM := encodePEM("EC PRIVATE KEY", mustMarshalKey(t, key))
	cases := map[string]func(*Config){
		"unknown_mode":   func(c *Config) { c.TLSMode = "ftps" },
		"ca_on_plain":    func(c *Config) { c.CAPem = authority.caPEM },
		"ca_not_pem":     func(c *Config) { c.TLSMode = TLSModeExplicit; c.CAPem = "not a certificate" },
		"ca_private_key": func(c *Config) { c.TLSMode = TLSModeExplicit; c.CAPem = privatePEM },
		"ca_blank":       func(c *Config) { c.TLSMode = TLSModeExplicit; c.CAPem = "   " },
		"ca_oversized": func(c *Config) {
			c.TLSMode = TLSModeImplicit
			c.CAPem = strings.Repeat("a", maxCAPemBytes+1)
		},
		"ca_with_headers": func(c *Config) {
			c.TLSMode = TLSModeImplicit
			c.CAPem = string(pem.EncodeToMemory(&pem.Block{
				Type: "CERTIFICATE", Headers: map[string]string{"proc-type": "type,types"}, Bytes: authority.ca.Raw,
			}))
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			cfg := validConfig()
			cfg.Password = "unit-private"
			mutate(&cfg)
			repository, err := Open(t.Context(), cfg)
			if repository != nil || !errors.Is(err, fs.ErrInvalid) {
				t.Fatalf("invalid TLS configuration admitted: repository=%v error=%v", repository != nil, err)
			}
			if strings.Contains(fmtDetail(err), "unit-private") {
				t.Fatal("TLS configuration error disclosed a credential")
			}
		})
	}
}

func fmtDetail(err error) string {
	var typed *Error
	if !errors.As(err, &typed) {
		return err.Error()
	}
	return typed.Kind + " " + typed.Op
}

func mustMarshalKey(t *testing.T, key *ecdsa.PrivateKey) []byte {
	t.Helper()
	der, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return der
}

// TestTransportSettingsFollowTheSelection checks the frozen transport shape
// without a network: minimum version, the identity every channel is verified
// against, the trust source and the shared session cache a protected data
// channel needs to resume the control session.
func TestTransportSettingsFollowTheSelection(t *testing.T) {
	authority := newAuthority(t)
	cfg := validConfig()
	cfg.Address = "ftps.example:990"
	cfg.TLSMode = TLSModeExplicit
	cfg.CAPem = authority.caPEM
	normalized, transport, err := normalizeConfig(cfg)
	if err != nil {
		t.Fatal(err)
	}
	requireVerifiedTransport(t, transport)
	if normalized.DialTimeout != 10*time.Second || normalized.IdleTimeout != 30*time.Second {
		t.Fatalf("timeouts = %s %s", normalized.DialTimeout, normalized.IdleTimeout)
	}
	cfg.CAPem = ""
	if _, transport, err = normalizeConfig(cfg); err != nil || transport.RootCAs != nil {
		t.Fatalf("system roots were replaced: %v %v", transport, err)
	}
	cfg.TLSMode = TLSModePlain
	if _, transport, err = normalizeConfig(cfg); err != nil || transport != nil {
		t.Fatalf("plain selected a TLS transport: %v %v", transport, err)
	}
}

// requireVerifiedTransport asserts the frozen settings of a TLS transport
// built with a supplied bundle.
func requireVerifiedTransport(t *testing.T, transport *tls.Config) {
	t.Helper()
	if transport == nil || transport.MinVersion != tls.VersionTLS12 || transport.ServerName != "ftps.example" {
		t.Fatalf("transport settings = %+v", transport)
	}
	if transport.InsecureSkipVerify {
		t.Fatal("verification can never be skipped")
	}
	if transport.RootCAs == nil {
		t.Fatal("the supplied CA bundle was dropped")
	}
	if transport.ClientSessionCache == nil {
		t.Fatal("FTPS data channels cannot resume the control session")
	}
}
