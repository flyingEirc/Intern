package server

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

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
		`"full_cron":"0 2 * * *","incremental_cron":"0 * * * *","timezone":"UTC","max_chain_length":3,` +
		`"parallel":2,"throttle":0,"zstd_level":3,"page_tracking":false,"enabled":false}`
	bodyRunTrigger = `{"kind":"full"}`
)

// Every registered route must appear explicitly. Adding a route without deciding
// all four access outcomes fails this test, as does documenting a nonexistent one.
func TestEveryRoutePermissionMatrix(t *testing.T) {
	cases := map[string]permissionCase{
		"GET /healthz":                      {"", 200, 200, 200, 200},
		"GET /readyz":                       {"", 200, 200, 200, 200},
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
	}
	// Routes that require the Idempotency-Key header get one here, so the
	// request body is validated rather than rejected for a missing parameter.
	keys := map[string]string{
		"POST /api/v1/policies/:id/runs": "matrix-trigger",
	}
	fixture := newAuthFixtureWithRuns(t)
	if len(fixture.engine.Routes()) != len(cases) {
		t.Fatal("permission matrix and route counts differ")
	}
	for _, route := range fixture.engine.Routes() {
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
			t.Run(key+"/"+role, func(t *testing.T) { exercisePermission(t, route.Method, route.Path, role, row, headerKey) })
		}
	}
}

// exercisePermission runs one route as one role against freshly seeded rows, so
// a 2xx really means the operation ran rather than only passing authorization.
func exercisePermission(t *testing.T, method, path, role string, row permissionCase, headerKey string) {
	t.Helper()
	f := newAuthFixtureWithRuns(t)
	f.seed(t, "loginuser", "viewer")
	target := f.seed(t, "target", "viewer")
	seeded := f.seedResources(t)
	var cookie *http.Cookie
	if role != "anonymous" {
		f.seed(t, "actor", role)
		cookie = f.login(t, "actor", testPassword)
	}
	status := map[string]int{"admin": row.admin, "operator": row.operator, "viewer": row.viewer, "anonymous": row.anonymous}[role]
	if strings.HasPrefix(path, "/api/v1/runs") {
		seeded.runID = f.createRunRow(t, seeded.policyID, seeded.freeSource)
	}
	if path == "/api/v1/policies/:id/runs" {
		// Enable directly: this fixture cannot run the preflight the API path
		// requires, and an enabled policy is what reaches the runtime check.
		if _, err := f.database.DB().ExecContext(t.Context(), `UPDATE policies SET enabled = 1 WHERE id = ?`, seeded.policyID); err != nil {
			t.Fatal(err)
		}
	}
	resolved := strings.ReplaceAll(path, ":id", seeded.targetID(path, target.ID))
	resolved = strings.ReplaceAll(resolved, ":repository_id", seeded.policyRepository)
	req := apiRequest(method, resolved, seeded.expand(row.body), cookie)
	if headerKey != "" {
		req.Header.Set("Idempotency-Key", headerKey)
	}
	if method == http.MethodGet && strings.HasSuffix(resolved, "/events") {
		recorder, err := f.client.roundTripStream(req)
		if err != nil {
			t.Fatal(err)
		}
		if recorder.Code != status {
			t.Fatalf("status = %d, body = %s; want %d", recorder.Code, recorder.Body, status)
		}
		return
	}
	requestAPI(t, f.client, req, status)
}

// seededResources holds the rows a matrix case may reference. The policy points
// at its own source and repository so the "free" pair stays deletable.
// newAuthFixtureWithRuns serves the production engine shape: the runs routes
// exist only when a dispatcher is attached, so the matrix needs one. The
// fixture runtime stays unconfigured, which is why an allowed manual trigger
// answers 503 rather than creating a run.
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
	t.Cleanup(func() { _ = dispatcher.Stop() })
	engine, err := NewEngineWithResources(f.database, slog.New(slog.NewJSONHandler(&f.logs, nil)), nil, f.service,
		&resources.Service{Store: f.database, Settings: settings}, dispatcher)
	if err != nil {
		t.Fatal(err)
	}
	f.engine, f.client = engine, newContractClient(t, Handler(engine))
	return f
}

type seededResources struct {
	root             string
	freeRepository   string
	freeSource       string
	policyID         string
	policySource     string
	policyRepository string
	runID            string
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
		MaxChainLength: 3, Parallel: 1, Throttle: 0, ZstdLevel: 3,
	}, store.SystemAudit("policies.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	seeded.policyID = policy.ID
	seeded.policySource = policySource
	seeded.policyRepository = policyRepository
	return seeded
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
	default:
		return userID
	}
}
