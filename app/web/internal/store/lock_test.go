package store

import (
	"context"
	"database/sql"
	"fmt"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWritableBeginReservesTheWriteLock(t *testing.T) {
	database, path := openLockedStore(t)
	tx := beginOn(t, database.db, nil)
	scanOne(t, tx)
	if err := writeInstance(path); !busyError(err) {
		t.Fatalf("writable transaction left the database writable: %v", err)
	}
	rollback(t, tx)
}

func TestReadOnlyBeginDoesNotReserveTheWriteLock(t *testing.T) {
	database, path := openLockedStore(t)
	tx := beginOn(t, database.db, &sql.TxOptions{ReadOnly: true})
	scanOne(t, tx)
	if err := writeInstance(path); err != nil {
		t.Fatalf("read-only transaction blocked a writer: %v", err)
	}
	rollback(t, tx)
	readOnly, err := openDatabase(t.Context(), path, true)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { readOnly.Close() })
	status := beginOn(t, readOnly, nil)
	var count int
	if err = status.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM instance").Scan(&count); err != nil {
		t.Fatalf("read-only connection could not start a deferred read: %v", err)
	}
	rollback(t, status)
}

func TestSessionAndUserWritesWaitForExternalLock(t *testing.T) {
	database, path := openLockedStore(t)
	database.SetSessionLifetime(time.Hour)
	user, err := database.CreateUser(t.Context(), "viewer", "hash", "viewer", SystemAudit("users.create", AuditDetail{Role: "viewer"}))
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour)
	assertWaitsForLock(t, path, func() error {
		return database.Sessions().Commit("held", []byte("data"), expiry)
	})
	assertSessionPresent(t, database.Sessions(), "held")
	assertWaitsForLock(t, path, func() error {
		return database.Sessions().Delete("held")
	})
	assertSessionAbsent(t, database.Sessions(), "held")
	role := "operator"
	assertWaitsForLock(t, path, func() error {
		updated, updateErr := database.UpdateUser(t.Context(), user.ID, UserChanges{Role: &role}, SystemAudit("users.update", AuditDetail{Role: role}))
		if updateErr != nil {
			return updateErr
		}
		if updated.Role != "operator" {
			return fmt.Errorf("role=%s", updated.Role)
		}
		return nil
	})
}

func TestCreateAdminConcurrentWithSessionWrites(t *testing.T) {
	dir := privateTempDir(t)
	database := openTestStore(t, dir)
	database.SetSessionLifetime(time.Hour)
	admin, err := OpenCurrent(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { admin.Close() })
	ctx, cancel := context.WithTimeout(t.Context(), 400*time.Millisecond)
	defer cancel()
	var failures atomic.Int64
	var writes atomic.Int64
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go writeSessionsUntil(ctx, &wg, database.Sessions(), &failures, &writes)
	}
	created := 0
	for i := 0; ctx.Err() == nil; i++ {
		if _, err = admin.CreateAdmin(context.WithoutCancel(ctx), fmt.Sprintf("adm%04d", i), "hash-value"); err != nil {
			cancel()
			wg.Wait()
			t.Fatal(err)
		}
		created++
	}
	wg.Wait()
	if failures.Load() != 0 {
		t.Fatalf("session writes failed: %d", failures.Load())
	}
	if created < 1 || writes.Load() < 1 {
		t.Fatalf("created=%d session writes=%d", created, writes.Load())
	}
}

func writeSessionsUntil(ctx context.Context, wg *sync.WaitGroup, sessions *SessionStore, failures, writes *atomic.Int64) {
	defer wg.Done()
	var sequence atomic.Int64
	for ctx.Err() == nil {
		token := fmt.Sprintf("tok-%d-%d", time.Now().UnixNano(), sequence.Add(1))
		if err := sessions.Commit(token, []byte("data"), time.Now().Add(time.Minute)); err != nil {
			failures.Add(1)
			return
		}
		if err := sessions.Delete(token); err != nil {
			failures.Add(1)
			return
		}
		writes.Add(1)
	}
}

func openLockedStore(t *testing.T) (*Store, string) {
	t.Helper()
	dir := privateTempDir(t)
	return openTestStore(t, dir), filepath.Join(dir, "intern.db")
}

func beginOn(t *testing.T, db *sql.DB, opts *sql.TxOptions) *sql.Tx {
	t.Helper()
	tx, err := db.BeginTx(t.Context(), opts)
	if err != nil {
		t.Fatal(err)
	}
	return tx
}

func scanOne(t *testing.T, tx *sql.Tx) {
	t.Helper()
	var one int
	if err := tx.QueryRowContext(t.Context(), "SELECT 1").Scan(&one); err != nil {
		t.Fatal(err)
	}
}

func rollback(t *testing.T, tx *sql.Tx) {
	t.Helper()
	if err := tx.Rollback(); err != nil {
		t.Fatal(err)
	}
}

func assertSessionPresent(t *testing.T, sessions *SessionStore, token string) {
	t.Helper()
	_, found, err := sessions.Find(token)
	if err != nil || !found {
		t.Fatalf("session %s missing: found=%v err=%v", token, found, err)
	}
}

func assertWaitsForLock(t *testing.T, path string, op func() error) {
	t.Helper()
	release := holdExternalWriteLock(t, path)
	done := make(chan error, 1)
	started := make(chan struct{})
	go func() {
		close(started)
		done <- op()
	}()
	select {
	case <-started:
	case <-time.After(time.Second):
		release()
		t.Fatal("write did not start")
	}
	select {
	case err := <-done:
		release()
		t.Fatalf("finished while the write lock was held: %v", err)
	case <-time.After(300 * time.Millisecond):
	}
	release()
	select {
	case err := <-done:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("did not finish after the write lock was released")
	}
}

func holdExternalWriteLock(t *testing.T, path string) func() {
	t.Helper()
	db := openBusy(t, path, 0)
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = tx.ExecContext(context.Background(), "UPDATE instance SET created_at = created_at"); err != nil {
		t.Fatal(err)
	}
	return func() { _ = tx.Rollback() }
}

func writeInstance(path string) error {
	db, err := openBusyDB(path, 0)
	if err != nil {
		return err
	}
	defer db.Close()
	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	_, err = tx.ExecContext(context.Background(), "UPDATE instance SET created_at = created_at")
	return err
}

func openBusy(t *testing.T, path string, timeoutMS int) *sql.DB {
	t.Helper()
	db, err := openBusyDB(path, timeoutMS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func openBusyDB(path string, timeoutMS int) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := u.Query()
	q.Add("_pragma", fmt.Sprintf("busy_timeout(%d)", timeoutMS))
	u.RawQuery = q.Encode()
	return sql.Open("sqlite", u.String())
}

func busyError(err error) bool {
	return err != nil && (strings.Contains(err.Error(), "SQLITE_BUSY") || strings.Contains(err.Error(), "database is locked"))
}
