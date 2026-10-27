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
	"errors"
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
	"slices"
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
	childLog   string
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
	root := os.Getenv("INTERN_W4_ROOT")
	if root == "" {
		root = t.TempDir()
	} else {
		// The harness creates INTERN_W4_ROOT itself; the fixture root below it
		// is this worker's to create, with the same 0700 mode requirePrivateDir
		// checks below, so the non-recursive Mkdir calls that follow have a
		// parent. MkdirAll leaves an existing directory alone; a pre-existing
		// root with another mode fails the check instead of being repaired.
		must(t, os.MkdirAll(root, 0o700))
		root = filepath.Join(root, "fixture")
		must(t, os.MkdirAll(root, 0o700))
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

// allLogs reads the whole /runs/{id}/logs sequence through its paged endpoint:
// every page, ascending, until the endpoint stops returning rows.
func (f *fixture) allLogs(t *testing.T, runID string) []store.RunLog {
	t.Helper()
	const pageLimit = 1000
	out := make([]store.RunLog, 0, 16)
	var after int64
	for {
		path := fmt.Sprintf("/api/v1/runs/%s/logs?limit=%d", runID, pageLimit)
		if after > 0 {
			path += "&after_seq=" + strconv.FormatInt(after, 10)
		}
		var page logsPage
		must(t, json.Unmarshal(f.operator.request(t, http.MethodGet, path, "", http.StatusOK), &page))
		if len(page.Items) == 0 {
			return out
		}
		out = append(out, page.Items...)
		after = page.Items[len(page.Items)-1].Seq
		if len(page.Items) < pageLimit {
			return out
		}
	}
}

// sqliteLogs reads one run's stored log rows in sequence order, decoding the
// data column exactly as the store does when it serves the same rows.
func (f *fixture) sqliteLogs(t *testing.T, runID string) []store.RunLog {
	t.Helper()
	rows, err := f.analysis.QueryContext(t.Context(),
		`SELECT run_id, seq, ts, level, event, message, data FROM run_logs WHERE run_id = ? ORDER BY seq`, runID)
	must(t, err)
	defer func() { must(t, rows.Close()) }()
	var out []store.RunLog
	for rows.Next() {
		var entry store.RunLog
		var data sql.NullString
		must(t, rows.Scan(&entry.RunID, &entry.Seq, &entry.TS, &entry.Level, &entry.Event, &entry.Message, &data))
		if data.Valid && data.String != "" {
			var decoded any
			if err := json.Unmarshal([]byte(data.String), &decoded); err == nil {
				entry.Data = decoded
			}
		}
		out = append(out, entry)
	}
	must(t, rows.Err())
	return out
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
			t.Fatalf("run %s ended as %s class=%q code=%q", id, run.Status, errorClass(run), errorCode(run))
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

// waitAllReplicasVerified waits until every replica row of the fixture source
// is verified, so a later protection read compares settled points.
func (f *fixture) waitAllReplicasVerified(t *testing.T) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Minute)
	for time.Now().Before(deadline) {
		statuses, err := f.replicaStatuses()
		must(t, err)
		settled := len(statuses) > 0
		for _, status := range statuses {
			if status != "verified" {
				settled = false
				break
			}
		}
		if settled {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatal("not every replica reached verified before the deadline")
}

func (f *fixture) waitXtrabackup(t *testing.T, runID string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Minute)
	for time.Now().Before(deadline) {
		run := f.getRun(t, runID)
		if run.Status == store.RunStatusCompleted || run.Status == store.RunStatusFailed || run.Status == store.RunStatusCanceled {
			t.Fatalf("run %s ended as %s before xtrabackup was observed", runID, run.Status)
		}
		if run.Status == store.RunStatusRunning && f.xtrabackupRunning() {
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
	f.childLog = logPath
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

// completedBackupsForRun counts the completed backups the physical chain
// recorded for one web run. backups.run_id is the chain's own identifier, so
// the join goes through runs.backup_id, the published backup of the run.
func (f *fixture) completedBackupsForRun(t *testing.T, runID string) int {
	t.Helper()
	return f.sqliteCount(t, `SELECT COUNT(*) FROM backups WHERE status = 'completed'
AND id IN (SELECT backup_id FROM runs WHERE id = ? AND backup_id IS NOT NULL)`, runID)
}

// completedBackupsForPolicy counts one policy's completed backups, read before
// and after a fault so a published row cannot hide behind another policy's
// successful run.
func (f *fixture) completedBackupsForPolicy(t *testing.T, policyID string) int {
	t.Helper()
	return f.sqliteCount(t, `SELECT COUNT(*) FROM backups b JOIN runs r ON r.backup_id = b.id
WHERE r.policy_id = ? AND b.status = 'completed'`, policyID)
}

// completedBackupsSinceFault counts the completed backups created at or after
// the faulted run started. Backup intervals are globally disjoint, and global
// backup concurrency is 1, so any completed row in that window is a backup the
// fault should have prevented. The count reads backups alone: joining runs would
// multiply each row by the number of runs that name it and inflate the count.
func (f *fixture) completedBackupsSinceFault(t *testing.T, runID string) int {
	t.Helper()
	return f.sqliteCount(t, `SELECT COUNT(*) FROM backups
WHERE status = 'completed' AND created_at >= (SELECT started_at FROM runs WHERE id = ?)`, runID)
}

// childExitCode reports the serve child's exit status after waitChild. A
// signal death is reported as a non-zero code.
func (f *fixture) childExitCode(t *testing.T) int {
	t.Helper()
	if f.child == nil || f.child.ProcessState == nil {
		t.Fatal("serve child has not exited")
	}
	return f.child.ProcessState.ExitCode()
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

// assertNoSecret scans every byte the run may have written in the clear: the
// control-plane database file, its WAL and SHM companions, the in-process
// dispatcher log, the caller's extra payload and the serve child's captured
// stdout/stderr. The child log is read from its own handle so a scan during
// the run sees what has been written so far.
func (f *fixture) assertNoSecret(t *testing.T, extra []byte) {
	t.Helper()
	payloads := [][]byte{f.log.bytes(), extra, f.childOutput(t)}
	for _, name := range []string{"intern.db", "intern.db-wal", "intern.db-shm"} {
		path := filepath.Join(f.layout.metadata, name)
		raw, err := os.ReadFile(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		must(t, err)
		payloads = append(payloads, raw)
	}
	for _, secret := range []string{f.secret.source, f.secret.ftp, f.secret.admin, f.secret.operator} {
		needle := []byte(secret)
		for _, payload := range payloads {
			if bytes.Contains(payload, needle) {
				t.Fatal("secret appeared in the clear")
			}
		}
	}
}

// childOutput reads the serve child's captured output file by path: a fresh
// read never moves the offset the child itself writes through. Empty when no
// child has been started by this fixture.
func (f *fixture) childOutput(t *testing.T) []byte {
	t.Helper()
	if f.childLog == "" {
		return nil
	}
	raw, err := os.ReadFile(f.childLog)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	must(t, err)
	return raw
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

// xtrabackupRunning reports whether the configured xtrabackup executable is
// running as a descendant of this worker or of the serve child it started. The
// identity is the resolved /proc/<pid>/exe target, never a comm value or an
// argv basename, and the parent chain must reach one of this fixture's own
// processes, so a supervisor or a stale process cannot be mistaken for the
// tool of the run under test.
func (f *fixture) xtrabackupRunning() bool {
	want, err := filepath.EvalSymlinks(xtrabackupBin)
	if err != nil {
		return false
	}
	roots := []int{os.Getpid()}
	if f.child != nil && f.child.Process != nil {
		roots = append(roots, f.child.Process.Pid)
	}
	return anyProcessMatches(want, roots)
}

// anyProcessMatches walks /proc for one process whose executable is want and
// whose parent chain reaches one of roots.
func anyProcessMatches(want string, roots []int) bool {
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return false
	}
	for _, entry := range entries {
		pid, ok := procPID(entry.Name())
		if ok && executableIs(pid, want) && descendsFrom(pid, roots) {
			return true
		}
	}
	return false
}

// procPID parses one /proc entry name. This worker's own pid is skipped: it is
// the ancestor every match is tested against, never the tool.
func procPID(name string) (int, bool) {
	if name == strconv.Itoa(os.Getpid()) || !digitsOnly(name) {
		return 0, false
	}
	pid, err := strconv.Atoi(name)
	return pid, err == nil
}

// executableIs reports whether /proc/<pid>/exe resolves to the given file.
func executableIs(pid int, want string) bool {
	target, err := os.Readlink(filepath.Join("/proc", strconv.Itoa(pid), "exe"))
	if err != nil {
		return false
	}
	target = strings.TrimSuffix(target, " (deleted)")
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil || resolved != want {
		return false
	}
	// A configured wrapper script runs its interpreter instead; the process
	// identity is then the interpreter plus an argv entry that resolves to the
	// configured file, never a bare basename match.
	if !isScript(want) {
		return true
	}
	return scriptInterpreter(pid, want) && argvResolvesTo(pid, want)
}

// isScript reports whether the file starts with a shebang.
func isScript(path string) bool {
	file, err := os.Open(path)
	if err != nil {
		return false
	}
	defer func() { _ = file.Close() }()
	var magic [2]byte
	if _, err := io.ReadFull(file, magic[:]); err != nil {
		return false
	}
	return magic[0] == '#' && magic[1] == '!'
}

// scriptInterpreter reports whether the process runs a shebang interpreter.
func scriptInterpreter(pid int, script string) bool {
	comm, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "comm"))
	if err != nil {
		return false
	}
	switch strings.TrimSpace(string(comm)) {
	case "perl", "sh", "bash", "dash", "python", "python3":
		return true
	}
	return false
}

// argvResolvesTo reports whether one argv entry names the given file.
func argvResolvesTo(pid int, want string) bool {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "cmdline"))
	if err != nil {
		return false
	}
	for _, part := range bytes.Split(bytes.TrimRight(raw, "\x00"), []byte{0}) {
		if len(part) == 0 {
			continue
		}
		if resolved, err := filepath.EvalSymlinks(string(part)); err == nil && resolved == want {
			return true
		}
	}
	return false
}

// descendsFrom walks /proc/<pid>/stat parent links looking for one of roots.
func descendsFrom(pid int, roots []int) bool {
	wanted := make(map[int]struct{}, len(roots))
	for _, root := range roots {
		wanted[root] = struct{}{}
	}
	for depth := 0; depth < 32 && pid > 1; depth++ {
		parent, err := parentPID(pid)
		if err != nil || parent <= 1 {
			return false
		}
		if _, ok := wanted[parent]; ok {
			return true
		}
		pid = parent
	}
	return false
}

// parentPID reads the ppid field of /proc/<pid>/stat. The comm field may hold
// spaces and parentheses, so the fields after the last ')' are what count.
func parentPID(pid int) (int, error) {
	raw, err := os.ReadFile(filepath.Join("/proc", strconv.Itoa(pid), "stat"))
	if err != nil {
		return 0, err
	}
	text := string(raw)
	end := strings.LastIndexByte(text, ')')
	if end < 0 || end+2 >= len(text) {
		return 0, fmt.Errorf("stat of %d is malformed", pid)
	}
	fields := strings.Fields(text[end+2:])
	if len(fields) < 2 {
		return 0, fmt.Errorf("stat of %d is malformed", pid)
	}
	return strconv.Atoi(fields[1])
}

// digitsOnly reports whether a /proc entry name is a plain pid.
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

// parseStamp parses one stored or served UTC timestamp.
func parseStamp(t *testing.T, value string) time.Time {
	t.Helper()
	if value == "" {
		t.Fatal("missing timestamp")
	}
	parsed, err := time.Parse(time.RFC3339Nano, value)
	must(t, err)
	return parsed
}

// replicationWatch samples the protection points and the replica rows while
// replication is in progress. Every sample reads the protection API first and
// the replica statuses from SQLite immediately after: a status can only move
// forward to verified, so a verify that lands between the two reads can hide a
// violation but can never invent one.
//
// The uploaded-but-unverified window is therefore observed, never inferred: a
// sample counts it only when a replica row is exactly `uploaded` in SQLite and
// the remote protection point does not include that backup. `pending` and
// `uploading` are not the window: nothing is committed yet in those states. The
// sampler runs on its own goroutine with no test helpers, so a broken client
// fails the scenario from the test goroutine instead of panicking the worker.
type replicationWatch struct {
	f               *fixture
	stopCh          chan struct{}
	doneCh          chan struct{}
	mu              sync.Mutex
	samples, behind int
	seen, verified  map[string]struct{}
	uploaded        map[string]struct{}
	failure         string
	stopped         bool
}

// replicationReport is the aggregate evidence of one sampling run.
type replicationReport struct {
	samples, behind, seen, verified, uploaded int
	// window names the backups a sample really caught between uploaded and
	// verified, so the marker can quote state instead of a claim.
	window []string
}

// watchReplication starts sampling. Call report to stop and evaluate it; the
// test cleanup also stops the goroutine, so a failing scenario cannot leave a
// sampler polling a client that a later scenario replaces.
func (f *fixture) watchReplication(t *testing.T) *replicationWatch {
	t.Helper()
	w := &replicationWatch{
		f: f, stopCh: make(chan struct{}), doneCh: make(chan struct{}),
		seen: map[string]struct{}{}, verified: map[string]struct{}{},
		uploaded: map[string]struct{}{},
	}
	go func() {
		defer close(w.doneCh)
		for {
			// The sampling period follows the observed state: while any copy is
			// in flight the sampler runs at the fast period, so the window
			// between the durable `uploaded` status and the verification
			// readback cannot be skipped. Idle periods are sampled slowly, which
			// keeps the request load off the run's own I/O.
			busy := w.sample()
			if w.finishing() {
				return
			}
			period := replicaSampleInterval
			if !busy {
				period = replicaIdleSampleInterval
			}
			select {
			case <-w.stopCh:
				return
			case <-time.After(period):
			}
		}
	}()
	t.Cleanup(func() { w.stopAfterFailure() })
	return w
}

// stopAfterFailure is the cleanup path: stop the goroutine once, whether report
// already did or the scenario failed before reaching it.
func (w *replicationWatch) stopAfterFailure() {
	w.mu.Lock()
	stopped := w.stopped
	w.stopped = true
	w.mu.Unlock()
	if stopped {
		return
	}
	close(w.stopCh)
	<-w.doneCh
}

// finishing reports whether the sampler must stop: a read failed or the
// scenario stopped it.
func (w *replicationWatch) finishing() bool {
	w.mu.Lock()
	defer w.mu.Unlock()
	return w.failure != "" || w.stopped
}

// sample reads one protection point and the replica statuses behind it and
// reports whether any copy was still in flight. A read failure records the
// failure and stops the goroutine instead of retrying a broken client for the
// rest of the scenario; the test goroutine reports it.
func (w *replicationWatch) sample() bool {
	protection, statuses, err := w.read()
	if err != nil {
		w.fail(err.Error())
		return false
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.recordLocked(statuses)
	if violation := unverifiedRemotePoint(protection, statuses); violation != "" {
		w.failLocked(violation)
	}
	if observed := uploadedBehindRemote(protection, statuses); len(observed) > 0 {
		w.behind++
		for _, backup := range observed {
			w.uploaded[backup] = struct{}{}
		}
	}
	return inFlight(statuses)
}

// inFlight reports whether any replica is queued for or in the middle of a
// copy. Those are the states in which the uploaded window can open, so the
// sampler must be at its fast period.
func inFlight(statuses map[string]string) bool {
	for _, status := range statuses {
		switch status {
		case "pending", "uploading", "uploaded":
			return true
		}
	}
	return false
}

// read fetches the protection view and then the replica statuses. The order
// matters: verified only moves forward, so a verify landing between the two
// reads can hide a violation but can never invent one.
func (w *replicationWatch) read() (dispatch.Protection, map[string]string, error) {
	raw, err := w.f.operator.get("/api/v1/sources/" + w.f.sourceID + "/protection")
	if err != nil {
		return dispatch.Protection{}, nil, err
	}
	var protection dispatch.Protection
	if err = json.Unmarshal(raw, &protection); err != nil {
		return dispatch.Protection{}, nil, fmt.Errorf("protection body: %w", err)
	}
	statuses, err := w.f.replicaStatuses()
	if err != nil {
		return dispatch.Protection{}, nil, fmt.Errorf("replica statuses: %w", err)
	}
	return protection, statuses, nil
}

// recordLocked counts the sample and the backup coverage it proved.
func (w *replicationWatch) recordLocked(statuses map[string]string) {
	w.samples++
	for backup, status := range statuses {
		w.seen[backup] = struct{}{}
		if status == "verified" {
			w.verified[backup] = struct{}{}
		}
	}
}

// unverifiedRemotePoint reports the first remote point whose replica is not
// verified in the same snapshot, empty when the whole prefix is verified.
func unverifiedRemotePoint(protection dispatch.Protection, statuses map[string]string) string {
	for _, point := range protection.Remote {
		status, known := statuses[point.BackupID]
		if !known || status != "verified" {
			return fmt.Sprintf("remote protection advanced to %s while its replica was %q", point.BackupID, status)
		}
	}
	return ""
}

// uploadedBehindRemote reports the backups of one real uploaded-but-unverified
// sample: each is exactly `uploaded` in SQLite while the remote protection point
// still does not include it. `pending` and `uploading` mean the copy is not
// committed yet, so they say nothing about whether uploaded is treated as
// verified, and a failed or exhausted row is a different state again. A replica
// status only moves forward, and it is read after the protection view, so a row
// that is uploaded here was not verified when the protection view was read
// either.
func uploadedBehindRemote(protection dispatch.Protection, statuses map[string]string) []string {
	remote := make(map[string]struct{}, len(protection.Remote))
	for _, point := range protection.Remote {
		remote[point.BackupID] = struct{}{}
	}
	var observed []string
	for backup, status := range statuses {
		if status != "uploaded" {
			continue
		}
		if _, included := remote[backup]; included {
			continue
		}
		observed = append(observed, backup)
	}
	slices.Sort(observed)
	return observed
}

// replicaStatuses reads every replica status of the fixture source from SQLite.
func (f *fixture) replicaStatuses() (map[string]string, error) {
	rows, err := f.analysis.QueryContext(context.Background(),
		`SELECT r.backup_id, r.status FROM replicas r
JOIN backups b ON b.id = r.backup_id JOIN chains c ON c.id = b.chain_id
WHERE c.source_id = ? AND r.repository_id = ?`, f.sourceID, f.ftpID)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rows.Close() }()
	statuses := map[string]string{}
	for rows.Next() {
		var backup, status string
		if err := rows.Scan(&backup, &status); err != nil {
			return nil, err
		}
		statuses[backup] = status
	}
	return statuses, rows.Err()
}

func (w *replicationWatch) fail(message string) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.failLocked(message)
}

func (w *replicationWatch) failLocked(message string) {
	if w.failure == "" {
		w.failure = message
	}
}

// report stops the sampler and evaluates its evidence.
func (w *replicationWatch) report(t *testing.T) replicationReport {
	t.Helper()
	w.stopAfterFailure()
	w.mu.Lock()
	defer w.mu.Unlock()
	if w.failure != "" {
		t.Fatal(w.failure)
	}
	if w.samples == 0 {
		t.Fatal("the replication sampler never ran a complete sample")
	}
	window := make([]string, 0, len(w.uploaded))
	for backup := range w.uploaded {
		window = append(window, backup)
	}
	slices.Sort(window)
	return replicationReport{
		samples: w.samples, behind: w.behind, seen: len(w.seen), verified: len(w.verified),
		uploaded: len(w.uploaded), window: window,
	}
}

// get performs one read used from the sampler goroutine, where a test helper
// may not call t.Fatal.
func (c *client) get(path string) ([]byte, error) {
	req, err := http.NewRequestWithContext(context.Background(), http.MethodGet, c.base+path, nil)
	if err != nil {
		return nil, err
	}
	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, err
	}
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("%s as %s: status %d", path, c.role, resp.StatusCode)
	}
	return raw, nil
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

// errorClass and errorCode dereference the run's optional failure fields, so a
// failure message prints the class and code themselves rather than two
// pointers.
func errorClass(run store.Run) string { return ptrString(run.ErrorClass) }

func errorCode(run store.Run) string { return ptrString(run.ErrorCode) }
