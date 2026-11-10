package store

import (
	"context"
	"errors"

	"github.com/flyingEirc/Intern/app/web/internal/store/internal/storesql"
)

// ErrSetupCompleted is returned when the one-time administrator setup is
// already closed. It is deliberately not ErrConflict: the caller must be able
// to distinguish an already-initialized deployment from a duplicate username.
var ErrSetupCompleted = errors.New("initial administrator setup is already complete")

// SetupRequired reports whether the anonymous setup entry point is still open.
//
// The persistent marker is authoritative. The users check is a defence for an
// upgraded database: any existing user, enabled or disabled, closes setup.
// A missing marker fails closed with a store error.
func (s *Store) SetupRequired(ctx context.Context) (bool, error) {
	q := storesql.New(s.db)
	completed, err := q.GetSetupCompleted(ctx)
	if err != nil || completed {
		return false, err
	}
	hasUsers, err := q.HasUsers(ctx)
	return !hasUsers && err == nil, err
}

// SetupAdmin creates the first administrator, writes its success audit, and
// marks setup complete in one transaction. The setup marker row is locked
// before the user count and inserts, so concurrent writers in multiple
// processes cannot create two first administrators. Any audit or marker error
// rolls back the administrator as well.
func (s *Store) SetupAdmin(ctx context.Context, username, hash string, entry AuditLog) (User, error) {
	var user User
	err := s.withTx(ctx, func(ctx context.Context, queries *storesql.Queries) error {
		completed, err := queries.LockSetupState(ctx)
		if err != nil {
			return err
		}
		hasUsers, err := queries.HasUsers(ctx)
		if err != nil {
			return err
		}
		if completed || hasUsers {
			return ErrSetupCompleted
		}
		created, err := createUser(ctx, queries, username, hash, "admin")
		if err != nil {
			return err
		}
		user = created
		if err = insertSuccessAudit(ctx, queries, entry, "user", user.ID); err != nil {
			return err
		}
		return queries.CompleteSetup(ctx)
	})
	if err != nil {
		return User{}, err
	}
	return user, nil
}
