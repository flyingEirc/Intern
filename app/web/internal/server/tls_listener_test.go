package server

import (
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/json/v2"
	"encoding/pem"
	"io"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// tlsMaterial writes a self-signed certificate and key pair for 127.0.0.1 into
// dir and returns their paths and the pool that trusts them.
func tlsMaterial(t *testing.T, dir string) (string, string, *x509.CertPool) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:        true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPath := filepath.Join(dir, "server.crt")
	keyPath := filepath.Join(dir, "server.key")
	if err = os.WriteFile(certPath, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600); err != nil {
		t.Fatal(err)
	}
	encoded, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(keyPath, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded}), 0o600); err != nil {
		t.Fatal(err)
	}
	parsed, err := x509.ParseCertificate(der)
	if err != nil {
		t.Fatal(err)
	}
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return certPath, keyPath, pool
}

// tlsClient dials the real listener through the server's TLS configuration path.
// It asserts the observed protocol floor rather than trusting the config struct.
type tlsClient struct {
	base   string
	client *http.Client
	jar    *cookiejar.Jar
	state  *tls.ConnectionState
}

func newTLSClient(t *testing.T, address string, pool *x509.CertPool) *tlsClient {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	state := &tls.ConnectionState{}
	client := &http.Client{
		Timeout: 10 * time.Second,
		Jar:     jar,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12},
			DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
				dialer := &tls.Dialer{Config: &tls.Config{RootCAs: pool, ServerName: "127.0.0.1"}}
				conn, err := dialer.DialContext(ctx, network, address)
				if err != nil {
					return nil, err
				}
				if tlsConn, ok := conn.(*tls.Conn); ok {
					connectionState := tlsConn.ConnectionState()
					*state = connectionState
				}
				return conn, nil
			},
		},
	}
	return &tlsClient{base: "https://" + address, client: client, jar: jar, state: state}
}

func (c *tlsClient) do(t *testing.T, method, path, body string, want int) []byte {
	t.Helper()
	return c.roundTrip(t, method, path, body, want).body
}

// roundTrip keeps the response headers, which a cookie jar does not retain.
func (c *tlsClient) roundTrip(t *testing.T, method, path, body string, want int) tlsResponse {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, c.base+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	response, err := c.client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	data, err := io.ReadAll(response.Body)
	if err != nil {
		t.Fatal(err)
	}
	if response.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, response.StatusCode, want, data)
	}
	return tlsResponse{body: data, header: response.Header}
}

type tlsResponse struct {
	body   []byte
	header http.Header
}

// TestRealTLSListenerServesEveryNewRouteGroup serves the whole engine over a real
// TLS listener built by server.New/Run, then exercises one call per new route
// group over that encrypted connection.
func TestRealTLSListenerServesEveryNewRouteGroup(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	listener := startTLSListener(t, f)
	client := listener.client
	client.do(t, "GET", "/healthz", "", http.StatusOK)
	client.do(t, "POST", "/api/v1/auth/login", `{"username":"admin","password":"`+testPassword+`"}`, http.StatusOK)
	assertTLSFloor(t, client.state)
	exerciseResourceGroupsOverTLS(t, client)
	assertPlaintextRefused(t, listener.address)
	listener.shutdown(t)
}

// startTLSListener runs the real server on a real self-signed TLS listener and
// returns a client that trusts that certificate.
// tlsListener is one running server plus the handles a test needs to stop it.
type tlsListener struct {
	address string
	client  *tlsClient
	done    <-chan error
	stop    context.CancelFunc
	server  *http.Server
}

// shutdown stops the listener and waits for Run to return. It also closes the
// socket so a failed assertion cannot leave the port bound.
func (l *tlsListener) shutdown(t *testing.T) {
	t.Helper()
	l.stop()
	awaitResult(t, l.done, "TLS server shutdown")
	_ = l.server.Close()
}

func startTLSListener(t *testing.T, f *authFixture) *tlsListener {
	t.Helper()
	certFile, keyFile, pool := tlsMaterial(t, t.TempDir())
	address := freeAddress(t)
	config := Config{Listen: address, CertFile: certFile, KeyFile: keyFile, ShutdownTimeout: 5 * time.Second}
	srv := New(config, Handler(f.engine))
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
	})
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv, config) }()
	waitListening(t, address, true)
	return &tlsListener{address: address, client: newTLSClient(t, address, pool), done: done, stop: cancel, server: srv}
}

func assertTLSFloor(t *testing.T, state *tls.ConnectionState) {
	t.Helper()
	if state.Version < tls.VersionTLS12 {
		t.Fatalf("TLS floor violated: %x", state.Version)
	}
}

// exerciseResourceGroupsOverTLS makes one call per new route group over the
// encrypted connection and returns the rows it created.
func exerciseResourceGroupsOverTLS(t *testing.T, client *tlsClient) {
	t.Helper()
	repository := decodeJSONTLS[store.Repository](t, client.do(t, "POST", "/api/v1/repositories",
		localRepository("tls-local", newRepositoryDir(t)), http.StatusCreated))
	if repository.ID == "" || repository.HasSecret {
		t.Fatalf("repository = %+v", repository)
	}
	probe := decodeJSONTLS[probeResponse](t, client.do(t, "POST", "/api/v1/repositories/"+repository.ID+"/probe", "", http.StatusOK))
	if !probe.OK {
		t.Fatalf("probe = %+v", probe)
	}
	source := decodeJSONTLS[store.Source](t, client.do(t, "POST", "/api/v1/sources", sourceBody("tls-source"), http.StatusCreated))
	if !source.HasSecret || source.ServerUUID != nil {
		t.Fatalf("source = %+v", source)
	}
	policy := decodeJSONTLS[store.Policy](t, client.do(t, "POST", "/api/v1/policies",
		policyBody("tls-policy", source.ID, repository.ID), http.StatusCreated))
	if policy.ID == "" || policy.Enabled {
		t.Fatalf("policy = %+v", policy)
	}
	client.do(t, "POST", "/api/v1/policies/"+policy.ID+"/disable", "", http.StatusOK)
	for _, path := range []string{"/api/v1/repositories?limit=1", "/api/v1/sources?limit=1", "/api/v1/policies?limit=1"} {
		client.do(t, "GET", path, "", http.StatusOK)
	}
	client.do(t, "DELETE", "/api/v1/policies/"+policy.ID, "", http.StatusNoContent)
}

// assertPlaintextRefused proves the same port does not answer plain HTTP.
func assertPlaintextRefused(t *testing.T, address string) {
	t.Helper()
	plain, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + address + "/healthz")
	if err != nil {
		return
	}
	defer plain.Body.Close()
	if plain.StatusCode == http.StatusOK {
		t.Fatal("plaintext request was served on the TLS listener")
	}
}

func decodeJSONTLS[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return value
}

// TestTLSCookieIsSecureOverTheRealListener checks the session cookie attributes
// as observed on the wire.
func TestTLSCookieIsSecureOverTheRealListener(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "viewer", "viewer")
	listener := startTLSListener(t, f)
	client := listener.client
	login := client.roundTrip(t, "POST", "/api/v1/auth/login", `{"username":"viewer","password":"`+testPassword+`"}`, http.StatusOK)
	if strings.Contains(string(login.body), testPassword) {
		t.Fatal("login response leaked the password")
	}
	client.do(t, "GET", "/api/v1/repositories", "", http.StatusOK)
	// A viewer is read-only, so a write over TLS is forbidden before validation.
	client.do(t, "POST", "/api/v1/repositories", `{"name":"","kind":"local"}`, http.StatusForbidden)
	errorBody := client.do(t, "POST", "/api/v1/sources/missing/preflight", `{"local_repository_id":"x"}`, http.StatusForbidden)
	var failure render.ErrorResponse
	if err := json.Unmarshal(errorBody, &failure); err != nil || failure.Error.Code != render.Forbidden {
		t.Fatalf("error body = %s", errorBody)
	}
	parsed, err := url.Parse(client.base)
	if err != nil {
		t.Fatal(err)
	}
	assertSecureSessionCookie(t, client.jar.Cookies(parsed), login.header.Values("Set-Cookie"))
	listener.shutdown(t)
}

// assertSecureSessionCookie checks the stored cookie and the header that carried it.
func assertSecureSessionCookie(t *testing.T, stored []*http.Cookie, set []string) {
	t.Helper()
	if len(stored) != 1 || stored[0].Name != "intern_session" {
		t.Fatalf("jar cookies = %+v", stored)
	}
	if len(set) != 1 || !containsAll(set[0], "intern_session=", "Secure", "HttpOnly", "SameSite=Lax") {
		t.Fatalf("Set-Cookie = %v", set)
	}
}

func containsAll(value string, parts ...string) bool {
	for _, part := range parts {
		if !strings.Contains(value, part) {
			return false
		}
	}
	return true
}
