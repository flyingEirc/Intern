package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgconn"

	"github.com/flyingEirc/Intern/app/web/internal/store/internal/storesql"
)

var (
	ErrNotFound  = errors.New("resource not found")
	ErrConflict  = errors.New("username already exists")
	ErrLastAdmin = errors.New("cannot disable or demote the last enabled administrator")
)

// User deliberately excludes authentication secrets from every JSON response.
type User struct {
	ID           string `json:"id"`
	Username     string `json:"username"`
	Role         string `json:"role"`
	Disabled     bool   `json:"disabled"`
	CreatedAt    string `json:"created_at"`
	UpdatedAt    string `json:"updated_at"`
	PasswordHash string `json:"-"`
	SessionEpoch int64  `json:"-"`
}

type UserChanges struct {
	Role         *string
	Disabled     *bool
	PasswordHash *string
}

func userFromRow(row storesql.User) User {
	return User{
		ID: row.ID, Username: row.Username, Role: row.Role, Disabled: row.Disabled != 0,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, PasswordHash: row.PasswordHash, SessionEpoch: row.SessionEpoch,
	}
}

func userError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) && pgErr.Code == "23505" {
		return ErrConflict
	}
	return err
}

func createUser(ctx context.Context, queries *storesql.Queries, username, hash, role string) (User, error) {
	id, err := uuid.NewV7()
	if err != nil {
		return User{}, err
	}
	now := time.Now().UTC().Format(time.RFC3339Nano)
	row, err := queries.CreateUser(ctx, storesql.CreateUserParams{
		ID: id.String(), Username: username, PasswordHash: hash, Role: role, CreatedAt: now, UpdatedAt: now,
	})
	return userFromRow(row), userError(err)
}

// CreateUser commits the new user, its success audit, and the permanent setup
// closure together. Any existing user closes anonymous setup; deleting every
// user later cannot reopen it.
func (s *Store) CreateUser(ctx context.Context, username, hash, role string, entry AuditLog) (User, error) {
	var user User
	err := s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		created, err := createUser(ctx, q, username, hash, role)
		if err != nil {
			return err
		}
		user = created
		if err := insertSuccessAudit(ctx, q, entry, "user", user.ID); err != nil {
			return err
		}
		return q.CompleteSetup(ctx)
	})
	if err != nil {
		return User{}, err
	}
	return user, nil
}

func (s *Store) User(ctx context.Context, id string) (User, error) {
	row, err := storesql.New(s.db).GetUser(ctx, id)
	return userFromRow(row), userError(err)
}

func (s *Store) UserByUsername(ctx context.Context, username string) (User, error) {
	row, err := storesql.New(s.db).GetUserByUsername(ctx, username)
	return userFromRow(row), userError(err)
}

func (s *Store) Users(ctx context.Context) ([]User, error) {
	rows, err := storesql.New(s.db).ListUsers(ctx)
	if err != nil {
		return nil, err
	}
	users := make([]User, len(rows))
	for i, row := range rows {
		users[i] = userFromRow(row)
	}
	return users, nil
}

// UpdateUser commits the user change and its success audit together. The
// last-admin guard stays in the same UPDATE on the single owned connection.
// An audit failure rolls the change back.
func (s *Store) UpdateUser(ctx context.Context, id string, changes UserChanges, entry AuditLog) (User, error) {
	var user User
	err := s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		updated, err := updateUser(ctx, q, id, changes)
		if err != nil {
			return err
		}
		user = updated
		return insertSuccessAudit(ctx, q, entry, "user", user.ID)
	})
	if err != nil {
		return User{}, err
	}
	return user, nil
}

func updateUser(ctx context.Context, queries *storesql.Queries, id string, changes UserChanges) (User, error) {
	params := storesql.UpdateUserParams{UserID: id, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
	if changes.Role != nil {
		params.Role = sql.NullString{String: *changes.Role, Valid: true}
	}
	if changes.Disabled != nil {
		params.Disabled.Valid = true
		if *changes.Disabled {
			params.Disabled.Int64 = 1
		}
	}
	if changes.PasswordHash != nil {
		params.PasswordHash = sql.NullString{String: *changes.PasswordHash, Valid: true}
	}
	row, err := queries.UpdateUser(ctx, params)
	if errors.Is(err, sql.ErrNoRows) {
		// Same transaction as the UPDATE. A second pool connection would deadlock
		// because the writable pool allows only one connection.
		if _, lookupErr := queries.GetUser(ctx, id); lookupErr != nil {
			return User{}, userError(lookupErr)
		}
		return User{}, ErrLastAdmin
	}
	return userFromRow(row), err
}

// ChangePassword commits the password, epoch bump, and success audit together.
// Session token rotation is a later cookie write and is not part of this transaction.
func (s *Store) ChangePassword(ctx context.Context, user User, hash string, entry AuditLog) (User, error) {
	var updated User
	err := s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		changed, err := changePassword(ctx, q, user, hash)
		if err != nil {
			return err
		}
		updated = changed
		return insertSuccessAudit(ctx, q, entry, "user", updated.ID)
	})
	if err != nil {
		return User{}, err
	}
	return updated, nil
}

func changePassword(ctx context.Context, queries *storesql.Queries, user User, hash string) (User, error) {
	row, err := queries.ChangePassword(ctx, storesql.ChangePasswordParams{
		ID: user.ID, SessionEpoch: user.SessionEpoch, PasswordHash: hash, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	return userFromRow(row), userError(err)
}

// RevokeSessions commits the epoch bump and its success audit together.
func (s *Store) RevokeSessions(ctx context.Context, id string, entry AuditLog) error {
	return s.withTx(ctx, func(ctx context.Context, q *storesql.Queries) error {
		if err := revokeSessions(ctx, q, id); err != nil {
			return err
		}
		return insertSuccessAudit(ctx, q, entry, "user", id)
	})
}

func revokeSessions(ctx context.Context, queries *storesql.Queries, id string) error {
	_, err := queries.RevokeUserSessions(ctx, storesql.RevokeUserSessionsParams{
		ID: id, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	return userError(err)
}
