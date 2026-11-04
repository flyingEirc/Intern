package server

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/x509"
	"crypto/x509/pkix"
	"database/sql"
	"encoding/json/v2"
	"encoding/pem"
	"log/slog"
	"math/big"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/credential"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
)

// repositoryBody and sourceBody are built per call because the local root must
// be a fresh absolute directory.
func localRepository(name, root string) string {
	return `{"name":"` + name + `","kind":"local","tls_mode":"none","root":"` + root + `"}`
}

func ftpRepository(name, password string) string {
	return `{"name":"` + name + `","kind":"ftp","tls_mode":"plain","ftp":{"host":"127.0.0.1","port":21,` +
		`"base_path":"/backups","username":"backup"},"password":"` + password + `"}`
}

func sourceBody(name string) string {
	return `{"name":"` + name + `","socket":"/run/mysqld/mysqld.sock","datadir":"/var/lib/mysql",` +
		`"username":"backup","password":"` + matrixSecret + `"}`
}

func policyBody(name, sourceID, repositoryID string) string {
	return `{"name":"` + name + `","source_id":"` + sourceID + `","local_repository_id":"` + repositoryID + `",` +
		`"full_cron":"0 2 * * *","incremental_cron":"0 * * * *","timezone":"UTC","max_chain_length":3,` +
		`"parallel":2,"throttle":0,"zstd_level":3,"page_tracking":false,"enabled":false}`
}

func newRepositoryDir(t *testing.T) string {
	t.Helper()
	root := filepath.Join(t.TempDir(), "repository")
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	return root
}

// assertNoSecrets scans the response, the audit table and the logs for the
// supplied secret markers and for the stored ciphertext.
func assertNoSecrets(t *testing.T, f *authFixture, response string, secrets ...string) {
	t.Helper()
	stored := rawResourceSecrets(t, f.dir)
	all := response + rawAudit(t, f.dir) + f.logs.String() + stored
	for _, secret := range secrets {
		if secret != "" && strings.Contains(all, secret) {
			t.Fatalf("secret %q leaked", secret)
		}
	}
}

// rawResourceSecrets reads every repository and source text column, so a leaked
// value cannot hide in a column the API does not return.
func rawResourceSecrets(t *testing.T, dir string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "intern.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var text string
	query := `SELECT COALESCE(group_concat(name || kind || config_json || hex(secret_ciphertext) || tls_mode, ''), '') FROM repositories`
	if err = db.QueryRowContext(t.Context(), query).Scan(&text); err != nil {
		t.Fatal(err)
	}
	var sources string
	query = `SELECT COALESCE(group_concat(name || socket || datadir || username || hex(secret_ciphertext), ''), '') FROM sources`
	if err = db.QueryRowContext(t.Context(), query).Scan(&sources); err != nil {
		t.Fatal(err)
	}
	return text + sources
}

func TestRepositoryCRUDIsWriteOnly(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	root := newRepositoryDir(t)

	local := f.request(t, "POST", "/api/v1/repositories", localRepository("local-a", root), cookie, http.StatusCreated)
	created := decodeResponse[store.Repository](t, local)
	assertLocalRepository(t, created, root)
	ftp := f.request(t, "POST", "/api/v1/repositories", ftpRepository("ftp-a", "{{SECRET_ftp_a_password}}"), cookie, http.StatusCreated)
	ftpCreated := decodeResponse[store.Repository](t, ftp)
	assertFTPRepository(t, ftpCreated)
	assertNoSecrets(t, f, local.Body.String()+ftp.Body.String(), "{{SECRET_ftp_a_password}}")

	read := f.request(t, "GET", "/api/v1/repositories/"+ftpCreated.ID, "", cookie, http.StatusOK)
	if strings.Contains(read.Body.String(), "secret_ciphertext") || !strings.Contains(read.Body.String(), `"has_secret":true`) {
		t.Fatalf("read body = %s", read.Body)
	}
	assertFTPRotation(t, f, cookie, ftpCreated.ID)
	f.request(t, "DELETE", "/api/v1/repositories/"+ftpCreated.ID, "", cookie, http.StatusNoContent)
	f.request(t, "GET", "/api/v1/repositories/"+ftpCreated.ID, "", cookie, http.StatusNotFound)
	expectAuditCount(t, f.dir, "repositories.create", "success", 2)
	expectAuditCount(t, f.dir, "repositories.update", "success", 2)
	expectAuditCount(t, f.dir, "repositories.delete", "success", 1)
}

func assertLocalRepository(t *testing.T, repository store.Repository, root string) {
	t.Helper()
	if repository.Kind != store.RepositoryLocal || repository.TLSMode != store.TLSModeNone ||
		repository.HasSecret || repository.PlaintextCredentials {
		t.Fatalf("local repository = %+v", repository)
	}
	if repository.Local == nil || repository.Local.Root != root || repository.FTP != nil {
		t.Fatalf("local repository config = %+v", repository.Local)
	}
}

func assertFTPRepository(t *testing.T, repository store.Repository) {
	t.Helper()
	if repository.Kind != store.RepositoryFTP || repository.TLSMode != store.TLSModePlain ||
		!repository.HasSecret || !repository.PlaintextCredentials {
		t.Fatalf("ftp repository = %+v", repository)
	}
	if repository.FTP == nil || repository.FTP.Port != 21 || repository.Local != nil {
		t.Fatalf("ftp repository config = %+v", repository.FTP)
	}
}

// assertFTPRotation proves a supplied password replaces the ciphertext in place
// and an omitted one keeps it.
func assertFTPRotation(t *testing.T, f *authFixture, cookie *http.Cookie, id string) {
	t.Helper()
	rotation := `{"name":"ftp-a","kind":"ftp","tls_mode":"plain","ftp":{"host":"127.0.0.1","port":21,` +
		`"base_path":"/backups","username":"backup"},"password":"{{SECRET_ftp_b_password}}"}`
	f.request(t, "PUT", "/api/v1/repositories/"+id, rotation, cookie, http.StatusOK)
	first := storedCiphertext(t, f.dir, "repositories", id)
	if first == "" {
		t.Fatal("rotation removed the ciphertext")
	}
	opened, err := f.sealer.Unseal("repositories", id, "secret_ciphertext", []byte(first))
	if err != nil || string(opened) != "{{SECRET_ftp_b_password}}" {
		t.Fatalf("rotated secret = %q %v", opened, err)
	}
	second := f.request(t, "PUT", "/api/v1/repositories/"+id,
		`{"name":"ftp-a","kind":"ftp","tls_mode":"plain","ftp":{"host":"127.0.0.1","port":21,"base_path":"/backups","username":"backup"}}`,
		cookie, http.StatusOK)
	if storedCiphertext(t, f.dir, "repositories", id) != first {
		t.Fatal("an omitted password replaced the stored secret")
	}
	assertNoSecrets(t, f, second.Body.String(), "{{SECRET_ftp_b_password}}")
}

// requestRejected asserts that a request is refused. The kin-openapi round trip
// still validates the body whenever the schema accepts it; when the schema itself
// rejects the body, the request is marked invalid for the run and only the
// response is validated. Either way the observed status must match.
func (f *authFixture) requestRejected(t *testing.T, method, path, body string, cookie *http.Cookie, status int) *httptest.ResponseRecorder {
	t.Helper()
	response, err := f.client.roundTripRequest(apiRequest(method, path, body, cookie), false)
	if err != nil {
		if !strings.Contains(err.Error(), "request contract") {
			t.Fatal(err)
		}
		response, err = f.client.roundTripRequest(apiRequest(method, path, body, cookie), true)
		if err != nil {
			t.Fatal(err)
		}
	}
	if response.Code != status {
		t.Fatalf("status=%d, want=%d; %s", response.Code, status, response.Body)
	}
	return response
}

// storedCiphertext returns the stored ciphertext of a sealed column.
func storedCiphertext(t *testing.T, dir, table, id string) string {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "intern.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var ciphertext []byte
	query := `SELECT secret_ciphertext FROM ` + table + ` WHERE id = ?`
	if err = db.QueryRowContext(t.Context(), query, id).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	return string(ciphertext)
}

// TestRepositoryValidationRejectsUnsafeShapes pins the frozen 422/400 split of a
// repository body. A tls_mode that does not match the kind is a shape violation
// (422), and so is a ca_pem that is misplaced, unparsable or a private key; every
// other malformed field stays a 400.
func TestRepositoryValidationRejectsUnsafeShapes(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	root := newRepositoryDir(t)
	bundle, privateKey := testBundles(t)
	rejected := map[string]string{
		"omitted tls_mode":   `{"name":"a","kind":"ftp","ftp":{"host":"127.0.0.1","port":21,"base_path":"/b","username":"u"},"password":"x"}`,
		"unknown tls_mode":   `{"name":"a","kind":"ftp","tls_mode":"starttls","ftp":{"host":"127.0.0.1","port":21,"base_path":"/b","username":"u"},"password":"x"}`,
		"none for ftp":       `{"name":"a","kind":"ftp","tls_mode":"none","ftp":{"host":"127.0.0.1","port":21,"base_path":"/b","username":"u"},"password":"x"}`,
		"local with plain":   `{"name":"a","kind":"local","tls_mode":"plain","root":"` + root + `"}`,
		"local with ftps":    `{"name":"a","kind":"local","tls_mode":"explicit","root":"` + root + `"}`,
		"ca_pem on plain":    repositoryBody("a", "plain", 21, bundle),
		"ca_pem not a cert":  repositoryBody("a", "explicit", 21, "not a certificate"),
		"ca_pem private key": repositoryBody("a", "implicit", 990, privateKey),
		// The bundle limit sits under the request-body limit, so an over-limit
		// bundle is the handler's 422 and never the generic 413.
		"ca_pem oversized": repositoryBody("a", "explicit", 21, strings.Repeat("a", ftp.MaxCAPemBytes+1)),
	}
	for name, body := range rejected {
		t.Run(name, func(t *testing.T) {
			response := f.requestRejected(t, "POST", "/api/v1/repositories", body, cookie, http.StatusUnprocessableEntity)
			assertSingleError(t, response, render.ValidationFailed)
		})
	}
	for name, body := range map[string]string{
		"relative root":       `{"name":"a","kind":"local","tls_mode":"none","root":"relative/path"}`,
		"empty name":          `{"name":"","kind":"local","tls_mode":"none","root":"` + root + `"}`,
		"long name":           `{"name":"` + strings.Repeat("n", 65) + `","kind":"local","tls_mode":"none","root":"` + root + `"}`,
		"unknown kind":        `{"name":"a","kind":"s3","tls_mode":"none","root":"` + root + `"}`,
		"ftp without secret":  `{"name":"a","kind":"ftp","tls_mode":"plain","ftp":{"host":"127.0.0.1","port":21,"base_path":"/b","username":"u"}}`,
		"ftps without secret": `{"name":"a","kind":"ftp","tls_mode":"explicit","ftp":{"host":"127.0.0.1","port":21,"base_path":"/b","username":"u"}}`,
		"ftp relative path":   `{"name":"a","kind":"ftp","tls_mode":"plain","ftp":{"host":"127.0.0.1","port":21,"base_path":"b","username":"u"},"password":"x"}`,
		"ftp bad port":        `{"name":"a","kind":"ftp","tls_mode":"plain","ftp":{"host":"127.0.0.1","port":0,"base_path":"/b","username":"u"},"password":"x"}`,
		"local with ftp":      `{"name":"a","kind":"local","tls_mode":"none","root":"` + root + `","ftp":{"host":"h","port":21,"base_path":"/b","username":"u"}}`,
	} {
		t.Run(name, func(t *testing.T) {
			// These bodies are out of the OpenAPI schema as well as out of the
			// handler's rules; both layers must reject them.
			response := f.requestRejected(t, "POST", "/api/v1/repositories", body, cookie, http.StatusBadRequest)
			assertSingleError(t, response, render.ValidationFailed)
		})
	}
	expectAuditCount(t, f.dir, "repositories.create", "success", 0)
}

// repositoryBodyPassword is distinctive enough for assertNoSecrets to prove the
// stored FTPS credential never reaches a response, the audit table or the logs.
const repositoryBodyPassword = "unit-ftps-secret-value"

// repositoryBody builds one FTP repository body with the given transport mode,
// trust bundle and password. Encoding it keeps a multi-line PEM out of
// hand-written JSON.
func repositoryBody(name, mode string, port int64, bundle string) string {
	type ftp struct {
		Host     string `json:"host"`
		Port     int64  `json:"port"`
		BasePath string `json:"base_path"`
		Username string `json:"username"`
		CAPem    string `json:"ca_pem,omitempty"`
	}
	body := struct {
		Name     string `json:"name"`
		Kind     string `json:"kind"`
		TLSMode  string `json:"tls_mode"`
		FTP      ftp    `json:"ftp"`
		Password string `json:"password"`
	}{
		Name: name, Kind: "ftp", TLSMode: mode, Password: repositoryBodyPassword,
		FTP: ftp{Host: "ftps.example", Port: port, BasePath: "/backups", Username: "backup", CAPem: bundle},
	}
	encoded, err := json.Marshal(body)
	if err != nil {
		panic(err)
	}
	return string(encoded)
}

// testBundles returns one parsable CA certificate PEM and one private key PEM:
// the first is what a repository may store, the second must never be accepted.
func testBundles(t *testing.T) (bundle, privateKey string) {
	t.Helper()
	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	template := &x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "unit-test-ca"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign,
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, template, template, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	bundle = string(pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der}))
	secret, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	return bundle, string(pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: secret}))
}

// TestFTPSRepositoryCRUD covers S2-A3 at the API: both FTPS modes are created,
// read back with the bundle the operator sent, report no plaintext credential and
// record no plaintext_ftp audit, and refuse any later change of transport.
func TestFTPSRepositoryCRUD(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	bundle, _ := testBundles(t)
	creates := 0
	for _, testCase := range []struct {
		mode string
		port int64
	}{{"explicit", 21}, {"implicit", 990}} {
		t.Run(testCase.mode, func(t *testing.T) {
			creates++
			requireFTPSRepositoryLifecycle(t, f, cookie, bundle, testCase.mode, testCase.port, creates)
		})
	}
}

// requireFTPSRepositoryLifecycle creates one FTPS repository, checks the
// stored and audited shape, reads it back, and proves the transport cannot be
// moved to plain while an in-mode replacement still succeeds. creates is the
// number of successful repository creates expected in the audit afterwards.
func requireFTPSRepositoryLifecycle(t *testing.T, f *authFixture, cookie *http.Cookie, bundle, mode string, port int64, creates int) {
	t.Helper()
	repository := createFTPSRepository(t, f, cookie, bundle, mode, port)
	if got := countAudit(t, f.dir, "repositories.create", "success"); got != creates {
		t.Fatalf("creates audited = %d, want %d", got, creates)
	}
	if strings.Contains(rawAudit(t, f.dir), `"plaintext_ftp":true`) {
		t.Fatal("an FTPS create recorded plaintext_ftp")
	}
	read := f.request(t, "GET", "/api/v1/repositories/"+repository.ID, "", cookie, http.StatusOK)
	if !strings.Contains(read.Body.String(), `"tls_mode":"`+mode+`"`) ||
		strings.Contains(read.Body.String(), `"plaintext_credentials":true`) {
		t.Fatalf("%s read body = %s", mode, read.Body)
	}
	assertNoSecrets(t, f, read.Body.String(), repositoryBodyPassword)
	moved := f.requestRejected(t, "PUT", "/api/v1/repositories/"+repository.ID,
		repositoryBody("ftps-"+mode, "plain", port, ""), cookie, http.StatusUnprocessableEntity)
	assertSingleError(t, moved, render.ValidationFailed)
	rotated := repositoryBody("ftps-"+mode, mode, port, bundle)
	f.request(t, "PUT", "/api/v1/repositories/"+repository.ID, rotated, cookie, http.StatusOK)
}

// createFTPSRepository creates one FTPS repository and checks the returned
// row carries the selected mode, the sent bundle and no plaintext credential.
func createFTPSRepository(t *testing.T, f *authFixture, cookie *http.Cookie, bundle, mode string, port int64) store.Repository {
	t.Helper()
	created := f.request(t, "POST", "/api/v1/repositories",
		repositoryBody("ftps-"+mode, mode, port, bundle), cookie, http.StatusCreated)
	var repository store.Repository
	if err := json.Unmarshal(created.Body.Bytes(), &repository); err != nil {
		t.Fatal(err)
	}
	if repository.Kind != store.RepositoryFTP || repository.TLSMode != mode ||
		!repository.HasSecret || repository.PlaintextCredentials {
		t.Fatalf("%s repository = %+v", mode, repository)
	}
	if repository.FTP == nil || repository.FTP.CAPem != bundle || repository.FTP.Port != port {
		t.Fatalf("%s repository config = %+v", mode, repository.FTP)
	}
	return repository
}

// TestLocalRepositoryRootMustNotContainKEK covers the repository-time location
// check: startup only rejects a KEK inside metadata_dir and work_root, so a new
// local repository root is checked when it is created.
func TestLocalRepositoryRootMustNotContainKEK(t *testing.T) {
	f := newUnconfiguredAuthFixture(t)
	f.seed(t, "admin", "admin")
	container := t.TempDir()
	keyFile := filepath.Join(container, "kek.json")
	if err := artifact.GenerateKey(keyFile); err != nil {
		t.Fatal(err)
	}
	sealer, err := credential.Open(keyFile, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngineWithResources(f.database, slog.New(slog.NewJSONHandler(&f.logs, nil)), nil, f.service,
		&resources.Service{Store: f.database, Settings: resources.Settings{
			Sealer: sealer, KeyFile: keyFile, MetadataDir: f.dir,
		}}, nil)
	if err != nil {
		t.Fatal(err)
	}
	f.client = newContractClient(t, Handler(engine))
	cookie := f.login(t, "admin", testPassword)
	response := f.request(t, "POST", "/api/v1/repositories", localRepository("local-a", container), cookie, http.StatusBadRequest)
	assertSingleError(t, response, render.ValidationFailed)
	outside := newRepositoryDir(t)
	if outside == container {
		t.Fatal("test roots coincide")
	}
	f.request(t, "POST", "/api/v1/repositories", localRepository("local-a", outside), cookie, http.StatusCreated)
}

// TestDeleteRefusesReferencedRows covers both reference kinds: the rows the
// policy uses cannot be deleted, and unreferenced rows can.
func TestDeleteRefusesReferencedRows(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	seeded := f.seedResources(t)
	for _, target := range []string{
		"/api/v1/repositories/" + seeded.policyRepository,
		"/api/v1/sources/" + seeded.policySource,
	} {
		response := f.request(t, "DELETE", target, "", cookie, http.StatusConflict)
		assertSingleError(t, response, render.Conflict)
	}
	f.request(t, "DELETE", "/api/v1/repositories/"+seeded.freeRepository, "", cookie, http.StatusNoContent)
	f.request(t, "DELETE", "/api/v1/sources/"+seeded.freeSource, "", cookie, http.StatusNoContent)
	expectAuditCount(t, f.dir, "repositories.delete", "success", 1)
	expectAuditCount(t, f.dir, "sources.delete", "success", 1)
	// A physical chain keeps its source, even when no policy uses it.
	chained := f.createSourceRow(t, t.Context(), "chained-source", f.sealer)
	insertChain(t, f.dir, chained)
	f.request(t, "DELETE", "/api/v1/sources/"+chained, "", cookie, http.StatusConflict)
	expectAuditCount(t, f.dir, "sources.delete", "success", 1)
}

// insertChain adds one physical chain row that refers to sourceID. chains is
// owned by the physical migration, so this uses the shared file directly.
func insertChain(t *testing.T, dir, sourceID string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "intern.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	_, err = db.ExecContext(t.Context(), `INSERT INTO chains
		(id, source_id, server_uuid, server_version, tool_version, started_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		"chain-1", sourceID, "00000000-0000-0000-0000-000000000000", "8.4.11", "8.4.0-7", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
}

func TestSourceCRUDNameConflictsAndPreflightUnavailable(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "operator", "operator")
	cookie := f.login(t, "operator", testPassword)
	created := f.request(t, "POST", "/api/v1/sources", sourceBody("source-a"), cookie, http.StatusCreated)
	source := decodeResponse[store.Source](t, created)
	if !source.HasSecret || source.ServerUUID != nil || source.LastPreflight != nil {
		t.Fatalf("source = %+v", source)
	}
	assertNoSecrets(t, f, created.Body.String(), matrixSecret)
	f.request(t, "POST", "/api/v1/sources", sourceBody("SOURCE-A"), cookie, http.StatusConflict)
	for name, body := range map[string]string{
		"relative socket": `{"name":"b","socket":"relative","datadir":"/var/lib/mysql","username":"u","password":"x"}`,
		"missing secret":  `{"name":"b","socket":"/s","datadir":"/d","username":"u"}`,
		"empty name":      `{"name":"","socket":"/s","datadir":"/d","username":"u","password":"x"}`,
	} {
		t.Run(name, func(t *testing.T) {
			response := f.requestRejected(t, "POST", "/api/v1/sources", body, cookie, http.StatusBadRequest)
			assertSingleError(t, response, render.ValidationFailed)
		})
	}
	// The physical preflight needs a work root, an XtraBackup path and capacity
	// thresholds; this fixture has none. The endpoint reports 503 and the source
	// keeps its empty preflight state.
	repository := f.createRepositoryRow(t, t.Context(), "local-a", newRepositoryDir(t), f.sealer)
	unavailable := f.request(t, "POST", "/api/v1/sources/"+source.ID+"/preflight",
		`{"local_repository_id":"`+repository+`"}`, cookie, http.StatusServiceUnavailable)
	assertSingleError(t, unavailable, render.Unavailable)
	f.requestRejected(t, "POST", "/api/v1/sources/"+source.ID+"/preflight", `{}`, cookie, http.StatusBadRequest)
	reloaded, err := f.database.Source(t.Context(), source.ID)
	if err != nil || reloaded.LastPreflight != nil || reloaded.ServerUUID != nil {
		t.Fatalf("failed preflight persisted state: %+v %v", reloaded, err)
	}
	// Updating without a password keeps the sealed secret.
	f.request(t, "PUT", "/api/v1/sources/"+source.ID,
		`{"name":"source-a","socket":"/run/mysqld/other.sock","datadir":"/var/lib/mysql","username":"backup"}`, cookie, http.StatusOK)
	opened, err := f.database.SourceSecret(t.Context(), source.ID, f.sealer)
	if err != nil || string(opened) != matrixSecret {
		t.Fatalf("source secret = %q %v", opened, err)
	}
}

func TestPolicyValidationAndRoleKinds(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "operator", "operator")
	cookie := f.login(t, "operator", testPassword)
	source := f.createSourceRow(t, t.Context(), "source-a", f.sealer)
	local := f.createRepositoryRow(t, t.Context(), "local-a", newRepositoryDir(t), f.sealer)
	ftp := f.createFTPRepositoryRow(t, t.Context(), "ftp-a", "{{SECRET_policy_ftp}}", f.sealer)

	for name, body := range map[string]string{
		"bad full cron":     strings.Replace(policyBody("p", source, local), `"full_cron":"0 2 * * *"`, `"full_cron":"not a cron"`, 1),
		"bad incremental":   strings.Replace(policyBody("p", source, local), `"incremental_cron":"0 * * * *"`, `"incremental_cron":"61 * * * *"`, 1),
		"bad timezone":      strings.Replace(policyBody("p", source, local), `"timezone":"UTC"`, `"timezone":"Mars/Base"`, 1),
		"missing source":    strings.Replace(policyBody("p", source, local), `"source_id":"`+source+`"`, `"source_id":"missing"`, 1),
		"missing local":     strings.Replace(policyBody("p", source, local), `"local_repository_id":"`+local+`"`, `"local_repository_id":"missing"`, 1),
		"local is ftp":      strings.Replace(policyBody("p", source, local), `"local_repository_id":"`+local+`"`, `"local_repository_id":"`+ftp+`"`, 1),
		"replica is local":  strings.Replace(policyBody("p", source, local), `"page_tracking":false`, `"replica_repository_id":"`+local+`","page_tracking":false`, 1),
		"zero chain length": strings.Replace(policyBody("p", source, local), `"max_chain_length":3`, `"max_chain_length":0`, 1),
		"zero parallel":     strings.Replace(policyBody("p", source, local), `"parallel":2`, `"parallel":0`, 1),
		"negative throttle": strings.Replace(policyBody("p", source, local), `"throttle":0`, `"throttle":-1`, 1),
		"zstd out of range": strings.Replace(policyBody("p", source, local), `"zstd_level":3`, `"zstd_level":23`, 1),
		"empty name":        strings.Replace(policyBody("p", source, local), `"name":"p"`, `"name":""`, 1),
	} {
		t.Run(name, func(t *testing.T) {
			response := f.requestRejected(t, "POST", "/api/v1/policies", body, cookie, http.StatusBadRequest)
			assertSingleError(t, response, render.ValidationFailed)
		})
	}
	created := f.request(t, "POST", "/api/v1/policies", policyBody("policy-a", source, local), cookie, http.StatusCreated)
	policy := decodeResponse[store.Policy](t, created)
	if policy.Enabled || policy.ReplicaRepositoryID != nil || policy.LastPreflight != nil {
		t.Fatalf("policy = %+v", policy)
	}
	f.request(t, "POST", "/api/v1/policies", policyBody("POLICY-A", source, local), cookie, http.StatusConflict)
	replica := strings.Replace(policyBody("policy-b", source, local), `"page_tracking":false`,
		`"replica_repository_id":"`+ftp+`","page_tracking":false`, 1)
	withReplica := decodeResponse[store.Policy](t, f.request(t, "POST", "/api/v1/policies", replica, cookie, http.StatusCreated))
	if withReplica.ReplicaRepositoryID == nil || *withReplica.ReplicaRepositoryID != ftp {
		t.Fatalf("replica = %+v", withReplica.ReplicaRepositoryID)
	}
	f.request(t, "POST", "/api/v1/policies/"+policy.ID+"/disable", "", cookie, http.StatusOK)
	assertNoSecrets(t, f, created.Body.String()+replica, "{{SECRET_policy_ftp}}")
	expectAuditCount(t, f.dir, "policies.create", "success", 2)
	expectAuditCount(t, f.dir, "policies.disable", "success", 1)
}

// TestPolicyEnableRequiresPreflight covers the deployment that has no preflight
// inputs: enable must not silently flip the flag on.
func TestPolicyEnableRequiresPreflight(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "operator", "operator")
	cookie := f.login(t, "operator", testPassword)
	source := f.createSourceRow(t, t.Context(), "source-a", f.sealer)
	local := f.createRepositoryRow(t, t.Context(), "local-a", newRepositoryDir(t), f.sealer)
	// enabled=true cannot be satisfied without preflight inputs.
	enabled := strings.Replace(policyBody("policy-a", source, local), `"enabled":false`, `"enabled":true`, 1)
	f.request(t, "POST", "/api/v1/policies", enabled, cookie, http.StatusServiceUnavailable)
	policy := decodeResponse[store.Policy](t,
		f.request(t, "POST", "/api/v1/policies", policyBody("policy-a", source, local), cookie, http.StatusCreated))
	f.request(t, "POST", "/api/v1/policies/"+policy.ID+"/enable", "", cookie, http.StatusServiceUnavailable)
	reloaded, err := f.database.Policy(t.Context(), policy.ID)
	if err != nil || reloaded.Enabled || reloaded.LastPreflight != nil {
		t.Fatalf("enable persisted without preflight: %+v %v", reloaded, err)
	}
	expectAuditCount(t, f.dir, "policies.enable", "success", 0)
	// Disable is always allowed.
	f.request(t, "POST", "/api/v1/policies/"+policy.ID+"/disable", "", cookie, http.StatusOK)
}

func TestResourceListsAreBoundedAndSecretFree(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	f.seedResources(t)
	for _, path := range []string{"/api/v1/repositories", "/api/v1/sources", "/api/v1/policies"} {
		assertListRejectsBadPagination(t, f, path, cookie)
		assertListIsSecretFree(t, f, path, cookie)
	}
	assertListCursorIsExclusive(t, f, cookie)
}

// assertListRejectsBadPagination covers the limit range and the cursor form.
func assertListRejectsBadPagination(t *testing.T, f *authFixture, path string, cookie *http.Cookie) {
	t.Helper()
	for _, query := range []string{"?limit=0", "?limit=201", "?limit=abc", "?before=not-a-uuid"} {
		response := f.requestRejected(t, "GET", path+query, "", cookie, http.StatusBadRequest)
		assertSingleError(t, response, render.ValidationFailed)
	}
}

// assertListIsSecretFree requires one bounded page with no sealed column.
func assertListIsSecretFree(t *testing.T, f *authFixture, path string, cookie *http.Cookie) {
	t.Helper()
	list := f.request(t, "GET", path+"?limit=1", "", cookie, http.StatusOK)
	body := list.Body.String()
	if !strings.Contains(body, `"next_before":`) {
		t.Fatalf("%s page = %s", path, body)
	}
	assertNoSecrets(t, f, body, matrixSecret)
	page := decodeResponse[struct {
		Items      []map[string]any `json:"items"`
		NextBefore *string          `json:"next_before"`
	}](t, list)
	if len(page.Items) != 1 {
		t.Fatalf("%s items = %d", path, len(page.Items))
	}
	for key := range page.Items[0] {
		if strings.Contains(key, "secret") && key != "has_secret" {
			t.Fatalf("%s exposed %q", path, key)
		}
	}
}

// assertListCursorIsExclusive proves the before cursor never repeats a row.
func assertListCursorIsExclusive(t *testing.T, f *authFixture, cookie *http.Cookie) {
	t.Helper()
	type page struct {
		Items      []store.Repository `json:"items"`
		NextBefore *string            `json:"next_before"`
	}
	only := decodeResponse[page](t, f.request(t, "GET", "/api/v1/repositories?limit=1", "", cookie, http.StatusOK))
	if len(only.Items) != 1 {
		t.Fatalf("repositories = %d", len(only.Items))
	}
	next := decodeResponse[page](t, f.request(t, "GET", "/api/v1/repositories?before="+only.Items[0].ID, "", cookie, http.StatusOK))
	for _, item := range next.Items {
		if item.ID == only.Items[0].ID {
			t.Fatal("cursor repeated a row")
		}
	}
}

// TestSecretWritesWithoutKEKAreUnavailable covers every secret-bearing write
// without a configured sealer: source POST/PUT with a password and FTP POST/PUT
// with a password return 503 unavailable. Omitting the password on PUT does not
// need a sealer.
func TestSecretWritesWithoutKEKAreUnavailable(t *testing.T) {
	f := newUnconfiguredAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	keyFile := filepath.Join(f.keys, "kek.json")
	if err := artifact.GenerateKey(keyFile); err != nil {
		t.Fatal(err)
	}
	sealer, err := credential.Open(keyFile, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	for name, req := range map[string]struct{ method, path, body string }{
		"create source": {"POST", "/api/v1/sources", sourceBody("source-a")},
		"create ftp":    {"POST", "/api/v1/repositories", ftpRepository("ftp-a", "{{SECRET_ftp_nokek}}")},
	} {
		t.Run(name, func(t *testing.T) {
			response := f.request(t, req.method, req.path, req.body, cookie, http.StatusServiceUnavailable)
			assertSingleError(t, response, render.Unavailable)
		})
	}
	source := f.createSourceRow(t, t.Context(), "existing-source", sealer)
	ftp := f.createFTPRepositoryRow(t, t.Context(), "existing-ftp", "{{SECRET_ftp_existing}}", sealer)
	putSource := `{"name":"existing-source","socket":"/run/mysqld/mysqld.sock","datadir":"/var/lib/mysql","username":"backup","password":"{{SECRET_source_put}}"}`
	putFTP := ftpRepository("existing-ftp", "{{SECRET_ftp_put}}")
	for name, req := range map[string]struct{ method, path, body string }{
		"put source password": {"PUT", "/api/v1/sources/" + source, putSource},
		"put ftp password":    {"PUT", "/api/v1/repositories/" + ftp, putFTP},
	} {
		t.Run(name, func(t *testing.T) {
			response := f.request(t, req.method, req.path, req.body, cookie, http.StatusServiceUnavailable)
			assertSingleError(t, response, render.Unavailable)
		})
	}
	f.request(t, "PUT", "/api/v1/sources/"+source,
		`{"name":"existing-source","socket":"/run/mysqld/other.sock","datadir":"/var/lib/mysql","username":"backup"}`,
		cookie, http.StatusOK)
	f.request(t, "PUT", "/api/v1/repositories/"+ftp,
		`{"name":"existing-ftp","kind":"ftp","tls_mode":"plain","ftp":{"host":"127.0.0.1","port":21,"base_path":"/backups","username":"backup"}}`,
		cookie, http.StatusOK)
}

// TestLocalProbeDoesNotRequireKEK proves a local repository can be created and
// probed in a deployment that has no sealer.
func TestLocalProbeDoesNotRequireKEK(t *testing.T) {
	f := newUnconfiguredAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	root := newRepositoryDir(t)
	created := decodeResponse[store.Repository](t,
		f.request(t, "POST", "/api/v1/repositories", localRepository("local-a", root), cookie, http.StatusCreated))
	response := f.request(t, "POST", "/api/v1/repositories/"+created.ID+"/probe", "", cookie, http.StatusOK)
	if body := decodeResponse[probeResponse](t, response); !body.OK || body.Error != nil {
		t.Fatalf("probe = %+v", body)
	}
}

// TestRepositoryPutRejectsKindAndShapeChanges reads the stored repository first:
// a kind or tls_mode change, a local-shaped body against FTP, and a local→FTP
// replacement are 422 validation_failed and leave the row unchanged.
func TestRepositoryPutRejectsKindAndShapeChanges(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	root := newRepositoryDir(t)
	local := decodeResponse[store.Repository](t,
		f.request(t, "POST", "/api/v1/repositories", localRepository("local-a", root), cookie, http.StatusCreated))
	ftp := decodeResponse[store.Repository](t,
		f.request(t, "POST", "/api/v1/repositories", ftpRepository("ftp-a", "{{SECRET_ftp_shape}}"), cookie, http.StatusCreated))
	localCiphertext := storedCiphertext(t, f.dir, "repositories", local.ID)
	ftpCiphertext := storedCiphertext(t, f.dir, "repositories", ftp.ID)
	rejects := map[string]struct{ id, body string }{
		"local to ftp": {local.ID, ftpRepository("local-a", "{{SECRET_ftp_put_never}}")},
		"ftp root":     {ftp.ID, `{"name":"ftp-a","kind":"ftp","tls_mode":"plain","root":"` + root + `"}`},
		"local tls":    {local.ID, `{"name":"local-a","kind":"local","tls_mode":"plain","root":"` + root + `"}`},
		"ftp kind":     {ftp.ID, localRepository("ftp-a", root)},
	}
	for name, req := range rejects {
		t.Run(name, func(t *testing.T) {
			response := f.requestRejected(t, "PUT", "/api/v1/repositories/"+req.id, req.body, cookie, http.StatusUnprocessableEntity)
			assertSingleError(t, response, render.ValidationFailed)
		})
	}
	assertRepositoryPutUnchanged(t, f, local.ID, ftp.ID, root, localCiphertext, ftpCiphertext)
}

func assertRepositoryPutUnchanged(t *testing.T, f *authFixture, localID, ftpID, root, localCiphertext, ftpCiphertext string) {
	t.Helper()
	unchangedLocal, err := f.database.Repository(t.Context(), localID)
	if err != nil || unchangedLocal.Kind != store.RepositoryLocal || unchangedLocal.Local == nil || unchangedLocal.Local.Root != root {
		t.Fatalf("local after rejected put: %+v %v", unchangedLocal, err)
	}
	unchangedFTP, err := f.database.Repository(t.Context(), ftpID)
	if err != nil || unchangedFTP.Kind != store.RepositoryFTP || unchangedFTP.Local != nil {
		t.Fatalf("ftp after rejected put: %+v %v", unchangedFTP, err)
	}
	if storedCiphertext(t, f.dir, "repositories", localID) != localCiphertext {
		t.Fatal("rejected put wrote a local secret")
	}
	if storedCiphertext(t, f.dir, "repositories", ftpID) != ftpCiphertext {
		t.Fatal("rejected put replaced the ftp secret")
	}
}

// TestPolicyTimezoneRejectsLocalAndEmpty requires a non-empty IANA name.
func TestPolicyTimezoneRejectsLocalAndEmpty(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "operator", "operator")
	cookie := f.login(t, "operator", testPassword)
	source := f.createSourceRow(t, t.Context(), "source-a", f.sealer)
	local := f.createRepositoryRow(t, t.Context(), "local-a", newRepositoryDir(t), f.sealer)
	for name, timezone := range map[string]string{"Local": "Local", "empty": ""} {
		t.Run(name, func(t *testing.T) {
			body := strings.Replace(policyBody("policy-tz", source, local), `"timezone":"UTC"`, `"timezone":"`+timezone+`"`, 1)
			response := f.requestRejected(t, "POST", "/api/v1/policies", body, cookie, http.StatusBadRequest)
			assertSingleError(t, response, render.ValidationFailed)
		})
	}
	var count int
	if err := queryOne(t, f.dir, `SELECT COUNT(*) FROM policies`, &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("rejected timezone persisted %d policies", count)
	}
}

// TestDeleteRefusesReplicaReferencedRepository covers a repository referenced
// only by a replicas row created through the real replication migration.
func TestDeleteRefusesReplicaReferencedRepository(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	repository := f.createRepositoryRow(t, t.Context(), "replica-target", newRepositoryDir(t), f.sealer)
	insertReplica(t, f.dir, repository)
	response := f.request(t, "DELETE", "/api/v1/repositories/"+repository, "", cookie, http.StatusConflict)
	assertSingleError(t, response, render.Conflict)
	if _, err := f.database.Repository(t.Context(), repository); err != nil {
		t.Fatalf("replica-referenced repository was deleted: %v", err)
	}
	expectAuditCount(t, f.dir, "repositories.delete", "success", 0)
}

// insertReplica adds one replicas row that refers to repositoryID. replicas is
// owned by the replication migration; backups.chain_id belongs to the physical
// migration, so this uses the shared intern.db file directly.
func insertReplica(t *testing.T, dir, repositoryID string) {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "intern.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	ctx := t.Context()
	_, err = db.ExecContext(ctx, `INSERT INTO chains
		(id, source_id, server_uuid, server_version, tool_version, started_at)
		VALUES (?, ?, ?, ?, ?, ?)`,
		"replica-chain", "replica-source", "00000000-0000-0000-0000-000000000000", "8.4.11", "8.4.0-7", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO backups
		(id, chain_id, seq, kind, status, instance_id, run_id, object_path, key_id, created_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"replica-backup", "replica-chain", 1, "physical-full", "completed", "instance", "run",
		"instance/source/chain/backup", "key", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
	_, err = db.ExecContext(ctx, `INSERT INTO replicas
		(backup_id, repository_id, idem_key, status, next_attempt_at, created_at, updated_at)
		VALUES (?, ?, ?, ?, ?, ?, ?)`,
		"replica-backup", repositoryID, "replicate:replica-backup:"+repositoryID, "pending",
		"2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z", "2026-01-01T00:00:00Z")
	if err != nil {
		t.Fatal(err)
	}
}
