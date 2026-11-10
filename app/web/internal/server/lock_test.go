package server

import (
	"context"
	"database/sql"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"
)

func TestAuthenticatedRequestsWaitForExternalWriteLock(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.seed(t, "admin", "admin")
	target := fixture.seed(t, "viewer", "viewer")
	cookie := fixture.login(t, "admin", testPassword)
	waitForMetadataLock(t, fixture, apiRequest(http.MethodGet, "/api/v1/auth/session", "", cookie), http.StatusOK)
	body := `{"role":"operator"}`
	waitForMetadataLock(t, fixture, apiRequest(http.MethodPatch, "/api/v1/users/"+target.ID, body, cookie), http.StatusOK)
	waitForMetadataLock(t, fixture, apiRequest(http.MethodPost, "/api/v1/auth/logout", "", cookie), http.StatusNoContent)
	fixture.request(t, http.MethodGet, "/api/v1/auth/session", "", cookie, http.StatusUnauthorized)
}

type lockedResponse struct {
	recorder *httptest.ResponseRecorder
	err      error
}

func waitForMetadataLock(t *testing.T, fixture *authFixture, req *http.Request, status int) {
	t.Helper()
	release := holdMetadataWriteLock(t, fixture.dsn)
	done := make(chan lockedResponse, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		recorder, err := fixture.client.roundTrip(req)
		done <- lockedResponse{recorder: recorder, err: err}
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		release()
		t.Fatal("request did not start")
	}
	select {
	case result := <-done:
		release()
		t.Fatalf("finished while the write lock was held: status=%v err=%v", result.recorder, result.err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case result := <-done:
		if result.err != nil {
			t.Fatal(result.err)
		}
		if result.recorder.Code != status {
			t.Fatalf("status=%d body=%s", result.recorder.Code, result.recorder.Body)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("request did not finish after the write lock was released")
	}
}

func holdMetadataWriteLock(t *testing.T, dsn string) func() {
	t.Helper()
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(context.Background(), "LOCK TABLE users, sessions, session_tombstones IN ACCESS EXCLUSIVE MODE"); err != nil {
		t.Fatal(err)
	}
	return func() { _ = tx.Rollback() }
}
