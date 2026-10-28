//go:build integration

// Package acceptance runs the real Web control-plane handler in-process inside
// the pinned PXB worker container. Every call crosses a real TLS listener with a
// real login session, and the operations under test reach the real mounted MySQL
// source and the real Pure-FTPd fixture. A missing fixture is a failure, never a
// skip.
package acceptance

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	_ "modernc.org/sqlite"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/server"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/credential"
)

// The fixture paths are the ones the harness mounts; the worker never guesses
// them. wrongDataDir is a second real volume mounted for the wrong-volume case.
// matrixSocket and matrixDataDir are the remapped MySQL 9.7.1 mounts.
const (
	backupUser = "intern_bkp"
	deniedUser = "intern_denied"

	socketPath   = "/var/run/mysqld/mysqld.sock"
	dataDir      = "/var/lib/mysql"
	wrongDataDir = "/w3-wrong"

	// matrixSocket and matrixDataDir are the worker-side mounts of the real
	// MySQL 9.7.1 fixture. Keep them synchronized with w3MatrixSocket and
	// w3MatrixDataDir in the harness. The server is outside the compatibility
	// matrix, so identity succeeds and admission fails in compatibility.
	matrixSocket        = "/w3-matrix/mysqld/mysqld.sock"
	matrixDataDir       = "/w3-matrix/mysql"
	matrixServerVersion = "9.7.1"

	realXtrabackup = "/usr/bin/xtrabackup"
	ftpPort        = 2121
)

// layout is the worker's private fixture tree. It lives in the container's
// writable layer so the unprivileged mysql uid owns every path.
type layout struct {
	root, metadata, keys, kek, work, repository string
}

// secrets holds every generated credential the harness injected. None of them
// may appear in a response, the log, the audit table or the database bytes.
type secrets struct {
	source, denied, ftp, ftpLocked, admin, operator string
	all                                             []string
}

// fixture owns the control-plane store, the real TLS listeners and the logged-in
// sessions the scenarios use.
type fixture struct {
	layout layout
	secret secrets

	database *store.Store
	analysis *sql.DB
	sealer   *credential.Sealer
	auth     *auth.Service
	logs     *lockedBuffer
	received *lockedBuffer

	admin    *client
	operator *client

	local, replica, locked store.Repository
	source                 store.Source
}

// response is one observed HTTP exchange. Every body is retained so the secret
// scan sees exactly what a client saw.
type response struct {
	status int
	body   []byte
	header http.Header
}

// client is one HTTPS user agent with its own session cookie jar. seen collects
// every response body for the end-of-run secret scan.
type client struct {
	base string
	http *http.Client
	seen *lockedBuffer
}

// lockedBuffer is a concurrency-safe sink for handler logs and response bodies.
type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// requireNative refuses any host where this worker is not the real acceptance
// environment. Emulation and root runs are not acceptance.
func requireNative(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Fatal("native Linux amd64 worker required")
	}
	if os.Geteuid() == 0 {
		t.Fatal("the Web acceptance worker must not run as root")
	}
}

func prepareLayout(t *testing.T) layout {
	t.Helper()
	root := os.Getenv("INTERN_W3_ROOT")
	if !filepath.IsAbs(root) {
		t.Fatal("the worker requires an absolute private fixture root")
	}
	paths := layout{
		root: root, metadata: filepath.Join(root, "metadata"), keys: filepath.Join(root, "keys"),
		work: filepath.Join(root, "work"), repository: filepath.Join(root, "repository"),
	}
	paths.kek = filepath.Join(paths.keys, "intern.key")
	for _, dir := range []string{paths.root, paths.metadata, paths.keys, paths.work, paths.repository} {
		requirePrivateDir(t, dir)
	}
	requireKey(t, paths.kek)
	return paths
}

// requirePrivateDir creates or verifies one 0700 directory owned by this uid.
func requirePrivateDir(t *testing.T, dir string) {
	t.Helper()
	must(t, os.MkdirAll(dir, 0o700))
	info, err := os.Lstat(dir)
	must(t, err)
	if !info.IsDir() || info.Mode() != os.ModeDir|0o700 {
		t.Fatalf("fixture directory %s must be a private 0700 directory: %v", dir, info.Mode())
	}
	sys, ok := info.Sys().(*syscall.Stat_t)
	if !ok || int(sys.Uid) != os.Geteuid() {
		t.Fatalf("fixture directory %s owner uid = %v, want %d", dir, info.Sys(), os.Geteuid())
	}
}

func requireKey(t *testing.T, path string) {
	t.Helper()
	if _, err := os.Lstat(path); err == nil {
		return
	} else if !os.IsNotExist(err) {
		must(t, err)
	}
	must(t, artifact.GenerateKey(path))
}

func loadSecrets(t *testing.T) secrets {
	t.Helper()
	loaded := secrets{
		source: os.Getenv("INTERN_W3_PASSWORD"), denied: os.Getenv("INTERN_W3_DENIED_PASSWORD"),
		ftp: os.Getenv("INTERN_W3_FTP_PASSWORD"), ftpLocked: os.Getenv("INTERN_W3_FTP_LOCKED_PASSWORD"),
		admin: os.Getenv("INTERN_W3_ADMIN_PASSWORD"), operator: os.Getenv("INTERN_W3_OPERATOR_PASSWORD"),
	}
	for name, value := range map[string]string{
		"INTERN_W3_PASSWORD": loaded.source, "INTERN_W3_DENIED_PASSWORD": loaded.denied,
		"INTERN_W3_FTP_PASSWORD": loaded.ftp, "INTERN_W3_FTP_LOCKED_PASSWORD": loaded.ftpLocked,
		"INTERN_W3_ADMIN_PASSWORD": loaded.admin, "INTERN_W3_OPERATOR_PASSWORD": loaded.operator,
		"INTERN_W3_FTP_HOST": ftpHost(),
	} {
		if value == "" {
			t.Fatalf("the Web acceptance worker requires %s", name)
		}
	}
	loaded.all = []string{
		loaded.source, loaded.denied, loaded.ftp, loaded.ftpLocked, loaded.admin, loaded.operator,
	}
	return loaded
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireNative(t)
	f := &fixture{
		layout: prepareLayout(t), secret: loadSecrets(t),
		logs: &lockedBuffer{}, received: &lockedBuffer{},
	}
	f.database = openStore(t, f.layout.metadata)
	f.sealer = openSealer(t, f.layout)
	f.analysis = openAnalysis(t, f.layout.metadata)
	f.auth = newAuthentication(t, f.database)
	// Admin and operator each get a real TLS listener probing the pinned
	// XtraBackup executable. The matrix miss uses a second real MySQL server,
	// not a second tool binary.
	f.admin = startEngine(t, f, nil)
	f.operator = startEngine(t, f, nil)
	f.seedUsers(t)
	for _, login := range []struct {
		c        *client
		username string
		password string
	}{
		{f.admin, "w3-admin", f.secret.admin},
		{f.operator, "w3-operator", f.secret.operator},
	} {
		f.login(t, login.c, login.username, login.password)
	}
	return f
}

func openStore(t *testing.T, metadata string) *store.Store {
	t.Helper()
	database, err := store.Open(t.Context(), metadata)
	must(t, err)
	t.Cleanup(func() { must(t, database.Close()) })
	return database
}

func openSealer(t *testing.T, paths layout) *credential.Sealer {
	t.Helper()
	sealer, err := credential.Open(paths.kek, paths.metadata, paths.work)
	must(t, err)
	return sealer
}

// analysisDSN opens an independent handle with the store's own pragmas, so the
// inspection connection waits on a lock exactly like a second writer would.
func analysisDSN(path string) string {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	for _, pragma := range []string{"busy_timeout(5000)", "foreign_keys(1)", "synchronous(FULL)"} {
		q.Add("_pragma", pragma)
	}
	u.RawQuery = q.Encode()
	return u.String()
}

// openAnalysis opens an independent handle for state assertions that must not go
// through the API under test.
func openAnalysis(t *testing.T, metadata string) *sql.DB {
	t.Helper()
	db, err := sql.Open("sqlite", analysisDSN(filepath.Join(metadata, "intern.db")))
	must(t, err)
	db.SetMaxOpenConns(1)
	t.Cleanup(func() { must(t, db.Close()) })
	return db
}

func newAuthentication(t *testing.T, database *store.Store) *auth.Service {
	t.Helper()
	service, err := auth.New(database, auth.DefaultSessionConfig(), auth.DefaultPasswordParams())
	must(t, err)
	stop := service.StartCleanup(t.Context(), time.Minute, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	t.Cleanup(stop)
	return service
}

// startEngine serves the real handler on a real TLS listener against the pinned
// XtraBackup executable. jar shares a session with another listener when non-nil.
func startEngine(t *testing.T, f *fixture, jar http.CookieJar) *client {
	t.Helper()
	settings := resources.Settings{
		Sealer: f.sealer, KeyFile: f.layout.kek, MetadataDir: f.layout.metadata,
		WorkRoot: f.layout.work, XtrabackupPath: realXtrabackup, MinWorkBytes: 1, MinBackupBytes: 1,
	}
	logger := slog.New(slog.NewJSONHandler(f.logs, nil))
	engine, err := server.NewEngineWithResources(f.database, logger, nil, f.auth,
		&resources.Service{Store: f.database, Settings: settings}, nil)
	must(t, err)
	srv := httptest.NewUnstartedServer(server.Handler(engine))
	srv.StartTLS()
	t.Cleanup(srv.Close)
	requireTLSListener(t, srv)
	return newClient(t, srv, f.received, jar)
}

// requireTLSListener proves the listener is real TLS and does not answer plain
// HTTP on the same port.
func requireTLSListener(t *testing.T, srv *httptest.Server) {
	t.Helper()
	if srv.TLS == nil || !strings.HasPrefix(srv.URL, "https://") {
		t.Fatalf("listener is not TLS: %s", srv.URL)
	}
	address := strings.TrimPrefix(srv.URL, "https://")
	plain, err := (&http.Client{Timeout: 5 * time.Second}).Get("http://" + address + "/healthz")
	if err != nil {
		return
	}
	defer func() { must(t, plain.Body.Close()) }()
	if plain.StatusCode == http.StatusOK {
		t.Fatal("plaintext request was served on the TLS listener")
	}
}

func newClient(t *testing.T, srv *httptest.Server, seen *lockedBuffer, jar http.CookieJar) *client {
	t.Helper()
	if jar == nil {
		created, err := cookiejar.New(nil)
		must(t, err)
		jar = created
	}
	// The transport carries this listener's trusted test certificate, and the
	// jar keeps the administrator and operator sessions apart.
	shared := srv.Client()
	return &client{
		base: srv.URL,
		http: &http.Client{Transport: shared.Transport, Jar: jar, Timeout: 150 * time.Second},
		seen: seen,
	}
}

// request performs one exchange. want == 0 asserts nothing about the status, so
// a negative scenario can inspect the body.
func (c *client) request(t *testing.T, method, path, body string, want int) response {
	t.Helper()
	return c.requestWithID(t, method, path, body, "", want)
}

// requestWithID is request with a caller-chosen request id, which is how a
// scenario binds one audit row to the exact exchange it belongs to.
func (c *client) requestWithID(t *testing.T, method, path, body, requestID string, want int) response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, c.base+path, reader)
	must(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if requestID != "" {
		req.Header.Set(render.RequestIDHeader, requestID)
	}
	// A browser sends Origin on an unsafe method; an exact match with Host is
	// what the server's cross-origin protection accepts.
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", c.base)
	}
	reply, err := c.http.Do(req)
	must(t, err)
	defer func() { must(t, reply.Body.Close()) }()
	data, err := io.ReadAll(io.LimitReader(reply.Body, 8<<20))
	must(t, err)
	if _, err = c.seen.Write(data); err != nil {
		t.Fatal(err)
	}
	if want != 0 && reply.StatusCode != want {
		t.Fatalf("%s %s = %d, want %d: %s", method, path, reply.StatusCode, want, data)
	}
	return response{status: reply.StatusCode, body: data, header: reply.Header}
}

func (f *fixture) seedUsers(t *testing.T) {
	t.Helper()
	adminHash, err := f.auth.Passwords.Hash(f.secret.admin)
	must(t, err)
	_, err = f.database.CreateAdmin(t.Context(), "w3-admin", adminHash)
	must(t, err)
	operatorHash, err := f.auth.Passwords.Hash(f.secret.operator)
	must(t, err)
	_, err = f.database.CreateUser(t.Context(), "w3-operator", operatorHash, "operator",
		store.SystemAudit("users.create", store.AuditDetail{Role: "operator"}))
	must(t, err)
}

func (f *fixture) login(t *testing.T, c *client, username, password string) {
	t.Helper()
	body := marshal(t, map[string]string{"username": username, "password": password})
	c.request(t, http.MethodPost, "/api/v1/auth/login", body, http.StatusOK)
}

func marshal(t *testing.T, value any) string {
	t.Helper()
	data, err := json.Marshal(value)
	must(t, err)
	return string(data)
}

func decode[T any](t *testing.T, data []byte) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(data, &value); err != nil {
		t.Fatalf("%v: %s", err, data)
	}
	return value
}

// ftpHost is the private-network alias the harness resolves the FTP fixture by.
func ftpHost() string { return os.Getenv("INTERN_W3_FTP_HOST") }
