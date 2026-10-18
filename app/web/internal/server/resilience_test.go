package server

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

type blockingBody struct {
	rest    io.Reader
	started chan struct{}
	release chan struct{}
	once    sync.Once
}

func (b *blockingBody) Read(p []byte) (int, error) {
	b.once.Do(func() { close(b.started) })
	<-b.release
	return b.rest.Read(p)
}

func (b *blockingBody) Close() error { return nil }

func TestInFlightRequestCannotResurrectLoggedOutSession(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	payload := `{"username":"created","password":"` + testPassword + `","role":"viewer"}`
	started := make(chan struct{})
	release := make(chan struct{})
	var once sync.Once
	defer once.Do(func() { close(release) })
	req := httptest.NewRequest(http.MethodPost, "https://intern.example/api/v1/users", &blockingBody{
		rest: strings.NewReader(payload), started: started, release: release,
	})
	req.Header.Set("Content-Type", "application/json")
	req.Host = "intern.example"
	req.AddCookie(cookie)
	req.ContentLength = int64(len(payload))
	done := make(chan *httptest.ResponseRecorder, 1)
	go func() {
		recorder := httptest.NewRecorder()
		f.client.handler.ServeHTTP(recorder, req)
		done <- recorder
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not reach the handler")
	}
	f.request(t, "POST", "/api/v1/auth/logout", "", cookie, 204)
	once.Do(func() { close(release) })
	var inflight *httptest.ResponseRecorder
	select {
	case inflight = <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("in-flight request did not finish")
	}
	if inflight.Code != http.StatusCreated {
		t.Fatalf("in-flight status=%d body=%s", inflight.Code, inflight.Body)
	}
	f.request(t, "GET", "/api/v1/auth/session", "", cookie, 401)
	assertStoredSessionAbsent(t, f.database.Sessions(), cookie.Value)
	assertLoggedOutAfterRestart(t, f, cookie)
}

func assertStoredSessionAbsent(t *testing.T, sessions *store.SessionStore, token string) {
	t.Helper()
	_, found, err := sessions.Find(token)
	if err != nil || found {
		t.Fatalf("session present: found=%v err=%v", found, err)
	}
}

func assertLoggedOutAfterRestart(t *testing.T, f *authFixture, cookie *http.Cookie) {
	t.Helper()
	if err := f.database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := store.Open(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	assertStoredSessionAbsent(t, reopened.Sessions(), cookie.Value)
	service, err := auth.New(reopened, auth.DefaultSessionConfig(), auth.PasswordParams{Memory: 8, Time: 1, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	engine, err := NewEngine(reopened, slog.New(slog.NewJSONHandler(io.Discard, nil)), nil, service)
	if err != nil {
		t.Fatal(err)
	}
	client := newContractClient(t, Handler(engine))
	requestAPI(t, client, apiRequest("GET", "/api/v1/auth/session", "", cookie), 401)
}

type failingCommitStore struct{ *store.SessionStore }

func (s failingCommitStore) Commit(string, []byte, time.Time) error {
	return errors.New("session store write failed")
}

func (s failingCommitStore) CommitCtx(context.Context, string, []byte, time.Time) error {
	return errors.New("session store write failed")
}

func TestFailedSessionCommitWritesOneLoggedError(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	f.service.Sessions.Store = failingCommitStore{SessionStore: f.database.Sessions()}
	response := f.request(t, "GET", "/api/v1/auth/session", "", cookie, 500)
	assertSingleError(t, response, render.Internal)
	if strings.Contains(response.Body.String(), "permissions") || strings.Contains(response.Body.String(), cookie.Value) {
		t.Fatal("commit failure mixed the handler body or a session token into the response")
	}
	assertErrorLog(t, f.logs.String(), "session persistence failed", "session store write failed")
	assertLoggedRequest(t, f.logs.String(), response.Header().Get(render.RequestIDHeader), 500)
	f.request(t, "GET", "/healthz", "", cookie, 200)
	f.request(t, "GET", "/readyz", "", cookie, 200)
}

func TestCrossOriginRejectionIsRequestLogged(t *testing.T) {
	f := newAuthFixture(t)
	req := apiRequest("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+testPassword+`"}`, nil)
	req.Header.Set("Origin", "https://evil.example")
	req.Header.Set(render.RequestIDHeader, "csrf-log")
	requestAPI(t, f.client, req, 403)
	assertLoggedRequest(t, f.logs.String(), "csrf-log", 403)
	health := apiRequest("POST", "/healthz", "", nil)
	health.Header.Set("Origin", "https://evil.example")
	health.Header.Set(render.RequestIDHeader, "csrf-health")
	requestAPI(t, f.client, health, 403)
	assertLoggedRequest(t, f.logs.String(), "csrf-health", 403)
}

type countingHasher struct {
	auth.PasswordHasher
	dummy int
}

func (c *countingHasher) VerifyDummy(password string) error {
	c.dummy++
	return c.PasswordHasher.VerifyDummy(password)
}

func TestUnknownUserLoginPerformsOneDummyVerification(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	counter := &countingHasher{PasswordHasher: f.service.Passwords}
	f.service.Passwords = counter
	f.request(t, "POST", "/api/v1/auth/login", `{"username":"missing","password":"`+testPassword+`"}`, nil, 401)
	if counter.dummy != 1 {
		t.Fatalf("dummy verifications=%d", counter.dummy)
	}
	f.request(t, "POST", "/api/v1/auth/login", `{"username":"admin","password":"wrong-password-12345"}`, nil, 401)
	if counter.dummy != 1 {
		t.Fatalf("known user used the dummy verifier: %d", counter.dummy)
	}
	f.request(t, "POST", "/api/v1/auth/login", `{"username":"`+strings.Repeat("x", 80)+`","password":"`+testPassword+`"}`, nil, 401)
	if counter.dummy != 2 {
		t.Fatalf("malformed username dummy verifications=%d", counter.dummy)
	}
}

type unavailableHasher struct{ auth.PasswordHasher }

func (unavailableHasher) Verify(string, string) (bool, error) {
	return false, auth.ErrUnavailable
}
func (unavailableHasher) VerifyDummy(string) error { return auth.ErrUnavailable }
func (unavailableHasher) Hash(string) (string, error) {
	return "", auth.ErrUnavailable
}

func TestSaturatedHasherReturnsUnavailable(t *testing.T) {
	f := newAuthFixture(t)
	f.service.Passwords = unavailableHasher{PasswordHasher: f.service.Passwords}
	start := time.Now()
	response := f.request(t, "POST", "/api/v1/auth/login", `{"username":"missing","password":"`+testPassword+`"}`, nil, 503)
	if time.Since(start) > time.Second {
		t.Fatalf("saturated login took %s", time.Since(start))
	}
	assertSingleError(t, response, render.Unavailable)
	if response.Header().Get("Retry-After") != "1" {
		t.Fatalf("Retry-After=%q", response.Header().Get("Retry-After"))
	}
}

func TestCurrentPasswordVerificationIsRateLimited(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "viewer", "viewer")
	cookie := f.login(t, "viewer", testPassword)
	body := `{"current_password":"wrong-password","new_password":"` + changedPassword + `"}`
	for range 5 {
		response := f.request(t, "PUT", "/api/v1/auth/password", body, cookie, 400)
		if decodeResponse[render.ErrorResponse](t, response).Error.Code != render.ValidationFailed {
			t.Fatal(response.Body)
		}
	}
	response := f.request(t, "PUT", "/api/v1/auth/password", body, cookie, 429)
	assertRateLimitResponse(t, response.Header().Get("Retry-After"), response.Body.Bytes())
}

func assertSingleError(t *testing.T, response *httptest.ResponseRecorder, code string) {
	t.Helper()
	var body render.ErrorResponse
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if body.Error.Code != code || body.Error.RequestID == "" {
		t.Fatalf("error body=%s", response.Body)
	}
	remainder := strings.TrimPrefix(response.Body.String(), strings.TrimSpace(response.Body.String()))
	if remainder != "" {
		t.Fatal("trailing response bytes")
	}
	if strings.Count(response.Body.String(), `"code"`) != 1 {
		t.Fatalf("response was not a single error: %s", response.Body)
	}
}

func assertErrorLog(t *testing.T, raw, message, errorText string) {
	t.Helper()
	for _, record := range logRecords(t, raw) {
		if record["msg"] != message {
			continue
		}
		if record["error"] != errorText {
			t.Fatalf("error attribute=%v", record["error"])
		}
		return
	}
	t.Fatalf("missing %s log: %s", message, raw)
}

func assertLoggedRequest(t *testing.T, raw, requestID string, status int) {
	t.Helper()
	for _, record := range logRecords(t, raw) {
		if record["msg"] != "request complete" || record["request_id"] != requestID {
			continue
		}
		got, ok := record["status"].(float64)
		if !ok || int(got) != status {
			t.Fatalf("request log status=%v", record["status"])
		}
		return
	}
	t.Fatalf("missing request log %s status %d: %s", requestID, status, raw)
}

func logRecords(t *testing.T, raw string) []map[string]any {
	t.Helper()
	var records []map[string]any
	for line := range strings.SplitSeq(raw, "\n") {
		if line == "" {
			continue
		}
		var record map[string]any
		if err := json.Unmarshal([]byte(line), &record); err != nil {
			t.Fatalf("log line: %s", line)
		}
		records = append(records, record)
	}
	return records
}
