package server

import (
	"encoding/json/v2"
	"log/slog"
	"net/http"
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
	admin := f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	f.login(t, "admin", testPassword)
	// Seed writes one system users.create row. Each successful login writes one
	// auth.login row. The ordered set comes from one unpaged read, not the cursor.
	expected, err := f.database.AuditLogs(t.Context(), 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(expected) != 3 {
		t.Fatalf("fixture audit rows=%d, want 3", len(expected))
	}
	want := assertFixtureAuditSet(t, admin, expected)
	got, seen := collectAuditPages(t, f, cookie, expected, want)
	assertPaginatedAuditIDs(t, expected, want, got, seen)
	for _, query := range []string{"limit=0", "limit=201", "limit=no", "before=0", "before=-1", "before=no"} {
		f.invalidRequest(t, "GET", "/api/v1/audit-logs?"+query, "", cookie, 400)
	}
}

func assertFixtureAuditSet(t *testing.T, admin store.User, expected []store.AuditLog) map[int64]store.AuditLog {
	t.Helper()
	want := map[int64]store.AuditLog{}
	creates, logins := 0, 0
	for i, entry := range expected {
		assertFixtureAuditID(t, want, expected, i, entry)
		want[entry.ID] = entry
		switch entry.Action {
		case "users.create":
			creates++
			assertSeedCreateAudit(t, admin, entry)
		case "auth.login":
			logins++
			assertSeedLoginAudit(t, admin, entry)
		default:
			t.Fatalf("unexpected fixture audit %s", entry.Action)
		}
	}
	if creates != 1 || logins != 2 {
		t.Fatalf("fixture audits creates=%d logins=%d", creates, logins)
	}
	return want
}

func assertFixtureAuditID(t *testing.T, want map[int64]store.AuditLog, expected []store.AuditLog, i int, entry store.AuditLog) {
	t.Helper()
	if _, ok := want[entry.ID]; ok || entry.ID < 1 {
		t.Fatalf("fixture audit id %d", entry.ID)
	}
	if i > 0 && entry.ID >= expected[i-1].ID {
		t.Fatalf("fixture audit ids not strictly decreasing: %d then %d", expected[i-1].ID, entry.ID)
	}
}

func assertSeedCreateAudit(t *testing.T, admin store.User, entry store.AuditLog) {
	t.Helper()
	if entry.ActorType != "system" || entry.Result != "success" || entry.ResourceID != admin.ID {
		t.Fatalf("seed audit: %+v", entry)
	}
}

func assertSeedLoginAudit(t *testing.T, admin store.User, entry store.AuditLog) {
	t.Helper()
	if entry.Result != "success" || entry.ActorID != admin.ID {
		t.Fatalf("login audit: %+v", entry)
	}
}

func collectAuditPages(t *testing.T, f *authFixture, cookie *http.Cookie, expected []store.AuditLog, want map[int64]store.AuditLog) ([]int64, map[int64]struct{}) {
	t.Helper()
	seen := map[int64]struct{}{}
	var got []int64
	var before string
	for pageNumber := 0; ; pageNumber++ {
		if pageNumber >= len(expected) {
			t.Fatal("audit pagination did not terminate")
		}
		page := decodeResponse[auditPage](t, f.request(t, "GET", auditPagePath(before), "", cookie, 200))
		id := acceptAuditPageItem(t, page, pageNumber, seen, want, got)
		seen[id] = struct{}{}
		got = append(got, id)
		if page.NextBefore == nil {
			break
		}
		before = auditCursor(t, page.NextBefore, id)
	}
	return got, seen
}

func auditPagePath(before string) string {
	path := "/api/v1/audit-logs?limit=1"
	if before != "" {
		path += "&before=" + before
	}
	return path
}

func acceptAuditPageItem(t *testing.T, page auditPage, pageNumber int, seen map[int64]struct{}, want map[int64]store.AuditLog, got []int64) int64 {
	t.Helper()
	if len(page.Items) != 1 {
		t.Fatalf("page %d items=%d", pageNumber, len(page.Items))
	}
	id := page.Items[0].ID
	if _, ok := seen[id]; ok {
		t.Fatalf("duplicate audit id %d", id)
	}
	if _, ok := want[id]; !ok {
		t.Fatalf("page returned audit id %d outside the fixture set", id)
	}
	if len(got) > 0 && id >= got[len(got)-1] {
		t.Fatalf("audit ids not strictly decreasing: %d then %d", got[len(got)-1], id)
	}
	return id
}

func auditCursor(t *testing.T, next *int64, id int64) string {
	t.Helper()
	if *next != id {
		t.Fatalf("next_before=%d, last item=%d", *next, id)
	}
	cursor, err := json.Marshal(next)
	if err != nil {
		t.Fatal(err)
	}
	return string(cursor)
}

func assertPaginatedAuditIDs(t *testing.T, expected []store.AuditLog, want map[int64]store.AuditLog, got []int64, seen map[int64]struct{}) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("paginated ids=%v, fixture ids missing or extra", got)
	}
	for i, id := range got {
		if id != expected[i].ID {
			t.Fatalf("page order id=%d, fixture id=%d", id, expected[i].ID)
		}
	}
	for id := range want {
		if _, ok := seen[id]; !ok {
			t.Fatalf("audit id %d omitted", id)
		}
	}
}
