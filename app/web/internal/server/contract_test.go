package server

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/flyingEirc/Intern/app/web/api"
	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

type readinessFunc func(context.Context) error

func (f readinessFunc) Ready(ctx context.Context) error { return f(ctx) }

type contractClient struct {
	handler http.Handler
	doc     *openapi3.T
	router  routers.Router
}

func loadContract(t *testing.T) *openapi3.T {
	t.Helper()
	doc, err := openapi3.NewLoader().LoadFromData(api.OpenAPI)
	if err != nil {
		t.Fatal(err)
	}
	if err = doc.Validate(t.Context()); err != nil {
		t.Fatal(err)
	}
	return doc
}

func newContractClient(t *testing.T, handler http.Handler) *contractClient {
	t.Helper()
	doc := loadContract(t)
	router, err := gorillamux.NewRouter(doc)
	if err != nil {
		t.Fatal(err)
	}
	return &contractClient{handler: handler, doc: doc, router: router}
}

// roundTrip validates both sides for every implemented API operation, including errors.
// Unregistered paths and methods must be rejected by the specification router;
// their wire bodies are validated separately against ErrorResponse, not invented routes.
func (client *contractClient) roundTrip(req *http.Request) (*httptest.ResponseRecorder, error) {
	return client.roundTripRequest(req, false)
}

func (client *contractClient) roundTripRequest(req *http.Request, invalidRequest bool) (*httptest.ResponseRecorder, error) {
	route, params, routeErr := client.router.FindRoute(req)
	var input *openapi3filter.RequestValidationInput
	if routeErr == nil {
		input = &openapi3filter.RequestValidationInput{
			Request: req, Route: route, PathParams: params,
			Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
		}
		if err := openapi3filter.ValidateRequest(req.Context(), input); (err != nil) != invalidRequest {
			return nil, fmt.Errorf("request contract (invalid expected=%t): %w", invalidRequest, err)
		}
	} else if !errors.Is(routeErr, routers.ErrPathNotFound) && !errors.Is(routeErr, routers.ErrMethodNotAllowed) {
		return nil, routeErr
	}
	recorder := httptest.NewRecorder()
	client.handler.ServeHTTP(recorder, req)
	if routeErr != nil {
		want := unmatchedStatus(req, routeErr)
		if recorder.Code != want {
			return nil, fmt.Errorf("specification rejected route, got status %d instead of %d", recorder.Code, want)
		}
		return recorder, client.validateError(recorder)
	}
	response := &openapi3filter.ResponseValidationInput{
		RequestValidationInput: input, Status: recorder.Code, Header: recorder.Header(),
		Options: &openapi3filter.Options{IncludeResponseStatus: true},
	}
	if err := openapi3filter.ValidateResponse(req.Context(), response.SetBodyBytes(recorder.Body.Bytes())); err != nil {
		return nil, fmt.Errorf("response contract: %w", err)
	}
	return recorder, nil
}

// roundTripStream validates the request and the response's declared status and
// media type without decoding the body: a live SSE body is not JSON, so
// kin-openapi cannot run the schema validator over it. The status must be
// documented and the Content-Type must resolve to a media type that operation
// declares for that status.
func (client *contractClient) roundTripStream(req *http.Request) (*httptest.ResponseRecorder, error) {
	route, params, err := client.router.FindRoute(req)
	if err != nil {
		return nil, err
	}
	input := &openapi3filter.RequestValidationInput{
		Request: req, Route: route, PathParams: params,
		Options: &openapi3filter.Options{AuthenticationFunc: openapi3filter.NoopAuthenticationFunc},
	}
	if err = openapi3filter.ValidateRequest(req.Context(), input); err != nil {
		return nil, fmt.Errorf("stream request contract: %w", err)
	}
	recorder := httptest.NewRecorder()
	client.handler.ServeHTTP(recorder, req)
	declared := route.Operation.Responses.Status(recorder.Code)
	if declared == nil || declared.Value == nil {
		return nil, fmt.Errorf("stream status %d is not documented", recorder.Code)
	}
	contentType := recorder.Header().Get("Content-Type")
	if declared.Value.Content.Get(contentType) == nil {
		return nil, fmt.Errorf("stream content type %q is not declared for status %d", contentType, recorder.Code)
	}
	return recorder, nil
}

func unmatchedStatus(req *http.Request, routeErr error) int {
	if http.NewCrossOriginProtection().Check(req) != nil {
		return http.StatusForbidden
	}
	if errors.Is(routeErr, routers.ErrMethodNotAllowed) {
		return http.StatusMethodNotAllowed
	}
	return http.StatusNotFound
}

func (client *contractClient) validateError(recorder *httptest.ResponseRecorder) error {
	if recorder.Header().Get("Content-Type") != "application/json; charset=utf-8" {
		return errors.New("error response must be JSON")
	}
	var body any
	if err := json.Unmarshal(recorder.Body.Bytes(), &body); err != nil {
		return err
	}
	return client.doc.Components.Schemas["ErrorResponse"].Value.VisitJSON(body)
}

func requestAPI(t *testing.T, client *contractClient, req *http.Request, status int) *httptest.ResponseRecorder {
	t.Helper()
	recorder, err := client.roundTrip(req)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.Code != status {
		t.Fatalf("status = %d, body = %s; want %d", recorder.Code, recorder.Body, status)
	}
	return recorder
}

func testEngine(t *testing.T, database Readiness, output io.Writer, proxies []string) *gin.Engine {
	t.Helper()
	metadata, ok := database.(*store.Store)
	if !ok {
		var err error
		metadata, err = store.Open(t.Context(), filepath.Join(t.TempDir(), "metadata"))
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { metadata.Close() })
	}
	service, err := auth.New(metadata, auth.DefaultSessionConfig(), auth.PasswordParams{Memory: 64, Time: 1, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(database, slog.New(slog.NewJSONHandler(output, nil)), proxies, service)
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

func ginPath(path string) string {
	parts := strings.Split(path, "/")
	for i, part := range parts {
		if strings.HasPrefix(part, ":") {
			parts[i] = "{" + strings.TrimPrefix(part, ":") + "}"
		}
	}
	return strings.Join(parts, "/")
}

func TestOpenAPIRoutesMatchBothDirections(t *testing.T) {
	doc := loadContract(t)
	// The runs routes exist only when the engine carries a dispatcher, so the
	// two-way comparison uses the same production wiring as serve.
	engine := testEngineWithRuns(t)
	implemented := make(map[string]bool)
	for _, route := range engine.Routes() {
		implemented[route.Method+" "+ginPath(route.Path)] = true
	}
	documented := make(map[string]bool)
	for path, item := range doc.Paths.Map() {
		for method := range item.Operations() {
			documented[method+" "+path] = true
		}
	}
	if !reflect.DeepEqual(implemented, documented) {
		t.Fatalf("route contract mismatch: Gin=%v OpenAPI=%v", implemented, documented)
	}
	if gin.Mode() != gin.ReleaseMode || !engine.HandleMethodNotAllowed || engine.RedirectTrailingSlash || engine.RedirectFixedPath {
		t.Fatal("engine is not configured for production")
	}
}

func TestOperationSecurityAndExplicitErrors(t *testing.T) {
	doc := loadContract(t)
	public := map[string]bool{
		"GET /healthz": true, "GET /readyz": true, "POST /api/v1/auth/login": true,
	}
	required := map[string][]string{
		"POST /api/v1/auth/login":            {"401", "403", "429", "503"},
		"POST /api/v1/auth/logout":           {"401", "403"},
		"GET /api/v1/auth/session":           {"401"},
		"PUT /api/v1/auth/password":          {"400", "401", "403", "429", "503"},
		"GET /api/v1/users":                  {"401", "403"},
		"POST /api/v1/users":                 {"401", "403", "409", "503"},
		"GET /api/v1/users/{id}":             {"401", "403"},
		"PATCH /api/v1/users/{id}":           {"401", "403", "409", "503"},
		"DELETE /api/v1/users/{id}/sessions": {"401", "403"},
		"GET /api/v1/audit-logs":             {"401", "403"},

		"GET /api/v1/repositories":             {"400", "401", "403"},
		"POST /api/v1/repositories":            {"400", "401", "403", "409", "503"},
		"GET /api/v1/repositories/{id}":        {"401", "403", "404"},
		"PUT /api/v1/repositories/{id}":        {"400", "401", "403", "404", "409", "503"},
		"DELETE /api/v1/repositories/{id}":     {"401", "403", "404", "409"},
		"POST /api/v1/repositories/{id}/probe": {"401", "403", "404", "503"},

		"GET /api/v1/sources":                 {"400", "401", "403"},
		"POST /api/v1/sources":                {"400", "401", "403", "409", "503"},
		"GET /api/v1/sources/{id}":            {"401", "403", "404"},
		"PUT /api/v1/sources/{id}":            {"400", "401", "403", "404", "409", "503"},
		"DELETE /api/v1/sources/{id}":         {"401", "403", "404", "409"},
		"POST /api/v1/sources/{id}/preflight": {"400", "401", "403", "404", "503"},

		"GET /api/v1/policies":               {"400", "401", "403"},
		"POST /api/v1/policies":              {"400", "401", "403", "404", "409", "503"},
		"GET /api/v1/policies/{id}":          {"401", "403", "404"},
		"PUT /api/v1/policies/{id}":          {"400", "401", "403", "404", "409", "503"},
		"DELETE /api/v1/policies/{id}":       {"401", "403", "404"},
		"POST /api/v1/policies/{id}/enable":  {"401", "403", "404", "409", "503"},
		"POST /api/v1/policies/{id}/disable": {"401", "403", "404"},
		"GET /api/v1/policies/{id}/schedule": {"400", "401", "403", "404"},

		"POST /api/v1/policies/{id}/runs":                          {"400", "401", "403", "404", "409", "503"},
		"GET /api/v1/runs":                                         {"400", "401", "403"},
		"GET /api/v1/runs/{id}":                                    {"401", "403", "404"},
		"GET /api/v1/runs/{id}/logs":                               {"400", "401", "403", "404"},
		"POST /api/v1/runs/{id}/cancel":                            {"401", "403", "404", "409"},
		"GET /api/v1/runs/{id}/events":                             {"400", "401", "403", "404"},
		"GET /api/v1/sources/{id}/chains":                          {"401", "403", "404"},
		"GET /api/v1/backups/{id}":                                 {"401", "403", "404"},
		"GET /api/v1/sources/{id}/protection":                      {"401", "403", "404"},
		"GET /api/v1/replicas":                                     {"400", "401", "403"},
		"POST /api/v1/backups/{id}/replicas/{repository_id}/retry": {"401", "403", "404"},

		"POST /api/v1/restores/plan":        {"400", "401", "403", "404", "409", "415", "422", "503"},
		"GET /api/v1/restores":              {"400", "401", "403"},
		"POST /api/v1/restores":             {"400", "401", "403", "404", "409", "415", "422", "429", "503"},
		"GET /api/v1/restores/{id}":         {"401", "403", "404"},
		"GET /api/v1/restores/{id}/logs":    {"400", "401", "403", "404"},
		"POST /api/v1/restores/{id}/cancel": {"401", "403", "404", "409"},
	}
	retryAfter := map[string][]string{
		"POST /api/v1/auth/login":   {"429", "503"},
		"PUT /api/v1/auth/password": {"429", "503"},
		"POST /api/v1/users":        {"503"},
		"PATCH /api/v1/users/{id}":  {"503"},
		"POST /api/v1/restores":     {"429"},
	}
	seen := map[string]bool{}
	for path, item := range doc.Paths.Map() {
		for method, operation := range item.Operations() {
			key := method + " " + path
			seen[key] = true
			assertOperationContract(t, doc, operation, key, public[key], required[key], retryAfter[key])
		}
	}
	assertSpecKeysSeen(t, seen, public, required)
}

func assertOperationContract(t *testing.T, doc *openapi3.T, operation *openapi3.Operation, key string, public bool, required, retryAfter []string) {
	t.Helper()
	if public {
		if operation.Security == nil || len(*operation.Security) != 0 {
			t.Fatalf("%s security=%v, want []", key, operation.Security)
		}
	} else if !requiresCookie(doc, operation) {
		t.Fatalf("%s does not require cookieAuth", key)
	}
	for _, status := range required {
		if operation.Responses == nil || operation.Responses.Value(status) == nil {
			t.Fatalf("%s missing explicit %s", key, status)
		}
	}
	for _, status := range retryAfter {
		assertDeclaredRetryAfter(t, operation, key, status)
	}
}

func assertSpecKeysSeen(t *testing.T, seen, public map[string]bool, required map[string][]string) {
	t.Helper()
	for key := range public {
		if !seen[key] {
			t.Fatalf("public operation %s missing from spec", key)
		}
	}
	for key := range required {
		if !seen[key] {
			t.Fatalf("operation %s missing from spec", key)
		}
	}
}

func requiresCookie(doc *openapi3.T, operation *openapi3.Operation) bool {
	requirements := doc.Security
	if operation.Security != nil {
		requirements = *operation.Security
	}
	for _, requirement := range requirements {
		if _, ok := requirement["cookieAuth"]; ok {
			return true
		}
	}
	return false
}

func assertDeclaredRetryAfter(t *testing.T, operation *openapi3.Operation, key, status string) {
	t.Helper()
	ref := operation.Responses.Value(status)
	if ref == nil || ref.Value == nil {
		t.Fatalf("%s %s is not an explicit response", key, status)
	}
	header := ref.Value.Headers["Retry-After"]
	if header == nil || header.Value == nil || !header.Value.Required {
		t.Fatalf("%s %s does not require Retry-After", key, status)
	}
}

func TestHealthAndReadinessAPI(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "metadata")
	database, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	client := newContractClient(t, testEngine(t, database, io.Discard, nil))
	for _, path := range []string{"/healthz", "/readyz"} {
		requestAPI(t, client, httptest.NewRequest(http.MethodGet, path, nil), http.StatusOK)
	}
	other, err := sql.Open("sqlite", filepath.Join(dir, "intern.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer other.Close()
	if _, err = other.ExecContext(t.Context(), "INSERT INTO web_goose_db_version (version_id, is_applied) VALUES (99, 1)"); err != nil {
		t.Fatal(err)
	}
	requestAPI(t, client, httptest.NewRequest(http.MethodGet, "/readyz", nil), http.StatusServiceUnavailable)
	requestAPI(t, client, httptest.NewRequest(http.MethodGet, "/healthz", nil), http.StatusOK)
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	response := requestAPI(t, client, httptest.NewRequest(http.MethodGet, "/readyz", nil), http.StatusServiceUnavailable)
	if !strings.Contains(response.Body.String(), `"code":"unavailable"`) {
		t.Fatal(response.Body.String())
	}
	requestAPI(t, client, httptest.NewRequest(http.MethodGet, "/healthz", nil), http.StatusOK)
}

// runExecutable writes a real executable named xtrabackup that prints its own
// absolute path in the pinned version banner. ProbeVersion requires that exact
// banner shape, so this is a real process, not a mock of the tested component.
func runExecutable(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "xtrabackup")
	script := "#!/bin/sh\nprintf '%s version 8.4.0-7\\n' \"$0\"\n"
	if err := os.WriteFile(path, []byte(script), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// runSettings is a configured runtime: the KEK from the fixture, a private work
// root, positive byte thresholds and the real xtrabackup executable above. A
// run therefore passes the configuration gate and fails at the absent MySQL
// socket with error_class preflight.
func runSettings(t *testing.T, f *authFixture) resources.Settings {
	t.Helper()
	workRoot := filepath.Join(t.TempDir(), "work")
	if err := os.Mkdir(workRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	return resources.Settings{
		Sealer: f.sealer, KeyFile: f.keyFile, MetadataDir: f.dir,
		WorkRoot: workRoot, XtrabackupPath: runExecutable(t), MinWorkBytes: 1, MinBackupBytes: 1,
	}
}

// testEngineWithRuns builds the engine the way serve does: with the real
// dispatcher attached, so the runs routes are registered.
func testEngineWithRuns(t *testing.T) *gin.Engine {
	t.Helper()
	f := newAuthFixture(t)
	settings := runSettings(t, f)
	dispatcher, err := dispatch.Start(t.Context(), dispatch.Config{
		Database: f.database, Settings: settings, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Stop() })
	engine, err := NewEngineWithResources(f.database, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil, f.service,
		&resources.Service{Store: f.database, Settings: settings}, dispatcher, WithRestores(dispatcher))
	if err != nil {
		t.Fatal(err)
	}
	return engine
}

// runContractFixture is the real-body contract harness for the W4 routes: an
// authenticated engine with the dispatcher attached and one seeded policy.
type runContractFixture struct {
	*authFixture
	t          *testing.T
	cookie     *http.Cookie
	policy     store.Policy
	source     string
	dispatcher *dispatch.Dispatcher
}

func newRunContractFixture(t *testing.T) *runContractFixture {
	t.Helper()
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	settings := runSettings(t, f)
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
	f.engine = engine
	f.client = newContractClient(t, Handler(engine))
	fixture := &runContractFixture{authFixture: f, t: t, cookie: f.login(t, "admin", testPassword), dispatcher: dispatcher}
	seeded := f.seedResources(t)
	fixture.source = seeded.freeSource
	if _, err = f.database.DB().ExecContext(t.Context(), `UPDATE policies SET enabled = 1 WHERE id = ?`, seeded.policyID); err != nil {
		t.Fatal(err)
	}
	policy, err := f.database.Policy(t.Context(), seeded.policyID)
	if err != nil {
		t.Fatal(err)
	}
	fixture.policy = policy
	return fixture
}

// TestRunAPIContractRoundTrips validates real request and response bodies of
// every new W4 route against the specification, including the error bodies.
func TestRunAPIContractRoundTrips(t *testing.T) {
	f := newRunContractFixture(t)
	// A stopped dispatcher is the real shutdown state: the worker no longer
	// claims, so the created run stays queued and this test does not race it.
	f.stopRuns()
	queued := f.triggerRun(t, f.policy.ID, "full", "contract-full")
	if queued.Status != store.RunStatusQueued {
		t.Fatalf("triggered run status = %q, want queued", queued.Status)
	}
	f.request(t, "GET", "/api/v1/runs?policy_id="+f.policy.ID+"&source_id="+f.policy.SourceID+"&status=queued&limit=2", "", f.cookie, http.StatusOK)
	f.request(t, "GET", "/api/v1/runs/"+queued.ID, "", f.cookie, http.StatusOK)
	f.request(t, "GET", "/api/v1/runs/"+uuid.Must(uuid.NewV7()).String(), "", f.cookie, http.StatusNotFound)
	f.request(t, "GET", "/api/v1/runs/"+queued.ID+"/logs?after_seq=0&limit=10", "", f.cookie, http.StatusOK)
	f.invalidRequest(t, "GET", "/api/v1/runs?status=finalizing", "", f.cookie, http.StatusBadRequest)
	f.invalidRequest(t, "GET", "/api/v1/runs/"+queued.ID+"/logs?after_seq=-1", "", f.cookie, http.StatusBadRequest)
	// Same idempotency key, same body: the replayed 202 carries the same run.
	replayed := f.triggerRun(t, f.policy.ID, "full", "contract-full")
	if replayed.ID != queued.ID {
		t.Fatalf("replay created a second run: %s != %s", replayed.ID, queued.ID)
	}
	// Same key, different body: a conflict, and the original run is untouched.
	conflict := f.requestWithKey(t, "/api/v1/policies/"+f.policy.ID+"/runs", `{"kind":"incremental"}`, "contract-full", http.StatusConflict)
	if decodeResponse[render.ErrorResponse](t, conflict).Error.Code != render.Conflict {
		t.Fatal(conflict.Body.String())
	}
	f.invalidRequest(t, "POST", "/api/v1/policies/"+f.policy.ID+"/runs", `{"kind":"full"}`, f.cookie, http.StatusBadRequest)
	f.invalidKeyRequest(t, "/api/v1/policies/"+f.policy.ID+"/runs", `{"kind":"full"}`, strings.Repeat("k", 129), http.StatusBadRequest)
	f.requestWithKey(t, "/api/v1/policies/"+uuid.Must(uuid.NewV7()).String()+"/runs", `{"kind":"full"}`, "missing", http.StatusNotFound)
	f.invalidKeyRequest(t, "/api/v1/policies/"+f.policy.ID+"/runs", `{"kind":"differential"}`, "bad-kind", http.StatusBadRequest)

	// A disabled policy answers 409 conflict.
	disabled, err := f.database.CreatePolicy(t.Context(), store.PolicyInput{
		Name: "contract-disabled", SourceID: f.policy.SourceID, LocalRepositoryID: f.policy.LocalRepositoryID,
		FullCron: "0 2 * * *", IncrementalCron: "0 * * * *", Timezone: "UTC", MaxChainLength: 3, Parallel: 1, ZstdLevel: 3,
	}, store.SystemAudit("policies.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	f.requestWithKey(t, "/api/v1/policies/"+disabled.ID+"/runs", `{"kind":"full"}`, "disabled", http.StatusConflict)

	// A queued run cancels without running; a second cancel finds it terminal.
	canceled := f.triggerRun(t, f.policy.ID, "incremental", "contract-cancel")
	f.request(t, "POST", "/api/v1/runs/"+canceled.ID+"/cancel", "", f.cookie, http.StatusAccepted)
	f.request(t, "POST", "/api/v1/runs/"+canceled.ID+"/cancel", "", f.cookie, http.StatusConflict)
	f.request(t, "POST", "/api/v1/runs/"+uuid.Must(uuid.NewV7()).String()+"/cancel", "", f.cookie, http.StatusNotFound)

	f.request(t, "GET", "/api/v1/sources/"+f.source+"/chains", "", f.cookie, http.StatusOK)
	f.request(t, "GET", "/api/v1/sources/"+uuid.Must(uuid.NewV7()).String()+"/chains", "", f.cookie, http.StatusNotFound)
	f.request(t, "GET", "/api/v1/sources/"+f.source+"/protection", "", f.cookie, http.StatusOK)
	f.request(t, "GET", "/api/v1/sources/"+uuid.Must(uuid.NewV7()).String()+"/protection", "", f.cookie, http.StatusNotFound)
	f.request(t, "GET", "/api/v1/backups/"+uuid.Must(uuid.NewV7()).String(), "", f.cookie, http.StatusNotFound)
	f.request(t, "GET", "/api/v1/replicas?limit=10", "", f.cookie, http.StatusOK)
	f.invalidRequest(t, "GET", "/api/v1/replicas?status=published", "", f.cookie, http.StatusBadRequest)
	f.request(t, "POST", "/api/v1/backups/"+uuid.Must(uuid.NewV7()).String()+"/replicas/"+uuid.Must(uuid.NewV7()).String()+"/retry", "", f.cookie, http.StatusNotFound)
}

// requestWithKey posts one body with the Idempotency-Key header set.
func (f *runContractFixture) requestWithKey(t *testing.T, path, body, key string, status int) *httptest.ResponseRecorder {
	t.Helper()
	req := apiRequest("POST", path, body, f.cookie)
	req.Header.Set("Idempotency-Key", key)
	return requestAPI(t, f.client, req, status)
}

// invalidKeyRequest posts a header-bearing body the specification rejects, so
// both the required Idempotency-Key parameter and the body schema are checked.
func (f *runContractFixture) invalidKeyRequest(t *testing.T, path, body, key string, status int) {
	t.Helper()
	req := apiRequest("POST", path, body, f.cookie)
	req.Header.Set("Idempotency-Key", key)
	recorder, err := f.client.roundTripRequest(req, true)
	if err != nil {
		t.Fatal(err)
	}
	if recorder.Code != status {
		t.Fatalf("status = %d, body = %s; want %d", recorder.Code, recorder.Body, status)
	}
}

// stopRuns shuts the dispatcher's loops down while the HTTP surface stays
// served, so created runs stay queued instead of racing the worker's poll.
func (f *runContractFixture) stopRuns() {
	if f.dispatcher != nil {
		_ = f.dispatcher.Stop()
		f.dispatcher = nil
	}
}

// awaitTerminal polls the stored run until it reaches a terminal status.
func (f *runContractFixture) awaitTerminal(runID string) store.Run {
	f.t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		run, err := f.database.Run(f.t.Context(), runID)
		if err != nil {
			f.t.Fatal(err)
		}
		switch run.Status {
		case store.RunStatusCompleted, store.RunStatusFailed, store.RunStatusCanceled:
			return run
		}
		if time.Now().After(deadline) {
			f.t.Fatalf("run %s stayed %s", runID, run.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// triggerRun posts one manual trigger and returns the stored run.
func (f *runContractFixture) triggerRun(t *testing.T, policyID, kind, key string) store.Run {
	t.Helper()
	req := apiRequest("POST", "/api/v1/policies/"+policyID+"/runs", `{"kind":"`+kind+`"}`, f.cookie)
	req.Header.Set("Idempotency-Key", key)
	response := requestAPI(t, f.client, req, http.StatusAccepted)
	accepted := decodeResponse[acceptedResponse](t, response)
	run, err := f.database.Run(t.Context(), accepted.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// schedulePreviewResponse is the preview wire shape the contract test decodes.
type schedulePreviewResponse struct {
	PolicyID string `json:"policy_id"`
	Enabled  bool   `json:"enabled"`
	Timezone string `json:"timezone"`
	Fires    []struct {
		Kind   string    `json:"kind"`
		At     time.Time `json:"at"`
		Missed bool      `json:"missed"`
	} `json:"fires"`
}

// TestPolicySchedulePreviewContract validates the preview through the real
// engine: the body matches the contract, the fires are chronological and
// exactly count long, and previewing an enabled policy whose schedule is not
// initialized yet neither fills the stored columns nor writes an audit row.
// No role lacks resources:read, so the forbidden outcome is not reachable; the
// permission matrix locks the per-role statuses and the no-session case is 401.
func TestPolicySchedulePreviewContract(t *testing.T) {
	f := newRunContractFixture(t)
	before := auditCount(t, f)
	if full, incremental := policyScheduleColumns(t, f, f.policy.ID); full != nil || incremental != nil {
		t.Fatalf("seeded policy schedule = %v %v, want uninitialized", full, incremental)
	}

	response := f.request(t, "GET", "/api/v1/policies/"+f.policy.ID+"/schedule?count=3", "", f.cookie, http.StatusOK)
	preview := decodeResponse[schedulePreviewResponse](t, response)
	assertPreviewFires(t, preview, f)

	// The default count applies when the query is absent.
	defaulted := decodeResponse[schedulePreviewResponse](t,
		f.request(t, "GET", "/api/v1/policies/"+f.policy.ID+"/schedule", "", f.cookie, http.StatusOK))
	if len(defaulted.Fires) != 10 {
		t.Fatalf("default fires = %d, want 10", len(defaulted.Fires))
	}

	if full, incremental := policyScheduleColumns(t, f, f.policy.ID); full != nil || incremental != nil {
		t.Fatalf("preview wrote the schedule: %v %v", full, incremental)
	}
	if after := auditCount(t, f); after != before {
		t.Fatalf("audit rows = %d, want %d: the preview must not record one", after, before)
	}

	// A disabled policy previews too, reporting itself disabled.
	disabled := f.request(t, "GET", "/api/v1/policies/"+f.disabledPolicy(t)+"/schedule?count=1", "", f.cookie, http.StatusOK)
	if decodeResponse[schedulePreviewResponse](t, disabled).Enabled {
		t.Fatal("disabled policy preview reports enabled")
	}

	f.request(t, "GET", "/api/v1/policies/"+f.policy.ID+"/schedule", "", nil, http.StatusUnauthorized)
	f.request(t, "GET", "/api/v1/policies/"+uuid.Must(uuid.NewV7()).String()+"/schedule", "", f.cookie, http.StatusNotFound)
	for _, count := range []string{"0", "51", "abc", "1.5", "-3"} {
		f.invalidRequest(t, "GET", "/api/v1/policies/"+f.policy.ID+"/schedule?count="+count, "", f.cookie, http.StatusBadRequest)
	}
}

// assertPreviewFires checks the preview's identity and that its fires are a
// chronological UTC sequence of the two run kinds, none of them missed.
func assertPreviewFires(t *testing.T, preview schedulePreviewResponse, f *runContractFixture) {
	t.Helper()
	if preview.PolicyID != f.policy.ID || !preview.Enabled || preview.Timezone != f.policy.Timezone {
		t.Fatalf("preview identity = %+v", preview)
	}
	if len(preview.Fires) != 3 {
		t.Fatalf("fires = %d, want 3", len(preview.Fires))
	}
	for i, fire := range preview.Fires {
		assertOneFire(t, fire, i, preview)
	}
}

// assertOneFire checks one fire's kind, missed mark, UTC zone and order.
func assertOneFire(t *testing.T, fire struct {
	Kind   string    `json:"kind"`
	At     time.Time `json:"at"`
	Missed bool      `json:"missed"`
}, i int, preview schedulePreviewResponse,
) {
	t.Helper()
	if fire.Kind != "full" && fire.Kind != "incremental" {
		t.Fatalf("fire %d kind = %q", i, fire.Kind)
	}
	if fire.Missed {
		t.Fatalf("fire %d of an uninitialized policy is marked missed", i)
	}
	if fire.At.Location() != time.UTC {
		t.Fatalf("fire %d location = %s, want UTC", i, fire.At.Location())
	}
	if i > 0 && !fire.At.After(preview.Fires[i-1].At) {
		t.Fatalf("fires out of order: %s then %s", preview.Fires[i-1].At, fire.At)
	}
}

// disabledPolicy creates one disabled policy on the fixture's source and
// repository and returns its id.
func (f *runContractFixture) disabledPolicy(t *testing.T) string {
	t.Helper()
	policy, err := f.database.CreatePolicy(t.Context(), store.PolicyInput{
		Name: "preview-disabled", SourceID: f.policy.SourceID, LocalRepositoryID: f.policy.LocalRepositoryID,
		FullCron: "0 2 * * *", IncrementalCron: "0 * * * *", Timezone: "UTC", MaxChainLength: 3, Parallel: 1, ZstdLevel: 3,
	}, store.SystemAudit("policies.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	return policy.ID
}

// policyScheduleColumns reads the stored next trigger times.
func policyScheduleColumns(t *testing.T, f *runContractFixture, policyID string) (*time.Time, *time.Time) {
	t.Helper()
	full, incremental, err := f.database.PolicySchedule(t.Context(), policyID)
	if err != nil {
		t.Fatal(err)
	}
	return full, incremental
}

// auditCount counts the audit rows, so a read can prove it added none.
func auditCount(t *testing.T, f *runContractFixture) int {
	t.Helper()
	var count int
	if err := f.database.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_logs`).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// TestRunEventStreamContract covers the SSE route's pre-stream JSON failures
// and the streamed response: content type, event ids, replay and the final
// end event. After the first byte no JSON is mixed in.
func TestRunEventStreamContract(t *testing.T) {
	f, run, terminal := newRunContractFixture(t).streamFixture(t)
	// Pre-stream failures are ordinary JSON errors validated by the contract.
	f.streamErrors(t, run.ID)
	// The stream itself: replay, the single end event, no JSON after byte one.
	body := f.streamBody(t, run.ID, "")
	if !strings.Contains(body, "id: 1\n") || !strings.Contains(body, "event: message\n") {
		t.Fatalf("replay missing: %s", body)
	}
	assertStreamEnd(t, body, terminal.Status)
	if strings.Contains(body, "{\"error\"") {
		t.Fatalf("JSON error mixed into the stream: %s", body)
	}
	// Last-Event-ID resumes past the rows already seen.
	resumed := f.streamBody(t, run.ID, "1")
	if strings.Contains(resumed, "id: 1\n") {
		t.Fatalf("resume replayed a seen event: %s", resumed)
	}
	assertStreamEnd(t, resumed, terminal.Status)
}

// streamFixture runs one run to a terminal status through the real dispatcher.
func (f *runContractFixture) streamFixture(t *testing.T) (*runContractFixture, store.Run, store.Run) {
	t.Helper()
	run := f.triggerRun(t, f.policy.ID, "full", "contract-stream")
	terminal := f.awaitTerminal(run.ID)
	if terminal.Status != store.RunStatusFailed || terminal.ErrorClass == nil || *terminal.ErrorClass != store.RunErrorPreflight {
		t.Fatalf("run = %+v, want failed/preflight", terminal)
	}
	return f, run, terminal
}

// streamErrors exercises every pre-stream JSON failure of the events route.
func (f *runContractFixture) streamErrors(t *testing.T, runID string) {
	t.Helper()
	f.request(t, "GET", "/api/v1/runs/"+runID+"/events", "", nil, http.StatusUnauthorized)
	f.request(t, "GET", "/api/v1/runs/"+uuid.Must(uuid.NewV7()).String()+"/events", "", f.cookie, http.StatusNotFound)
	f.invalidRequest(t, "GET", "/api/v1/runs/"+runID+"/events?after_seq=-1", "", f.cookie, http.StatusBadRequest)
	badResume := apiRequest("GET", "/api/v1/runs/"+runID+"/events", "", f.cookie)
	badResume.Header.Set("Last-Event-ID", "not-a-seq")
	if _, err := f.client.roundTripRequest(badResume, true); err != nil {
		t.Fatal(err)
	}
}

// streamBody opens the event stream and returns its body, checking the status
// and the declared media type on the way.
func (f *runContractFixture) streamBody(t *testing.T, runID, lastEventID string) string {
	t.Helper()
	req := apiRequest("GET", "/api/v1/runs/"+runID+"/events", "", f.cookie)
	if lastEventID == "" {
		req = apiRequest("GET", "/api/v1/runs/"+runID+"/events?after_seq=0", "", f.cookie)
	} else {
		req.Header.Set("Last-Event-ID", lastEventID)
	}
	response, err := f.client.roundTripStream(req)
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if mediaType := response.Header().Get("Content-Type"); mediaType != "text/event-stream" {
		t.Fatalf("content type=%q", mediaType)
	}
	return response.Body.String()
}

// assertStreamEnd requires exactly one terminal end frame as the last bytes.
func assertStreamEnd(t *testing.T, body, status string) {
	t.Helper()
	if !strings.HasSuffix(body, "event: end\ndata: "+status+"\n\n") {
		t.Fatalf("stream did not end with the terminal status: %s", body)
	}
}

// TestRunBodiesCarryTheirRealEnums validates the concrete run, log and
// protection bodies against the specification's closed enums.
func TestRunBodiesCarryTheirRealEnums(t *testing.T) {
	f := newRunContractFixture(t)
	f.stopRuns()
	run := f.triggerRun(t, f.policy.ID, "full", "contract-enums")
	assertQueuedRun(t, f.request(t, "GET", "/api/v1/runs/"+run.ID, "", f.cookie, http.StatusOK))
	assertQueuedLogs(t, f.request(t, "GET", "/api/v1/runs/"+run.ID+"/logs", "", f.cookie, http.StatusOK))
	protection := decodeResponse[dispatch.Protection](t,
		f.request(t, "GET", "/api/v1/sources/"+f.source+"/protection", "", f.cookie, http.StatusOK))
	if protection.Local != nil || len(protection.Remote) != 0 {
		t.Fatalf("protection = %+v", protection)
	}
}

// assertQueuedRun checks the closed run enums of a freshly queued row.
func assertQueuedRun(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	body := decodeResponse[store.Run](t, response)
	if body.Status != store.RunStatusQueued || body.Kind != store.RunKindFull || body.Trigger != store.RunTriggerManual || body.CancelRequested {
		t.Fatalf("run = %+v", body)
	}
}

// assertQueuedLogs checks the closed log enums of the queued event only.
func assertQueuedLogs(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	page := decodeResponse[runLogsPage](t, response)
	if len(page.Items) != 1 || page.Items[0].Event != "queued" || page.Items[0].Level != "info" || page.Truncated {
		t.Fatalf("logs = %+v", page)
	}
}

func TestNoRouteAndNoMethodErrors(t *testing.T) {
	var output bytes.Buffer
	client := newContractClient(t, testEngine(t, readinessFunc(func(context.Context) error { return nil }), &output, nil))
	for _, tc := range []struct {
		method, path, code string
		status             int
	}{
		{http.MethodGet, "/api/v1/unknown", render.NotFound, 404},
		{http.MethodPost, "/healthz", render.MethodNotAllowed, 405},
		{http.MethodDelete, "/readyz", render.MethodNotAllowed, 405},
		{http.MethodGet, "/healthz/", render.NotFound, 404},
		{http.MethodPost, "/healthz/", render.NotFound, 404},
	} {
		output.Reset()
		req := httptest.NewRequest(tc.method, tc.path, nil)
		req.Header.Set(render.RequestIDHeader, "route-test")
		response := requestAPI(t, client, req, tc.status)
		var body render.ErrorResponse
		if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
			t.Fatal(err)
		}
		if body.Error.Code != tc.code || body.Error.RequestID != "route-test" || response.Header().Get(render.RequestIDHeader) != "route-test" {
			t.Fatalf("error = %+v, request id = %q", body, response.Header().Get(render.RequestIDHeader))
		}
		if strings.HasSuffix(tc.path, "/") {
			assertRequestLog(t, output.Bytes(), "unmatched", tc.status, "192.0.2.1")
		}
	}
}

func TestPanicRecoveryContractAndLogPrivacy(t *testing.T) {
	var output bytes.Buffer
	engine := testEngine(t, readinessFunc(func(context.Context) error { panic("private-panic-detail") }), &output, nil)
	client := newContractClient(t, engine)
	req := httptest.NewRequest(http.MethodGet, "/readyz?query=private-query", strings.NewReader("private-body"))
	req.Header.Set("Authorization", "Bearer private-auth")
	req.Header.Set("Cookie", "session=private-cookie")
	req.Header.Set(render.RequestIDHeader, "panic-test")
	response := requestAPI(t, client, req, http.StatusInternalServerError)
	if strings.Contains(response.Body.String(), "private-panic-detail") || strings.Contains(response.Body.String(), "goroutine") {
		t.Fatalf("panic leaked: %s", response.Body)
	}
	for _, secret := range []string{"private-query", "private-body", "private-auth", "private-cookie", "?query"} {
		if strings.Contains(output.String(), secret) {
			t.Fatalf("log leaked %q: %s", secret, &output)
		}
	}
	assertPanicLog(t, output.Bytes(), "panic-test", "private-panic-detail")
	assertRequestLog(t, output.Bytes(), "/readyz", 500, "192.0.2.1")
}

func TestPanicLogTruncatesValue(t *testing.T) {
	var output bytes.Buffer
	value := strings.Repeat("A", middleware.PanicValueLimit) + "panic-tail-marker"
	engine := testEngine(t, readinessFunc(func(context.Context) error { panic(value) }), &output, nil)
	client := newContractClient(t, engine)
	req := httptest.NewRequest(http.MethodGet, "/readyz", nil)
	req.Header.Set(render.RequestIDHeader, "panic-trunc")
	response := requestAPI(t, client, req, http.StatusInternalServerError)
	if strings.Contains(response.Body.String(), "panic-tail-marker") || strings.Contains(response.Body.String(), "goroutine") || strings.Contains(response.Body.String(), strings.Repeat("A", 32)) {
		t.Fatalf("panic leaked: %s", response.Body)
	}
	if strings.Contains(output.String(), "panic-tail-marker") {
		t.Fatal("log kept the truncated panic tail")
	}
	assertPanicLog(t, output.Bytes(), "panic-trunc", strings.Repeat("A", middleware.PanicValueLimit))
}

func assertPanicLog(t *testing.T, data []byte, requestID, value string) {
	t.Helper()
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte("\n")) {
		var fields map[string]any
		if err := json.Unmarshal(line, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["msg"] != "request handler panicked" {
			continue
		}
		stack, _ := fields["stack"].(string)
		if fields["level"] != "ERROR" || fields["request_id"] != requestID || fields["panic_value"] != value || !strings.Contains(stack, "goroutine") || !strings.Contains(stack, "Recovery") {
			t.Fatalf("panic log = %#v", fields)
		}
		return
	}
	t.Fatal("panic not logged")
}

func assertRequestLog(t *testing.T, data []byte, route string, status int, clientIP string) {
	t.Helper()
	for line := range bytes.SplitSeq(bytes.TrimSpace(data), []byte("\n")) {
		var fields map[string]any
		if err := json.Unmarshal(line, &fields); err != nil {
			t.Fatal(err)
		}
		if fields["msg"] != "request complete" {
			continue
		}
		for _, key := range []string{"request_id", "method", "route", "status", "latency", "client_ip"} {
			if _, exists := fields[key]; !exists {
				t.Fatalf("missing log field %q: %v", key, fields)
			}
		}
		if fields["route"] != route || fields["status"] != float64(status) || fields["client_ip"] != clientIP {
			t.Fatalf("request log: %v", fields)
		}
		return
	}
	t.Fatal("no structured request log")
}

func TestClientIPRequiresExplicitProxyTrust(t *testing.T) {
	for _, tc := range []struct {
		name    string
		proxies []string
		want    string
	}{
		{"default", nil, "192.0.2.1"},
		{"trusted IP", []string{"192.0.2.1"}, "198.51.100.4"},
		{"trusted CIDR", []string{"192.0.2.0/24"}, "198.51.100.4"},
		{"different proxy", []string{"203.0.113.0/24"}, "192.0.2.1"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			var output bytes.Buffer
			client := newContractClient(t, testEngine(t, readinessFunc(func(context.Context) error { return nil }), &output, tc.proxies))
			req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
			req.Header.Set("X-Forwarded-For", "198.51.100.4")
			req.Header.Set(render.RequestIDHeader, "proxy-test")
			requestAPI(t, client, req, 200)
			assertRequestLog(t, output.Bytes(), "/healthz", 200, tc.want)
		})
	}
}

func TestRequestIDsAndBodyLimitAPI(t *testing.T) {
	client := newContractClient(t, testEngine(t, readinessFunc(func(context.Context) error { return nil }), io.Discard, nil))
	for _, id := range []string{"", "with spaces", strings.Repeat("a", 65), "allowed-ID.1_"} {
		req := httptest.NewRequest(http.MethodGet, "/healthz", nil)
		req.Header.Set(render.RequestIDHeader, id)
		response := requestAPI(t, client, req, 200)
		got := response.Header().Get(render.RequestIDHeader)
		if id == "allowed-ID.1_" {
			if got != id {
				t.Fatalf("request ID not echoed: %q", got)
			}
		} else if parsed, err := uuid.Parse(got); err != nil || parsed.Version() != 7 {
			t.Fatalf("request ID not UUIDv7: %q, %v", got, err)
		}
	}
	req := httptest.NewRequest(http.MethodGet, "/healthz", strings.NewReader(strings.Repeat("x", int(middleware.MaxRequestBodyBytes)+1)))
	response := requestAPI(t, client, req, http.StatusRequestEntityTooLarge)
	if !strings.Contains(response.Body.String(), render.PayloadTooLarge) {
		t.Fatal(response.Body.String())
	}
}

func TestUnmatchedRequestLogUsesFixedRoute(t *testing.T) {
	var output bytes.Buffer
	client := newContractClient(t, testEngine(t, readinessFunc(func(context.Context) error { return nil }), &output, nil))
	requestAPI(t, client, httptest.NewRequest(http.MethodGet, "/api/v1/private-path?password=private-query", nil), 404)
	assertRequestLog(t, output.Bytes(), "unmatched", 404, "192.0.2.1")
	if strings.Contains(output.String(), "private-path") || strings.Contains(output.String(), "private-query") {
		t.Fatal("unmatched request logged user-controlled URL")
	}
}
