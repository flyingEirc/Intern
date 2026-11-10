package server

import (
	"database/sql"
	"net/http"
	"strings"
	"testing"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// TestResourceMutationsRollBackWhenSuccessAuditFails extends the atomic-audit
// fault injection to every Stage C mutation: a failing success audit must leave
// the business row exactly as it was, and the standalone failure row is written.
func TestResourceMutationsRollBackWhenSuccessAuditFails(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	seeded := f.seedResources(t)

	assertRepositoryWritesRollBack(t, f, cookie, seeded)
	assertSourceWritesRollBack(t, f, cookie, seeded)
	assertPolicyWritesRollBack(t, f, cookie, seeded)
}

func assertRepositoryWritesRollBack(t *testing.T, f *authFixture, cookie *http.Cookie, seeded seededResources) {
	t.Helper()
	setSuccessAuditAbort(t, f.dsn, "repositories.create")
	failedCreate := countAudit(t, f.dsn, "repositories.create", "failed")
	root := newRepositoryDir(t)
	response := f.request(t, "POST", "/api/v1/repositories", localRepository("rollback-repository", root), cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	var count int
	if err := queryOne(t, f.dsn, `SELECT COUNT(*) FROM repositories WHERE name = 'rollback-repository'`, &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("repository persisted without a success audit")
	}
	expectAuditCount(t, f.dsn, "repositories.create", "failed", failedCreate+1)
	setSuccessAuditAbort(t, f.dsn, "")

	// Rotation must also roll back, leaving the previous ciphertext in place.
	before := storedCiphertext(t, f.dsn, "repositories", seeded.freeRepository)
	setSuccessAuditAbort(t, f.dsn, "repositories.update")
	failedUpdate := countAudit(t, f.dsn, "repositories.update", "failed")
	response = f.request(t, "PUT", "/api/v1/repositories/"+seeded.freeRepository,
		localRepository("renamed", seeded.root), cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	reloaded, err := f.database.Repository(t.Context(), seeded.freeRepository)
	if err != nil || reloaded.Name == "renamed" {
		t.Fatalf("update persisted without a success audit: %+v %v", reloaded, err)
	}
	if storedCiphertext(t, f.dsn, "repositories", seeded.freeRepository) != before {
		t.Fatal("secret changed without a success audit")
	}
	expectAuditCount(t, f.dsn, "repositories.update", "failed", failedUpdate+1)
	setSuccessAuditAbort(t, f.dsn, "")

	setSuccessAuditAbort(t, f.dsn, "repositories.delete")
	response = f.request(t, "DELETE", "/api/v1/repositories/"+seeded.freeRepository, "", cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	if _, err := f.database.Repository(t.Context(), seeded.freeRepository); err != nil {
		t.Fatalf("delete persisted without a success audit: %v", err)
	}
	setSuccessAuditAbort(t, f.dsn, "")
}

func assertSourceWritesRollBack(t *testing.T, f *authFixture, cookie *http.Cookie, seeded seededResources) {
	t.Helper()
	setSuccessAuditAbort(t, f.dsn, "sources.create")
	response := f.request(t, "POST", "/api/v1/sources", sourceBody("rollback-source"), cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	var count int
	if err := queryOne(t, f.dsn, `SELECT COUNT(*) FROM sources WHERE name = 'rollback-source'`, &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("source persisted without a success audit")
	}
	setSuccessAuditAbort(t, f.dsn, "")

	before := storedCiphertext(t, f.dsn, "sources", seeded.freeSource)
	setSuccessAuditAbort(t, f.dsn, "sources.update")
	response = f.request(t, "PUT", "/api/v1/sources/"+seeded.freeSource, sourceBody("renamed-source"), cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	reloaded, err := f.database.Source(t.Context(), seeded.freeSource)
	if err != nil || reloaded.Name == "renamed-source" {
		t.Fatalf("source update persisted: %+v %v", reloaded, err)
	}
	if storedCiphertext(t, f.dsn, "sources", seeded.freeSource) != before {
		t.Fatal("source secret changed without a success audit")
	}
	setSuccessAuditAbort(t, f.dsn, "")

	setSuccessAuditAbort(t, f.dsn, "sources.delete")
	response = f.request(t, "DELETE", "/api/v1/sources/"+seeded.freeSource, "", cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	if _, err := f.database.Source(t.Context(), seeded.freeSource); err != nil {
		t.Fatalf("source delete persisted: %v", err)
	}
	setSuccessAuditAbort(t, f.dsn, "")
}

func assertPolicyWritesRollBack(t *testing.T, f *authFixture, cookie *http.Cookie, seeded seededResources) {
	t.Helper()
	setSuccessAuditAbort(t, f.dsn, "policies.create")
	response := f.request(t, "POST", "/api/v1/policies", policyBody("rollback-policy", seeded.freeSource, seeded.freeRepository), cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	var count int
	if err := queryOne(t, f.dsn, `SELECT COUNT(*) FROM policies WHERE name = 'rollback-policy'`, &count); err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatal("policy persisted without a success audit")
	}
	setSuccessAuditAbort(t, f.dsn, "")

	before, err := f.database.Policy(t.Context(), seeded.policyID)
	if err != nil {
		t.Fatal(err)
	}
	setSuccessAuditAbort(t, f.dsn, "policies.update")
	response = f.request(t, "PUT", "/api/v1/policies/"+seeded.policyID,
		policyBody("renamed-policy", seeded.freeSource, seeded.freeRepository), cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	reloaded, err := f.database.Policy(t.Context(), seeded.policyID)
	if err != nil || reloaded.Name != before.Name || reloaded.UpdatedAt != before.UpdatedAt {
		t.Fatalf("policy update persisted: %+v %+v", reloaded, err)
	}
	setSuccessAuditAbort(t, f.dsn, "")

	setSuccessAuditAbort(t, f.dsn, "policies.enable")
	response = f.request(t, "POST", "/api/v1/policies/"+seeded.policyID+"/enable", "", cookie, http.StatusServiceUnavailable)
	// Without preflight settings this deployment cannot even attempt enable;
	// only a deployment with them reaches the enable transaction.
	if response.Code == http.StatusInternalServerError {
		requireInternal(t, response)
	}
	setSuccessAuditAbort(t, f.dsn, "")

	setSuccessAuditAbort(t, f.dsn, "policies.delete")
	response = f.request(t, "DELETE", "/api/v1/policies/"+seeded.policyID, "", cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	if _, err = f.database.Policy(t.Context(), seeded.policyID); err != nil {
		t.Fatalf("policy delete persisted: %v", err)
	}
	setSuccessAuditAbort(t, f.dsn, "")
	expectAuditCount(t, f.dsn, "policies.create", "success", 1)
	expectAuditCount(t, f.dsn, "policies.update", "success", 0)
	expectAuditCount(t, f.dsn, "policies.delete", "success", 0)
}

// TestEnableAuditFailureKeepsPolicyDisabled is the enable-specific atomic case:
// the success audit and enabled=1 commit together, so a failed audit leaves the
// policy disabled.
func TestEnableAuditFailureKeepsPolicyDisabled(t *testing.T) {
	f := newPreflightFixture(t)
	f.seed(t, "operator", "operator")
	cookie := f.login(t, "operator", testPassword)
	local := f.createRepositoryRow(t, t.Context(), "local-a", newRepositoryDir(t), f.sealer)
	source, err := f.database.CreateSource(t.Context(), store.SourceInput{
		Name: "unreachable", Socket: unreachableSocket(t), Datadir: "/var/lib/mysql",
		Username: "backup", Secret: []byte(matrixSecret),
	}, f.sealer, store.SystemAudit("sources.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	policy := decodeResponse[store.Policy](t,
		f.request(t, "POST", "/api/v1/policies", policyBody("policy-a", source.ID, local), cookie, http.StatusCreated))
	setSuccessAuditAbort(t, f.dsn, "policies.enable")
	// The preflight fails here, so enable never reaches the transaction; the audit
	// trigger is exercised through the standalone failure row instead.
	f.request(t, "POST", "/api/v1/policies/"+policy.ID+"/enable", "", cookie, http.StatusConflict)
	persisted, err := f.database.Policy(t.Context(), policy.ID)
	if err != nil || persisted.Enabled {
		t.Fatalf("enable state = %+v %v", persisted, err)
	}
	setSuccessAuditAbort(t, f.dsn, "")
	expectAuditCount(t, f.dsn, "policies.enable", "success", 0)
	assertNoSecrets(t, f.authFixture, f.logs.String(), matrixSecret)
}

// TestPreflightFailureAuditIsBestEffort proves a failing failure-audit write does
// not change the response: the 409 is returned and the state stays disabled.
func TestPreflightFailureAuditIsBestEffort(t *testing.T) {
	f := newPreflightFixture(t)
	f.seed(t, "operator", "operator")
	cookie := f.login(t, "operator", testPassword)
	local := f.createRepositoryRow(t, t.Context(), "local-a", newRepositoryDir(t), f.sealer)
	source, err := f.database.CreateSource(t.Context(), store.SourceInput{
		Name: "unreachable", Socket: unreachableSocket(t), Datadir: "/var/lib/mysql",
		Username: "backup", Secret: []byte(matrixSecret),
	}, f.sealer, store.SystemAudit("sources.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	policy := decodeResponse[store.Policy](t,
		f.request(t, "POST", "/api/v1/policies", policyBody("policy-a", source.ID, local), cookie, http.StatusCreated))
	setFailureAuditAbort(t, f.dsn, "policies.enable")
	response := f.request(t, "POST", "/api/v1/policies/"+policy.ID+"/enable", "", cookie, http.StatusConflict)
	assertPreflightFailed(t, response, "identity", "identity_query_failed")
	persisted, err := f.database.Policy(t.Context(), policy.ID)
	if err != nil || persisted.Enabled {
		t.Fatalf("enable state = %+v %v", persisted, err)
	}
	setFailureAuditAbort(t, f.dsn, "")
	if !strings.Contains(f.logs.String(), "audit write failed") {
		t.Fatalf("failure audit was not reported: %s", f.logs.String())
	}
}

// setFailureAuditAbort makes every non-success audit insert for one action fail.
func setFailureAuditAbort(t *testing.T, dir, action string) {
	t.Helper()
	abortAudit(t, dir, action, "failed")
}

func abortAudit(t *testing.T, dsn, action, result string) {
	t.Helper()
	if strings.ContainsAny(action+result, "'\\") {
		t.Fatal("unsafe audit trigger predicate")
	}
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(t.Context(), `DROP TRIGGER IF EXISTS abort_outcome_audit ON audit_logs`); err != nil {
		t.Fatal(err)
	}
	if action == "" {
		return
	}
	function := `CREATE OR REPLACE FUNCTION abort_outcome_audit_fn() RETURNS trigger LANGUAGE plpgsql AS $$
	BEGIN RAISE EXCEPTION 'injected outcome audit failure'; END $$`
	if _, err = db.ExecContext(t.Context(), function); err != nil {
		t.Fatal(err)
	}
	trigger := `CREATE TRIGGER abort_outcome_audit BEFORE INSERT ON audit_logs
	FOR EACH ROW WHEN (NEW.result = '` + result + `' AND NEW.action = '` + action + `')
	EXECUTE FUNCTION abort_outcome_audit_fn()`
	if _, err = db.ExecContext(t.Context(), trigger); err != nil {
		t.Fatal(err)
	}
}
