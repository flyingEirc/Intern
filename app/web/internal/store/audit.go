package store

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"math"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store/internal/storesql"
)

// AuditDetail is a closed whitelist; callers cannot attach arbitrary request data.
type AuditDetail struct {
	Role            string `json:"role,omitempty"`
	Disabled        *bool  `json:"disabled,omitempty"`
	PasswordChanged bool   `json:"password_changed,omitzero"`
	Method          string `json:"method,omitempty"`
	Route           string `json:"route,omitempty"`
}

type AuditLog struct {
	ID           int64       `json:"id"`
	TS           string      `json:"ts"`
	ActorType    string      `json:"actor_type"`
	ActorID      string      `json:"actor_id"`
	ActorName    string      `json:"actor_name"`
	Action       string      `json:"action"`
	ResourceType string      `json:"resource_type"`
	ResourceID   string      `json:"resource_id"`
	IP           string      `json:"ip"`
	Result       string      `json:"result"`
	Status       *int64      `json:"status"`
	RequestID    string      `json:"request_id"`
	Detail       AuditDetail `json:"detail"`
}

func insertAudit(ctx context.Context, queries *storesql.Queries, entry AuditLog) error {
	detail, err := json.Marshal(entry.Detail)
	if err != nil {
		return err
	}
	params := storesql.InsertAuditLogParams{
		Ts: time.Now().UTC().Format(time.RFC3339Nano), ActorType: entry.ActorType,
		ActorID: entry.ActorID, ActorName: entry.ActorName, Action: entry.Action,
		ResourceType: entry.ResourceType, ResourceID: entry.ResourceID, Ip: entry.IP,
		Result: entry.Result, RequestID: entry.RequestID, DetailJson: string(detail),
	}
	if entry.Status != nil {
		params.Status = sql.NullInt64{Int64: *entry.Status, Valid: true}
	}
	return queries.InsertAuditLog(ctx, params)
}

func (s *Store) Audit(ctx context.Context, entry AuditLog) error {
	return insertAudit(ctx, storesql.New(s.db), entry)
}

func (s *Store) AuditLogs(ctx context.Context, before int64, limit int) ([]AuditLog, error) {
	if before == 0 {
		before = math.MaxInt64
	}
	rows, err := storesql.New(s.db).ListAuditLogs(ctx, storesql.ListAuditLogsParams{ID: before, Limit: int64(limit)})
	if err != nil {
		return nil, err
	}
	entries := make([]AuditLog, len(rows))
	for i, row := range rows {
		entry := AuditLog{ID: row.ID, TS: row.Ts, ActorType: row.ActorType, ActorID: row.ActorID,
			ActorName: row.ActorName, Action: row.Action, ResourceType: row.ResourceType,
			ResourceID: row.ResourceID, IP: row.Ip, Result: row.Result, RequestID: row.RequestID}
		if row.Status.Valid {
			status := row.Status.Int64
			entry.Status = &status
		}
		if err = json.Unmarshal([]byte(row.DetailJson), &entry.Detail); err != nil {
			return nil, err
		}
		entries[i] = entry
	}
	return entries, nil
}

// CreateAdmin commits the new administrator and system audit entry atomically.
// BEGIN IMMEDIATE on the writable pool reserves the write lock before either insert.
func (s *Store) CreateAdmin(ctx context.Context, username, hash string) (_ User, retErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return User{}, err
	}
	defer func() {
		if retErr != nil {
			if rollbackErr := tx.Rollback(); !errors.Is(rollbackErr, sql.ErrTxDone) {
				retErr = errors.Join(retErr, rollbackErr)
			}
		}
	}()
	queries := storesql.New(tx)
	user, err := createUser(ctx, queries, username, hash, "admin")
	if err != nil {
		return User{}, err
	}
	err = insertAudit(ctx, queries, AuditLog{ActorType: "system", Action: "users.create_admin",
		ResourceType: "user", ResourceID: user.ID, Result: "success", Detail: AuditDetail{Role: "admin"}})
	if err != nil {
		return User{}, err
	}
	return user, tx.Commit()
}
