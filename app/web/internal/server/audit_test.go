package server

import (
	"database/sql"
	"encoding/json/v2"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/app/web/internal/storetest"
)

func TestMutationAuditCoverageAndSecretScan(t *testing.T) {
	f := newAuthFixture(t)
	hash, err := f.service.Passwords.Hash(testPassword)
	if err != nil {
		t.Fatal(err)
	}
	admin := storetest.SeedAdmin(t, f.database, "admin", hash)
	cookie := f.login(t, "admin", testPassword)
	failed := f.request(t, "POST", "/api/v1/auth/login", `{"username":"missing","password":"wrong-private-credential"}`, nil, 401)
	if decodeResponse[render.ErrorResponse](t, failed).Error.Code != render.Unauthorized {
		t.Fatal("wrong login failure code")
	}
	f.request(t, "POST", "/api/v1/users", `{"username":"operator","password":"operator-private-credential","role":"operator"}`, cookie, 201)
	req := apiRequest("PUT", "/api/v1/auth/password", `{"current_password":"`+testPassword+`","new_password":"`+changedPassword+`"}`, cookie)
	req.Header.Set("Authorization", "Bearer private-authorization-marker")
	req.Header.Set(render.RequestIDHeader, "audit-password")
	response := requestAPI(t, f.client, req, 204)
	renewed := response.Result().Cookies()[0]
	f.request(t, "POST", "/api/v1/auth/logout", "", renewed, 204)
	f.request(t, "POST", "/api/v1/auth/logout", "", nil, 401)
	entries, err := f.database.AuditLogs(t.Context(), 0, 200)
	if err != nil {
		t.Fatal(err)
	}
	assertMutationAuditFields(t, entries, admin.ID)
	encoded, err := json.Marshal(entries)
	if err != nil {
		t.Fatal(err)
	}
	all := string(encoded) + rawAudit(t, f.dsn) + f.logs.String()
	for _, secret := range []string{
		testPassword, changedPassword, "wrong-private-credential", "operator-private-credential", "$argon2id$", hash,
		cookie.Value, renewed.Value, cookie.String(), renewed.String(), "private-authorization-marker",
	} {
		if strings.Contains(all, secret) {
			t.Fatal("audit or log output contains an authentication secret")
		}
	}
}

func assertMutationAuditFields(t *testing.T, entries []store.AuditLog, actorID string) {
	t.Helper()
	if len(entries) != 7 {
		t.Fatalf("audit rows=%d, want 7", len(entries))
	}
	assertAnonymousAuditDenial(t, entries[0])
	assertPasswordAudit(t, entries[2], actorID)
	assertFailedLoginAudit(t, entries[4])
	assertSetupAudit(t, entries[6])
}

func assertAnonymousAuditDenial(t *testing.T, entry store.AuditLog) {
	t.Helper()
	if entry.ActorType != "anonymous" || entry.Result != "denied" || *entry.Status != 401 {
		t.Fatal("anonymous denial missing")
	}
}

func assertFailedLoginAudit(t *testing.T, entry store.AuditLog) {
	t.Helper()
	if entry.ActorType != "anonymous" || entry.ActorName != "missing" || entry.Result != "denied" {
		t.Fatal("failed login actor missing")
	}
}

func assertSetupAudit(t *testing.T, entry store.AuditLog) {
	t.Helper()
	if entry.ActorType != "system" || entry.Action != "auth.setup" || entry.Detail.Role != "admin" {
		t.Fatal("browser setup audit missing")
	}
}

func assertPasswordAudit(t *testing.T, entry store.AuditLog, actorID string) {
	t.Helper()
	if entry.Action != "auth.password" || entry.ActorID != actorID || entry.RequestID != "audit-password" || !entry.Detail.PasswordChanged {
		t.Fatal("password-change audit fields missing")
	}
}

// Read every persisted text column, including raw detail_json. A typed response
// alone could hide an accidental extra JSON property while still leaking on disk.
func rawAudit(t *testing.T, dir string) string {
	t.Helper()
	db, err := sql.Open("pgx", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var text string
	err = db.QueryRowContext(t.Context(), `SELECT COALESCE(string_agg(ts || actor_type || actor_id || actor_name || action || resource_type || resource_id || ip || result || COALESCE(status::text, '') || request_id || detail_json, ''), '') FROM audit_logs`).Scan(&text)
	if err != nil {
		t.Fatal(err)
	}
	return text
}

func TestAuditDetailJSONContainsOnlyWhitelistedKeys(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	f.request(t, "POST", "/api/v1/auth/login", `{"username":"missing","password":"wrong-password"}`, nil, 401)
	f.request(t, "POST", "/api/v1/users", `{"username":"viewer1","password":"`+testPassword+`","role":"viewer"}`, cookie, 201)
	f.request(t, "PUT", "/api/v1/auth/password", `{"current_password":"`+testPassword+`","new_password":"`+changedPassword+`"}`, cookie, 204)
	req := apiRequest("POST", "/api/v1/users", `{"username":"other","password":"`+testPassword+`","role":"viewer"}`, cookie)
	req.Header.Set("Origin", "https://evil.example")
	requestAPI(t, f.client, req, 403)
	got := detailJSONList(t, f.dsn)
	want := []string{
		`{"role":"admin"}`,
		`{}`,
		`{}`,
		`{"role":"viewer"}`,
		`{"password_changed":true}`,
		`{"method":"POST","route":"/api/v1/users"}`,
	}
	if len(got) != len(want) {
		t.Fatalf("detail_json=%v", got)
	}
	allowed := map[string]bool{"role": true, "disabled": true, "password_changed": true, "method": true, "route": true}
	for i, raw := range got {
		if raw != want[i] {
			t.Fatalf("detail_json[%d]=%s, want %s", i, raw, want[i])
		}
		var object map[string]any
		if err := json.Unmarshal([]byte(raw), &object); err != nil {
			t.Fatal(err)
		}
		for key := range object {
			if !allowed[key] {
				t.Fatalf("unexpected detail key %s in %s", key, raw)
			}
		}
		if strings.Contains(raw, `"password_changed":false`) {
			t.Fatalf("zero password_changed stored: %s", raw)
		}
	}
}

func detailJSONList(t *testing.T, dir string) []string {
	t.Helper()
	db, err := sql.Open("pgx", dir)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	rows, err := db.QueryContext(t.Context(), `SELECT detail_json FROM audit_logs ORDER BY id`)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var detail string
		if err = rows.Scan(&detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return details
}

func TestLoginHTTPRateLimitAndUntrustedForwardedIP(t *testing.T) {
	f := newAuthFixture(t)
	for i := range 6 {
		name := "missing"
		if i%2 == 1 {
			name = "MISSING"
		}
		req := apiRequest("POST", "/api/v1/auth/login", `{"username":"`+name+`","password":"wrong-private-credential"}`, nil)
		req.Header.Set("X-Forwarded-For", "198.51.100."+strconv.Itoa(i+1))
		status := 401
		if i == 5 {
			status = 429
		}
		response := requestAPI(t, f.client, req, status)
		if status == 429 {
			assertRateLimitResponse(t, response.Header().Get("Retry-After"), response.Body.Bytes())
		}
	}
	entries, err := f.database.AuditLogs(t.Context(), 0, 10)
	if err != nil || len(entries) != 6 || entries[0].IP != "192.0.2.1" || entries[0].Result != "denied" || *entries[0].Status != 429 {
		t.Fatalf("rate-limit audit incorrect: %v", err)
	}
}

func assertRateLimitResponse(t *testing.T, retryAfter string, data []byte) {
	t.Helper()
	retry, err := strconv.Atoi(retryAfter)
	if err != nil || retry < 1 {
		t.Fatal("rate-limit response missing Retry-After")
	}
	var body render.ErrorResponse
	if err = json.Unmarshal(data, &body); err != nil || body.Error.Code != render.RateLimited {
		t.Fatal("rate-limit response error code")
	}
}

// TestLoginHTTPIPLimitAcrossUsernames proves that one IP's login attempts share
// a single bucket whatever username they name. Every attempt names a new
// account, so no per-user bucket can deny it and the 429 can only come from the
// IP bucket: a burst of 30 that gets one token back every two seconds. Tokens
// come back in real time, so which attempt is denied depends on how long the
// requests took on this machine. The test measures that time and asserts exactly
// what a correct bucket allows for it: at least the whole burst, and at most the
// burst plus one token for every refill interval that really passed. On a quick
// machine that is 30 attempts allowed and the 31st denied.
func TestLoginHTTPIPLimitAcrossUsernames(t *testing.T) {
	const (
		burst       = 30
		refill      = 2 * time.Second
		attemptsCap = 300
	)
	f := newAuthFixture(t)
	start := time.Now()
	allowed := 0
	for {
		if allowed >= attemptsCap {
			t.Fatalf("the IP bucket still allowed attempt %d", allowed+1)
		}
		body := `{"username":"missing` + strconv.Itoa(allowed) + `","password":"wrong-password"}`
		response, err := f.client.roundTrip(apiRequest("POST", "/api/v1/auth/login", body, nil))
		if err != nil {
			t.Fatal(err)
		}
		if response.Code == 429 {
			break
		}
		if response.Code != 401 {
			t.Fatalf("attempt %d: status = %d, body = %s; want 401 or 429", allowed+1, response.Code, response.Body)
		}
		allowed++
	}
	elapsed := time.Since(start)
	if most := burst + int(elapsed/refill); allowed < burst || allowed > most {
		t.Fatalf("%d attempts were allowed in %s; a burst of %d that refills one token per %s allows %d to %d",
			allowed, elapsed, burst, refill, burst, most)
	}
}

func TestAuditAttemptedUsernameIsTruncated(t *testing.T) {
	f := newAuthFixture(t)
	f.request(t, "POST", "/api/v1/auth/login", `{"username":"`+strings.Repeat("界", 70)+`","password":"wrong-password"}`, nil, 401)
	entries, err := f.database.AuditLogs(t.Context(), 0, 10)
	if err != nil || len(entries) != 1 || entries[0].ActorName != strings.Repeat("界", 64) {
		t.Fatal("attempted username was not truncated to 64 characters")
	}
}
