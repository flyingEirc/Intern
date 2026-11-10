package server

import (
	"log/slog"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/alexedwards/scs/v2"

	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

type sessionFindSpy struct {
	scs.Store
	finds int
}

func (s *sessionFindSpy) Find(token string) ([]byte, bool, error) {
	s.finds++
	return s.Store.Find(token)
}

func TestCrossOriginAuditExactlyOnceWithoutSessionLoading(t *testing.T) {
	for _, proxies := range [][]string{nil, {"192.0.2.0/24"}} {
		f := newAuthFixture(t)
		logger := slog.New(slog.NewJSONHandler(&f.logs, nil))
		engine, err := NewEngine(f.database, logger, proxies, f.service)
		if err != nil {
			t.Fatal(err)
		}
		spy := &sessionFindSpy{Store: f.service.Sessions.Store}
		f.service.Sessions.Store = spy
		f.client = newContractClient(t, Handler(engine))
		req := apiRequest("POST", "/api/v1/users?password=csrf-query-secret", `{"username":"newuser","password":"csrf-body-secret-credential","role":"viewer"}`, &http.Cookie{Name: "intern_session", Value: "csrf-cookie-secret"})
		req.Header.Set("Origin", "https://evil.example")
		req.Header.Set("X-Forwarded-For", "198.51.100.9")
		req.Header.Set(render.RequestIDHeader, "csrf-audit")
		response := requestAPI(t, f.client, req, 403)
		if spy.finds != 0 {
			t.Fatal("cross-origin rejection loaded a session")
		}
		body := decodeResponse[render.ErrorResponse](t, response)
		entries, err := f.database.AuditLogs(t.Context(), 0, 10)
		if err != nil {
			t.Fatal(err)
		}
		ip := "192.0.2.1"
		if len(proxies) != 0 {
			ip = "198.51.100.9"
		}
		assertCrossOriginAudit(t, entries, body.Error.RequestID, ip)
		all := rawAudit(t, f.dsn) + f.logs.String()
		for _, secret := range []string{"csrf-query-secret", "csrf-body-secret-credential", "csrf-cookie-secret", "https://evil.example"} {
			if strings.Contains(all, secret) {
				t.Fatal("cross-origin audit leaked request data")
			}
		}
	}
}

func assertCrossOriginAudit(t *testing.T, entries []store.AuditLog, requestID, ip string) {
	t.Helper()
	if len(entries) != 1 {
		t.Fatalf("cross-origin audit rows=%d", len(entries))
	}
	entry := entries[0]
	if entry.ActorType != "anonymous" || entry.Result != "denied" || *entry.Status != 403 {
		t.Fatal("cross-origin audit classification")
	}
	if entry.Action != "security.cross_origin_rejected" || entry.RequestID != requestID || entry.IP != ip {
		t.Fatal("cross-origin audit identity")
	}
	if !reflect.DeepEqual(entry.Detail, store.AuditDetail{Method: "POST", Route: "/api/v1/users"}) {
		t.Fatal("cross-origin audit detail not whitelisted")
	}
}

func TestCrossOriginAuditScopeAndSameOriginNoDuplicate(t *testing.T) {
	f := newAuthFixture(t)
	for _, path := range []string{"/healthz", "/other/private-path?secret=csrf-query-secret"} {
		req := apiRequest("POST", path, "", nil)
		req.Header.Set("Origin", "https://evil.example")
		requestAPI(t, f.client, req, 403)
	}
	entries, err := f.database.AuditLogs(t.Context(), 0, 10)
	if err != nil || len(entries) != 0 {
		t.Fatal("non-API cross-origin request audited")
	}
	req := apiRequest("POST", "/api/v1/users", `{"username":"newuser","password":"csrf-body-secret-credential","role":"viewer"}`, nil)
	req.Header.Set("Origin", "https://intern.example")
	requestAPI(t, f.client, req, 401)
	entries, err = f.database.AuditLogs(t.Context(), 0, 10)
	if err != nil || len(entries) != 1 || entries[0].Action != "users.create" {
		t.Fatal("same-origin audit missing or duplicated")
	}
	req = apiRequest("POST", "/api/v1/unknown-private?secret=csrf-query-secret", "", nil)
	req.Header.Set("Sec-Fetch-Site", "cross-site")
	requestAPI(t, f.client, req, 403)
	entries, err = f.database.AuditLogs(t.Context(), 0, 10)
	if err != nil || len(entries) != 2 || entries[0].Detail.Route != "unmatched" {
		t.Fatal("unknown cross-origin route template leaked")
	}
}
