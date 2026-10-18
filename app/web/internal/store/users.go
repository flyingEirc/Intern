package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/google/uuid"
	"modernc.org/sqlite"

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
	return User{ID: row.ID, Username: row.Username, Role: row.Role, Disabled: row.Disabled != 0,
		CreatedAt: row.CreatedAt, UpdatedAt: row.UpdatedAt, PasswordHash: row.PasswordHash, SessionEpoch: row.SessionEpoch}
}

func userError(err error) error {
	if errors.Is(err, sql.ErrNoRows) {
		return ErrNotFound
	}
	var sqliteErr *sqlite.Error
	if errors.As(err, &sqliteErr) && sqliteErr.Code() == 2067 {
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

func (s *Store) CreateUser(ctx context.Context, username, hash, role string) (User, error) {
	return createUser(ctx, storesql.New(s.db), username, hash, role)
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

// UpdateUser uses one guarded SQL statement so concurrent requests cannot remove
// the last enabled administrator, including requests from create-admin's connection.
// SQLite reserves the write lock at the start of that UPDATE, before the
// last-admin subquery, so busy_timeout applies without a deferred lock upgrade.
func (s *Store) UpdateUser(ctx context.Context, id string, changes UserChanges) (User, error) {
	params := storesql.UpdateUserParams{ID: id, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano)}
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
	row, err := storesql.New(s.db).UpdateUser(ctx, params)
	if errors.Is(err, sql.ErrNoRows) {
		if _, lookupErr := s.User(ctx, id); lookupErr != nil {
			return User{}, lookupErr
		}
		return User{}, ErrLastAdmin
	}
	return userFromRow(row), err
}

func (s *Store) ChangePassword(ctx context.Context, user User, hash string) (User, error) {
	row, err := storesql.New(s.db).ChangePassword(ctx, storesql.ChangePasswordParams{
		ID: user.ID, SessionEpoch: user.SessionEpoch, PasswordHash: hash, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	return userFromRow(row), userError(err)
}

func (s *Store) RevokeSessions(ctx context.Context, id string) error {
	_, err := storesql.New(s.db).RevokeUserSessions(ctx, storesql.RevokeUserSessionsParams{
		ID: id, UpdatedAt: time.Now().UTC().Format(time.RFC3339Nano),
	})
	return userError(err)
}
