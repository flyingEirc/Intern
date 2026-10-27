//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"encoding/pem"
	"fmt"
	"io"
	"log/slog"
	"math/big"
	"net"
	"net/http"
	"net/http/cookiejar"
	"net/http/httptest"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	mysqldriver "github.com/go-sql-driver/mysql"
	_ "modernc.org/sqlite"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/server"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/credential"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

const (
	backupUser    = "intern_bkp"
	ftpUser       = "normal"
	ftpPort       = 2121
	socketPath    = "/var/run/mysqld/mysqld.sock"
	datadirPath   = "/var/lib/mysql"
	xtrabackupBin = "/usr/bin/xtrabackup"
	cronFull      = "0 2 * * *"
	cronInc       = "0 * * * *"
	objectPayload = "payload.bin"
)

type layout struct {
	root, metadata, kek, work, repository, local, tlsDir, cert, key string
}

type secrets struct {
	source, ftp, admin, operator string
}

type fixture struct {
	t          *testing.T
	layout     layout
	secret     secrets
	database   *store.Store
	analysis   *sql.DB
	sealer     *credential.Sealer
	auth       *auth.Service
	stopAuth   func()
	authOnce   sync.Once
	settings   resources.Settings
	service    *resources.Service
	dispatcher *dispatch.Dispatcher
	http       *httptest.Server
	admin      *client
	operator   *client
	log        *lockedBuffer
	instanceID string
	sourceID   string
	localID    string
	ftpID      string
	e2ePolicy  string
	listen     string
	certPool   *x509.CertPool
	child      *exec.Cmd
	childLog   *os.File
	closed     bool
}

type client struct {
	t      *testing.T
	base   string
	http   *http.Client
	origin string
	role   string
}

type createdID struct {
	ID string `json:"id"`
}

type createdRun struct {
	RunID string `json:"run_id"`
}

type runsPage struct {
	Items      []store.Run `json:"items"`
	NextBefore *string     `json:"next_before"`
}

type logsPage struct {
	Items     []store.RunLog `json:"items"`
	NextAfter *int64         `json:"next_after"`
}

func newFixture(t *testing.T) *fixture {
	t.Helper()
	requireNative(t)
	paths := newLayout(t)
	secret := loadSecrets(t)
	logger := slog.New(slog.NewJSONHandler(io.Discard, nil))
	database := openStore(t, paths.metadata)
	analysis := openAnalysis(t, paths.metadata)
	must(t, artifact.GenerateKey(paths.kek))
	sealer, err := credential.Open(paths.kek, paths.metadata, paths.work)
	must(t, err)
	authn, err := auth.New(database, auth.DefaultSessionConfig(), auth.DefaultPasswordParams())
	must(t, err)
	settings := resources.Settings{
		Sealer: sealer, KeyFile: paths.kek, MetadataDir: paths.metadata, WorkRoot: paths.work,
		XtrabackupPath: xtrabackupBin, MinWorkBytes: 1, MinBackupBytes: 1,
	}
	svc := &resources.Service{Store: database, Settings: settings}
	f := &fixture{
		t: t, layout: paths, secret: secret, database: database, analysis: analysis,
		sealer: sealer, auth: authn, settings: settings, service: svc,
		instanceID: database.InstanceID(), log: new(lockedBuffer),
	}
	f.stopAuth = authn.StartCleanup(context.Background(), time.Minute, logger)
	t.Cleanup(f.stopAuthOnce)
	seedUsers(t, f)
	certPool := writeTLS(t, paths)
	f.certPool = certPool
	f.startEngine(t, false)
	return f
}

// newLayout builds the worker's private fixture tree below the harness-provided
// root, which the container creates for the mysql uid, or below t.TempDir() when
// the variable is absent (a local native run).
func newLayout(t *testing.T) layout {
	t.Helper()
	root := os.Getenv("INTERN_W4_ROOT")
	if root == "" {
		root = t.TempDir()
	} else {
		must(t, os.MkdirAll(root, 0o700))
		root = filepath.Join(root, "fixture")
	}
	paths := layout{
		root: root, metadata: filepath.Join(root, "metadata"), kek: filepath.Join(root, "keys", "intern.key"),
		work: filepath.Join(root, "work"), repository: filepath.Join(root, "repository"),
		tlsDir: filepath.Join(root, "tls"),
	}
	paths.local = filepath.Join(paths.repository, "local")
	paths.cert = filepath.Join(paths.tlsDir, "server.crt")
	paths.key = filepath.Join(paths.tlsDir, "server.key")
	for _, dir := range []string{paths.metadata, filepath.Dir(paths.kek), paths.work, paths.repository, paths.local, paths.tlsDir} {
		must(t, os.Mkdir(dir, 0o700))
	}
	for _, dir := range []string{root, paths.metadata, filepath.Dir(paths.kek), paths.work, paths.repository, paths.local, paths.tlsDir} {
		requirePrivateDir(t, dir)
	}
	return paths
}

func loadSecrets(t *testing.T) secrets {
	t.Helper()
	secret := secrets{
		source:   os.Getenv("INTERN_W4_PASSWORD"),
		ftp:      os.Getenv("INTERN_W4_FTP_PASSWORD"),
		admin:    os.Getenv("INTERN_W4_ADMIN_PASSWORD"),
		operator: os.Getenv("INTERN_W4_OPERATOR_PASSWORD"),
	}
	seen := map[string]string{}
	for name, value := range map[string]string{
		"INTERN_W4_PASSWORD": secret.source, "INTERN_W4_FTP_PASSWORD": secret.ftp,
		"INTERN_W4_ADMIN_PASSWORD": secret.admin, "INTERN_W4_OPERATOR_PASSWORD": secret.operator,
	} {
		if len(value) < 12 {
			t.Fatalf("%s is missing", name)
		}
		if other, ok := seen[value]; ok {
			t.Fatalf("%s equals %s", name, other)
		}
		seen[value] = name
	}
	return secret
}

func openStore(t *testing.T, dir string) *store.Store {
	t.Helper()
	database, err := store.Open(t.Context(), dir)
	must(t, err)
	t.Cleanup(func() { _ = database.Close() })
	return database
}

func openAnalysis(t *testing.T, dir string) *sql.DB {
	t.Helper()
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", filepath.Join(dir, "intern.db"))
	db, err := sql.Open("sqlite", dsn)
	must(t, err)
	t.Cleanup(func() { must(t, db.Close()) })
	return db
}

func seedUsers(t *testing.T, f *fixture) {
	t.Helper()
	adminHash, err := f.auth.Passwords.Hash(f.secret.admin)
	must(t, err)
	_, err = f.database.CreateAdmin(t.Context(), "admin", adminHash)
	must(t, err)
	operatorHash, err := f.auth.Passwords.Hash(f.secret.operator)
	must(t, err)
	_, err = f.database.CreateUser(t.Context(), "operator", operatorHash, "operator",
		store.SystemAudit("users.create", store.AuditDetail{Role: "operator"}))
	must(t, err)
}

func writeTLS(t *testing.T, paths layout) *x509.CertPool {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	must(t, err)
	serial, err := rand.Int(rand.Reader, new(big.Int).Lsh(big.NewInt(1), 128))
	must(t, err)
	template := &x509.Certificate{
		SerialNumber: serial, Subject: pkix.Name{CommonName: "127.0.0.1"},
		NotBefore: time.Now().Add(-time.Minute), NotAfter: time.Now().Add(24 * time.Hour),
		KeyUsage:    x509.KeyUsageDigitalSignature | x509.KeyUsageCertSign,
		ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses: []net.IP{net.ParseIP("127.0.0.1")},
		IsCA:        true, BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	must(t, err)
	must(t, os.WriteFile(paths.cert, pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}), 0o600))
	encoded, err := x509.MarshalECPrivateKey(key)
	must(t, err)
	must(t, os.WriteFile(paths.key, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: encoded}), 0o600))
	parsed, err := x509.ParseCertificate(der)
	must(t, err)
	pool := x509.NewCertPool()
	pool.AddCert(parsed)
	return pool
}

func (f *fixture) startEngine(t *testing.T, withDispatch bool) {
	t.Helper()
	if f.http != nil {
		f.http.Close()
		f.http = nil
	}
	if f.dispatcher != nil {
		_ = f.dispatcher.Stop()
		f.dispatcher = nil
	}
	logger := slog.New(slog.NewJSONHandler(f.log, nil))
	var dispatcher server.DispatcherControl
	if withDispatch {
		started, err := dispatch.Start(context.Background(), dispatch.Config{
			Database: f.database, Settings: f.settings, Logger: logger, Preflight: f.service,
		})
		must(t, err)
		f.dispatcher = started
		dispatcher = started
	}
	engine, err := server.NewEngineWithResources(f.database, logger, nil, f.auth, f.service, dispatcher)
	must(t, err)
	srv := httptest.NewUnstartedServer(server.Handler(engine))
	srv.StartTLS()
	f.http = srv
	f.admin = newClient(t, srv.Client(), srv.URL, "admin")
	f.operator = newClient(t, srv.Client(), srv.URL, "operator")
	f.login(t)
}

func (f *fixture) startDispatch(t *testing.T) {
	t.Helper()
	f.startEngine(t, true)
}

func (f *fixture) login(t *testing.T) {
	t.Helper()
	f.admin.login(t, "admin", f.secret.admin)
	f.operator.login(t, "operator", f.secret.operator)
}

func (f *fixture) topology(t *testing.T) {
	t.Helper()
	f.localID = f.createLocal(t)
	f.ftpID = f.createFTP(t)
	f.sourceID = f.createSource(t)
	f.e2ePolicy = f.createPolicy(t, "w4-e2e", 3)
	f.enable(t, f.e2ePolicy)
}

func (f *fixture) createLocal(t *testing.T) string {
	t.Helper()
	body := fmt.Sprintf(`{"name":"w4-local","kind":"local","tls_mode":"none","root":%q}`, f.layout.local)
	return f.decodeID(t, f.admin.request(t, http.MethodPost, "/api/v1/repositories", body, http.StatusCreated))
}

func (f *fixture) createFTP(t *testing.T) string {
	t.Helper()
	host := os.Getenv("INTERN_W4_FTP_HOST")
	if host == "" {
		t.Fatal("INTERN_W4_FTP_HOST is required")
	}
	body := fmt.Sprintf(
		`{"name":"w4-ftp","kind":"ftp","tls_mode":"plain","ftp":{"host":%q,"port":%d,"base_path":"/","username":%q},"password":%q}`,
		host, ftpPort, ftpUser, f.secret.ftp)
	return f.decodeID(t, f.admin.request(t, http.MethodPost, "/api/v1/repositories", body, http.StatusCreated))
}

func (f *fixture) createSource(t *testing.T) string {
	t.Helper()
	body := fmt.Sprintf(
		`{"name":"w4-source","socket":%q,"datadir":%q,"username":%q,"password":%q}`,
		socketPath, datadirPath, backupUser, f.secret.source)
	return f.decodeID(t, f.admin.request(t, http.MethodPost, "/api/v1/sources", body, http.StatusCreated))
}

func (f *fixture) createPolicy(t *testing.T, name string, maxChain int) string {
	t.Helper()
	body := fmt.Sprintf(
		`{"name":%q,"source_id":%q,"local_repository_id":%q,"replica_repository_id":%q,`+
			`"full_cron":%q,"incremental_cron":%q,"timezone":"UTC","max_chain_length":%d,`+
			`"parallel":1,"throttle":0,"zstd_level":3,"page_tracking":false,"enabled":false}`,
		name, f.sourceID, f.localID, f.ftpID, cronFull, cronInc, maxChain)
	return f.decodeID(t, f.admin.request(t, http.MethodPost, "/api/v1/policies", body, http.StatusCreated))
}

func (f *fixture) enable(t *testing.T, policyID string) {
	t.Helper()
	f.admin.requestWithID(t, http.MethodPost, "/api/v1/policies/"+policyID+"/enable", "", "enable-"+policyID, http.StatusOK)
}

func (f *fixture) decodeID(t *testing.T, raw []byte) string {
	t.Helper()
	var created createdID
	must(t, json.Unmarshal(raw, &created))
	if created.ID == "" {
		t.Fatalf("missing id: %s", raw)
	}
	return created.ID
}

func (f *fixture) trigger(t *testing.T, policyID, kind, key string, want int) []byte {
	t.Helper()
	body := fmt.Sprintf(`{"kind":%q}`, kind)
	return f.operator.requestWithKey(t, http.MethodPost, "/api/v1/policies/"+policyID+"/runs", body, key, want)
}

func (f *fixture) triggerRun(t *testing.T, policyID, kind, key string) string {
	t.Helper()
	raw := f.trigger(t, policyID, kind, key, http.StatusAccepted)
	var created createdRun
	must(t, json.Unmarshal(raw, &created))
	if created.RunID == "" {
		t.Fatalf("missing run_id: %s", raw)
	}
	return created.RunID
}

func (f *fixture) cancel(t *testing.T, runID string) {
	t.Helper()
	f.operator.request(t, http.MethodPost, "/api/v1/runs/"+runID+"/cancel", "", http.StatusAccepted)
}

func (f *fixture) getRun(t *testing.T, id string) store.Run {
	t.Helper()
	var run store.Run
	must(t, json.Unmarshal(f.operator.request(t, http.MethodGet, "/api/v1/runs/"+id, "", http.StatusOK), &run))
	return run
}

func (f *fixture) listRuns(t *testing.T, query string) []store.Run {
	t.Helper()
	path := "/api/v1/runs"
	if query != "" {
		path += "?" + query
	}
	var page runsPage
	must(t, json.Unmarshal(f.operator.request(t, http.MethodGet, path, "", http.StatusOK), &page))
	return page.Items
}

func (f *fixture) listLogs(t *testing.T, runID string) []store.RunLog {
	t.Helper()
	var page logsPage
	must(t, json.Unmarshal(f.operator.request(t, http.MethodGet, "/api/v1/runs/"+runID+"/logs", "", http.StatusOK), &page))
	return page.Items
}

func (f *fixture) getBackup(t *testing.T, id string) dispatch.BackupView {
	t.Helper()
	var view dispatch.BackupView
	must(t, json.Unmarshal(f.operator.request(t, http.MethodGet, "/api/v1/backups/"+id, "", http.StatusOK), &view))
	return view
}

func (f *fixture) getProtection(t *testing.T) dispatch.Protection {
	t.Helper()
	var view dispatch.Protection
	must(t, json.Unmarshal(f.operator.request(t, http.MethodGet, "/api/v1/sources/"+f.sourceID+"/protection", "", http.StatusOK), &view))
	return view
}

func (f *fixture) listChains(t *testing.T) []dispatch.ChainView {
	t.Helper()
	var page struct {
		Items []dispatch.ChainView `json:"items"`
	}
	must(t, json.Unmarshal(f.operator.request(t, http.MethodGet, "/api/v1/sources/"+f.sourceID+"/chains", "", http.StatusOK), &page))
	return page.Items
}

func (f *fixture) listReplicas(t *testing.T, query string) []dispatch.ReplicaView {
	t.Helper()
	path := "/api/v1/replicas"
	if query != "" {
		path += "?" + query
	}
	var page struct {
		Items []dispatch.ReplicaView `json:"items"`
	}
	must(t, json.Unmarshal(f.operator.request(t, http.MethodGet, path, "", http.StatusOK), &page))
	return page.Items
}

func (f *fixture) waitRun(t *testing.T, id string, statuses ...string) store.Run {
	t.Helper()
	want := map[string]struct{}{}
	for _, status := range statuses {
		want[status] = struct{}{}
	}
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		run := f.getRun(t, id)
		if _, ok := want[run.Status]; ok {
			return run
		}
		if run.Status == store.RunStatusFailed || run.Status == store.RunStatusCanceled || run.Status == store.RunStatusCompleted {
			t.Fatalf("run %s ended as %s class=%v code=%v", id, run.Status, run.ErrorClass, run.ErrorCode)
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach %s", id, strings.Join(statuses, "|"))
	return store.Run{}
}

func (f *fixture) waitReplica(t *testing.T, backupID, status string) dispatch.ReplicaView {
	t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		items := f.listReplicas(t, "source_id="+url.QueryEscape(f.sourceID))
		for _, item := range items {
			if item.BackupID == backupID && item.Status == status {
				return item
			}
		}
		time.Sleep(100 * time.Millisecond)
	}
	t.Fatalf("replica of %s did not become %s", backupID, status)
	return dispatch.ReplicaView{}
}

func (f *fixture) waitXtrabackup(t *testing.T, runID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		run := f.getRun(t, runID)
		if run.Status == store.RunStatusCompleted || run.Status == store.RunStatusFailed || run.Status == store.RunStatusCanceled {
			t.Fatalf("run %s ended as %s before xtrabackup was observed", runID, run.Status)
		}
		if run.Status == store.RunStatusRunning && xtrabackupRunning() {
			return
		}
		time.Sleep(25 * time.Millisecond)
	}
	t.Fatal("xtrabackup never appeared while the run was live")
}

func (f *fixture) dml(t *testing.T, n int) {
	t.Helper()
	cfg := mysqldriver.NewConfig()
	cfg.User = backupUser
	cfg.Passwd = f.secret.source
	cfg.Net = "unix"
	cfg.Addr = socketPath
	cfg.DBName = "fixture"
	db, err := sql.Open("mysql", cfg.FormatDSN())
	must(t, err)
	defer func() { must(t, db.Close()) }()
	_, err = db.ExecContext(t.Context(), "INSERT INTO fixture.records(value) VALUES (?)", n)
	must(t, err)
	payload := bytes.Repeat([]byte{byte(n + 1)}, 1<<20)
	_, err = db.ExecContext(t.Context(), "INSERT INTO fixture.lsn_pressure(payload) VALUES (?)", payload)
	must(t, err)
}

// stopAuthOnce stops the session cleanup goroutine exactly once, whether the
// fixture tears down in-process or hands the database to a serve child.
func (f *fixture) stopAuthOnce() { f.authOnce.Do(f.stopAuth) }

func (f *fixture) shutdownInProcess(t *testing.T) {
	t.Helper()
	if f.closed {
		return
	}
	f.closed = true
	if f.http != nil {
		f.http.Close()
		f.http = nil
	}
	if f.dispatcher != nil {
		_ = f.dispatcher.Stop()
		f.dispatcher = nil
	}
	f.stopAuthOnce()
	must(t, f.database.Close())
	f.database = nil
}

func (f *fixture) startServeChild(t *testing.T) {
	t.Helper()
	f.shutdownInProcess(t)
	if f.child != nil && f.child.ProcessState == nil {
		_ = f.child.Process.Kill()
		_ = f.child.Wait()
	}
	listen := freeListen(t)
	f.listen = listen
	executable, err := os.Executable()
	must(t, err)
	logPath := filepath.Join(f.layout.root, "serve-"+strconv.FormatInt(time.Now().UnixNano(), 10)+".log")
	logFile, err := os.Create(logPath)
	must(t, err)
	f.childLog = logFile
	command := exec.Command(executable, "-test.v", "-test.run=^TestW4ServeChild$", "-test.timeout=15m")
	command.Env = append(os.Environ(),
		"INTERN_W4_SERVE=1",
		"INTERN_W4_METADATA_DIR="+f.layout.metadata,
		"INTERN_W4_LISTEN="+listen,
		"INTERN_W4_TLS_CERT="+f.layout.cert,
		"INTERN_W4_TLS_KEY="+f.layout.key,
		"INTERN_W4_KEK="+f.layout.kek,
		"INTERN_W4_WORK_ROOT="+f.layout.work,
		"INTERN_W4_XTRABACKUP="+xtrabackupBin,
	)
	command.Stdout = logFile
	command.Stderr = logFile
	must(t, command.Start())
	f.child = command
	t.Cleanup(func() {
		if command.ProcessState == nil && command.Process != nil {
			_ = command.Process.Kill()
			_ = command.Wait()
		}
		_ = logFile.Close()
	})
	f.bindChildClients(t)
	f.waitHealth(t)
	f.login(t)
}

func (f *fixture) bindChildClients(t *testing.T) {
	t.Helper()
	base := "https://" + f.listen
	transport := &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.certPool, MinVersion: tls.VersionTLS12}}
	f.admin = newTLSClient(t, base, transport, "admin")
	f.operator = newTLSClient(t, base, transport, "operator")
}

func (f *fixture) waitHealth(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(60 * time.Second)
	client := &http.Client{
		Timeout:   2 * time.Second,
		Transport: &http.Transport{TLSClientConfig: &tls.Config{RootCAs: f.certPool, MinVersion: tls.VersionTLS12}},
	}
	for time.Now().Before(deadline) {
		resp, err := client.Get("https://" + f.listen + "/healthz")
		if err == nil {
			_ = resp.Body.Close()
			if resp.StatusCode == http.StatusOK {
				return
			}
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("serve child never became healthy on %s", f.listen)
}

func (f *fixture) signalChild(t *testing.T, sig syscall.Signal) {
	t.Helper()
	if f.child == nil || f.child.Process == nil {
		t.Fatal("no serve child")
	}
	must(t, f.child.Process.Signal(sig))
}

func (f *fixture) waitChild(t *testing.T, bound time.Duration) {
	t.Helper()
	done := make(chan error, 1)
	go func() { done <- f.child.Wait() }()
	select {
	case <-time.After(bound):
		_ = f.child.Process.Kill()
		t.Fatalf("serve child did not exit within %s", bound)
	case <-done:
	}
}

func (f *fixture) pendingDirs() []string {
	matches, _ := filepath.Glob(filepath.Join(f.layout.local, ".pending-*"))
	return matches
}

func (f *fixture) workDirs() []string {
	matches, _ := filepath.Glob(filepath.Join(f.layout.work, "run-*"))
	return matches
}

func (f *fixture) sqliteCount(t *testing.T, query string, args ...any) int {
	t.Helper()
	var n int
	must(t, f.analysis.QueryRowContext(t.Context(), query, args...).Scan(&n))
	return n
}

func (f *fixture) sqliteString(t *testing.T, query string, args ...any) string {
	t.Helper()
	var value string
	must(t, f.analysis.QueryRowContext(t.Context(), query, args...).Scan(&value))
	return value
}

func (f *fixture) openFTP(t *testing.T) *ftp.Repository {
	t.Helper()
	host := os.Getenv("INTERN_W4_FTP_HOST")
	repo, err := ftp.Open(t.Context(), ftp.Config{
		Address: net.JoinHostPort(host, strconv.Itoa(ftpPort)), Username: ftpUser, Password: f.secret.ftp,
		BasePath: "/", InstanceID: f.instanceID, TLSMode: "plain",
		DialTimeout: 10 * time.Second, IdleTimeout: 30 * time.Second,
	})
	must(t, err)
	t.Cleanup(func() { must(t, repo.Close()) })
	return repo
}

func (f *fixture) hashLocal(t *testing.T, objectPath string) string {
	t.Helper()
	sum, err := fileSHA256(filepath.Join(f.layout.local, objectPath, objectPayload))
	must(t, err)
	return sum
}

func (f *fixture) hashFTP(t *testing.T, remote *ftp.Repository, objectPath string) string {
	t.Helper()
	key := objectPath + "/" + objectPayload
	body, err := remote.Open(t.Context(), key, 0)
	must(t, err)
	defer func() { must(t, body.Close()) }()
	sum := sha256.New()
	_, err = io.Copy(sum, body)
	must(t, err)
	return hex.EncodeToString(sum.Sum(nil))
}

func (f *fixture) listLocalObjects(t *testing.T) []string {
	t.Helper()
	repo, err := local.Open(f.layout.local)
	must(t, err)
	defer func() { must(t, repo.Close()) }()
	keys, err := repo.List("")
	must(t, err)
	return keys
}

func (f *fixture) assertNoSecret(t *testing.T, extra []byte) {
	t.Helper()
	raw, err := os.ReadFile(filepath.Join(f.layout.metadata, "intern.db"))
	must(t, err)
	payloads := [][]byte{f.log.bytes(), extra, raw}
	for _, secret := range []string{f.secret.source, f.secret.ftp, f.secret.admin, f.secret.operator} {
		needle := []byte(secret)
		for _, payload := range payloads {
			if bytes.Contains(payload, needle) {
				t.Fatal("secret appeared in the clear")
			}
		}
	}
}

func newClient(t *testing.T, httpClient *http.Client, base, role string) *client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	must(t, err)
	clone := *httpClient
	clone.Jar = jar
	clone.Timeout = 10 * time.Minute
	clone.CheckRedirect = func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse }
	return &client{t: t, base: base, http: &clone, origin: base, role: role}
}

func newTLSClient(t *testing.T, base string, transport *http.Transport, role string) *client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	must(t, err)
	return &client{
		t: t, base: base, origin: base, role: role,
		http: &http.Client{
			Jar: jar, Timeout: 10 * time.Minute, Transport: transport,
			CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
		},
	}
}

func (c *client) login(t *testing.T, username, password string) {
	t.Helper()
	body := fmt.Sprintf(`{"username":%q,"password":%q}`, username, password)
	c.request(t, http.MethodPost, "/api/v1/auth/login", body, http.StatusOK)
}

func (c *client) request(t *testing.T, method, path, body string, want int) []byte {
	t.Helper()
	return c.requestWithID(t, method, path, body, "", want)
}

func (c *client) requestWithKey(t *testing.T, method, path, body, key string, want int) []byte {
	t.Helper()
	return c.do(t, method, path, body, "", key, want)
}

func (c *client) requestWithID(t *testing.T, method, path, body, requestID string, want int) []byte {
	t.Helper()
	return c.do(t, method, path, body, requestID, "", want)
}

func (c *client) do(t *testing.T, method, path, body, requestID, idempotencyKey string, want int) []byte {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), method, c.base+path, strings.NewReader(body))
	must(t, err)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if method != http.MethodGet && method != http.MethodHead {
		req.Header.Set("Origin", c.origin)
	}
	if requestID != "" {
		req.Header.Set(render.RequestIDHeader, requestID)
	}
	if idempotencyKey != "" {
		req.Header.Set("Idempotency-Key", idempotencyKey)
	}
	resp, err := c.http.Do(req)
	must(t, err)
	defer func() { must(t, resp.Body.Close()) }()
	raw, err := io.ReadAll(resp.Body)
	must(t, err)
	if resp.StatusCode != want {
		t.Fatalf("%s %s as %s: status %d want %d body %s", method, path, c.role, resp.StatusCode, want, raw)
	}
	return raw
}

func (c *client) stream(t *testing.T, path string) (*http.Response, io.ReadCloser) {
	t.Helper()
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.base+path, nil)
	must(t, err)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set("Origin", c.origin)
	streamClient := *c.http
	streamClient.Timeout = 0
	resp, err := streamClient.Do(req)
	must(t, err)
	if resp.StatusCode != http.StatusOK {
		raw, _ := io.ReadAll(resp.Body)
		_ = resp.Body.Close()
		t.Fatalf("sse status %d body %s", resp.StatusCode, raw)
	}
	if resp.Header.Get("Content-Type") != "text/event-stream" {
		_ = resp.Body.Close()
		t.Fatalf("sse content-type %q", resp.Header.Get("Content-Type"))
	}
	return resp, resp.Body
}

func requireNative(t *testing.T) {
	t.Helper()
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Fatal("W4 worker requires linux/amd64")
	}
	if os.Geteuid() == 0 {
		t.Fatal("W4 worker must not run as root")
	}
	if _, err := os.Stat(xtrabackupBin); err != nil {
		t.Fatal("xtrabackup is required")
	}
}

func requirePrivateDir(t *testing.T, path string) {
	t.Helper()
	info, err := os.Lstat(path)
	must(t, err)
	if !info.IsDir() || info.Mode()&os.ModePerm != 0o700 {
		t.Fatalf("%s is not a private 0700 directory", path)
	}
}

func freeListen(t *testing.T) string {
	t.Helper()
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	must(t, err)
	addr := ln.Addr().String()
	must(t, ln.Close())
	return addr
}

func fileSHA256(path string) (string, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}

func xtrabackupRunning() bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	self := strconv.Itoa(os.Getpid())
	for _, entry := range entries {
		if entry.Name() == self || !digitsOnly(entry.Name()) {
			continue
		}
		dir := filepath.Join("/proc", entry.Name())
		comm, _ := os.ReadFile(filepath.Join(dir, "comm"))
		raw, _ := os.ReadFile(filepath.Join(dir, "cmdline"))
		if xtrabackupProcess(string(comm), raw) {
			return true
		}
	}
	return false
}

func xtrabackupProcess(comm string, cmdline []byte) bool {
	if strings.TrimSpace(comm) == "xtrabackup" {
		return true
	}
	for _, part := range bytes.Split(bytes.TrimRight(cmdline, "\x00"), []byte{0}) {
		if len(part) > 0 && filepath.Base(string(part)) == "xtrabackup" {
			return true
		}
	}
	return false
}

func digitsOnly(value string) bool {
	if value == "" {
		return false
	}
	for _, r := range value {
		if r < '0' || r > '9' {
			return false
		}
	}
	return true
}

func must(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

type lockedBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *lockedBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *lockedBuffer) bytes() []byte {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := make([]byte, b.buf.Len())
	copy(out, b.buf.Bytes())
	return out
}

func ptrString(value *string) string {
	if value == nil {
		return ""
	}
	return *value
}

func errorClass(run store.Run) string {
	if run.ErrorClass == nil {
		return ""
	}
	return *run.ErrorClass
}
