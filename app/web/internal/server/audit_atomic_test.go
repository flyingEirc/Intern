package server

import (
	"database/sql"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

func TestSecurityMutationsRollBackWhenSuccessAuditFails(t *testing.T) {
	f := newAuthFixture(t)
	admin := f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	assertLastAdminDisableConflicts(t, f, admin, cookie)
	operator := assertCreateRollsBackWithoutSuccessAudit(t, f, cookie)
	operator = assertUpdateRollsBackWithoutSuccessAudit(t, f, operator, cookie)
	assertRevokeRollsBackWithoutSuccessAudit(t, f, operator, cookie)
	cookie = assertPasswordRollsBackWithoutSuccessAudit(t, f, admin, cookie)
	assertLogoutRollsBackWithoutSuccessAudit(t, f, cookie)
	assertAuditsAndLogsOmitPasswords(t, f)
}

func assertLastAdminDisableConflicts(t *testing.T, f *authFixture, admin store.User, cookie *http.Cookie) {
	t.Helper()
	f.request(t, "PATCH", "/api/v1/users/"+admin.ID, `{"disabled":true}`, cookie, http.StatusConflict)
	expectAuditCount(t, f.dir, "users.update", "success", 0)
	reloaded, err := f.database.User(t.Context(), admin.ID)
	if err != nil || reloaded.Disabled || reloaded.Role != "admin" {
		t.Fatalf("last admin changed: %+v %v", reloaded, err)
	}
}

func assertCreateRollsBackWithoutSuccessAudit(t *testing.T, f *authFixture, cookie *http.Cookie) store.User {
	t.Helper()
	setSuccessAuditAbort(t, f.dir, "users.create")
	failedCreate := countAudit(t, f.dir, "users.create", "failed")
	response := f.request(t, "POST", "/api/v1/users", `{"username":"operator","password":"`+changedPassword+`","role":"operator"}`, cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	if _, err := f.database.UserByUsername(t.Context(), "operator"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("create persisted without success audit: %v", err)
	}
	expectAuditCount(t, f.dir, "users.create", "success", 1)
	expectAuditCount(t, f.dir, "users.create", "failed", failedCreate+1)
	setSuccessAuditAbort(t, f.dir, "")
	f.request(t, "POST", "/api/v1/users", `{"username":"operator","password":"`+changedPassword+`","role":"operator"}`, cookie, http.StatusCreated)
	expectAuditCount(t, f.dir, "users.create", "success", 2)
	operator := requireUser(t, f, "operator")
	if operator.Role != "operator" {
		t.Fatal(operator.Role)
	}
	return operator
}

func assertUpdateRollsBackWithoutSuccessAudit(t *testing.T, f *authFixture, operator store.User, cookie *http.Cookie) store.User {
	t.Helper()
	setSuccessAuditAbort(t, f.dir, "users.update")
	failedUpdate := countAudit(t, f.dir, "users.update", "failed")
	response := f.request(t, "PATCH", "/api/v1/users/"+operator.ID, `{"role":"viewer"}`, cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	operator = requireUser(t, f, "operator")
	if operator.Role != "operator" {
		t.Fatalf("role changed without success audit: %s", operator.Role)
	}
	expectAuditCount(t, f.dir, "users.update", "success", 0)
	expectAuditCount(t, f.dir, "users.update", "failed", failedUpdate+1)
	setSuccessAuditAbort(t, f.dir, "")
	f.request(t, "PATCH", "/api/v1/users/"+operator.ID, `{"role":"viewer"}`, cookie, http.StatusOK)
	expectAuditCount(t, f.dir, "users.update", "success", 1)
	if requireUser(t, f, "operator").Role != "viewer" {
		t.Fatal("role was not updated")
	}
	return operator
}

func assertRevokeRollsBackWithoutSuccessAudit(t *testing.T, f *authFixture, operator store.User, cookie *http.Cookie) {
	t.Helper()
	setSuccessAuditAbort(t, f.dir, "users.revoke_sessions")
	failedRevoke := countAudit(t, f.dir, "users.revoke_sessions", "failed")
	epoch := requireUser(t, f, "operator").SessionEpoch
	response := f.request(t, "DELETE", "/api/v1/users/"+operator.ID+"/sessions", "", cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	if got := requireUser(t, f, "operator").SessionEpoch; got != epoch {
		t.Fatalf("epoch changed without success audit: %d to %d", epoch, got)
	}
	expectAuditCount(t, f.dir, "users.revoke_sessions", "success", 0)
	expectAuditCount(t, f.dir, "users.revoke_sessions", "failed", failedRevoke+1)
	setSuccessAuditAbort(t, f.dir, "")
	f.request(t, "DELETE", "/api/v1/users/"+operator.ID+"/sessions", "", cookie, http.StatusNoContent)
	expectAuditCount(t, f.dir, "users.revoke_sessions", "success", 1)
	if got := requireUser(t, f, "operator").SessionEpoch; got != epoch+1 {
		t.Fatalf("epoch=%d, want %d", got, epoch+1)
	}
}

func assertPasswordRollsBackWithoutSuccessAudit(t *testing.T, f *authFixture, admin store.User, cookie *http.Cookie) *http.Cookie {
	t.Helper()
	beforePassword := assertPasswordChangeRolledBack(t, f, admin, cookie)
	return assertPasswordChangeCommits(t, f, admin, beforePassword, cookie)
}

func assertPasswordChangeRolledBack(t *testing.T, f *authFixture, admin store.User, cookie *http.Cookie) store.User {
	t.Helper()
	beforePassword, err := f.database.User(t.Context(), admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	setSuccessAuditAbort(t, f.dir, "auth.password")
	failedPassword := countAudit(t, f.dir, "auth.password", "failed")
	passwordBody := `{"current_password":"` + testPassword + `","new_password":"` + changedPassword + `"}`
	response := f.request(t, "PUT", "/api/v1/auth/password", passwordBody, cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	if len(response.Result().Cookies()) != 0 {
		t.Fatal("failed password change issued a cookie")
	}
	afterPassword, err := f.database.User(t.Context(), admin.ID)
	if err != nil {
		t.Fatal(err)
	}
	if afterPassword.PasswordHash != beforePassword.PasswordHash || afterPassword.SessionEpoch != beforePassword.SessionEpoch {
		t.Fatal("password or epoch changed without success audit")
	}
	expectAuditCount(t, f.dir, "auth.password", "success", 0)
	expectAuditCount(t, f.dir, "auth.password", "failed", failedPassword+1)
	f.request(t, "GET", "/api/v1/auth/session", "", cookie, http.StatusOK)
	return beforePassword
}

func assertPasswordChangeCommits(t *testing.T, f *authFixture, admin, beforePassword store.User, cookie *http.Cookie) *http.Cookie {
	t.Helper()
	setSuccessAuditAbort(t, f.dir, "")
	passwordBody := `{"current_password":"` + testPassword + `","new_password":"` + changedPassword + `"}`
	response := f.request(t, "PUT", "/api/v1/auth/password", passwordBody, cookie, http.StatusNoContent)
	expectAuditCount(t, f.dir, "auth.password", "success", 1)
	rotated := response.Result().Cookies()
	if len(rotated) != 1 || rotated[0].Value == cookie.Value {
		t.Fatal("password change did not rotate the session token")
	}
	changed, err := f.database.User(t.Context(), admin.ID)
	if err != nil || changed.PasswordHash == beforePassword.PasswordHash || changed.SessionEpoch != beforePassword.SessionEpoch+1 {
		t.Fatalf("password commit = %+v %v", changed, err)
	}
	f.request(t, "GET", "/api/v1/auth/session", "", cookie, http.StatusUnauthorized)
	f.request(t, "GET", "/api/v1/auth/session", "", rotated[0], http.StatusOK)
	return rotated[0]
}

func assertLogoutRollsBackWithoutSuccessAudit(t *testing.T, f *authFixture, cookie *http.Cookie) {
	t.Helper()
	setSuccessAuditAbort(t, f.dir, "auth.logout")
	failedLogout := countAudit(t, f.dir, "auth.logout", "failed")
	response := f.request(t, "POST", "/api/v1/auth/logout", "", cookie, http.StatusInternalServerError)
	requireInternal(t, response)
	if len(response.Result().Cookies()) != 0 {
		t.Fatal("failed logout cleared the cookie")
	}
	f.request(t, "GET", "/api/v1/auth/session", "", cookie, http.StatusOK)
	if _, found, findErr := f.database.Sessions().Find(cookie.Value); findErr != nil || !found {
		t.Fatalf("logout persisted without success audit: found=%t err=%v", found, findErr)
	}
	expectAuditCount(t, f.dir, "auth.logout", "success", 0)
	expectAuditCount(t, f.dir, "auth.logout", "failed", failedLogout+1)
	setSuccessAuditAbort(t, f.dir, "")
	f.request(t, "POST", "/api/v1/auth/logout", "", cookie, http.StatusNoContent)
	expectAuditCount(t, f.dir, "auth.logout", "success", 1)
	f.request(t, "GET", "/api/v1/auth/session", "", cookie, http.StatusUnauthorized)
	if _, found, findErr := f.database.Sessions().Find(cookie.Value); findErr != nil || found {
		t.Fatalf("logout session retained: found=%t err=%v", found, findErr)
	}
}

func assertAuditsAndLogsOmitPasswords(t *testing.T, f *authFixture) {
	t.Helper()
	persisted := rawAudit(t, f.dir) + f.logs.String()
	if strings.Contains(persisted, testPassword) || strings.Contains(persisted, changedPassword) {
		t.Fatal("audit or logs contain a password")
	}
}

func TestPasswordChangeKeepsOldEpochWhenRotationFails(t *testing.T) {
	f := newAuthFixture(t)
	user := f.seed(t, "viewer", "viewer")
	current := f.login(t, "viewer", testPassword)
	other := f.login(t, "viewer", testPassword)
	before, err := f.database.User(t.Context(), user.ID)
	if err != nil {
		t.Fatal(err)
	}
	setSessionTombstoneAbort(t, f.dir, current.Value)
	body := `{"current_password":"` + testPassword + `","new_password":"` + changedPassword + `"}`
	response := f.request(t, "PUT", "/api/v1/auth/password", body, current, http.StatusNoContent)
	assertRotationFailureDidNotPromote(t, f, response, current, before)
	assertOldTokenRejectedWhileTombstoneFails(t, f, current, other)
	setSessionTombstoneAbort(t, f.dir, "")
	f.request(t, "GET", "/api/v1/auth/session", "", current, http.StatusUnauthorized)
	f.request(t, "GET", "/api/v1/auth/session", "", other, http.StatusUnauthorized)
	f.request(t, "POST", "/api/v1/auth/login", `{"username":"viewer","password":"`+testPassword+`"}`, nil, http.StatusUnauthorized)
	f.login(t, "viewer", changedPassword)
}

func assertRotationFailureDidNotPromote(t *testing.T, f *authFixture, response *httptest.ResponseRecorder, current *http.Cookie, before store.User) {
	t.Helper()
	for _, cookie := range response.Result().Cookies() {
		if cookie.Value != current.Value {
			t.Fatalf("rotation failure issued token %q", cookie.Value)
		}
	}
	assertPasswordCommit(t, f, before)
	loaded, err := f.service.Sessions.Load(t.Context(), current.Value)
	if err != nil {
		t.Fatal(err)
	}
	if f.service.Sessions.GetInt64(loaded, "epoch") != before.SessionEpoch {
		t.Fatalf("old token epoch = %d, want %d", f.service.Sessions.GetInt64(loaded, "epoch"), before.SessionEpoch)
	}
	logs := f.logs.String()
	if !strings.Contains(logs, "log in again") || strings.Contains(logs, testPassword) || strings.Contains(logs, changedPassword) {
		t.Fatalf("rotation log = %s", logs)
	}
}

func assertPasswordCommit(t *testing.T, f *authFixture, before store.User) {
	t.Helper()
	changed, err := f.database.User(t.Context(), before.ID)
	if err != nil || changed.PasswordHash == before.PasswordHash || changed.SessionEpoch != before.SessionEpoch+1 {
		t.Fatalf("password commit = %+v %v", changed, err)
	}
	expectAuditCount(t, f.dir, "auth.password", "success", 1)
	expectAuditCount(t, f.dir, "auth.password", "failed", 0)
}

func assertOldTokenRejectedWhileTombstoneFails(t *testing.T, f *authFixture, current, other *http.Cookie) {
	t.Helper()
	response, err := f.client.roundTrip(apiRequest(http.MethodGet, "/api/v1/auth/session", "", current))
	if err != nil {
		t.Fatal(err)
	}
	if response.Code == http.StatusOK || (response.Code != http.StatusInternalServerError && response.Code != http.StatusUnauthorized) {
		t.Fatalf("old token status=%d body=%s", response.Code, response.Body)
	}
	if _, found, findErr := f.database.Sessions().Find(current.Value); findErr != nil || !found {
		t.Fatalf("failed rotation removed the old session: found=%t err=%v", found, findErr)
	}
	f.request(t, "GET", "/api/v1/auth/session", "", other, http.StatusUnauthorized)
}

func setSessionTombstoneAbort(t *testing.T, dir, token string) {
	t.Helper()
	if strings.ContainsAny(token, "'\\") {
		t.Fatalf("token is not safe for the tombstone trigger: %q", token)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "intern.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(t.Context(), `PRAGMA busy_timeout = 5000`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `DROP TRIGGER IF EXISTS abort_session_tombstone`); err != nil {
		t.Fatal(err)
	}
	if token == "" {
		return
	}
	query := `CREATE TRIGGER abort_session_tombstone BEFORE INSERT ON session_tombstones
WHEN NEW.token = '` + token + `'
BEGIN SELECT RAISE(ABORT, 'injected tombstone failure'); END`
	if _, err = db.ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
}

func TestLoginSuccessAuditFailureDoesNotFailLogin(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	setSuccessAuditAbort(t, f.dir, "auth.login")
	cookie := f.login(t, "admin", testPassword)
	expectAuditCount(t, f.dir, "auth.login", "success", 0)
	if !strings.Contains(f.logs.String(), "audit write failed") || strings.Contains(f.logs.String(), testPassword) {
		t.Fatal(f.logs.String())
	}
	f.request(t, "GET", "/api/v1/auth/session", "", cookie, http.StatusOK)
	denied := countAudit(t, f.dir, "auth.login", "denied")
	f.request(t, "POST", "/api/v1/auth/login", `{"username":"missing","password":"wrong-password-12345"}`, nil, http.StatusUnauthorized)
	expectAuditCount(t, f.dir, "auth.login", "success", 0)
	expectAuditCount(t, f.dir, "auth.login", "denied", denied+1)
}

func requireInternal(t *testing.T, response *httptest.ResponseRecorder) {
	t.Helper()
	assertSingleError(t, response, render.Internal)
	if strings.Contains(response.Body.String(), "injected") || strings.Contains(response.Body.String(), testPassword) || strings.Contains(response.Body.String(), changedPassword) {
		t.Fatalf("internal error leaked: %s", response.Body)
	}
}

func requireUser(t *testing.T, f *authFixture, username string) store.User {
	t.Helper()
	user, err := f.database.UserByUsername(t.Context(), username)
	if err != nil {
		t.Fatal(err)
	}
	return user
}

func expectAuditCount(t *testing.T, dir, action, result string, want int) {
	t.Helper()
	if got := countAudit(t, dir, action, result); got != want {
		t.Fatalf("%s %s audits=%d, want %d", action, result, got, want)
	}
}

func countAudit(t *testing.T, dir, action, result string) int {
	t.Helper()
	db, err := sql.Open("sqlite", filepath.Join(dir, "intern.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var n int
	if err = db.QueryRowContext(t.Context(), `SELECT COUNT(*) FROM audit_logs WHERE action = ? AND result = ?`, action, result).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func setSuccessAuditAbort(t *testing.T, dir, action string) {
	t.Helper()
	switch action {
	case "", "users.create", "users.update", "auth.password", "users.revoke_sessions", "auth.logout", "auth.login":
	default:
		t.Fatalf("refusing audit trigger for %q", action)
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "intern.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(t.Context(), `PRAGMA busy_timeout = 5000`); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(t.Context(), `DROP TRIGGER IF EXISTS abort_success_audit`); err != nil {
		t.Fatal(err)
	}
	if action == "" {
		return
	}
	query := `CREATE TRIGGER abort_success_audit BEFORE INSERT ON audit_logs
WHEN NEW.result = 'success' AND NEW.action = '` + action + `'
BEGIN SELECT RAISE(ABORT, 'injected audit failure'); END`
	if _, err = db.ExecContext(t.Context(), query); err != nil {
		t.Fatal(err)
	}
}
