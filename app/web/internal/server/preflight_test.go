package server

import (
	"database/sql"
	"encoding/json/v2"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/credential"
)

// preflightFixture serves with the full preflight configuration so the real
// checks can run. The MySQL socket and the FTP address are deliberately
// unreachable, which is the failure this environment can produce natively.
type preflightFixture struct {
	*authFixture
	workRoot string
	settings resources.Settings
}

func newPreflightFixture(t *testing.T) *preflightFixture {
	t.Helper()
	f := newUnconfiguredAuthFixture(t)
	workRoot := filepath.Join(t.TempDir(), "work")
	if err := os.MkdirAll(workRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	keyFile := filepath.Join(f.keys, "kek.json")
	if err := artifact.GenerateKey(keyFile); err != nil {
		t.Fatal(err)
	}
	sealer, err := credential.Open(keyFile, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	settings := resources.Settings{
		Sealer: sealer, KeyFile: keyFile, MetadataDir: f.dir, WorkRoot: workRoot,
		// The binary must exist and be executable for the tool probe; the socket
		// failure precedes it, so any private absolute path is sufficient here.
		XtrabackupPath: "/usr/bin/false", MinWorkBytes: 1, MinBackupBytes: 1,
	}
	f.keyFile = keyFile
	f.sealer = sealer
	engine, err := NewEngineWithResources(f.database, slog.New(slog.NewJSONHandler(&f.logs, nil)), nil, f.service,
		&resources.Service{Store: f.database, Settings: settings})
	if err != nil {
		t.Fatal(err)
	}
	f.engine = engine
	f.client = newContractClient(t, Handler(engine))
	return &preflightFixture{authFixture: f, workRoot: workRoot, settings: settings}
}

// unreachableSocket is an absolute socket path nothing listens on.
func unreachableSocket(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "absent.sock")
	if _, err := os.Stat(path); !os.IsNotExist(err) {
		t.Fatalf("socket path exists: %v", err)
	}
	return path
}

// closedAddress returns a loopback address with no listener.
func closedAddress(t *testing.T) (string, int64) {
	t.Helper()
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	port := listener.Addr().(*net.TCPAddr).Port
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	return "127.0.0.1", int64(port)
}

// TestSourcePreflightMapsRealFailures runs the real physical preflight against an
// unreachable MySQL socket. It must report a structured failure and persist
// nothing, and the audit must record the attempt without a secret.
func TestSourcePreflightMapsRealFailures(t *testing.T) {
	f := newPreflightFixture(t)
	f.seed(t, "operator", "operator")
	cookie := f.login(t, "operator", testPassword)
	repository := f.createRepositoryRow(t, t.Context(), "local-a", newRepositoryDir(t), f.sealer)
	source, err := f.database.CreateSource(t.Context(), store.SourceInput{
		Name: "unreachable", Socket: unreachableSocket(t), Datadir: "/var/lib/mysql",
		Username: "backup", Secret: []byte("{{SECRET_preflight_source}}"),
	}, f.sealer, store.SystemAudit("sources.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	response := f.request(t, "POST", "/api/v1/sources/"+source.ID+"/preflight",
		`{"local_repository_id":"`+repository+`"}`, cookie, http.StatusOK)
	body := decodeResponse[preflightResponse](t, response)
	// The driver reports the failed identity query, because connecting to a
	// socket path that does not exist fails inside the first query.
	assertPreflightBody(t, body, "identity", "identity_query_failed")
	persisted, err := f.database.Source(t.Context(), source.ID)
	if err != nil {
		t.Fatal(err)
	}
	if persisted.LastPreflight != nil || persisted.ServerUUID != nil || persisted.ServerVersion != nil {
		t.Fatalf("failed preflight persisted state: %+v", persisted)
	}
	expectAuditCount(t, f.dir, "sources.preflight", "success", 0)
	// The contract permits paths the client supplied; it forbids the secret and
	// any diagnostic the server observed on its own.
	f.noSecrets(t, response.Body.String(), "{{SECRET_preflight_source}}")
}

// assertPreflightBody checks the reported failure and requires a message.
func assertPreflightBody(t *testing.T, body preflightResponse, check, code string) {
	t.Helper()
	if body.OK || body.Error == nil {
		t.Fatalf("preflight = %+v", body)
	}
	if body.Error.Check != check || body.Error.Code != code || body.Error.Message == "" {
		t.Fatalf("preflight error = %+v", body.Error)
	}
}

// TestRepositoryProbeReportsRealFTPFailure opens a real FTP client against a
// closed loopback port and checks the safe code, with no raw reply text.
func TestRepositoryProbeReportsRealFTPFailure(t *testing.T) {
	f := newPreflightFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	host, port := closedAddress(t)
	repository, err := f.database.CreateRepository(t.Context(), store.RepositoryInput{
		Name: "ftp-closed", Kind: store.RepositoryFTP, TLSMode: store.TLSModePlain,
		FTP:    &store.FTPRepositoryConfig{Host: host, Port: port, BasePath: "/backups", Username: "backup"},
		Secret: []byte("{{SECRET_probe_ftp}}"),
	}, f.sealer, store.SystemAudit("repositories.create", store.AuditDetail{PlaintextFTP: true}))
	if err != nil {
		t.Fatal(err)
	}
	response := f.request(t, "POST", "/api/v1/repositories/"+repository.ID+"/probe", "", cookie, http.StatusOK)
	body := decodeResponse[probeResponse](t, response)
	if body.OK || body.Error == nil {
		t.Fatalf("probe = %+v", body)
	}
	if body.Error.Code != "transport" || body.Error.ReplyCode != 0 {
		t.Fatalf("probe error = %+v", body.Error)
	}
	f.noSecrets(t, response.Body.String(), "{{SECRET_probe_ftp}}")
}

// TestRepositoryProbeLocalWritesAndRemoves proves the local probe really wrote and
// removed a staged object, leaving the repository empty.
func TestRepositoryProbeLocalWritesAndRemoves(t *testing.T) {
	f := newPreflightFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	root := newRepositoryDir(t)
	repository := f.createRepositoryRow(t, t.Context(), "local-probe", root, f.sealer)
	response := f.request(t, "POST", "/api/v1/repositories/"+repository+"/probe", "", cookie, http.StatusOK)
	if body := decodeResponse[probeResponse](t, response); !body.OK || body.Error != nil {
		t.Fatalf("probe = %+v", body)
	}
	entries, err := os.ReadDir(root)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("probe left entries: %v", entries)
	}
}

// TestPolicyEnableFailsWithPreflightAndStaysDisabled is the API-level A3 shape:
// a real preflight failure yields 409 preflight_failed, enabled stays 0, and the
// persisted row is unchanged.
func TestPolicyEnableFailsWithPreflightAndStaysDisabled(t *testing.T) {
	f := newPreflightFixture(t)
	f.seed(t, "operator", "operator")
	cookie := f.login(t, "operator", testPassword)
	local := f.createRepositoryRow(t, t.Context(), "local-a", newRepositoryDir(t), f.sealer)
	source, err := f.database.CreateSource(t.Context(), store.SourceInput{
		Name: "unreachable", Socket: unreachableSocket(t), Datadir: "/var/lib/mysql",
		Username: "backup", Secret: []byte("{{SECRET_enable_source}}"),
	}, f.sealer, store.SystemAudit("sources.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	policy := decodeResponse[store.Policy](t,
		f.request(t, "POST", "/api/v1/policies", policyBody("policy-a", source.ID, local), cookie, http.StatusCreated))
	response := f.request(t, "POST", "/api/v1/policies/"+policy.ID+"/enable", "", cookie, http.StatusConflict)
	assertPreflightFailed(t, response, "identity", "identity_query_failed")
	persisted, err := f.database.Policy(t.Context(), policy.ID)
	if err != nil || persisted.Enabled || persisted.LastPreflight != nil {
		t.Fatalf("enable persisted: %+v %v", persisted, err)
	}
	expectAuditCount(t, f.dir, "policies.enable", "success", 0)

	// Creating with enabled=true is refused the same way and leaves no row.
	enabled := strings.Replace(policyBody("policy-b", source.ID, local), `"enabled":false`, `"enabled":true`, 1)
	created := f.request(t, "POST", "/api/v1/policies", enabled, cookie, http.StatusConflict)
	assertPreflightFailed(t, created, "identity", "identity_query_failed")
	var count int
	if err = queryOne(t, f.dir, `SELECT COUNT(*) FROM policies WHERE name = 'policy-b'`, &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("failed enable persisted a policy row")
	}
	// Updating to enabled=true leaves the stored row exactly as it was.
	updated := strings.Replace(policyBody("policy-a", source.ID, local), `"enabled":false`, `"enabled":true`, 1)
	put := f.request(t, "PUT", "/api/v1/policies/"+policy.ID, updated, cookie, http.StatusConflict)
	assertPreflightFailed(t, put, "identity", "identity_query_failed")
	persisted, err = f.database.Policy(t.Context(), policy.ID)
	if err != nil || persisted.Enabled || persisted.Name != "policy-a" {
		t.Fatalf("update persisted: %+v %v", persisted, err)
	}
	f.noSecrets(t, response.Body.String()+created.Body.String()+put.Body.String(),
		"{{SECRET_enable_source}}", "{{SECRET_ftp_probe_never}}")
}

// assertPreflightFailed checks the frozen 409 body: preflight_failed with the
// structured outcome and no credential material.
func (f *preflightFixture) noSecrets(t *testing.T, response string, secrets ...string) {
	t.Helper()
	assertNoSecrets(t, f.authFixture, response, secrets...)
}

func assertPreflightFailed(t *testing.T, response *httptest.ResponseRecorder, check, code string) {
	t.Helper()
	var body render.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != render.PreflightFailed || body.Error.Message == "" || body.Error.RequestID == "" {
		t.Fatalf("error = %+v", body.Error)
	}
	if body.Error.Preflight == nil || body.Error.Preflight.Error == nil {
		t.Fatalf("preflight detail missing: %s", response.Body)
	}
	if body.Error.Preflight.OK || body.Error.Preflight.Error.Check != check || body.Error.Preflight.Error.Code != code {
		t.Fatalf("preflight = %+v", body.Error.Preflight)
	}
}

// TestPolicyEnableDisableRoundTrip covers the disable path after a failed enable:
// disabling an already disabled policy stays a clean 200.
func TestDisableAfterFailedEnableSucceeds(t *testing.T) {
	f := newPreflightFixture(t)
	f.seed(t, "operator", "operator")
	cookie := f.login(t, "operator", testPassword)
	local := f.createRepositoryRow(t, t.Context(), "local-a", newRepositoryDir(t), f.sealer)
	source := f.createSourceRow(t, t.Context(), "source-a", f.sealer)
	policy := decodeResponse[store.Policy](t,
		f.request(t, "POST", "/api/v1/policies", policyBody("policy-a", source, local), cookie, http.StatusCreated))
	f.request(t, "POST", "/api/v1/policies/"+policy.ID+"/enable", "", cookie, http.StatusConflict)
	disabled := decodeResponse[store.Policy](t,
		f.request(t, "POST", "/api/v1/policies/"+policy.ID+"/disable", "", cookie, http.StatusOK))
	if disabled.Enabled {
		t.Fatal("disable left the policy enabled")
	}
	expectAuditCount(t, f.dir, "policies.disable", "success", 1)
}

func queryOne(t *testing.T, dir, query string, dst *int) error {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "intern.db"))
	if err != nil {
		return err
	}
	defer db.Close()
	return db.QueryRowContext(t.Context(), query).Scan(dst)
}

// TestPreflightBodiesMatchTheContract validates the real preflight and probe
// bodies against the specification, including the optional nested objects.
func TestPreflightBodiesMatchTheContract(t *testing.T) {
	f := newPreflightFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	local := f.createRepositoryRow(t, t.Context(), "local-a", newRepositoryDir(t), f.sealer)
	source, err := f.database.CreateSource(t.Context(), store.SourceInput{
		Name: "unreachable", Socket: unreachableSocket(t), Datadir: "/var/lib/mysql",
		Username: "backup", Secret: []byte(matrixSecret),
	}, f.sealer, store.SystemAudit("sources.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	// roundTrip validates both the request and the concrete response body.
	f.request(t, "POST", "/api/v1/sources/"+source.ID+"/preflight", `{"local_repository_id":"`+local+`"}`, cookie, http.StatusOK)
	f.request(t, "POST", "/api/v1/repositories/"+local+"/probe", "", cookie, http.StatusOK)
	ftp := f.createClosedFTPRepositoryRow(t, "ftp-closed")
	body := decodeResponse[probeResponse](t, f.request(t, "POST", "/api/v1/repositories/"+ftp+"/probe", "", cookie, http.StatusOK))
	if body.OK || body.Error == nil {
		t.Fatalf("probe = %+v", body)
	}
	policy := decodeResponse[store.Policy](t,
		f.request(t, "POST", "/api/v1/policies", policyBody("policy-a", source.ID, local), cookie, http.StatusCreated))
	// The 409 preflight_failed body must satisfy the declared Error schema.
	f.request(t, "POST", "/api/v1/policies/"+policy.ID+"/enable", "", cookie, http.StatusConflict)
}

func (f *preflightFixture) createClosedFTPRepositoryRow(t *testing.T, name string) string {
	t.Helper()
	_, port := closedAddress(t)
	repository, err := f.database.CreateRepository(t.Context(), store.RepositoryInput{
		Name: name, Kind: store.RepositoryFTP, TLSMode: store.TLSModePlain,
		FTP: &store.FTPRepositoryConfig{
			Host: "127.0.0.1", Port: port, BasePath: "/backups", Username: "backup",
		},
		Secret: []byte("{{SECRET_contract_ftp}}"),
	}, f.sealer, store.SystemAudit("repositories.create", store.AuditDetail{PlaintextFTP: true}))
	if err != nil {
		t.Fatal(err)
	}
	return repository.ID
}
