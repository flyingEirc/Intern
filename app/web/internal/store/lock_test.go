package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestWritableRowLockExcludesExternalWriter(t *testing.T) {
	database, dsn := openTestStoreDSN(t)
	tx := beginOn(t, database.db, nil)
	var id string
	if err := tx.QueryRowContext(t.Context(), "SELECT id FROM instance FOR UPDATE").Scan(&id); err != nil {
		t.Fatal(err)
	}
	if err := writeInstance(dsn); err == nil {
		t.Fatal("writable transaction left the database writable")
	}
	rollback(t, tx)
}

func TestReadOnlyBeginDoesNotReserveTheWriteLock(t *testing.T) {
	database, dsn := openTestStoreDSN(t)
	tx := beginOn(t, database.db, &sql.TxOptions{ReadOnly: true})
	scanOne(t, tx)
	if err := writeInstance(dsn); err != nil {
		t.Fatalf("read-only transaction blocked a writer: %v", err)
	}
	rollback(t, tx)
}

func TestSessionAndUserWritesWaitForExternalLock(t *testing.T) {
	database, dsn := openTestStoreDSN(t)
	database.SetSessionLifetime(time.Hour)
	user, err := database.CreateUser(t.Context(), "viewer", "hash", "viewer", SystemAudit("users.create", AuditDetail{Role: "viewer"}))
	if err != nil {
		t.Fatal(err)
	}
	expiry := time.Now().Add(time.Hour)
	assertWaitsForLock(t, dsn, func() error { return database.Sessions().Commit("held", []byte("data"), expiry) })
	assertSessionPresent(t, database.Sessions(), "held")
	assertWaitsForLock(t, dsn, func() error { return database.Sessions().Delete("held") })
	assertSessionAbsent(t, database.Sessions(), "held")
	role := "operator"
	assertWaitsForLock(t, dsn, func() error {
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

func TestSetupConcurrentWithSessionWrites(t *testing.T) {
	database, _ := openTestStoreDSN(t)
	database.SetSessionLifetime(time.Hour)
	var failures, writes atomic.Int64
	ctx, cancel := context.WithTimeout(context.Background(), 500*time.Millisecond)
	defer cancel()
	var wg sync.WaitGroup
	wg.Add(1)
	go writeSessionsUntil(ctx, &wg, database.Sessions(), &failures, &writes)
	for i := 0; ctx.Err() == nil; i++ {
		_, err := database.SetupAdmin(context.WithoutCancel(ctx), fmt.Sprintf("adm%04d", i), "hash-value", SystemAudit("auth.setup", AuditDetail{Role: "admin"}))
		if err != nil && !errorsIsSetup(err) {
			cancel()
			wg.Wait()
			t.Fatal(err)
		}
	}
	cancel()
	wg.Wait()
	if failures.Load() != 0 || writes.Load() == 0 {
		t.Fatalf("failures=%d writes=%d", failures.Load(), writes.Load())
	}
}

func errorsIsSetup(err error) bool { return errors.Is(err, ErrSetupCompleted) }

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

func writeInstance(dsn string) error {
	db, err := sql.Open("pgx", dsn)
	if err != nil {
		return err
	}
	defer db.Close()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	_, err = db.ExecContext(ctx, "UPDATE instance SET created_at = created_at WHERE singleton = 1")
	return err
}

func assertWaitsForLock(t *testing.T, dsn string, op func() error) {
	t.Helper()
	release := holdExternalWriteLock(t, dsn)
	result := make(chan error, 1)
	go func() { result <- op() }()
	select {
	case err := <-result:
		t.Fatalf("operation did not wait for PostgreSQL lock: %v", err)
	case <-time.After(200 * time.Millisecond):
	}
	release()
	select {
	case err := <-result:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("operation did not finish after PostgreSQL lock release")
	}
}

func assertSessionPresent(t *testing.T, sessions *SessionStore, token string) {
	t.Helper()
	_, found, err := sessions.Find(token)
	if err != nil || !found {
		t.Fatalf("session %q present=%v err=%v", token, found, err)
	}
}
