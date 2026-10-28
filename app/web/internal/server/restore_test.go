package server

import (
	"context"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"testing"

	"github.com/google/uuid"

	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/restore"
)

// restoreControlStub is a deterministic RestoreControl for the route contract
// and password tests. Created executions insert the real restore row with the
// caller's audit entry, so the audit-atomic path is exercised through the store.
type restoreControlStub struct {
	database     *store.Store
	plan         store.RestorePlan
	planErr      error
	outcome      dispatch.RestoreOutcome
	code         string
	executeCalls int
	planCalls    int
	lastErr      error
}

func (s *restoreControlStub) PlanRestore(context.Context, string) (store.RestorePlan, error) {
	s.planCalls++
	if s.planErr != nil {
		return store.RestorePlan{}, s.planErr
	}
	return s.plan, nil
}

func (s *restoreControlStub) ExecuteRestore(ctx context.Context, in dispatch.RestoreExecute) (dispatch.RestoreOutcome, dispatch.RestoreDecision) {
	s.executeCalls++
	if s.outcome == dispatch.RestoreReplayed {
		return s.outcome, dispatch.RestoreDecision{RestoreID: s.storedID(ctx)}
	}
	if s.outcome != dispatch.RestoreCreated {
		return s.outcome, dispatch.RestoreDecision{Code: s.code}
	}
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	row, err := s.database.CreateRestore(ctx, store.RestoreInput{
		ID: id, BackupID: in.BackupID, Plan: s.plan, PlanDigest: in.PlanDigest,
		Rehearse: in.Rehearse, VerifySQL: in.VerifySQL, IdempotencyKey: in.IdempotencyKey,
		RequestHash: in.RequestHash, RequestedBy: in.ActorID, Audit: in.Audit,
	})
	if err != nil {
		s.lastErr = err
		return dispatch.RestoreInternal, dispatch.RestoreDecision{}
	}
	return dispatch.RestoreCreated, dispatch.RestoreDecision{RestoreID: row.ID}
}

// storedID returns the newest stored restore id for the replay case.
func (s *restoreControlStub) storedID(ctx context.Context) string {
	rows, err := s.database.Restores(ctx, store.RestoresFilter{Limit: 1})
	if err != nil || len(rows) == 0 {
		return ""
	}
	return rows[0].ID
}

// RequestRestoreCancel mirrors the dispatcher's cancel semantics through the
// real store: a queued row becomes canceled, a terminal row conflicts.
func (s *restoreControlStub) RequestRestoreCancel(ctx context.Context, id string) error {
	row, err := s.database.Restore(ctx, id)
	if err != nil {
		return err
	}
	if row.Status == store.RestoreStatusQueued {
		changed, err := s.database.CancelQueuedRestore(ctx, id,
			store.SystemAudit("restores.canceled", store.AuditDetail{}),
			&store.RunLogWrite{Level: "warn", Message: "restore canceled"})
		if err != nil {
			return err
		}
		if changed {
			return nil
		}
	}
	return s.database.RequestRestoreCancel(ctx, id, store.SystemAudit("restores.cancel_requested", store.AuditDetail{}))
}

func (s *restoreControlStub) RestoreConfigured() bool {
	return s.outcome != dispatch.RestoreUnavailable
}

// fakeServerRestoreEngine is the dispatch seam for the end-to-end route tests
// that run the real dispatcher.
type fakeServerRestoreEngine struct {
	plan      store.RestorePlan
	executed  int
	lastReq   dispatch.RestoreRequest
	executeFn func(ctx context.Context, req dispatch.RestoreRequest) (dispatch.RestoreResult, error)
}

func (e *fakeServerRestoreEngine) Plan(context.Context, string) (store.RestorePlan, error) {
	return e.plan, nil
}

func (e *fakeServerRestoreEngine) Execute(ctx context.Context, req dispatch.RestoreRequest, _ dispatch.RestoreProgress) (dispatch.RestoreResult, error) {
	e.executed++
	e.lastReq = req
	if e.executeFn != nil {
		return e.executeFn(ctx, req)
	}
	return dispatch.RestoreResult{
		ExportPath: "/export/" + req.RestoreID, ExportBytes: 1,
		SideEffects: inrestorePublished(), VerifyStatus: store.RestoreVerifyNotRequested,
	}, nil
}

func (e *fakeServerRestoreEngine) Recover(context.Context, []string) error { return nil }

func inrestorePublished() store.RestoreSideEffects {
	return store.RestoreSideEffects{WorkDirRemoved: true, ExportPublished: true}
}

// newRestoreAPIFixture serves the production engine shape with the restore API
// attached to a deterministic control.
func newRestoreAPIFixture(t *testing.T, control RestoreControl) *authFixture {
	t.Helper()
	f := newAuthFixture(t)
	settings := resources.Settings{Sealer: f.sealer, KeyFile: f.keyFile, MetadataDir: f.dir}
	engine, err := NewEngineWithResources(f.database, slog.New(slog.NewJSONHandler(&f.logs, nil)), nil, f.service,
		&resources.Service{Store: f.database, Settings: settings}, nil, WithRestores(control))
	if err != nil {
		t.Fatal(err)
	}
	f.engine = engine
	f.client = newContractClient(t, Handler(engine))
	return f
}

// newRealRestoreAPIFixture serves the real dispatcher as the restore control,
// stopped immediately: the routes then run the production plan/execute path
// without a worker claiming the rows.
func newRealRestoreAPIFixture(t *testing.T, engine dispatch.RestoreEngine) (*authFixture, *dispatch.Dispatcher) {
	t.Helper()
	f := newAuthFixture(t)
	started, err := dispatch.Start(t.Context(), dispatch.Config{
		Database: f.database, Settings: resources.Settings{}, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Restore: engine,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = started.Stop(); err != nil {
		t.Fatal(err)
	}
	settings := resources.Settings{Sealer: f.sealer, KeyFile: f.keyFile, MetadataDir: f.dir}
	serverEngine, err := NewEngineWithResources(f.database, slog.New(slog.NewJSONHandler(&f.logs, nil)), nil, f.service,
		&resources.Service{Store: f.database, Settings: settings}, nil, WithRestores(started))
	if err != nil {
		t.Fatal(err)
	}
	f.engine = serverEngine
	f.client = newContractClient(t, Handler(serverEngine))
	return f, started
}

// restoreTestPlan is the wire-valid plan the stub returns.
func restoreTestPlan() store.RestorePlan {
	backupID := uuid.Must(uuid.NewV7()).String()
	return store.RestorePlan{
		BackupID: backupID, ChainID: uuid.Must(uuid.NewV7()).String(), SourceID: uuid.Must(uuid.NewV7()).String(),
		ServerUUID: uuid.Must(uuid.NewV7()).String(), ServerVersion: "8.4.11", ToolVersion: "8.4.0-7",
		TargetToLSN: 500,
		Links: []store.RestorePlanLink{{
			BackupID: backupID, Seq: 1, Kind: store.RunKindFull, FromLSN: 0, ToLSN: 500,
			Source: "local", RepositoryID: uuid.Must(uuid.NewV7()).String(), PayloadSize: 100, MetadataSize: 10,
		}},
		RequiredWorkBytes: 200, RequiredExportBytes: 800, AvailableWorkBytes: 4096, AvailableExportBytes: 4096,
		ExportPath: "/export/planned", Risks: []string{}, Digest: strings.Repeat("a", 64),
		RehearsalAvailable: true,
	}
}

func restoreExecuteBody(backupID, digest, password string, rehearse bool) string {
	body := `{"backup_id":"` + backupID + `","plan_digest":"` + digest + `","password":"` + password + `"`
	if rehearse {
		body += `,"rehearse":true`
	}
	return body + `}`
}

func (f *authFixture) seedQueuedRestore(t *testing.T, userID string) store.Restore {
	t.Helper()
	plan := restoreTestPlan()
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	created, err := f.database.CreateRestore(t.Context(), store.RestoreInput{
		ID: id, BackupID: plan.BackupID, Plan: plan, PlanDigest: plan.Digest, RequestedBy: userID,
		Audit: store.SystemAudit("restores.execute", store.AuditDetail{}),
	})
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// TestRestoreAPIContractRoundTrips validates real request and response bodies
// of every restore route against the specification, including refusals,
// password confirmation, plan changes, rehearsal unavailability and errors.
func TestRestoreAPIContractRoundTrips(t *testing.T) {
	plan := restoreTestPlan()
	stub := &restoreControlStub{plan: plan, outcome: dispatch.RestoreCreated}
	f := newRestoreAPIFixture(t, stub)
	stub.database = f.database
	admin := f.seed(t, "restore-admin", "admin")
	cookie := f.login(t, "restore-admin", testPassword)

	restorePlanRoundTrips(t, f, stub, plan, cookie)
	created := restoreExecuteRoundTrips(t, f, stub, plan, cookie)
	restoreReadRoundTrips(t, f, plan, created, cookie)
	restoreCancelRoundTrips(t, f, admin, cookie)
}

// restorePlanRoundTrips exercises the plan endpoint: the fresh plan body and
// every documented refusal code and status.
func restorePlanRoundTrips(t *testing.T, f *authFixture, stub *restoreControlStub, plan store.RestorePlan, cookie *http.Cookie) {
	t.Helper()
	planPath := "/api/v1/restores/plan"
	body := `{"backup_id":"` + plan.BackupID + `"}`
	response := f.request(t, "POST", planPath, body, cookie, http.StatusOK)
	if !strings.Contains(response.Body.String(), `"plan_digest":"`+plan.Digest+`"`) {
		t.Fatalf("plan body = %s", response.Body)
	}
	for _, refusal := range []struct {
		code   string
		status int
	}{
		{dispatch.RestoreCodeBackupNotFound, http.StatusNotFound},
		{dispatch.RestoreCodeMatrixUnsupported, http.StatusUnprocessableEntity},
		{"chain_broken", http.StatusConflict},
		{"insufficient_space", http.StatusConflict},
		{dispatch.RestoreCodeUnconfigured, http.StatusServiceUnavailable},
	} {
		stub.planErr = &dispatch.RestorePlanError{Code: refusal.code, BackupID: plan.BackupID}
		response = f.request(t, "POST", planPath, body, cookie, refusal.status)
		if refusal.code == dispatch.RestoreCodeUnconfigured {
			continue // the 503 body is the shared unavailable message
		}
		if !strings.Contains(response.Body.String(), `"detail":{"code":"`+refusal.code+`"}`) {
			t.Fatalf("refusal %s body = %s", refusal.code, response.Body)
		}
	}
	stub.planErr = nil
	// The format and lexical rules are handler validation, not spec shapes.
	f.request(t, "POST", planPath, `{"backup_id":"not-an-id"}`, cookie, http.StatusBadRequest)
	f.invalidRequest(t, "POST", planPath, `{"backup_id":`, cookie, http.StatusBadRequest)
}

// restoreExecuteRoundTrips exercises the execute endpoint and returns the
// accepted restore id.
func restoreExecuteRoundTrips(t *testing.T, f *authFixture, stub *restoreControlStub, plan store.RestorePlan, cookie *http.Cookie) string {
	t.Helper()
	executePath := "/api/v1/restores"
	request := apiRequest(http.MethodPost, executePath,
		restoreExecuteBody(plan.BackupID, plan.Digest, testPassword, false), cookie)
	request.Header.Set("Idempotency-Key", "contract-restore-1")
	recorder := requestAPI(t, f.client, request, http.StatusAccepted)
	var accepted struct {
		RestoreID string `json:"restore_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &accepted); err != nil || accepted.RestoreID == "" {
		t.Fatalf("accepted body %s: %v", recorder.Body, err)
	}
	if _, err := f.database.Restore(t.Context(), accepted.RestoreID); err != nil {
		t.Fatalf("accepted restore is not stored: %v", err)
	}
	// Each refusal uses its own administrator: password confirmation is limited
	// to five attempts per user per minute, and this case must measure the
	// refusal mapping rather than the limiter.
	for index, refusal := range []struct {
		name    string
		outcome dispatch.RestoreOutcome
		code    string
		status  int
	}{
		{"refused", dispatch.RestoreRefused, "chain_broken", http.StatusConflict},
		{"matrix", dispatch.RestoreRejected, dispatch.RestoreCodeMatrixUnsupported, http.StatusUnprocessableEntity},
		{"missing", dispatch.RestoreNotFound, dispatch.RestoreCodeBackupNotFound, http.StatusNotFound},
		{"plan_changed", dispatch.RestorePlanChanged, dispatch.RestoreCodePlanChanged, http.StatusConflict},
		{"rehearsal", dispatch.RestoreValidation, "rehearsal_unavailable", http.StatusUnprocessableEntity},
		{"unavailable", dispatch.RestoreUnavailable, dispatch.RestoreCodeUnconfigured, http.StatusServiceUnavailable},
	} {
		stub.outcome, stub.code = refusal.outcome, refusal.code
		name := "restore-refuse-" + strconv.Itoa(index)
		f.seed(t, name, "admin")
		refusalCookie := f.login(t, name, testPassword)
		response := f.request(t, "POST", executePath,
			restoreExecuteBody(plan.BackupID, plan.Digest, testPassword, false), refusalCookie, refusal.status)
		if refusal.outcome == dispatch.RestorePlanChanged &&
			!strings.Contains(response.Body.String(), `"code":"plan_changed"`) {
			t.Fatalf("plan_changed body = %s", response.Body)
		}
	}
	stub.outcome = dispatch.RestoreCreated
	// A short digest is a spec shape violation; the single-statement lexical
	// rule is handler validation, which the spec cannot express.
	f.invalidRequest(t, "POST", executePath, restoreExecuteBody(plan.BackupID, "short", testPassword, false), cookie, http.StatusBadRequest)
	f.request(t, "POST", executePath,
		`{"backup_id":"`+plan.BackupID+`","plan_digest":"`+plan.Digest+`","password":"`+testPassword+
			`","verify_sql":["SELECT 1; DROP TABLE users"]}`,
		cookie, http.StatusBadRequest)
	return accepted.RestoreID
}

// restoreReadRoundTrips exercises the list, detail and bounded log pages.
func restoreReadRoundTrips(t *testing.T, f *authFixture, plan store.RestorePlan, created string, cookie *http.Cookie) {
	t.Helper()
	f.request(t, "GET", "/api/v1/restores/"+created, "", cookie, http.StatusOK)
	f.request(t, "GET", "/api/v1/restores/"+created+"/logs?after_seq=0&limit=10", "", cookie, http.StatusOK)
	f.request(t, "GET", "/api/v1/restores?backup_id="+plan.BackupID+"&status=queued&limit=2", "", cookie, http.StatusOK)
	f.request(t, "GET", "/api/v1/restores/"+strings.Repeat("0", 32), "", cookie, http.StatusNotFound)
	f.request(t, "GET", "/api/v1/restores/"+strings.Repeat("0", 32)+"/logs", "", cookie, http.StatusNotFound)
	f.invalidRequest(t, "GET", "/api/v1/restores?status=finalizing", "", cookie, http.StatusBadRequest)
	f.invalidRequest(t, "GET", "/api/v1/restores/"+created+"/logs?after_seq=-1", "", cookie, http.StatusBadRequest)
}

// restoreCancelRoundTrips proves a queued restore is accepted and canceled, and
// a terminal restore conflicts.
func restoreCancelRoundTrips(t *testing.T, f *authFixture, admin store.User, cookie *http.Cookie) {
	t.Helper()
	queued := f.seedQueuedRestore(t, admin.ID)
	f.request(t, "POST", "/api/v1/restores/"+queued.ID+"/cancel", "", cookie, http.StatusAccepted)
	canceled, err := f.database.Restore(t.Context(), queued.ID)
	if err != nil || canceled.Status != store.RestoreStatusCanceled {
		t.Fatalf("canceled row = %+v, %v", canceled, err)
	}
	f.request(t, "POST", "/api/v1/restores/"+queued.ID+"/cancel", "", cookie, http.StatusConflict)
	f.request(t, "POST", "/api/v1/restores/"+strings.Repeat("0", 32)+"/cancel", "", cookie, http.StatusNotFound)
}

// TestRestoreExecuteRequiresPasswordConfirmation proves a wrong password is 403
// confirmation_failed, creates no row and leaves a denied audit row.
func TestRestoreExecuteRequiresPasswordConfirmation(t *testing.T) {
	plan := restoreTestPlan()
	stub := &restoreControlStub{plan: plan, outcome: dispatch.RestoreCreated}
	f := newRestoreAPIFixture(t, stub)
	admin := f.seed(t, "restore-admin", "admin")
	cookie := f.login(t, "restore-admin", testPassword)
	response := f.request(t, "POST", "/api/v1/restores",
		restoreExecuteBody(plan.BackupID, plan.Digest, changedPassword, false), cookie, http.StatusForbidden)
	if !strings.Contains(response.Body.String(), `"code":"confirmation_failed"`) {
		t.Fatalf("body = %s", response.Body)
	}
	if stub.executeCalls != 0 {
		t.Fatal("the control was called after a failed confirmation")
	}
	rows, err := f.database.Restores(t.Context(), store.RestoresFilter{Limit: 10})
	if err != nil || len(rows) != 0 {
		t.Fatalf("restores after a failed confirmation = %+v, %v", rows, err)
	}
	var denied int
	if err = f.database.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM audit_logs WHERE action = 'restores.execute' AND result = 'denied' AND actor_id = ?`,
		admin.ID).Scan(&denied); err != nil {
		t.Fatal(err)
	}
	if denied != 1 {
		t.Fatalf("denied audit rows = %d", denied)
	}
}

// TestRestoreExecuteIsRateLimited proves the per-user confirmation limiter
// answers 429 with Retry-After after five attempts, before any verification.
func TestRestoreExecuteIsRateLimited(t *testing.T) {
	plan := restoreTestPlan()
	stub := &restoreControlStub{plan: plan, outcome: dispatch.RestoreCreated}
	f := newRestoreAPIFixture(t, stub)
	stub.database = f.database
	f.seed(t, "restore-admin", "admin")
	cookie := f.login(t, "restore-admin", testPassword)
	body := restoreExecuteBody(plan.BackupID, plan.Digest, changedPassword, false)
	for attempt := 1; attempt <= 5; attempt++ {
		f.request(t, "POST", "/api/v1/restores", body, cookie, http.StatusForbidden)
	}
	response := f.request(t, "POST", "/api/v1/restores", body, cookie, http.StatusTooManyRequests)
	if response.Header().Get("Retry-After") == "" {
		t.Fatal("429 without Retry-After")
	}
	if !strings.Contains(response.Body.String(), `"code":"rate_limited"`) {
		t.Fatalf("body = %s", response.Body)
	}
	// The limiter runs before verification: even the correct password waits.
	f.request(t, "POST", "/api/v1/restores",
		restoreExecuteBody(plan.BackupID, plan.Digest, testPassword, false), cookie, http.StatusTooManyRequests)
}

// TestRestoreExecuteAuditIsAtomic proves an unavailable success audit rolls the
// restore row back and answers 500.
func TestRestoreExecuteAuditIsAtomic(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "restore-admin", "admin")
	cookie := f.login(t, "restore-admin", testPassword)
	plan := restoreTestPlan()
	engine := &fakeServerRestoreEngine{plan: plan}
	started, err := dispatch.Start(t.Context(), dispatch.Config{
		Database: f.database, Settings: resources.Settings{}, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
		Restore: engine,
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = started.Stop(); err != nil {
		t.Fatal(err)
	}
	settings := resources.Settings{Sealer: f.sealer, KeyFile: f.keyFile, MetadataDir: f.dir}
	serverEngine, err := NewEngineWithResources(f.database, slog.New(slog.NewJSONHandler(&f.logs, nil)), nil, f.service,
		&resources.Service{Store: f.database, Settings: settings}, nil, WithRestores(started))
	if err != nil {
		t.Fatal(err)
	}
	f.engine = serverEngine
	f.client = newContractClient(t, Handler(serverEngine))

	setSuccessAuditAbort(t, f.dir, "restores.execute")
	response := f.request(t, "POST", "/api/v1/restores",
		restoreExecuteBody(plan.BackupID, plan.Digest, testPassword, false), cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	rows, err := f.database.Restores(t.Context(), store.RestoresFilter{Limit: 10})
	if err != nil || len(rows) != 0 {
		t.Fatalf("restore persisted without its success audit: %+v, %v", rows, err)
	}
	if engine.executed != 0 {
		t.Fatal("the engine ran a restore whose row rolled back")
	}
	setSuccessAuditAbort(t, f.dir, "")
	f.request(t, "POST", "/api/v1/restores",
		restoreExecuteBody(plan.BackupID, plan.Digest, testPassword, false), cookie, http.StatusAccepted)
}

// TestRestoreExecuteVerifiesFreshPlanAndIdempotency runs the real dispatcher:
// a stale digest is 409 plan_changed, a rehearsal without mysqld is 422, and a
// replayed key returns the stored restore id.
func TestRestoreExecuteVerifiesFreshPlanAndIdempotency(t *testing.T) {
	plan := restoreTestPlan()
	plan.RehearsalAvailable = false
	engine := &fakeServerRestoreEngine{plan: plan}
	f, _ := newRealRestoreAPIFixture(t, engine)
	f.seed(t, "restore-admin", "admin")
	cookie := f.login(t, "restore-admin", testPassword)

	stale := restoreExecuteBody(plan.BackupID, strings.Repeat("b", 64), testPassword, false)
	response := f.request(t, "POST", "/api/v1/restores", stale, cookie, http.StatusConflict)
	if !strings.Contains(response.Body.String(), `"code":"plan_changed"`) {
		t.Fatalf("stale body = %s", response.Body)
	}
	rehearse := restoreExecuteBody(plan.BackupID, plan.Digest, testPassword, true)
	response = f.request(t, "POST", "/api/v1/restores", rehearse, cookie, http.StatusUnprocessableEntity)
	if !strings.Contains(response.Body.String(), `"code":"validation_failed"`) {
		t.Fatalf("rehearse body = %s", response.Body)
	}
	body := restoreExecuteBody(plan.BackupID, plan.Digest, testPassword, false)
	first := apiRequest(http.MethodPost, "/api/v1/restores", body, cookie)
	first.Header.Set("Idempotency-Key", "restore-replay")
	firstRecorder := requestAPI(t, f.client, first, http.StatusAccepted)
	var firstAccepted struct {
		RestoreID string `json:"restore_id"`
	}
	if err := json.Unmarshal(firstRecorder.Body.Bytes(), &firstAccepted); err != nil || firstAccepted.RestoreID == "" {
		t.Fatalf("first body %s: %v", firstRecorder.Body, err)
	}
	replay := apiRequest(http.MethodPost, "/api/v1/restores", body, cookie)
	replay.Header.Set("Idempotency-Key", "restore-replay")
	replayRecorder := requestAPI(t, f.client, replay, http.StatusAccepted)
	var replayed struct {
		RestoreID string `json:"restore_id"`
	}
	if err := json.Unmarshal(replayRecorder.Body.Bytes(), &replayed); err != nil || replayed.RestoreID != firstAccepted.RestoreID {
		t.Fatalf("replay body %s: %v", replayRecorder.Body, err)
	}
	// A different request hash with the same key is a refusal, not a replay.
	changedBody := `{"backup_id":"` + plan.BackupID + `","plan_digest":"` + plan.Digest + `","password":"` + testPassword + `","verify_sql":["SELECT 1"]}`
	changed := apiRequest(http.MethodPost, "/api/v1/restores", changedBody, cookie)
	changed.Header.Set("Idempotency-Key", "restore-replay")
	requestAPI(t, f.client, changed, http.StatusConflict)
	if engine.executed != 0 {
		t.Fatal("the stopped dispatcher executed a queued restore")
	}
}

// restoreVerifyPayload is one execute body with an explicit verify_sql list.
type restoreVerifyPayload struct {
	BackupID   string   `json:"backup_id"`
	PlanDigest string   `json:"plan_digest"`
	Password   string   `json:"password"`
	VerifySQL  []string `json:"verify_sql"`
}

func restoreVerifyBody(t *testing.T, payload restoreVerifyPayload) string {
	t.Helper()
	raw, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

// restoreVerifyFixture serves the restore routes with a plan the stub accepts,
// so the verify_sql cases judge the handler and not some other gate.
func restoreVerifyFixture(t *testing.T) (*authFixture, store.RestorePlan, *http.Cookie) {
	t.Helper()
	plan := restoreTestPlan()
	stub := &restoreControlStub{plan: plan, outcome: dispatch.RestoreCreated}
	f := newRestoreAPIFixture(t, stub)
	stub.database = f.database
	f.seed(t, "restore-admin", "admin")
	return f, plan, f.login(t, "restore-admin", testPassword)
}

// TestRestoreVerifySQLRefusesWhatTheEngineRefuses proves the API admits exactly
// what the restore engine admits: every statement list the engine would refuse
// is refused here, before any row or audit is written, so an accepted execute
// can never degrade into a restore that fails at claim time.
func TestRestoreVerifySQLRefusesWhatTheEngineRefuses(t *testing.T) {
	f, plan, cookie := restoreVerifyFixture(t)
	path := "/api/v1/restores"
	overBytes := "SELECT " + strings.Repeat("a", restore.MaxVerifySQLBytes-len("SELECT ")+1)
	tooMany := make([]string, 0, restore.MaxVerifySQLStatements+1)
	for range restore.MaxVerifySQLStatements + 1 {
		tooMany = append(tooMany, "SELECT 1")
	}
	// Bounds the document expresses are refused by both the document and the
	// handler, so a client cannot be told one limit and answered another.
	for _, statements := range [][]string{{""}, {overBytes}, tooMany} {
		f.invalidRequest(t, "POST", path, restoreVerifyBody(t, restoreVerifyPayload{
			BackupID: plan.BackupID, PlanDigest: plan.Digest, Password: testPassword, VerifySQL: statements,
		}), cookie, http.StatusBadRequest)
	}
	// Rules the document cannot express are handler validation with the engine's
	// own judgment: no separator after SELECT, a second statement, a statement
	// that is not a SELECT, and a NUL byte.
	for _, statements := range [][]string{{"selectx 1"}, {"SELECT 1; SELECT 2"}, {"DROP TABLE users"}, {"\x00"}} {
		response := f.request(t, "POST", path, restoreVerifyBody(t, restoreVerifyPayload{
			BackupID: plan.BackupID, PlanDigest: plan.Digest, Password: testPassword, VerifySQL: statements,
		}), cookie, http.StatusBadRequest)
		if decodeResponse[render.ErrorResponse](t, response).Error.Code != render.ValidationFailed {
			t.Fatalf("verify_sql %q body = %s", statements[0], response.Body)
		}
	}
	rows, err := f.database.Restores(t.Context(), store.RestoresFilter{Limit: 10})
	if err != nil || len(rows) != 0 {
		t.Fatalf("a refused verify_sql created rows: %+v, %v", rows, err)
	}
}

// TestRestoreVerifySQLAdmitsTheEngineBoundary proves the engine's boundary is
// admitted exactly: a statement of its maximum size, the maximum number of
// times, is queued rather than refused.
func TestRestoreVerifySQLAdmitsTheEngineBoundary(t *testing.T) {
	f, plan, cookie := restoreVerifyFixture(t)
	exact := "SELECT " + strings.Repeat("a", restore.MaxVerifySQLBytes-len("SELECT "))
	if len(exact) != restore.MaxVerifySQLBytes {
		t.Fatalf("boundary statement is %d bytes, want %d", len(exact), restore.MaxVerifySQLBytes)
	}
	maximum := make([]string, 0, restore.MaxVerifySQLStatements)
	for range restore.MaxVerifySQLStatements {
		maximum = append(maximum, exact)
	}
	f.request(t, "POST", "/api/v1/restores", restoreVerifyBody(t, restoreVerifyPayload{
		BackupID: plan.BackupID, PlanDigest: plan.Digest, Password: testPassword, VerifySQL: maximum,
	}), cookie, http.StatusAccepted)
}

// TestRestoreOpenAPICarriesTheEngineVerifyLimits keeps the specification and the
// engine from drifting: the documented bounds are the engine's constants.
func TestRestoreOpenAPICarriesTheEngineVerifyLimits(t *testing.T) {
	schema := loadContract(t).Components.Schemas["RestoreExecuteRequest"].Value.Properties["verify_sql"].Value
	if schema.MaxItems == nil || *schema.MaxItems != uint64(restore.MaxVerifySQLStatements) {
		t.Fatalf("openapi maxItems = %v, the engine allows %d", schema.MaxItems, restore.MaxVerifySQLStatements)
	}
	statement := schema.Items.Value
	if statement.MaxLength == nil || *statement.MaxLength != uint64(restore.MaxVerifySQLBytes) {
		t.Fatalf("openapi maxLength = %v, the engine allows %d bytes", statement.MaxLength, restore.MaxVerifySQLBytes)
	}
	if statement.MinLength != 1 {
		t.Fatalf("openapi minLength = %v, the engine requires at least 1 byte", statement.MinLength)
	}
}

// TestRestoreConfirmationSharesOnePasswordLimiterWithAuth proves the two routes
// that verify the current password spend one per-user budget: five failed
// restore confirmations leave PUT /auth/password with nothing, so a stolen
// session gets five guesses in total rather than five per route.
func TestRestoreConfirmationSharesOnePasswordLimiterWithAuth(t *testing.T) {
	plan := restoreTestPlan()
	stub := &restoreControlStub{plan: plan, outcome: dispatch.RestoreCreated}
	f := newRestoreAPIFixture(t, stub)
	f.seed(t, "restore-admin", "admin")
	cookie := f.login(t, "restore-admin", testPassword)
	wrong := restoreExecuteBody(plan.BackupID, plan.Digest, changedPassword, false)
	for range 5 {
		f.request(t, "POST", "/api/v1/restores", wrong, cookie, http.StatusForbidden)
	}
	response := f.request(t, "PUT", "/api/v1/auth/password",
		`{"current_password":"`+testPassword+`","new_password":"`+changedPassword+`"}`,
		cookie, http.StatusTooManyRequests)
	assertRateLimitResponse(t, response.Header().Get("Retry-After"), response.Body.Bytes())
	// The restore route is the same bucket: the correct password must now wait.
	f.request(t, "POST", "/api/v1/restores",
		restoreExecuteBody(plan.BackupID, plan.Digest, testPassword, false), cookie, http.StatusTooManyRequests)
}
