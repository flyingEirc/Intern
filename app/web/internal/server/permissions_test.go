package server

import (
	"net/http"
	"strings"
	"testing"

	"github.com/flyingEirc/Intern/app/web/internal/middleware"
)

type permissionCase struct {
	body      string
	admin     int
	operator  int
	viewer    int
	anonymous int
}

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
	}
	fixture := newAuthFixture(t)
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
		for _, role := range []string{"admin", "operator", "viewer", "anonymous"} {
			t.Run(key+"/"+role, func(t *testing.T) { exercisePermission(t, route.Method, route.Path, role, row) })
		}
	}
}

func exercisePermission(t *testing.T, method, path, role string, row permissionCase) {
	t.Helper()
	f := newAuthFixture(t)
	f.seed(t, "loginuser", "viewer")
	target := f.seed(t, "target", "viewer")
	var cookie *http.Cookie
	if role != "anonymous" {
		f.seed(t, "actor", role)
		cookie = f.login(t, "actor", testPassword)
	}
	status := map[string]int{"admin": row.admin, "operator": row.operator, "viewer": row.viewer, "anonymous": row.anonymous}[role]
	f.request(t, method, strings.ReplaceAll(path, ":id", target.ID), row.body, cookie, status)
}
