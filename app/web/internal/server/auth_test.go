package server

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/credential"
)

const (
	testPassword    = "test-only-password-12345"
	changedPassword = "changed-only-password-12345"
)

type authFixture struct {
	database *store.Store
	dir      string
	keys     string
	keyFile  string
	service  *auth.Service
	engine   *gin.Engine
	client   *contractClient
	sealer   *credential.Sealer
	logs     bytes.Buffer
}

// newAuthFixture serves with a real KEK so source and FTP repository writes can
// seal a secret. The physical preflight inputs stay absent, which is the
// deployment-independent default: those endpoints answer 503.
func newAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	fixture := newUnconfiguredAuthFixture(t)
	settings := fixture.resourceSettings(t)
	fixture.sealer = settings.Sealer
	engine, err := NewEngineWithResources(fixture.database, slog.New(slog.NewJSONHandler(&fixture.logs, nil)), nil, fixture.service,
		&resources.Service{Store: fixture.database, Settings: settings}, nil)
	if err != nil {
		t.Fatal(err)
	}
	fixture.engine = engine
	fixture.client = newContractClient(t, Handler(engine))
	return fixture
}

// newUnconfiguredAuthFixture serves with no KEK and no preflight settings.
func newUnconfiguredAuthFixture(t *testing.T) *authFixture {
	t.Helper()
	fixture := &authFixture{dir: filepath.Join(t.TempDir(), "metadata"), keys: t.TempDir()}
	database, err := store.Open(t.Context(), fixture.dir)
	if err != nil {
		t.Fatal(err)
	}
	fixture.database = database
	t.Cleanup(func() { database.Close() })
	fixture.service, err = auth.New(database, auth.DefaultSessionConfig(), auth.PasswordParams{Memory: 64, Time: 1, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(&fixture.logs, nil))
	fixture.engine, err = NewEngine(database, logger, nil, fixture.service)
	if err != nil {
		t.Fatal(err)
	}
	fixture.client = newContractClient(t, Handler(fixture.engine))
	stop := fixture.service.StartCleanup(t.Context(), time.Hour, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	t.Cleanup(stop)
	return fixture
}

// resourceSettings generates a KEK for this fixture and returns settings that
// seal secrets but cannot run the physical preflight.
func (f *authFixture) resourceSettings(t *testing.T) resources.Settings {
	t.Helper()
	keyFile := filepath.Join(f.keys, "kek.json")
	if err := artifact.GenerateKey(keyFile); err != nil {
		t.Fatal(err)
	}
	sealer, err := credential.Open(keyFile, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.keyFile = keyFile
	return resources.Settings{Sealer: sealer, KeyFile: keyFile, MetadataDir: f.dir}
}

func (f *authFixture) seed(t *testing.T, username, role string) store.User {
	t.Helper()
	hash, err := f.service.Passwords.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	user, err := f.database.CreateUser(t.Context(), username, hash, role, store.SystemAudit("users.create", store.AuditDetail{Role: role}))
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func apiRequest(method, path, body string, cookie *http.Cookie) *http.Request {
	req := httptest.NewRequest(method, "https://intern.example"+path, strings.NewReader(body))
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	return req
}

func (f *authFixture) request(t *testing.T, method, path, body string, cookie *http.Cookie, status int) *httptest.ResponseRecorder {
	t.Helper()
	return requestAPI(t, f.client, apiRequest(method, path, body, cookie), status)
}

func (f *authFixture) invalidRequest(t *testing.T, method, path, body string, cookie *http.Cookie, status int) *httptest.ResponseRecorder {
	t.Helper()
	response, err := f.client.roundTripRequest(apiRequest(method, path, body, cookie), true)
	if err != nil {
		t.Fatal(err)
	}
	if response.Code != status {
		t.Fatalf("status=%d, want=%d; %s", response.Code, status, response.Body)
	}
	return response
}

func (f *authFixture) login(t *testing.T, username, password string) *http.Cookie {
	t.Helper()
	body, err := json.Marshal(loginRequest{Username: username, Password: password})
	if err != nil {
		t.Fatal(err)
	}
	response := f.request(t, "POST", "/api/v1/auth/login", string(body), nil, 200)
	cookies := response.Result().Cookies()
	if len(cookies) != 1 {
		t.Fatalf("session cookies=%d", len(cookies))
	}
	cookie := cookies[0]
	if cookie.Name != "intern_session" || !cookie.Secure || !cookie.HttpOnly || cookie.SameSite != http.SameSiteLaxMode || cookie.Path != "/" {
		t.Fatalf("unsafe cookie configuration: %s", cookie.Name)
	}
	return cookie
}

func decodeResponse[T any](t *testing.T, response *httptest.ResponseRecorder) T {
	t.Helper()
	var value T
	if err := json.Unmarshal(response.Body.Bytes(), &value); err != nil {
		t.Fatal(err)
	}
	return value
}

func TestLoginFailuresAreIndistinguishableAndUnknownUserUsesDummy(t *testing.T) {
	f := newAuthFixture(t)
	user := f.seed(t, "disabled", "viewer")
	disabled := true
	if _, err := f.database.UpdateUser(t.Context(), user.ID, store.UserChanges{Disabled: &disabled}, store.SystemAudit("users.update", store.AuditDetail{Disabled: &disabled})); err != nil {
		t.Fatal(err)
	}
	for _, username := range []string{"missing", "disabled"} {
		for _, password := range []string{testPassword, "wrong-password-12345"} {
			data, err := json.Marshal(loginRequest{Username: username, Password: password})
			if err != nil {
				t.Fatal(err)
			}
			response := f.request(t, "POST", "/api/v1/auth/login", string(data), nil, 401)
			body := decodeResponse[render.ErrorResponse](t, response)
			if body.Error.Code != render.Unauthorized || body.Error.Message != "Invalid username or password" {
				t.Fatal("login reveals account state")
			}
		}
	}
}

func TestSessionTokenRenewalLogoutAndCurrentPermissions(t *testing.T) {
	f := newAuthFixture(t)
	user := f.seed(t, "admin", "admin")
	cookie := f.login(t, "ADMIN", testPassword)
	response := f.request(t, "GET", "/api/v1/auth/session", "", cookie, 200)
	current := decodeResponse[sessionResponse](t, response)
	if current.User.ID != user.ID || !reflect.DeepEqual(current.Permissions, auth.Permissions("admin")) || strings.Contains(response.Body.String(), "password_hash") {
		t.Fatal("invalid session response")
	}
	body, err := json.Marshal(loginRequest{Username: "admin", Password: testPassword})
	if err != nil {
		t.Fatal(err)
	}
	renewed := f.request(t, "POST", "/api/v1/auth/login", string(body), cookie, 200).Result().Cookies()[0]
	if renewed.Value == cookie.Value {
		t.Fatal("login did not renew session token")
	}
	f.request(t, "GET", "/api/v1/auth/session", "", cookie, 401)
	f.request(t, "POST", "/api/v1/auth/logout", "", renewed, 204)
	f.request(t, "GET", "/api/v1/auth/session", "", renewed, 401)
	if _, found, err := f.database.Sessions().Find(renewed.Value); err != nil || found {
		t.Fatalf("logout session retained: %v, %v", found, err)
	}
}

func TestCrossOriginProtectionAndNonBrowserRequests(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	for _, header := range []string{"Origin", "Sec-Fetch-Site"} {
		req := apiRequest("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+testPassword+`"}`, nil)
		if header == "Origin" {
			req.Header.Set(header, "https://evil.example")
		} else {
			req.Header.Set(header, "cross-site")
		}
		req.Header.Set(render.RequestIDHeader, "csrf-test")
		response := requestAPI(t, f.client, req, 403)
		body := decodeResponse[render.ErrorResponse](t, response)
		if body.Error.Code != render.CrossOriginRejected || body.Error.RequestID != "csrf-test" {
			t.Fatal("CSRF response format")
		}
		if len(response.Result().Cookies()) != 0 {
			t.Fatal("CSRF rejection loaded a session")
		}
	}
	req := apiRequest("POST", "/api/v1/auth/login", `{"username":"admin","password":"`+testPassword+`"}`, nil)
	req.Header.Set("Origin", "https://intern.example")
	req.Header.Set("Sec-Fetch-Site", "same-origin")
	requestAPI(t, f.client, req, 200)
	f.login(t, "admin", testPassword)
}
