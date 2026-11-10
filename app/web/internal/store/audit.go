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
	// PlaintextFTP records that the stored FTP credential is sent without TLS.
	// It is a risk flag for display, never a credential.
	PlaintextFTP bool `json:"plaintext_ftp,omitzero"`
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

type logoutAuditKey struct{}

// WithLogoutAudit attaches the success audit that must commit in the same
// transaction as an explicit logout's session tombstone. Login and password-change
// token rotation also delete a session row; they must not use this value, or they
// would record a false auth.logout.
func WithLogoutAudit(ctx context.Context, entry AuditLog) context.Context {
	entry.Result = "success"
	return context.WithValue(ctx, logoutAuditKey{}, entry)
}

func logoutAuditFrom(ctx context.Context) (AuditLog, bool) {
	entry, ok := ctx.Value(logoutAuditKey{}).(AuditLog)
	if !ok || entry.Action == "" {
		return AuditLog{}, false
	}
	return entry, true
}

// SystemAudit is the success-audit record for a caller that is not an HTTP
// request. Test fixtures and other in-process writers pass it explicitly;
// there is no mutation API that omits the audit.
func SystemAudit(action string, detail AuditDetail) AuditLog {
	return AuditLog{ActorType: "system", Action: action, Result: "success", Detail: detail}
}

func insertSuccessAudit(ctx context.Context, queries *storesql.Queries, entry AuditLog, resourceType, resourceID string) error {
	if entry.Action == "" {
		return errors.New("success audit action is required")
	}
	entry.Result = "success"
	if entry.ResourceType == "" {
		entry.ResourceType = resourceType
	}
	if entry.ResourceID == "" {
		entry.ResourceID = resourceID
	}
	return insertAudit(ctx, queries, entry)
}

func (s *Store) AuditLogs(ctx context.Context, before int64, limit int) ([]AuditLog, error) {
	if before == 0 {
		before = math.MaxInt64
	}
	rows, err := storesql.New(s.db).ListAuditLogs(ctx, storesql.ListAuditLogsParams{ID: before, RowLimit: int64(limit)})
	if err != nil {
		return nil, err
	}
	entries := make([]AuditLog, len(rows))
	for i, row := range rows {
		entry := AuditLog{
			ID: row.ID, TS: row.Ts, ActorType: row.ActorType, ActorID: row.ActorID,
			ActorName: row.ActorName, Action: row.Action, ResourceType: row.ResourceType,
			ResourceID: row.ResourceID, IP: row.Ip, Result: row.Result, RequestID: row.RequestID,
		}
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
