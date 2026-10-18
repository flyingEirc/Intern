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
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/getkin/kin-openapi/openapi3"
	"github.com/getkin/kin-openapi/openapi3filter"
	"github.com/getkin/kin-openapi/routers"
	"github.com/getkin/kin-openapi/routers/gorillamux"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"

	"github.com/flyingEirc/Intern/app/web/api"
	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
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
	route, params, routeErr := client.router.FindRoute(req)
	var input *openapi3filter.RequestValidationInput
	if routeErr == nil {
		input = &openapi3filter.RequestValidationInput{Request: req, Route: route, PathParams: params}
		if err := openapi3filter.ValidateRequest(req.Context(), input); err != nil {
			return nil, fmt.Errorf("request contract: %w", err)
		}
	} else if !errors.Is(routeErr, routers.ErrPathNotFound) && !errors.Is(routeErr, routers.ErrMethodNotAllowed) {
		return nil, routeErr
	}
	recorder := httptest.NewRecorder()
	client.handler.ServeHTTP(recorder, req)
	if routeErr != nil {
		want := http.StatusNotFound
		if errors.Is(routeErr, routers.ErrMethodNotAllowed) {
			want = http.StatusMethodNotAllowed
		}
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
	engine, err := NewEngine(database, slog.New(slog.NewJSONHandler(output, nil)), proxies)
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
	engine := testEngine(t, readinessFunc(func(context.Context) error { return nil }), io.Discard, nil)
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
