package server

import (
	"encoding/json/v2"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/http/httputil"
	"net/url"
	"os"
	"os/exec"
	"reflect"
	"strings"
	"testing"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// liveSetup exercises the real HTTP/TLS boundary while retaining the normal
// contract client: each request and response is validated against OpenAPI before
// the reverse proxy sends it across the listener. It never seeds an admin.
func liveSetup(t *testing.T, f *authFixture) *httptest.Server {
	t.Helper()
	engine, err := NewEngine(f.database, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil, f.service)
	if err != nil {
		t.Fatal(err)
	}
	srv := httptest.NewTLSServer(Handler(engine))
	t.Cleanup(srv.Close)
	target, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	proxy := httputil.NewSingleHostReverseProxy(target)
	proxy.Transport = srv.Client().Transport
	f.client = newContractClient(t, proxy)
	return srv
}

func setupBody(username string) string {
	return `{"username":"` + username + `","password":"` + testPassword + `"}`
}

func assertSetupState(t *testing.T, f *authFixture, required bool) {
	t.Helper()
	response := f.request(t, http.MethodGet, "/api/v1/auth/setup", "", nil, http.StatusOK)
	var object map[string]any
	if err := json.Unmarshal(response.Body.Bytes(), &object); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(object, map[string]any{"required": required}) {
		t.Fatalf("setup status leaked data or returned the wrong state: %s", response.Body)
	}
}

func assertSetupRows(t *testing.T, f *authFixture, users, successes int, completed bool) {
	t.Helper()
	var count int
	if err := f.database.DB().QueryRowContext(t.Context(), `SELECT COUNT(*) FROM users`).Scan(&count); err != nil || count != users {
		t.Fatalf("setup user count=%d, want %d, err=%v", count, users, err)
	}
	var marker bool
	if err := f.database.DB().QueryRowContext(t.Context(), `SELECT completed FROM setup_state WHERE singleton = 1`).Scan(&marker); err != nil || marker != completed {
		t.Fatalf("setup marker=%t, want %t, err=%v", marker, completed, err)
	}
	expectAuditCount(t, f.dsn, "auth.setup", "success", successes)
}

func TestSetupConcurrentHTTPAndRestart(t *testing.T) {
	if dsn := os.Getenv("INTERN_SETUP_PROCESS_PROBE_DSN"); dsn != "" {
		probeCompletedSetupProcess(t, dsn)
		return
	}
	f := newUnconfiguredAuthFixture(t)
	srv := liveSetup(t, f)
	assertSetupState(t, f, true)
	assertSetupRows(t, f, 0, 0, false)
	cookie := concurrentFirstAdmin(t, f)
	assertSetupRows(t, f, 1, 1, true)
	assertSetupState(t, f, false)
	assertAdminSession(t, f, cookie)
	srv.Close()
	restartSetupStore(t, f)
	assertSetupPermanentlyClosed(t, f, cookie)
}

type setupAttemptResult struct {
	response *httptest.ResponseRecorder
	err      error
}

func concurrentFirstAdmin(t *testing.T, f *authFixture) *http.Cookie {
	t.Helper()
	const attempts = 10
	start := make(chan struct{})
	results := make(chan setupAttemptResult, attempts)
	for i := range attempts {
		go func() {
			<-start
			response, err := f.client.roundTrip(apiRequest(http.MethodPost, "/api/v1/auth/setup",
				setupBody(fmt.Sprintf("first-admin-%02d", i)), nil))
			results <- setupAttemptResult{response: response, err: err}
		}()
	}
	close(start)
	var cookie *http.Cookie
	created, conflicts := 0, 0
	for range attempts {
		cookie = acceptSetupReply(t, <-results, cookie, &created, &conflicts)
	}
	if created != 1 || conflicts != attempts-1 {
		t.Fatalf("setup results: created=%d conflicts=%d", created, conflicts)
	}
	return cookie
}

func acceptSetupReply(t *testing.T, reply setupAttemptResult, cookie *http.Cookie, created, conflicts *int) *http.Cookie {
	t.Helper()
	if reply.err != nil {
		t.Fatal(reply.err)
	}
	switch reply.response.Code {
	case http.StatusCreated:
		*created++
		return createdSetupCookie(t, reply.response)
	case http.StatusConflict:
		*conflicts++
		assertSingleError(t, reply.response, render.SetupCompleted)
		return cookie
	default:
		t.Fatalf("valid concurrent setup returned %d: %s", reply.response.Code, reply.response.Body)
		return nil
	}
}

func createdSetupCookie(t *testing.T, response *httptest.ResponseRecorder) *http.Cookie {
	t.Helper()
	user := decodeResponse[userResponse](t, response).User
	if user.Role != "admin" || user.Disabled {
		t.Fatalf("first user=%+v", user)
	}
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || !cookies[0].Secure || !cookies[0].HttpOnly || cookies[0].SameSite != http.SameSiteLaxMode {
		t.Fatal("setup did not establish a secure normal session")
	}
	return cookies[0]
}

func assertAdminSession(t *testing.T, f *authFixture, cookie *http.Cookie) {
	t.Helper()
	current := decodeResponse[sessionResponse](t, f.request(t, http.MethodGet, "/api/v1/auth/session", "", cookie, http.StatusOK))
	if !reflect.DeepEqual(current.Permissions, auth.Permissions("admin")) {
		t.Fatalf("first administrator permissions=%v", current.Permissions)
	}
	f.request(t, http.MethodGet, "/api/v1/users", "", cookie, http.StatusOK)
	f.request(t, http.MethodGet, "/api/v1/users", "", nil, http.StatusUnauthorized)
}

func restartSetupStore(t *testing.T, f *authFixture) {
	t.Helper()
	if err := f.database.Close(); err != nil {
		t.Fatal(err)
	}
	probe := exec.CommandContext(t.Context(), os.Args[0], "-test.run=^TestSetupConcurrentHTTPAndRestart$", "-test.count=1")
	probe.Env = append(os.Environ(), "INTERN_SETUP_PROCESS_PROBE_DSN="+f.dsn)
	if err := probe.Run(); err != nil {
		t.Fatalf("independent process could not verify permanent setup closure: %v", err)
	}
	reopened, err := store.Open(t.Context(), f.dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	f.database = reopened
	f.service, err = auth.New(reopened, auth.DefaultSessionConfig(), auth.PasswordParams{Memory: 64, Time: 1, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	liveSetup(t, f)
}

func assertSetupPermanentlyClosed(t *testing.T, f *authFixture, cookie *http.Cookie) {
	t.Helper()
	assertSetupState(t, f, false)
	assertSingleError(t, f.request(t, http.MethodPost, "/api/v1/auth/setup", setupBody("another-admin"), nil, http.StatusConflict), render.SetupCompleted)
	f.request(t, http.MethodGet, "/api/v1/auth/session", "", cookie, http.StatusOK)
	if _, err := f.database.DB().ExecContext(t.Context(), `UPDATE users SET disabled = 1`); err != nil {
		t.Fatal(err)
	}
	assertSetupState(t, f, false)
	assertSingleError(t, f.request(t, http.MethodPost, "/api/v1/auth/setup", setupBody("disabled-reopen"), nil, http.StatusConflict), render.SetupCompleted)
	f.request(t, http.MethodGet, "/api/v1/auth/session", "", cookie, http.StatusUnauthorized)
	if _, err := f.database.DB().ExecContext(t.Context(), `DELETE FROM users`); err != nil {
		t.Fatal(err)
	}
	assertSetupState(t, f, false)
	assertSingleError(t, f.request(t, http.MethodPost, "/api/v1/auth/setup", setupBody("deleted-reopen"), nil, http.StatusConflict), render.SetupCompleted)
	assertSetupRows(t, f, 0, 1, true)
}

func probeCompletedSetupProcess(t *testing.T, dsn string) {
	t.Helper()
	database, err := store.Open(t.Context(), dsn)
	if err != nil {
		t.Fatal("independent setup probe could not open the control plane")
	}
	t.Cleanup(func() { database.Close() })
	service, err := auth.New(database, auth.DefaultSessionConfig(), auth.PasswordParams{Memory: 64, Time: 1, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	f := &authFixture{database: database, dsn: dsn, service: service}
	liveSetup(t, f)
	assertSetupState(t, f, false)
	assertSingleError(t, f.request(t, http.MethodPost, "/api/v1/auth/setup", setupBody("other-process"), nil, http.StatusConflict), render.SetupCompleted)
	assertSetupRows(t, f, 1, 1, true)
}

func TestSetupAuditRollbackValidationAndCrossOriginHTTP(t *testing.T) {
	f := newUnconfiguredAuthFixture(t)
	liveSetup(t, f)
	for _, body := range []string{
		`{"username":"ab","password":"` + testPassword + `"}`,
		`{"username":"valid-user","password":"short"}`,
	} {
		response := f.invalidRequest(t, http.MethodPost, "/api/v1/auth/setup", body, nil, http.StatusBadRequest)
		assertSingleError(t, response, render.ValidationFailed)
		assertSetupRows(t, f, 0, 0, false)
	}
	unknownField := `{"username":"valid-user","password":"` + testPassword + `","role":"admin"}`
	rejected := f.invalidRequest(t, http.MethodPost, "/api/v1/auth/setup", unknownField, nil, http.StatusBadRequest)
	assertSingleError(t, rejected, render.InvalidJSON)
	assertSetupRows(t, f, 0, 0, false)
	request := apiRequest(http.MethodPost, "/api/v1/auth/setup", setupBody("cross-origin"), nil)
	request.Header.Set("Origin", "https://evil.example")
	assertSingleError(t, requestAPI(t, f.client, request, http.StatusForbidden), render.CrossOriginRejected)
	assertSetupRows(t, f, 0, 0, false)
	setSuccessAuditAbort(t, f.dsn, "auth.setup")
	response := f.request(t, http.MethodPost, "/api/v1/auth/setup", setupBody("first-admin"), nil, http.StatusInternalServerError)
	requireInternal(t, response)
	if len(response.Result().Cookies()) != 0 {
		t.Fatal("failed setup issued a session cookie")
	}
	assertSetupRows(t, f, 0, 0, false)
	assertSetupState(t, f, true)
	setSuccessAuditAbort(t, f.dsn, "")
	f.request(t, http.MethodPost, "/api/v1/auth/setup", setupBody("first-admin"), nil, http.StatusCreated)
	assertSetupRows(t, f, 1, 1, true)
	if strings.Contains(rawAudit(t, f.dsn), testPassword) {
		t.Fatal("setup password leaked into the audit log")
	}
}

func TestExistingDisabledUserClosesSetup(t *testing.T) {
	f := newUnconfiguredAuthFixture(t)
	user := f.seed(t, "existing-viewer", "viewer")
	disabled := true
	if _, err := f.database.UpdateUser(t.Context(), user.ID, store.UserChanges{Disabled: &disabled}, store.SystemAudit("users.update", store.AuditDetail{Disabled: &disabled})); err != nil {
		t.Fatal(err)
	}
	liveSetup(t, f)
	assertSetupState(t, f, false)
	assertSingleError(t, f.request(t, http.MethodPost, "/api/v1/auth/setup", setupBody("first-admin"), nil, http.StatusConflict), render.SetupCompleted)
	expectAuditCount(t, f.dsn, "auth.setup", "success", 0)
}

func TestSetupSessionFailureLeavesCommittedAdministrator(t *testing.T) {
	f := newUnconfiguredAuthFixture(t)
	f.service.Sessions.Store = failingCommitStore{SessionStore: f.database.Sessions()}
	f.request(t, http.MethodPost, "/api/v1/auth/setup", setupBody("first-admin"), nil, http.StatusInternalServerError)
	assertSetupRows(t, f, 1, 1, true)
	assertSetupState(t, f, false)
	f.service.Sessions.Store = f.database.Sessions()
	cookie := f.login(t, "first-admin", testPassword)
	f.request(t, http.MethodGet, "/api/v1/auth/session", "", cookie, http.StatusOK)
}

func TestSetupSharesLoginRateLimitOverHTTP(t *testing.T) {
	f := newUnconfiguredAuthFixture(t)
	liveSetup(t, f)
	for i := range 5 {
		username := "initial"
		if i%2 != 0 {
			username = "INITIAL"
		}
		response := f.invalidRequest(t, http.MethodPost, "/api/v1/auth/setup",
			`{"username":"`+username+`","password":"short"}`, nil, http.StatusBadRequest)
		assertSingleError(t, response, render.ValidationFailed)
	}
	for _, path := range []string{"/api/v1/auth/setup", "/api/v1/auth/login"} {
		request := apiRequest(http.MethodPost, path, setupBody("initial"), nil)
		request.Header.Set("X-Forwarded-For", "198.51.100.100")
		response := requestAPI(t, f.client, request, http.StatusTooManyRequests)
		assertRateLimitResponse(t, response.Header().Get("Retry-After"), response.Body.Bytes())
	}
	assertSetupRows(t, f, 0, 0, false)
	assertSetupState(t, f, true)
}

func TestSetupRejectsOversizedPasswordsAndPayloadsOverHTTP(t *testing.T) {
	f := newUnconfiguredAuthFixture(t)
	liveSetup(t, f)
	password := `{"username":"initial","password":"` + strings.Repeat("密", 257) + `"}`
	response := f.invalidRequest(t, http.MethodPost, "/api/v1/auth/setup", password, nil, http.StatusBadRequest)
	assertSingleError(t, response, render.ValidationFailed)
	payload := `{"username":"initial","password":"` + strings.Repeat("x", int(middleware.MaxRequestBodyBytes)) + `"}`
	response = f.invalidRequest(t, http.MethodPost, "/api/v1/auth/setup", payload, nil, http.StatusRequestEntityTooLarge)
	assertSingleError(t, response, render.PayloadTooLarge)
	assertSetupRows(t, f, 0, 0, false)
	assertSetupState(t, f, true)
}
