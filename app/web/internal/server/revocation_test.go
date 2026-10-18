package server

import (
	"encoding/json/v2"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

func TestAdministrativeChangesRevokeEveryExistingSession(t *testing.T) {
	for _, operation := range []struct{ name, method, suffix, body string }{
		{"revoke", "DELETE", "/sessions", ""},
		{"disable", "PATCH", "", `{"disabled":true}`},
		{"role", "PATCH", "", `{"role":"operator"}`},
		{"reset password", "PATCH", "", `{"password":"` + changedPassword + `"}`},
	} {
		t.Run(operation.name, func(t *testing.T) {
			f := newAuthFixture(t)
			f.seed(t, "admin", "admin")
			target := f.seed(t, "target", "viewer")
			admin := f.login(t, "admin", testPassword)
			first := f.login(t, "target", testPassword)
			second := f.login(t, "target", testPassword)
			status := 200
			if operation.method == "DELETE" {
				status = 204
			}
			f.request(t, operation.method, "/api/v1/users/"+target.ID+operation.suffix, operation.body, admin, status)
			f.request(t, "GET", "/api/v1/auth/session", "", first, 401)
			f.request(t, "GET", "/api/v1/auth/session", "", second, 401)
			f.request(t, "GET", "/api/v1/auth/session", "", admin, 200)
		})
	}
}

func TestOwnPasswordChangeRetainsOnlyRenewedCurrentSession(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "viewer", "viewer")
	first := f.login(t, "viewer", testPassword)
	second := f.login(t, "viewer", testPassword)
	wrong := f.request(t, "PUT", "/api/v1/auth/password", `{"current_password":"wrong-password","new_password":"`+changedPassword+`"}`, first, 400)
	if decodeResponse[render.ErrorResponse](t, wrong).Error.Code != render.ValidationFailed {
		t.Fatal("wrong current password is not a validation failure")
	}
	f.request(t, "GET", "/api/v1/auth/session", "", first, 200)
	f.invalidRequest(t, "PUT", "/api/v1/auth/password", `{"current_password":"`+testPassword+`","new_password":"short"}`, first, 400)
	response := f.request(t, "PUT", "/api/v1/auth/password", `{"current_password":"`+testPassword+`","new_password":"`+changedPassword+`"}`, first, 204)
	cookies := response.Result().Cookies()
	if len(cookies) != 1 || cookies[0].Value == first.Value {
		t.Fatal("own password change did not renew token")
	}
	f.request(t, "GET", "/api/v1/auth/session", "", first, 401)
	f.request(t, "GET", "/api/v1/auth/session", "", second, 401)
	f.request(t, "GET", "/api/v1/auth/session", "", cookies[0], 200)
	f.request(t, "POST", "/api/v1/auth/login", `{"username":"viewer","password":"`+testPassword+`"}`, nil, 401)
	f.login(t, "viewer", changedPassword)
}

func TestUserPolicyConflictAndLastAdministratorAPI(t *testing.T) {
	f := newAuthFixture(t)
	admin := f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	for _, body := range []string{`{"disabled":true}`, `{"role":"viewer"}`} {
		f.request(t, "PATCH", "/api/v1/users/"+admin.ID, body, cookie, 409)
	}
	f.request(t, "POST", "/api/v1/users", `{"username":"ADMIN","password":"`+testPassword+`","role":"viewer"}`, cookie, 409)
	for _, body := range []string{
		`{"username":"ab","password":"` + testPassword + `","role":"viewer"}`,
		`{"username":"with space","password":"` + testPassword + `","role":"viewer"}`,
		`{"username":"valid","password":"short","role":"viewer"}`,
		`{"username":"valid","password":"` + testPassword + `","role":"owner"}`,
	} {
		f.invalidRequest(t, "POST", "/api/v1/users", body, cookie, 400)
	}
	for _, body := range []string{`{}`, `{"role":"owner"}`, `{"password":"short"}`} {
		f.invalidRequest(t, "PATCH", "/api/v1/users/"+admin.ID, body, cookie, 400)
	}
	f.request(t, "GET", "/api/v1/users/missing", "", cookie, 404)
	f.request(t, "PATCH", "/api/v1/users/missing", `{"role":"viewer"}`, cookie, 404)
	f.request(t, "DELETE", "/api/v1/users/missing/sessions", "", cookie, 404)
	response := f.request(t, "POST", "/api/v1/users", `{"username":"unicode","password":"`+strings.Repeat("密", 12)+`","role":"viewer"}`, cookie, 201)
	if strings.Contains(response.Body.String(), "password") || strings.Contains(response.Body.String(), "epoch") {
		t.Fatal("user response contains authentication internals")
	}
}

func TestSessionsSurviveServiceRestart(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "viewer", "viewer")
	cookie := f.login(t, "viewer", testPassword)
	if err := f.database.Close(); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(t.Context(), f.dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	service, err := auth.New(database, auth.DefaultSessionConfig(), auth.PasswordParams{Memory: 64, Time: 1, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	logger := slog.New(slog.NewJSONHandler(&f.logs, nil))
	engine, err := NewEngine(database, logger, nil, service)
	if err != nil {
		t.Fatal(err)
	}
	f.client = newContractClient(t, Handler(engine))
	f.request(t, "GET", "/api/v1/auth/session", "", cookie, 200)
}

func TestSessionIdleExpiry(t *testing.T) {
	f := newAuthFixture(t)
	f.service.Sessions.IdleTimeout = 20 * time.Millisecond
	f.seed(t, "viewer", "viewer")
	cookie := f.login(t, "viewer", testPassword)
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		_, found, err := f.database.Sessions().Find(cookie.Value)
		if err != nil {
			t.Fatal(err)
		}
		if !found {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("session idle expiry not enforced")
		case <-ticker.C:
		}
	}
	f.request(t, "GET", "/api/v1/auth/session", "", cookie, 401)
}

func TestSessionAbsoluteExpiryDespiteActivity(t *testing.T) {
	f := newAuthFixture(t)
	f.service.Sessions.IdleTimeout, f.service.Sessions.Lifetime = 200*time.Millisecond, 400*time.Millisecond
	f.seed(t, "viewer", "viewer")
	cookie := f.login(t, "viewer", testPassword)
	start := time.Now()
	deadline := time.NewTimer(2 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		response, err := f.client.roundTrip(apiRequest("GET", "/api/v1/auth/session", "", cookie))
		if err != nil {
			t.Fatal(err)
		}
		if response.Code == 401 {
			if time.Since(start) < 300*time.Millisecond {
				t.Fatal("activity did not extend idle timeout")
			}
			break
		}
		if response.Code != 200 {
			t.Fatal("unexpected session response")
		}
		select {
		case <-deadline.C:
			t.Fatal("absolute lifetime not enforced")
		case <-ticker.C:
		}
	}
}

func TestAuditPaginationBounds(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	f.login(t, "admin", testPassword)
	page := decodeResponse[auditPage](t, f.request(t, "GET", "/api/v1/audit-logs?limit=1", "", cookie, 200))
	if len(page.Items) != 1 || page.NextBefore == nil {
		t.Fatal("audit pagination missing cursor")
	}
	cursor, err := json.Marshal(page.NextBefore)
	if err != nil {
		t.Fatal(err)
	}
	next := decodeResponse[auditPage](t, f.request(t, "GET", "/api/v1/audit-logs?limit=1&before="+string(cursor), "", cookie, 200))
	if len(next.Items) != 1 || next.Items[0].ID >= page.Items[0].ID || next.NextBefore != nil {
		t.Fatal("audit cursor order or termination")
	}
	for _, query := range []string{"limit=0", "limit=201", "limit=no", "before=0", "before=-1", "before=no"} {
		f.invalidRequest(t, "GET", "/api/v1/audit-logs?"+query, "", cookie, 400)
	}
}
