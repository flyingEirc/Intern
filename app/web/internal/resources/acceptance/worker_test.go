//go:build integration

package acceptance

import (
	"bytes"
	"database/sql"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// Marker text asserted by the CI evidence gate. Each marker states the observed
// persisted state, not the intent.
const (
	markerPass       = "W3-A3 pass"
	markerRefused    = "W3-A3 refused"
	markerAtomic     = "W3-A4 atomic"
	markerCiphertext = "W3-A2 ciphertext"

	warningCandidateUV = "compatibility_candidate_unverified"
)

// TestW3Worker is the single worker entry point. It runs inside the pinned PXB
// container as the mysql uid, serves the real handler over real TLS, and drives
// the real mounted MySQL source and the real Pure-FTPd fixture. A missing case or
// a skip is a failure.
func TestW3Worker(t *testing.T) {
	f := newFixture(t)
	steps := []struct {
		name string
		run  func(*testing.T)
	}{
		{"a3-pass", f.pass},
		{"a4-atomic-enable", f.atomicEnable},
		{"a3-denied-grants", f.deniedGrants},
		{"a3-wrong-volume", f.wrongVolume},
		{"a3-matrix-miss", f.matrixMiss},
		{"a3-unwritable-ftp", f.unwritableFTP},
		{"a2-ciphertext", f.ciphertext},
	}
	for _, step := range steps {
		t.Run(step.name, func(t *testing.T) { step.run(t) })
	}
}

type repositoryWrite struct {
	Name     string    `json:"name"`
	Kind     string    `json:"kind"`
	TLSMode  string    `json:"tls_mode"`
	Root     string    `json:"root,omitempty"`
	FTP      *ftpWrite `json:"ftp,omitempty"`
	Password *string   `json:"password,omitempty"`
}

type ftpWrite struct {
	Host     string `json:"host"`
	Port     int64  `json:"port"`
	BasePath string `json:"base_path"`
	Username string `json:"username"`
	// CAPem is the public FTPS trust bundle, omitted when empty so a W3 body stays
	// byte-identical: the S2 worker is the only caller that fills it.
	CAPem string `json:"ca_pem,omitempty"`
}

type sourceWrite struct {
	Name     string  `json:"name"`
	Socket   string  `json:"socket"`
	Datadir  string  `json:"datadir"`
	Username string  `json:"username"`
	Password *string `json:"password,omitempty"`
}

type policyWrite struct {
	Name                string  `json:"name"`
	SourceID            string  `json:"source_id"`
	LocalRepositoryID   string  `json:"local_repository_id"`
	ReplicaRepositoryID *string `json:"replica_repository_id,omitempty"`
	FullCron            string  `json:"full_cron"`
	IncrementalCron     string  `json:"incremental_cron"`
	Timezone            string  `json:"timezone"`
	MaxChainLength      int64   `json:"max_chain_length"`
	Parallel            int64   `json:"parallel"`
	Throttle            int64   `json:"throttle"`
	ZstdLevel           int64   `json:"zstd_level"`
	PageTracking        bool    `json:"page_tracking"`
	Enabled             bool    `json:"enabled"`
}

type preflightReply struct {
	OK       bool          `json:"ok"`
	Checks   []string      `json:"checks"`
	Warnings []warningBody `json:"warnings"`
	Error    *failureBody  `json:"error"`
}

type warningBody struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

type failureBody struct {
	Check   string   `json:"check"`
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Missing []string `json:"missing"`
}

type errorEnvelope struct {
	Error struct {
		Code      string          `json:"code"`
		Message   string          `json:"message"`
		RequestID string          `json:"request_id"`
		Preflight *preflightReply `json:"preflight"`
	} `json:"error"`
}

type probeReply struct {
	OK    bool `json:"ok"`
	Error *struct {
		Code      string `json:"code"`
		ReplyCode int    `json:"reply_code"`
	} `json:"error"`
}

// requireBase refuses a later scenario whose fixture a failing earlier scenario
// never finished building.
func (f *fixture) requireBase(t *testing.T) {
	t.Helper()
	if f.local.ID == "" || f.replica.ID == "" || f.source.ID == "" {
		t.Fatal("the pass scenario did not establish the shared local repository, FTP replica and source")
	}
}

// pass runs the whole successful path against the real components.
func (f *fixture) pass(t *testing.T) {
	f.createRepositories(t)
	f.source = f.createSource(t, sourceWrite{
		Name: "w3-source", Socket: socketPath, Datadir: dataDir, Username: backupUser, Password: &f.secret.source,
	})
	persisted := f.passPreflight(t)
	f.passPolicy(t, persisted)
}

// createRepositories creates one local repository and one explicit-plain FTP
// repository, probes both, and requires the FTP create to be audited as
// plaintext. The local probe establishes the real 0700 root the source preflight
// then measures as its backup volume, so both steps use one directory.
func (f *fixture) createRepositories(t *testing.T) {
	f.local = f.createLocal(t, "w3-local", filepath.Join(f.layout.repository, "local"))
	if f.local.HasSecret || f.local.PlaintextCredentials || f.local.Kind != store.RepositoryLocal || f.local.TLSMode != store.TLSModeNone {
		t.Fatalf("local repository = %+v", f.local)
	}
	requireProbeOK(t, f, f.local.ID, "local")
	f.replica = f.createFTP(t, "w3-replica", "normal", f.secret.ftp)
	if !f.replica.HasSecret || !f.replica.PlaintextCredentials || f.replica.TLSMode != store.TLSModePlain {
		t.Fatalf("FTP repository = %+v", f.replica)
	}
	requireProbeOK(t, f, f.replica.ID, "FTP")
	if count := f.audit(t, "repositories.create", "success"); count != 2 {
		t.Fatalf("repositories.create success audits = %d", count)
	}
	if details := f.auditDetails(t, "repositories.create", f.replica.ID); !strings.Contains(details, `"plaintext_ftp":true`) {
		t.Fatalf("FTP create audit detail = %q", details)
	}
}

func requireProbeOK(t *testing.T, f *fixture, id, kind string) {
	t.Helper()
	probe := decode[probeReply](t, f.admin.request(t, http.MethodPost, "/api/v1/repositories/"+id+"/probe", "", http.StatusOK).body)
	if !probe.OK {
		t.Fatalf("%s probe = %+v", kind, probe)
	}
}

// passPreflight runs the real source preflight and requires the candidate
// warning plus the persisted server identity.
func (f *fixture) passPreflight(t *testing.T) store.Source {
	t.Helper()
	preflight := f.sourcePreflight(t, f.operator, f.source.ID, f.local.ID)
	if !preflight.OK {
		t.Fatalf("source preflight = %+v", preflight)
	}
	if !warns(preflight.Warnings, warningCandidateUV) {
		t.Fatalf("source preflight warnings = %+v", preflight.Warnings)
	}
	persisted := f.readSource(t, f.source.ID)
	if persisted.ServerUUID == nil || persisted.ServerVersion == nil || persisted.LastPreflight == nil || !persisted.LastPreflight.OK {
		t.Fatalf("persisted source = %+v", persisted)
	}
	return persisted
}

// passPolicy creates a policy, enables it against the real components, and
// requires the persisted enable, the stored preflight and the same-transaction
// success audit.
func (f *fixture) passPolicy(t *testing.T, persisted store.Source) {
	t.Helper()
	policy := f.createPolicy(t, policyWrite{
		Name: "w3-policy", SourceID: f.source.ID, LocalRepositoryID: f.local.ID,
		ReplicaRepositoryID: &f.replica.ID, FullCron: "0 2 * * *", IncrementalCron: "0 * * * *",
		Timezone: "UTC", MaxChainLength: 3, Parallel: 2, Throttle: 0, ZstdLevel: 3,
	})
	if policy.Enabled || policy.LastPreflight != nil {
		t.Fatalf("new policy = %+v", policy)
	}
	enabled := f.enable(t, f.operator, policy.ID, "w3-pass-enable")
	if !enabled.Enabled || enabled.LastPreflight == nil || !enabled.LastPreflight.OK {
		t.Fatalf("enabled policy = %+v", enabled)
	}
	f.requireEnabledRow(t, policy.ID, 1)
	if stored := f.storedPreflight(t, "policies", policy.ID); stored == "" {
		t.Fatal("the enabled policy stored no preflight record")
	}
	t.Logf("%s source=%s local=%s ftp=%s policy=%s enabled=1 persisted=1 last_preflight=stored success_audit=same_tx server_uuid=%s server_version=%s warning=%s plaintext_ftp=audited ftp_probe=ok",
		markerPass, f.source.ID, f.local.ID, f.replica.ID, policy.ID,
		*persisted.ServerUUID, *persisted.ServerVersion, warningCandidateUV)
}

// atomicEnable proves the enable decision, its preflight record and the success
// audit share one transaction: an injected audit abort leaves enabled=0.
func (f *fixture) atomicEnable(t *testing.T) {
	f.requireBase(t)
	policy := f.createPolicy(t, policyWrite{
		Name: "w3-atomic", SourceID: f.source.ID, LocalRepositoryID: f.local.ID,
		ReplicaRepositoryID: &f.replica.ID, FullCron: "0 3 * * *", IncrementalCron: "15 * * * *",
		Timezone: "UTC", MaxChainLength: 2, Parallel: 1, Throttle: 0, ZstdLevel: 1,
	})
	f.setSuccessAuditAbort(t, "policies.enable")
	aborted := f.operator.requestWithID(t, http.MethodPost, "/api/v1/policies/"+policy.ID+"/enable", "", "w3-atomic-abort", 0)
	f.setSuccessAuditAbort(t, "")
	if aborted.status != http.StatusInternalServerError {
		t.Fatalf("aborted enable = %d: %s", aborted.status, aborted.body)
	}
	f.requireEnabledRow(t, policy.ID, 0)
	if stored := f.storedPreflight(t, "policies", policy.ID); stored != "" {
		t.Fatalf("aborted enable stored a preflight: %q", stored)
	}
	if f.readPolicy(t, policy.ID).Enabled {
		t.Fatal("aborted enable left the policy enabled")
	}
	enabled := f.enable(t, f.operator, policy.ID, "w3-atomic-clean")
	if !enabled.Enabled {
		t.Fatalf("policy after a clean enable = %+v", enabled)
	}
	f.requireEnabledRow(t, policy.ID, 1)
	t.Logf("%s policy=%s audit_abort=500 enabled_after_abort=0 preflight_after_abort=absent clean_enable=200 enabled=1",
		markerAtomic, policy.ID)
}

// deniedGrants refuses an account without the backup privileges.
func (f *fixture) deniedGrants(t *testing.T) {
	f.requireBase(t)
	source := f.createSource(t, sourceWrite{
		Name: "w3-denied", Socket: socketPath, Datadir: dataDir, Username: deniedUser, Password: &f.secret.denied,
	})
	preflight := f.sourcePreflight(t, f.operator, source.ID, f.local.ID)
	requireFailure(t, preflight, "privileges", "privileges_missing")
	if len(preflight.Error.Missing) == 0 {
		t.Fatalf("privileges_missing names no privilege: %+v", preflight.Error)
	}
	f.refuseEnable(t, f.refusalPolicy("w3-denied-policy", source.ID, nil), "privileges_missing", "w3-refuse-denied")
	t.Logf("%s case=privileges_missing source=%s check=%s code=%s missing=%d enabled=0 failure_audit=present",
		markerRefused, source.ID, preflight.Error.Check, preflight.Error.Code, len(preflight.Error.Missing))
}

// wrongVolume points the source at a second real volume, not its datadir.
func (f *fixture) wrongVolume(t *testing.T) {
	f.requireBase(t)
	source := f.createSource(t, sourceWrite{
		Name: "w3-wrong", Socket: socketPath, Datadir: wrongDataDir, Username: backupUser, Password: &f.secret.source,
	})
	preflight := f.sourcePreflight(t, f.operator, source.ID, f.local.ID)
	requireFailure(t, preflight, "volume", "datadir_mismatch")
	f.refuseEnable(t, f.refusalPolicy("w3-wrong-policy", source.ID, nil), "datadir_mismatch", "w3-refuse-wrong")
	t.Logf("%s case=wrong_volume source=%s check=%s code=%s mounted=%s enabled=0 failure_audit=present",
		markerRefused, source.ID, preflight.Error.Check, preflight.Error.Code, wrongDataDir)
}

// matrixMiss points a source at a real MySQL 9.7.1 server whose version is
// outside the pinned compatibility matrix. Identity succeeds against that
// server and the pinned XtraBackup; admission fails in the compatibility check.
func (f *fixture) matrixMiss(t *testing.T) {
	f.requireBase(t)
	source := f.createSource(t, sourceWrite{
		Name: "w3-matrix", Socket: matrixSocket, Datadir: matrixDataDir, Username: backupUser, Password: &f.secret.source,
	})
	preflight := f.sourcePreflight(t, f.operator, source.ID, f.local.ID)
	requireFailure(t, preflight, "compatibility", "version_unsupported")
	persisted := f.readSource(t, source.ID)
	if persisted.ServerVersion == nil || *persisted.ServerVersion != matrixServerVersion {
		t.Fatalf("matrix-miss server version = %v, want %s", persisted.ServerVersion, matrixServerVersion)
	}
	if persisted.LastPreflight == nil || persisted.LastPreflight.OK {
		t.Fatalf("matrix-miss last preflight = %+v", persisted.LastPreflight)
	}
	f.refuseEnable(t, f.refusalPolicy("w3-matrix-policy", source.ID, &f.replica.ID), "version_unsupported", "w3-refuse-matrix")
	t.Logf("%s case=matrix_miss check=%s code=%s server_version=%s source=%s enabled=0 failure_audit=present",
		markerRefused, preflight.Error.Check, preflight.Error.Code, *persisted.ServerVersion, source.ID)
}

// unwritableFTP refuses a replica whose server-side directory rejects the
// instance write with a real 550.
func (f *fixture) unwritableFTP(t *testing.T) {
	f.requireBase(t)
	f.locked = f.createFTP(t, "w3-locked", "locked", f.secret.ftpLocked)
	probe := decode[probeReply](t, f.admin.request(t, http.MethodPost, "/api/v1/repositories/"+f.locked.ID+"/probe", "", http.StatusOK).body)
	if probe.OK || probe.Error == nil || probe.Error.ReplyCode != 550 {
		t.Fatalf("locked FTP probe = %+v", probe)
	}
	policy := f.createPolicy(t, f.refusalPolicy("w3-locked-policy", f.source.ID, &f.locked.ID))
	f.refuse(t, f.operator, policy.ID, probe.Error.Code, "w3-refuse-ftp", "replica")
	t.Logf("%s case=unwritable_ftp repository=%s reply_code=%d code=%s check=replica enabled=0 failure_audit=present",
		markerRefused, f.locked.ID, probe.Error.ReplyCode, probe.Error.Code)
}

// ciphertext proves the stored rows hold ciphertext and that no generated secret
// reached a response, the worker log, the audit table or the database bytes.
func (f *fixture) ciphertext(t *testing.T) {
	f.requireBase(t)
	if f.locked.ID == "" {
		t.Fatal("the unwritable FTP scenario did not create the locked repository")
	}
	for _, row := range []struct{ table, id, secret string }{
		{"sources", f.source.ID, f.secret.source},
		{"repositories", f.replica.ID, f.secret.ftp},
		{"repositories", f.locked.ID, f.secret.ftpLocked},
		{"sources", f.byName(t, "sources", "w3-denied"), f.secret.denied},
	} {
		f.requireSealed(t, row.table, row.id, row.secret)
	}
	for name, text := range map[string]string{
		"responses": f.received.String(),
		"log":       f.logs.String(),
		"audit":     f.auditText(t),
		"database":  f.databaseBytes(t),
	} {
		assertNoSecret(t, []byte(text), f.secret.all)
		if name == "database" && strings.Contains(text, "has_secret") {
			t.Fatal("the database bytes contain a wire response")
		}
	}
	t.Logf("%s rows=4 unseal=exact cross_row=refused cross_field=refused responses=clean log=clean audit=clean database_bytes=clean",
		markerCiphertext)
}

// requireSealed checks one stored secret: it is ciphertext, it opens only under
// its own row and field, and it is not the plaintext.
func (f *fixture) requireSealed(t *testing.T, table, id, secret string) {
	t.Helper()
	if id == "" {
		t.Fatalf("%s has no row to inspect", table)
	}
	ciphertext := f.storedCiphertext(t, table, id)
	if len(ciphertext) == 0 {
		t.Fatalf("%s %s holds no ciphertext", table, id)
	}
	if bytes.Contains(ciphertext, []byte(secret)) {
		t.Fatalf("%s %s ciphertext contains the plaintext", table, id)
	}
	opened, err := f.sealer.Unseal(table, id, "secret_ciphertext", ciphertext)
	if err != nil || string(opened) != secret {
		t.Fatalf("%s %s sealed secret does not open to the generated value: %v", table, id, err)
	}
	if _, err = f.sealer.Unseal(table, "00000000-0000-0000-0000-000000000000", "secret_ciphertext", ciphertext); err == nil {
		t.Fatalf("%s %s ciphertext opened under another row identity", table, id)
	}
	if _, err = f.sealer.Unseal(table, id, "secret_plaintext", ciphertext); err == nil {
		t.Fatalf("%s %s ciphertext opened under another field", table, id)
	}
}

// createLocal creates one local repository as the administrator.
func (f *fixture) createLocal(t *testing.T, name, root string) store.Repository {
	t.Helper()
	body := repositoryWrite{Name: name, Kind: store.RepositoryLocal, TLSMode: store.TLSModeNone, Root: root}
	return decode[store.Repository](t, f.admin.request(t, http.MethodPost, "/api/v1/repositories", marshal(t, body), http.StatusCreated).body)
}

// createFTP creates one explicit-plain FTP repository as the administrator.
func (f *fixture) createFTP(t *testing.T, name, username, password string) store.Repository {
	t.Helper()
	body := repositoryWrite{
		Name: name, Kind: store.RepositoryFTP, TLSMode: store.TLSModePlain,
		FTP:      &ftpWrite{Host: ftpHost(), Port: ftpPort, BasePath: "/", Username: username},
		Password: &password,
	}
	return decode[store.Repository](t, f.admin.request(t, http.MethodPost, "/api/v1/repositories", marshal(t, body), http.StatusCreated).body)
}

func (f *fixture) createSource(t *testing.T, body sourceWrite) store.Source {
	t.Helper()
	return decode[store.Source](t, f.operator.request(t, http.MethodPost, "/api/v1/sources", marshal(t, body), http.StatusCreated).body)
}

func (f *fixture) createPolicy(t *testing.T, body policyWrite) store.Policy {
	t.Helper()
	return decode[store.Policy](t, f.operator.request(t, http.MethodPost, "/api/v1/policies", marshal(t, body), http.StatusCreated).body)
}

// refusalPolicy is a disabled policy over one source, with the shared local
// repository and an optional replica.
func (f *fixture) refusalPolicy(name, sourceID string, replica *string) policyWrite {
	return policyWrite{
		Name: name, SourceID: sourceID, LocalRepositoryID: f.local.ID, ReplicaRepositoryID: replica,
		FullCron: "0 1 * * *", IncrementalCron: "45 * * * *", Timezone: "UTC",
		MaxChainLength: 2, Parallel: 1, Throttle: 0, ZstdLevel: 2,
	}
}

func (f *fixture) sourcePreflight(t *testing.T, c *client, sourceID, repositoryID string) preflightReply {
	t.Helper()
	body := marshal(t, map[string]string{"local_repository_id": repositoryID})
	return decode[preflightReply](t, c.request(t, http.MethodPost, "/api/v1/sources/"+sourceID+"/preflight", body, http.StatusOK).body)
}

// enable enables one policy and proves the success audit of that exact request
// exists with the policy as its resource. That row is committed inside the
// business transaction, so its presence is the observable half of atomicity.
func (f *fixture) enable(t *testing.T, c *client, policyID, requestID string) store.Policy {
	t.Helper()
	reply := c.requestWithID(t, http.MethodPost, "/api/v1/policies/"+policyID+"/enable", "", requestID, http.StatusOK)
	policy := decode[store.Policy](t, reply.body)
	entry := f.enableAudit(t, requestID)
	if entry.status != http.StatusOK || entry.result != "success" || entry.resourceID != policyID {
		t.Fatalf("enable audit = %+v for policy %s", entry, policyID)
	}
	return policy
}

// refuse enables one policy that the preflight must refuse, then reads the
// refusal back through the API, SQLite and the audit table.
func (f *fixture) refuse(t *testing.T, c *client, policyID, wantCode, requestID, wantCheck string) {
	t.Helper()
	refused := c.requestWithID(t, http.MethodPost, "/api/v1/policies/"+policyID+"/enable", "", requestID, http.StatusConflict)
	envelope := decode[errorEnvelope](t, refused.body)
	if envelope.Error.Code != "preflight_failed" {
		t.Fatalf("refusal error code = %q: %s", envelope.Error.Code, refused.body)
	}
	if envelope.Error.Preflight == nil || envelope.Error.Preflight.OK {
		t.Fatalf("refusal carried no failed preflight: %s", refused.body)
	}
	failure := envelope.Error.Preflight.Error
	if failure == nil || failure.Code != wantCode || failure.Check != wantCheck {
		t.Fatalf("refusal preflight = %s, want %s on %s", refused.body, wantCode, wantCheck)
	}
	f.requireRefusalState(t, policyID)
	f.requireFailureAudit(t, requestID)
	assertNoSecret(t, refused.body, f.secret.all)
}

// refuseEnable is refuse with the shared operator client.
func (f *fixture) refuseEnable(t *testing.T, body policyWrite, wantCode, requestID string) {
	t.Helper()
	policy := f.createPolicy(t, body)
	f.refuse(t, f.operator, policy.ID, wantCode, requestID, failureCheck(wantCode))
}

// failureCheck is the check name the frozen preflight reports for a code.
func failureCheck(code string) string {
	switch code {
	case "privileges_missing":
		return "privileges"
	case "datadir_mismatch":
		return "volume"
	case "version_unsupported":
		return "compatibility"
	default:
		return "replica"
	}
}

// requireRefusalState reads the refusal back: enabled stays 0, no preflight
// record is stored, and the API agrees with SQLite.
func (f *fixture) requireRefusalState(t *testing.T, policyID string) {
	t.Helper()
	f.requireEnabledRow(t, policyID, 0)
	if f.readPolicy(t, policyID).Enabled {
		t.Fatalf("policy %s is enabled after a refused enable", policyID)
	}
	if stored := f.storedPreflight(t, "policies", policyID); stored != "" {
		t.Fatalf("policy %s stored a preflight after a refusal: %q", policyID, stored)
	}
}

func (f *fixture) readPolicy(t *testing.T, id string) store.Policy {
	t.Helper()
	return decode[store.Policy](t, f.admin.request(t, http.MethodGet, "/api/v1/policies/"+id, "", http.StatusOK).body)
}

func (f *fixture) readSource(t *testing.T, id string) store.Source {
	t.Helper()
	return decode[store.Source](t, f.admin.request(t, http.MethodGet, "/api/v1/sources/"+id, "", http.StatusOK).body)
}

func requireFailure(t *testing.T, preflight preflightReply, check, code string) {
	t.Helper()
	if preflight.OK || preflight.Error == nil || preflight.Error.Check != check || preflight.Error.Code != code {
		t.Fatalf("preflight = %+v, want %s/%s", preflight, check, code)
	}
}

func warns(warnings []warningBody, code string) bool {
	for _, warning := range warnings {
		if warning.Code == code {
			return true
		}
	}
	return false
}

// assertNoSecret refuses output that contains any generated credential.
func assertNoSecret(t *testing.T, text []byte, generated []string) {
	t.Helper()
	for _, secret := range generated {
		if secret != "" && bytes.Contains(text, []byte(secret)) {
			t.Fatalf("a generated credential appeared in %d bytes of observed output", len(text))
		}
	}
}

// auditEntryRow is one observed audit row.
type auditEntryRow struct {
	status     int
	result     string
	resourceID string
}

// enableAudit requires the audit row of one enable request. The success row is
// committed inside the business transaction, so the short poll only tolerates
// scheduling, not a missing write.
func (f *fixture) enableAudit(t *testing.T, requestID string) auditEntryRow {
	t.Helper()
	var row auditEntryRow
	var resource sql.NullString
	query := `SELECT COALESCE(status, 0), result, resource_id FROM audit_logs
WHERE action = 'policies.enable' AND request_id = ? ORDER BY id DESC LIMIT 1`
	deadline := time.Now().Add(20 * time.Second)
	for {
		err := f.analysis.QueryRowContext(t.Context(), query, requestID).Scan(&row.status, &row.result, &resource)
		if err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("no enable audit for request %s: %v", requestID, err)
		}
		time.Sleep(50 * time.Millisecond)
	}
	row.resourceID = resource.String
	return row
}

// requireFailureAudit requires one failed enable audit for this exact request.
// The failure audit is written after the response, so the poll is the observable
// ordering, not a substitute for the write.
func (f *fixture) requireFailureAudit(t *testing.T, requestID string) {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		var count int
		query := `SELECT COUNT(*) FROM audit_logs WHERE action = 'policies.enable' AND request_id = ? AND result = 'failed'`
		if err := f.analysis.QueryRowContext(t.Context(), query, requestID).Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 1 {
			return
		}
		if count > 1 {
			t.Fatalf("failed enable audits for %s = %d", requestID, count)
		}
		if time.Now().After(deadline) {
			t.Fatalf("no failed enable audit for request %s", requestID)
		}
		time.Sleep(50 * time.Millisecond)
	}
}

func (f *fixture) audit(t *testing.T, action, result string) int {
	t.Helper()
	var count int
	query := `SELECT COUNT(*) FROM audit_logs WHERE action = ? AND result = ?`
	if err := f.analysis.QueryRowContext(t.Context(), query, action, result).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

func (f *fixture) auditDetails(t *testing.T, action, resourceID string) string {
	t.Helper()
	var detail sql.NullString
	query := `SELECT detail_json FROM audit_logs WHERE action = ? AND resource_id = ? ORDER BY id DESC LIMIT 1`
	if err := f.analysis.QueryRowContext(t.Context(), query, action, resourceID).Scan(&detail); err != nil {
		t.Fatal(err)
	}
	return detail.String
}

func (f *fixture) auditText(t *testing.T) string {
	t.Helper()
	var text string
	query := `SELECT COALESCE(group_concat(action || resource_type || resource_id || detail_json, ''), '') FROM audit_logs`
	if err := f.analysis.QueryRowContext(t.Context(), query).Scan(&text); err != nil {
		t.Fatal(err)
	}
	return text
}

// requireEnabledRow reads the persisted enable flag straight from SQLite.
func (f *fixture) requireEnabledRow(t *testing.T, id string, want int) {
	t.Helper()
	var enabled int
	if err := f.analysis.QueryRowContext(t.Context(), `SELECT enabled FROM policies WHERE id = ?`, id).Scan(&enabled); err != nil {
		t.Fatal(err)
	}
	if enabled != want {
		t.Fatalf("policies.enabled = %d, want %d", enabled, want)
	}
}

func (f *fixture) storedPreflight(t *testing.T, table, id string) string {
	t.Helper()
	var stored sql.NullString
	query := `SELECT last_preflight_json FROM ` + table + ` WHERE id = ?`
	if err := f.analysis.QueryRowContext(t.Context(), query, id).Scan(&stored); err != nil {
		t.Fatal(err)
	}
	return stored.String
}

func (f *fixture) storedCiphertext(t *testing.T, table, id string) []byte {
	t.Helper()
	var ciphertext []byte
	query := `SELECT secret_ciphertext FROM ` + table + ` WHERE id = ?`
	if err := f.analysis.QueryRowContext(t.Context(), query, id).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	return ciphertext
}

// byName resolves one resource id by its unique name.
func (f *fixture) byName(t *testing.T, table, name string) string {
	t.Helper()
	var id string
	if err := f.analysis.QueryRowContext(t.Context(), `SELECT id FROM `+table+` WHERE name = ?`, name).Scan(&id); err != nil {
		t.Fatal(err)
	}
	return id
}

// databaseBytes reads the database and its WAL, which is where a leaked
// plaintext would persist.
func (f *fixture) databaseBytes(t *testing.T) string {
	t.Helper()
	var combined bytes.Buffer
	for _, name := range []string{"intern.db", "intern.db-wal"} {
		data, err := os.ReadFile(filepath.Join(f.layout.metadata, name))
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		must(t, err)
		combined.Write(data)
	}
	if combined.Len() == 0 {
		t.Fatal("the control-plane database is empty")
	}
	return combined.String()
}

// setSuccessAuditAbort installs or removes a trigger that aborts one success
// audit, which is the only way to observe the transaction boundary from outside.
func (f *fixture) setSuccessAuditAbort(t *testing.T, action string) {
	t.Helper()
	if _, err := f.analysis.ExecContext(t.Context(), `DROP TRIGGER IF EXISTS abort_success_audit`); err != nil {
		t.Fatal(err)
	}
	if action == "" {
		return
	}
	query := `CREATE TRIGGER abort_success_audit BEFORE INSERT ON audit_logs
WHEN NEW.result = 'success' AND NEW.action = '` + action + `'
BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`
	if _, err := f.analysis.ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
}
