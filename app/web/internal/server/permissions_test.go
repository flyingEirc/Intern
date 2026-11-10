package server

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/google/uuid"

	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

type permissionCase struct {
	body      string
	admin     int
	operator  int
	viewer    int
	anonymous int
}

// Placeholders let one body serve every role while still naming the rows this
// fixture created, so a matrix pass means the route really ran.
const (
	matrixSecret = "{{SECRET_matrix_password}}"

	bodyLocalRepository = `{"name":"local","kind":"local","tls_mode":"none","root":"{{root}}"}`
	bodySource          = `{"name":"source","socket":"/run/mysqld/mysqld.sock","datadir":"/var/lib/mysql","username":"backup","password":"` + matrixSecret + `"}`
	bodyPolicy          = `{"name":"policy","source_id":"{{free_source}}","local_repository_id":"{{free_repository}}",` +
		`"full_cron":"0 2 * * *","incremental_cron":"0 * * * *","timezone":"UTC","enabled":false}`
	bodyRunTrigger = `{"kind":"full"}`
	// W5 restore bodies. The runtime is unconfigured, so an allowed plan or
	// execute answers 503 before any engine work; the fixed digest is a valid
	// 64-hex value so validation passes and the permission decision is what
	// the status measures.
	bodyRestorePlan = `{"backup_id":"00000000-0000-7000-8000-000000000000"}`
	// The password is the administrator's real login password: the fixture
	// runtime is unconfigured, so the allowed execute answers 503 only after
	// the confirmation succeeds.
	bodyRestoreExecute = `{"backup_id":"00000000-0000-7000-8000-000000000000","plan_digest":"` + restoreDigest + `","password":"` + testPassword + `","rehearse":false}`
	restoreDigest      = "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
)

// Every registered route must appear explicitly. Adding a route without deciding
// all four access outcomes fails this test, as does documenting a nonexistent one.
func TestEveryRoutePermissionMatrix(t *testing.T) {
	cases := map[string]permissionCase{
		"GET /healthz":                      {"", 200, 200, 200, 200},
		"GET /readyz":                       {"", 200, 200, 200, 200},
		"GET /api/v1/auth/setup":            {"", 200, 200, 200, 200},
		"POST /api/v1/auth/setup":           {`{"username":"initial","password":"` + testPassword + `"}`, 409, 409, 409, 409},
		"POST /api/v1/auth/login":           {`{"username":"loginuser","password":"` + testPassword + `"}`, 200, 200, 200, 200},
		"POST /api/v1/auth/logout":          {"", 204, 204, 204, 401},
		"GET /api/v1/auth/session":          {"", 200, 200, 200, 401},
		"PUT /api/v1/auth/password":         {`{"current_password":"` + testPassword + `","new_password":"` + changedPassword + `"}`, 204, 204, 204, 401},
		"GET /api/v1/users":                 {"", 200, 403, 403, 401},
		"POST /api/v1/users":                {`{"username":"created","password":"` + testPassword + `","role":"viewer"}`, 201, 403, 403, 401},
		"GET /api/v1/users/:id":             {"", 200, 403, 403, 401},
		"PATCH /api/v1/users/:id":           {`{"role":"operator"}`, 200, 403, 403, 401},
		"DELETE /api/v1/users/:id/sessions": {"", 204, 403, 403, 401},
		"GET /api/v1/audit-logs":            {"", 200, 403, 403, 401},

		// resources:read is granted to all three roles; writes follow the frozen
		// Stage C matrix: only admin writes repositories, and both admin and
		// operator write sources and policies.
		"GET /api/v1/repositories":            {"", 200, 200, 200, 401},
		"POST /api/v1/repositories":           {bodyLocalRepository, 201, 403, 403, 401},
		"GET /api/v1/repositories/:id":        {"", 200, 200, 200, 401},
		"PUT /api/v1/repositories/:id":        {bodyLocalRepository, 200, 403, 403, 401},
		"DELETE /api/v1/repositories/:id":     {"", 204, 403, 403, 401},
		"POST /api/v1/repositories/:id/probe": {"", 200, 403, 403, 401},

		"GET /api/v1/sources":                {"", 200, 200, 200, 401},
		"POST /api/v1/sources":               {bodySource, 201, 201, 403, 401},
		"GET /api/v1/sources/:id":            {"", 200, 200, 200, 401},
		"PUT /api/v1/sources/:id":            {bodySource, 200, 200, 403, 401},
		"DELETE /api/v1/sources/:id":         {"", 204, 204, 403, 401},
		"POST /api/v1/sources/:id/preflight": {`{"local_repository_id":"{{free_repository}}"}`, 503, 503, 403, 401},

		"GET /api/v1/policies":              {"", 200, 200, 200, 401},
		"POST /api/v1/policies":             {bodyPolicy, 201, 201, 403, 401},
		"GET /api/v1/policies/:id":          {"", 200, 200, 200, 401},
		"PUT /api/v1/policies/:id":          {bodyPolicy, 200, 200, 403, 401},
		"DELETE /api/v1/policies/:id":       {"", 204, 204, 403, 401},
		"POST /api/v1/policies/:id/enable":  {"", 503, 503, 403, 401},
		"POST /api/v1/policies/:id/disable": {"", 200, 200, 403, 401},
		// The preview is resources:read, so every role sees it, and it answers
		// even though this fixture's runtime is unconfigured and the policy is
		// disabled: the fires describe what enabling would schedule.
		"GET /api/v1/policies/:id/schedule": {"", 200, 200, 200, 401},

		// W4: viewers read every run, chain and replica view; only admin and
		// operator trigger, cancel and retry. The fixture runtime is
		// unconfigured, so a manual trigger answers 503 for the roles allowed
		// to call it.
		"POST /api/v1/policies/:id/runs":                         {bodyRunTrigger, 503, 503, 403, 401},
		"GET /api/v1/runs":                                       {"", 200, 200, 200, 401},
		"GET /api/v1/runs/:id":                                   {"", 200, 200, 200, 401},
		"GET /api/v1/runs/:id/logs":                              {"", 200, 200, 200, 401},
		"POST /api/v1/runs/:id/cancel":                           {"", 202, 202, 403, 401},
		"GET /api/v1/runs/:id/events":                            {"", 200, 200, 200, 401},
		"GET /api/v1/sources/:id/chains":                         {"", 200, 200, 200, 401},
		"GET /api/v1/backups/:id":                                {"", 404, 404, 404, 401},
		"GET /api/v1/sources/:id/protection":                     {"", 200, 200, 200, 401},
		"GET /api/v1/replicas":                                   {"", 200, 200, 200, 401},
		"POST /api/v1/backups/:id/replicas/:repository_id/retry": {"", 404, 404, 403, 401},

		// W5: viewers read restores, operators plan, and only admin executes or
		// cancels. The fixture runtime is unconfigured, so plan and execute
		// answer 503 for the roles allowed to call them.
		"POST /api/v1/restores/plan":       {bodyRestorePlan, 503, 503, 403, 401},
		"GET /api/v1/restores":             {"", 200, 200, 200, 401},
		"POST /api/v1/restores":            {bodyRestoreExecute, 503, 403, 403, 401},
		"GET /api/v1/restores/:id":         {"", 200, 200, 200, 401},
		"GET /api/v1/restores/:id/logs":    {"", 200, 200, 200, 401},
		"POST /api/v1/restores/:id/cancel": {"", 202, 403, 403, 401},
	}
	// Routes that require the Idempotency-Key header get one here, so the
	// request body is validated rather than rejected for a missing parameter.
	keys := map[string]string{
		"POST /api/v1/policies/:id/runs": "matrix-trigger",
	}
	f := newAuthFixtureWithRuns(t)
	hash, err := f.service.Passwords.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	f.seedHashed(t, "loginuser", "viewer", hash)
	target := f.seedHashed(t, "target", "viewer", hash)
	cookies := map[string]*http.Cookie{}
	for _, role := range []string{"admin", "operator", "viewer"} {
		f.seedHashed(t, role, role, hash)
		cookies[role] = f.login(t, role, testPassword)
	}
	if len(f.engine.Routes()) != len(cases) {
		t.Fatal("permission matrix and route counts differ")
	}
	for _, route := range f.engine.Routes() {
		key := route.Method + " " + route.Path
		row, exists := cases[key]
		if !exists {
			t.Fatalf("missing permission decisions for %s", key)
		}
		if middleware.IsMutation(route.Method) && strings.HasPrefix(route.Path, "/api/v1/") {
			if _, mapped := middleware.AuditAction(route.Method, route.Path); !mapped {
				t.Fatalf("missing audit action for %s", key)
			}
		}
		headerKey := keys[key]
		for _, role := range []string{"admin", "operator", "viewer", "anonymous"} {
			t.Run(key+"/"+role, func(t *testing.T) {
				exercisePermission(t, f, cookies, hash, target, route.Method, route.Path, role, row, headerKey)
			})
		}
	}
}

// TestPermissionMatrixFixtureNeverRunsSeededRows pins what makes the matrix
// deterministic: nothing in its fixture claims or executes a row it seeded. A
// live dispatcher claims a queued run or restore within its one-second worker
// poll and fails it as unconfigured, so an allowed cancel answers 409 instead of
// 202; executing a run also opens the policy's repository, which re-creates a
// root that a finished case's TempDir cleanup is removing. Waiting out two polls
// separates the stopped dispatcher from a live one.
func TestPermissionMatrixFixtureNeverRunsSeededRows(t *testing.T) {
	f := newAuthFixtureWithRuns(t)
	owner := f.seed(t, "owner", "admin")
	seeded := f.seedResources(t)
	runID := f.createRunRow(t, seeded.policyID, seeded.freeSource)
	restoreID := f.createRestoreRow(t, owner.ID)
	repository, err := f.database.Repository(t.Context(), seeded.policyRepository)
	if err != nil {
		t.Fatal(err)
	}
	// An absent root is what a finished case leaves behind: opening the
	// repository for a run would create it again.
	if err = os.RemoveAll(repository.Local.Root); err != nil {
		t.Fatal(err)
	}

	time.Sleep(2 * time.Second)

	run, err := f.database.Run(t.Context(), runID)
	if err != nil {
		t.Fatal(err)
	}
	restore, err := f.database.Restore(t.Context(), restoreID)
	if err != nil {
		t.Fatal(err)
	}
	if run.Status != store.RunStatusQueued {
		t.Errorf("seeded run is %q after two worker polls; want it left queued", run.Status)
	}
	if restore.Status != store.RestoreStatusQueued {
		t.Errorf("seeded restore is %q after two worker polls; want it left queued", restore.Status)
	}
	if _, err = os.Lstat(repository.Local.Root); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("policy repository root was created again after the case; lstat error = %v", err)
	}
}

// exercisePermission runs one route as one role against the shared fixture.
// Store open, password hashing and the dispatcher's start and stop run once;
// each case reseeds the rows it mutates so a 2xx still means the operation ran.
func exercisePermission(t *testing.T, f *authFixture, cookies map[string]*http.Cookie, hash string, target store.User, method, path, role string, row permissionCase, headerKey string) {
	t.Helper()
	f.resetResources(t)
	f.restoreTarget(t, target, hash)
	seeded := f.seedResources(t)
	f.seedRouteRows(t, &seeded, target, path)
	status := map[string]int{"admin": row.admin, "operator": row.operator, "viewer": row.viewer, "anonymous": row.anonymous}[role]
	req := f.matrixRequest(t, seeded, target, cookies, method, path, role, row.body, headerKey)
	assertMatrixStatus(t, f, seeded, req, method, status)
	assertCancelEffect(t, f, seeded, path, status)
	switch path {
	case "/api/v1/auth/login", "/api/v1/auth/logout", "/api/v1/auth/password", "/api/v1/runs/:id/events":
		f.restoreActor(t, cookies, role, hash)
	}
}

func (f *authFixture) matrixRequest(t *testing.T, seeded seededResources, target store.User, cookies map[string]*http.Cookie, method, path, role, body, headerKey string) *http.Request {
	t.Helper()
	var cookie *http.Cookie
	if role != "anonymous" {
		cookie = cookies[role]
	}
	if path == "/api/v1/policies/:id/runs" {
		// Enable directly: this fixture cannot run the preflight the API path
		// requires, and an enabled policy is what reaches the runtime check.
		if _, err := f.database.DB().ExecContext(t.Context(), `UPDATE policies SET enabled = 1 WHERE id = $1`, seeded.policyID); err != nil {
			t.Fatal(err)
		}
	}
	resolved := strings.ReplaceAll(path, ":id", seeded.targetID(path, target.ID))
	resolved = strings.ReplaceAll(resolved, ":repository_id", seeded.policyRepository)
	req := apiRequest(method, resolved, seeded.expand(body), cookie)
	if headerKey != "" {
		req.Header.Set("Idempotency-Key", headerKey)
	}
	return req
}

func assertMatrixStatus(t *testing.T, f *authFixture, seeded seededResources, req *http.Request, method string, status int) {
	t.Helper()
	if method == http.MethodGet && strings.HasSuffix(req.URL.Path, "/events") {
		assertMatrixStream(t, f, req, status)
		return
	}
	recorder := requestAPI(t, f.client, req, status)
	if req.URL.Path == "/api/v1/runs" && status == http.StatusOK {
		assertSeededRunListed(t, recorder.Body.Bytes(), seeded.runID)
	}
}

// assertMatrixStream reads one event stream to its end. The seeded run is
// already terminal, so an allowed role receives the replay and the final end
// frame instead of a stream that waits for a worker.
func assertMatrixStream(t *testing.T, f *authFixture, req *http.Request, status int) {
	t.Helper()
	recorder, err := f.client.roundTripStream(req)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.Code != status {
		t.Fatalf("status = %d, body = %s; want %d", recorder.Code, recorder.Body, status)
	}
	if status == http.StatusOK {
		assertStreamEnd(t, recorder.Body.String(), store.RunStatusCanceled)
	}
}

// assertSeededRunListed requires the runs list to name the run this case
// seeded: 200 on an empty or unrelated list would let a handler that returns
// nothing at all pass this matrix.
func assertSeededRunListed(t *testing.T, body []byte, runID string) {
	t.Helper()
	var page struct {
		Items []store.Run `json:"items"`
	}
	if err := json.Unmarshal(body, &page); err != nil {
		t.Fatalf("runs list body %s: %v", body, err)
	}
	for _, item := range page.Items {
		if item.ID == runID {
			return
		}
	}
	t.Fatalf("runs list %s does not name the seeded run %s", body, runID)
}

// assertCancelEffect proves what a cancel answer means: a 202 acted on a
// non-terminal row and left it canceled, and every refusal left it queued.
func assertCancelEffect(t *testing.T, f *authFixture, seeded seededResources, path string, status int) {
	t.Helper()
	var got, queued, canceled string
	switch path {
	case "/api/v1/runs/:id/cancel":
		run, err := f.database.Run(t.Context(), seeded.runID)
		if err != nil {
			t.Fatal(err)
		}
		got, queued, canceled = run.Status, store.RunStatusQueued, store.RunStatusCanceled
	case "/api/v1/restores/:id/cancel":
		restore, err := f.database.Restore(t.Context(), seeded.restoreID)
		if err != nil {
			t.Fatal(err)
		}
		got, queued, canceled = restore.Status, store.RestoreStatusQueued, store.RestoreStatusCanceled
	default:
		return
	}
	want := queued
	if status == http.StatusAccepted {
		want = canceled
	}
	if got != want {
		t.Fatalf("after the %d answer the row is %q; want %q", status, got, want)
	}
}

// newAuthFixtureWithRuns serves the production engine shape: the runs routes
// exist only when a dispatcher is attached, so the matrix needs one. The
// dispatcher is stopped as soon as it has started. The matrix judges permission
// decisions on rows it seeds itself, so nothing may claim them: a live worker
// polls every second, fails a queued row as unconfigured (an allowed cancel then
// answers 409 instead of 202) and opens the policy's repository after the case
// has finished, writing into a TempDir that is being removed. A stopped
// dispatcher still serves every control call over the database, the state the
// run, SSE and restore API fixtures use. The fixture runtime stays unconfigured,
// which is why an allowed manual trigger answers 503 rather than creating a run.
func newAuthFixtureWithRuns(t *testing.T) *authFixture {
	t.Helper()
	f := newAuthFixture(t)
	settings := resources.Settings{Sealer: f.sealer, KeyFile: f.keyFile, MetadataDir: f.dir}
	dispatcher, err := dispatch.Start(t.Context(), dispatch.Config{
		Database: f.database, Settings: settings, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	if err = dispatcher.Stop(); err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngineWithResources(f.database, slog.New(slog.NewJSONHandler(&f.logs, nil)), nil, f.service,
		&resources.Service{Store: f.database, Settings: settings}, dispatcher, WithRestores(dispatcher))
	if err != nil {
		t.Fatal(err)
	}
	f.engine, f.client = engine, newContractClient(t, Handler(engine))
	return f
}

func (f *authFixture) seedHashed(t *testing.T, username, role, hash string) store.User {
	t.Helper()
	user, err := f.database.CreateUser(t.Context(), username, hash, role, store.SystemAudit("users.create", store.AuditDetail{Role: role}))
	if err != nil {
		t.Fatal(err)
	}
	return user
}

// resetResources deletes every resource row the previous case left, children
// before parents, in one write transaction.
func (f *authFixture) resetResources(t *testing.T) {
	t.Helper()
	ctx := t.Context()
	tx, err := f.database.DB().BeginTx(ctx, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	for _, stmt := range []string{
		`DELETE FROM restore_logs`,
		`DELETE FROM restores`,
		`DELETE FROM run_logs`,
		`DELETE FROM runs`,
		`DELETE FROM policies`,
		`DELETE FROM sources`,
		`DELETE FROM repositories`,
	} {
		if _, err = tx.ExecContext(ctx, stmt); err != nil {
			t.Fatal(err)
		}
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
}

func (f *authFixture) restoreTarget(t *testing.T, target store.User, hash string) {
	t.Helper()
	role := "viewer"
	if _, err := f.database.UpdateUser(t.Context(), target.ID, store.UserChanges{Role: &role, PasswordHash: &hash},
		store.SystemAudit("users.update", store.AuditDetail{Role: role})); err != nil {
		t.Fatal(err)
	}
}

func (f *authFixture) restoreActor(t *testing.T, cookies map[string]*http.Cookie, role, hash string) {
	t.Helper()
	if role == "anonymous" {
		return
	}
	user, err := f.database.UserByUsername(t.Context(), role)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.database.UpdateUser(t.Context(), user.ID, store.UserChanges{PasswordHash: &hash},
		store.SystemAudit("users.update", store.AuditDetail{})); err != nil {
		t.Fatal(err)
	}
	cookies[role] = f.login(t, role, testPassword)
}

// seededResources holds the rows a matrix case may reference. The policy points
// at its own source and repository so the "free" pair stays deletable.
type seededResources struct {
	root             string
	freeRepository   string
	freeSource       string
	policyID         string
	policySource     string
	policyRepository string
	runID            string
	restoreID        string
}

func (f *authFixture) seedResources(t *testing.T) seededResources {
	t.Helper()
	ctx := t.Context()
	seeded := seededResources{root: filepath.Join(t.TempDir(), "repository")}
	if err := os.MkdirAll(seeded.root, 0o700); err != nil {
		t.Fatal(err)
	}
	sealed := f.sealer
	seeded.freeRepository = f.createRepositoryRow(t, ctx, "matrix-free-repository", seeded.root, sealed)
	seeded.freeSource = f.createSourceRow(t, ctx, "matrix-free-source", sealed)
	policyRepository := f.createRepositoryRow(t, ctx, "matrix-policy-repository", filepath.Join(t.TempDir(), "policy"), sealed)
	policySource := f.createSourceRow(t, ctx, "matrix-policy-source", sealed)
	policy, err := f.database.CreatePolicy(ctx, store.PolicyInput{
		Name: "matrix-policy", SourceID: policySource, LocalRepositoryID: policyRepository,
		FullCron: "0 2 * * *", IncrementalCron: "0 * * * *", Timezone: "UTC",
	}, store.SystemAudit("policies.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	seeded.policyID = policy.ID
	seeded.policySource = policySource
	seeded.policyRepository = policyRepository
	return seeded
}

// seedRouteRows creates the run or restore a route's :id names. The dispatcher
// is stopped, so a seeded row keeps the state it was created in until the
// request under test acts on it. Every runs route needs a row: the list must
// show it, and detail, logs, cancel and events need a real id. A queued run or
// restore is what a 202 cancel acts on; the events stream tails its run until
// the run is terminal, so that route gets one that has already ended.
func (f *authFixture) seedRouteRows(t *testing.T, seeded *seededResources, target store.User, path string) {
	t.Helper()
	switch {
	case path == "/api/v1/runs/:id/events":
		seeded.runID = f.createTerminalRunRow(t, seeded.policyID, seeded.freeSource)
	case strings.HasPrefix(path, "/api/v1/runs"):
		seeded.runID = f.createRunRow(t, seeded.policyID, seeded.freeSource)
	case strings.HasPrefix(path, "/api/v1/restores/") && strings.Contains(path, ":id"):
		seeded.restoreID = f.createRestoreRow(t, target.ID)
	}
}

// createRunRow inserts one queued run for the matrix's run-route cases.
func (f *authFixture) createRunRow(t *testing.T, policyID, sourceID string) string {
	t.Helper()
	run, err := f.database.CreateRun(t.Context(), store.RunInput{
		PolicyID: policyID, SourceID: sourceID, Kind: store.RunKindFull, Trigger: store.RunTriggerManual,
		IdempotencyKey: "matrix:" + policyID, RequestHash: "matrix", ScheduledAt: time.Now(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return run.ID
}

// createTerminalRunRow inserts one run that has already ended. The events stream
// tails a run until it is terminal, so a case reads the whole stream only when
// the run is already canceled; without a worker nothing else would end it.
func (f *authFixture) createTerminalRunRow(t *testing.T, policyID, sourceID string) string {
	t.Helper()
	runID := f.createRunRow(t, policyID, sourceID)
	changed, err := f.database.CancelQueuedRun(t.Context(), runID,
		store.SystemAudit("runs.canceled", store.AuditDetail{}),
		&store.RunLogWrite{Level: "warn", Event: "canceled", Message: "run canceled", Data: map[string]any{}})
	if err != nil {
		t.Fatal(err)
	}
	if !changed {
		t.Fatalf("run %s was not queued", runID)
	}
	return runID
}

// createRestoreRow inserts one queued restore for the matrix's restore-route
// cases. The dispatcher is stopped, so the row stays queued until the request
// under test acts on it: the detail and log cases read a real row, and a 202
// cancel can only have acted on a restore that was not terminal.
func (f *authFixture) createRestoreRow(t *testing.T, userID string) string {
	t.Helper()
	id := strings.ReplaceAll(uuid.NewString(), "-", "")
	backupID := uuid.Must(uuid.NewV7()).String()
	plan := store.RestorePlan{
		BackupID: backupID, ChainID: uuid.Must(uuid.NewV7()).String(), SourceID: uuid.Must(uuid.NewV7()).String(),
		ToolVersion: "8.4.0-7", Links: []store.RestorePlanLink{}, Risks: []string{}, Digest: restoreDigest,
	}
	if _, err := f.database.CreateRestore(t.Context(), store.RestoreInput{
		ID: id, BackupID: backupID, Plan: plan, PlanDigest: plan.Digest, RequestedBy: userID,
		Audit: store.SystemAudit("restores.execute", store.AuditDetail{}),
	}); err != nil {
		t.Fatal(err)
	}
	return id
}

func (f *authFixture) createRepositoryRow(t *testing.T, ctx context.Context, name, root string, sealed store.SecretSealer) string {
	t.Helper()
	if err := os.MkdirAll(root, 0o700); err != nil {
		t.Fatal(err)
	}
	repository, err := f.database.CreateRepository(ctx, store.RepositoryInput{
		Name: name, Kind: store.RepositoryLocal, TLSMode: store.TLSModeNone, Root: root,
	}, sealed, store.SystemAudit("repositories.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	return repository.ID
}

func (f *authFixture) createFTPRepositoryRow(t *testing.T, ctx context.Context, name, password string, sealed store.SecretSealer) string {
	t.Helper()
	repository, err := f.database.CreateRepository(ctx, store.RepositoryInput{
		Name: name, Kind: store.RepositoryFTP, TLSMode: store.TLSModePlain,
		FTP:    &store.FTPRepositoryConfig{Host: "127.0.0.1", Port: 21, BasePath: "/backups", Username: "backup"},
		Secret: []byte(password),
	}, sealed, store.SystemAudit("repositories.create", store.AuditDetail{PlaintextFTP: true}))
	if err != nil {
		t.Fatal(err)
	}
	return repository.ID
}

func (f *authFixture) createSourceRow(t *testing.T, ctx context.Context, name string, sealed store.SecretSealer) string {
	t.Helper()
	source, err := f.database.CreateSource(ctx, store.SourceInput{
		Name: name, Socket: "/run/mysqld/mysqld.sock", Datadir: "/var/lib/mysql",
		Username: "backup", Secret: []byte(matrixSecret),
	}, sealed, store.SystemAudit("sources.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	return source.ID
}

func (r seededResources) expand(body string) string {
	body = strings.ReplaceAll(body, "{{root}}", r.root)
	body = strings.ReplaceAll(body, "{{free_repository}}", r.freeRepository)
	return strings.ReplaceAll(body, "{{free_source}}", r.freeSource)
}

// targetID picks the row each resource family's :id refers to.
func (r seededResources) targetID(path, userID string) string {
	switch {
	case strings.HasPrefix(path, "/api/v1/repositories"):
		return r.freeRepository
	case strings.HasPrefix(path, "/api/v1/sources"):
		return r.freeSource
	case strings.HasPrefix(path, "/api/v1/policies"):
		return r.policyID
	case strings.HasPrefix(path, "/api/v1/runs"):
		return r.runID
	case strings.HasPrefix(path, "/api/v1/restores"):
		return r.restoreID
	default:
		return userID
	}
}
