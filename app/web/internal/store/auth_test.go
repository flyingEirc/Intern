package store

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

func TestSessionStorePersistsExpiresAndDeletes(t *testing.T) {
	dir := privateTempDir(t)
	database := openTestStore(t, dir)
	sessions := database.Sessions()
	if err := sessions.Commit("active", []byte("session-data"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	assertExpiredSessionCleanup(t, database)
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	database = openTestStore(t, dir)
	sessions = database.Sessions()
	data, found, err := sessions.Find("active")
	if err != nil || !found || !bytes.Equal(data, []byte("session-data")) {
		t.Fatalf("persisted session=%v, %v", found, err)
	}
	assertSessionDeletion(t, sessions)
}

func assertSessionDeletion(t *testing.T, sessions *SessionStore) {
	t.Helper()
	if err := sessions.Commit("active", []byte("updated"), time.Now().Add(time.Hour)); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Delete("active"); err != nil {
		t.Fatal(err)
	}
	if err := sessions.Delete("active"); err != nil {
		t.Fatal(err)
	}
	if _, found, err := sessions.Find("active"); err != nil || found {
		t.Fatalf("deleted=%v, %v", found, err)
	}
}

func assertExpiredSessionCleanup(t *testing.T, database *Store) {
	t.Helper()
	sessions := database.Sessions()
	if err := sessions.Commit("expired", []byte("expired-data"), time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	if _, found, err := sessions.Find("expired"); err != nil || found {
		t.Fatalf("expired = %v, %v", found, err)
	}
	if err := sessions.DeleteExpired(t.Context()); err != nil {
		t.Fatal(err)
	}
	var expired int
	if err := database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sessions WHERE token = 'expired'").Scan(&expired); err != nil || expired != 0 {
		t.Fatalf("expired rows=%d, %v", expired, err)
	}
}

func TestDeletedSessionIsNotResurrectedByCommit(t *testing.T) {
	dir := privateTempDir(t)
	database, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	database.SetSessionLifetime(time.Hour)
	sessions := database.Sessions()
	expiry := time.Now().Add(time.Hour)
	commitToken(t, sessions, "token-a", []byte("data"), expiry)
	if err = sessions.Delete("token-a"); err != nil {
		t.Fatal(err)
	}
	commitToken(t, sessions, "token-a", []byte("resurrected"), expiry)
	assertSessionAbsent(t, sessions, "token-a")
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { reopened.Close() })
	assertSessionAbsent(t, reopened.Sessions(), "token-a")
	assertExpiredTombstoneAllowsReuse(t, reopened)
}

func commitToken(t *testing.T, sessions *SessionStore, token string, data []byte, expiry time.Time) {
	t.Helper()
	if err := sessions.Commit(token, data, expiry); err != nil {
		t.Fatal(err)
	}
}

func assertSessionAbsent(t *testing.T, sessions *SessionStore, token string) {
	t.Helper()
	_, found, err := sessions.Find(token)
	if err != nil || found {
		t.Fatalf("session %s present: found=%v err=%v", token, found, err)
	}
}

func assertExpiredTombstoneAllowsReuse(t *testing.T, database *Store) {
	t.Helper()
	_, err := database.db.ExecContext(t.Context(), "UPDATE session_tombstones SET expiry = ?", unixSeconds(time.Now().Add(-time.Second)))
	if err != nil {
		t.Fatal(err)
	}
	if err = database.Sessions().DeleteExpired(t.Context()); err != nil {
		t.Fatal(err)
	}
	commitToken(t, database.Sessions(), "token-a", []byte("fresh"), time.Now().Add(time.Hour))
	data, found, err := database.Sessions().Find("token-a")
	if err != nil || !found || !bytes.Equal(data, []byte("fresh")) {
		t.Fatalf("expired tombstone still blocked token: %q %v %v", data, found, err)
	}
}

func TestSessionCleanupFailureLogsError(t *testing.T) {
	database, err := Open(t.Context(), privateTempDir(t))
	if err != nil {
		t.Fatal(err)
	}
	sessions := database.Sessions()
	if err = database.Close(); err != nil {
		t.Fatal(err)
	}
	var output lockedLog
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sessions.Cleanup(ctx, time.Millisecond, logger)
	}()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.Now().Add(2 * time.Second)
	for !strings.Contains(output.String(), `"msg":"session cleanup failed"`) {
		if time.Now().After(deadline) {
			t.Fatalf("cleanup log missing: %s", output.String())
		}
		time.Sleep(time.Millisecond)
	}
	cancel()
	<-done
	logged := output.String()
	if !strings.Contains(logged, `"error"`) || strings.Contains(logged, "token-a") {
		t.Fatalf("cleanup error attribute missing: %s", logged)
	}
}

type lockedLog struct {
	mu sync.Mutex
	b  bytes.Buffer
}

func (l *lockedLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.Write(p)
}

func (l *lockedLog) String() string {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.b.String()
}

func TestSQLSessionCleanupStopsAndRemovesExpiredRows(t *testing.T) {
	database := openTestStore(t, privateTempDir(t))
	sessions := database.Sessions()
	if err := sessions.Commit("expired", []byte("data"), time.Now().Add(-time.Second)); err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		sessions.Cleanup(ctx, time.Millisecond, slog.New(slog.NewJSONHandler(io.Discard, nil)))
	}()
	t.Cleanup(func() { cancel(); <-done })
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		var count int
		if err := database.db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM sessions").Scan(&count); err != nil {
			t.Fatal(err)
		}
		if count == 0 {
			break
		}
		select {
		case <-deadline.C:
			t.Fatal("expired session not cleaned")
		case <-ticker.C:
		}
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("cleanup goroutine did not stop")
	}
}

func TestUsersSchemaAndLastAdminGuard(t *testing.T) {
	database := openTestStore(t, privateTempDir(t))
	admin, err := database.CreateUser(t.Context(), "Admin", "test-hash", "admin", SystemAudit("users.create", AuditDetail{Role: "admin"}))
	if err != nil {
		t.Fatal(err)
	}
	assertUserSchema(t, database, admin)
	disabled, viewer := true, "viewer"
	for _, changes := range []UserChanges{{Disabled: &disabled}, {Role: &viewer}} {
		if _, err = database.UpdateUser(t.Context(), admin.ID, changes, SystemAudit("users.update", AuditDetail{})); !errors.Is(err, ErrLastAdmin) {
			t.Fatalf("last admin guard: %v", err)
		}
	}
	if _, err = database.CreateUser(t.Context(), "secondadmin", "other", "admin", SystemAudit("users.create", AuditDetail{Role: "admin"})); err != nil {
		t.Fatal(err)
	}
	updated, err := database.UpdateUser(t.Context(), admin.ID, UserChanges{Role: &viewer}, SystemAudit("users.update", AuditDetail{Role: viewer}))
	if err != nil || updated.Role != "viewer" || updated.SessionEpoch != 1 {
		t.Fatalf("demote with second admin: %+v, %v", updated, err)
	}
}

func assertUserSchema(t *testing.T, database *Store, admin User) {
	t.Helper()
	id, err := uuid.Parse(admin.ID)
	if err != nil || id.Version() != 7 {
		t.Fatal("user ID is not UUIDv7")
	}
	if _, err = database.CreateUser(t.Context(), "ADMIN", "other", "viewer", SystemAudit("users.create", AuditDetail{Role: "viewer"})); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate username: %v", err)
	}
	if _, err = database.CreateUser(t.Context(), "invalidrole", "other", "owner", SystemAudit("users.create", AuditDetail{Role: "owner"})); err == nil {
		t.Fatal("role CHECK missing")
	}
	if _, err = database.db.ExecContext(t.Context(), "UPDATE users SET disabled = 2 WHERE id = ?", admin.ID); err == nil {
		t.Fatal("disabled CHECK missing")
	}
}

func TestOpenCurrentRequiresExactMigrationsWithoutOwnership(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "metadata")
	if _, err := OpenCurrent(t.Context(), dir); err == nil {
		t.Fatal("uninitialized database accepted")
	}
	database := openTestStore(t, dir)
	other, err := OpenCurrent(t.Context(), dir)
	if err != nil {
		t.Fatalf("serve ownership blocked admin connection: %v", err)
	}
	defer other.Close()
	user, err := other.CreateAdmin(t.Context(), "bootstrap", "test-hash")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = database.User(t.Context(), user.ID); err != nil {
		t.Fatal(err)
	}
	assertBootstrapAudit(t, database, user.ID)
	if _, err = database.db.ExecContext(t.Context(), "DELETE FROM web_goose_db_version WHERE version_id = 4"); err != nil {
		t.Fatal(err)
	}
	if _, err = OpenCurrent(t.Context(), dir); err == nil {
		t.Fatal("old database version accepted")
	}
}

func assertBootstrapAudit(t *testing.T, database *Store, id string) {
	t.Helper()
	entries, err := database.AuditLogs(t.Context(), 0, 10)
	if err != nil || len(entries) != 1 || entries[0].ActorType != "system" || entries[0].ResourceID != id {
		t.Fatalf("system audit: %+v, %v", entries, err)
	}
}
