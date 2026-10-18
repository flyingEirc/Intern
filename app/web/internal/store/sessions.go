package store

import (
	"context"
	"database/sql"
	"errors"
	"log/slog"
	"time"

	"github.com/alexedwards/scs/v2"

	"github.com/flyingEirc/Intern/app/web/internal/store/internal/storesql"
)

// SessionStore is the sqlc-backed scs store. Cleanup has an explicit owner and
// finishes before serve closes the database; constructing a store starts no goroutine.
//
// Delete writes a tombstone kept until the session expiry (at least the configured
// lifetime). Commit refuses that token, so an in-flight Load cannot upsert it back,
// and Find treats it as absent across restarts. Expired tombstones are deleted with
// expired sessions. Both transactions are begun on the writable pool, whose
// BEGIN IMMEDIATE reserves the write lock before the expiry or tombstone read.
type SessionStore struct {
	store    *Store
	lifetime time.Duration
}

var _ scs.CtxStore = (*SessionStore)(nil)

// SetSessionLifetime records how long a deleted token stays dead when its row
// expiry is already gone. auth.New sets this before any request is served.
func (s *Store) SetSessionLifetime(lifetime time.Duration) {
	if lifetime > 0 {
		s.sessionLifetime = lifetime
	}
}

func (s *Store) Sessions() *SessionStore {
	return &SessionStore{store: s, lifetime: s.sessionLifetime}
}

func (s *SessionStore) Find(token string) ([]byte, bool, error) {
	return s.FindCtx(context.Background(), token)
}

func (s *SessionStore) FindCtx(ctx context.Context, token string) ([]byte, bool, error) {
	queries := storesql.New(s.store.db)
	now := unixSeconds(time.Now())
	if _, err := queries.TombstoneActive(ctx, storesql.TombstoneActiveParams{Token: token, Expiry: now}); err == nil {
		return nil, false, nil
	} else if !errors.Is(err, sql.ErrNoRows) {
		return nil, false, err
	}
	data, err := queries.FindSession(ctx, storesql.FindSessionParams{Token: token, Expiry: now})
	if errors.Is(err, sql.ErrNoRows) {
		return nil, false, nil
	}
	return data, err == nil, err
}

func (s *SessionStore) Commit(token string, data []byte, expiry time.Time) error {
	return s.CommitCtx(context.Background(), token, data, expiry)
}

func (s *SessionStore) CommitCtx(ctx context.Context, token string, data []byte, expiry time.Time) (retErr error) {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			if rollbackErr := tx.Rollback(); !errors.Is(rollbackErr, sql.ErrTxDone) {
				retErr = errors.Join(retErr, rollbackErr)
			}
		}
	}()
	queries := storesql.New(tx)
	now := unixSeconds(time.Now())
	if _, err = queries.TombstoneActive(ctx, storesql.TombstoneActiveParams{Token: token, Expiry: now}); err == nil {
		if err = queries.DeleteSession(ctx, token); err != nil {
			return err
		}
		return tx.Commit()
	} else if !errors.Is(err, sql.ErrNoRows) {
		return err
	}
	err = queries.CommitSession(ctx, storesql.CommitSessionParams{Token: token, Data: data, Expiry: unixSeconds(expiry)})
	if err != nil {
		return err
	}
	return tx.Commit()
}

func (s *SessionStore) Delete(token string) error { return s.DeleteCtx(context.Background(), token) }

func (s *SessionStore) DeleteCtx(ctx context.Context, token string) (retErr error) {
	tx, err := s.store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		retErr = rollbackOnFailure(tx, retErr)
	}()
	queries := storesql.New(tx)
	until, err := s.deletedSessionTombstoneExpiry(ctx, queries, token)
	if err != nil {
		return err
	}
	if err = queries.UpsertSessionTombstone(ctx, storesql.UpsertSessionTombstoneParams{Token: token, Expiry: until}); err != nil {
		return err
	}
	if err = queries.DeleteSession(ctx, token); err != nil {
		return err
	}
	if err = insertLogoutSuccessAudit(ctx, queries); err != nil {
		return err
	}
	return tx.Commit()
}

func rollbackOnFailure(tx *sql.Tx, err error) error {
	if err == nil {
		return nil
	}
	if rollbackErr := tx.Rollback(); !errors.Is(rollbackErr, sql.ErrTxDone) {
		return errors.Join(err, rollbackErr)
	}
	return err
}

func (s *SessionStore) deletedSessionTombstoneExpiry(ctx context.Context, queries *storesql.Queries, token string) (float64, error) {
	until := unixSeconds(s.tombstoneUntil(time.Now()))
	stored, err := queries.GetSessionExpiry(ctx, token)
	if err == nil && stored > until {
		return stored, nil
	}
	if err != nil && !errors.Is(err, sql.ErrNoRows) {
		return 0, err
	}
	return until, nil
}

func insertLogoutSuccessAudit(ctx context.Context, queries *storesql.Queries) error {
	entry, ok := logoutAuditFrom(ctx)
	if !ok {
		return nil
	}
	if entry.Result != "success" {
		return errors.New("logout audit must be a success record")
	}
	return insertSuccessAudit(ctx, queries, entry, "user", entry.ResourceID)
}

func (s *SessionStore) tombstoneUntil(now time.Time) time.Time {
	lifetime := s.lifetime
	if lifetime <= 0 {
		lifetime = 12 * time.Hour
	}
	return now.Add(lifetime)
}

func unixSeconds(t time.Time) float64 { return float64(t.Unix()) + float64(t.Nanosecond())/1e9 }

func (s *SessionStore) DeleteExpired(ctx context.Context) error {
	queries := storesql.New(s.store.db)
	now := unixSeconds(time.Now())
	if err := queries.DeleteExpiredSessions(ctx, now); err != nil {
		return err
	}
	return queries.DeleteExpiredTombstones(ctx, now)
}

// Cleanup runs synchronously until cancellation; the caller owns its goroutine.
func (s *SessionStore) Cleanup(ctx context.Context, interval time.Duration, logger *slog.Logger) {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			if err := s.DeleteExpired(ctx); err != nil && ctx.Err() == nil {
				logger.ErrorContext(ctx, "session cleanup failed", "error", err)
			}
		}
	}
}
