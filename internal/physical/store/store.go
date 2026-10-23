// Package store persists physical chains and their cleanup ownership in the
// caller's control-plane SQLite database, independently of the CLI catalog.
package store

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/flyingEirc/Intern/internal/physical/store/internal/storesql"
)

// Store does not own the database. The caller must use foreign_keys(1), WAL,
// synchronous(FULL), busy_timeout and _txlock=immediate, with one connection.
// A single process owner coordinates active runs and recovery.
type Store struct {
	db *sql.DB
}

func New(db *sql.DB) *Store { return &Store{db: db} }

type PlanRequest struct {
	SourceID, ServerUUID, ServerVersion, ToolVersion string
	Kind                                             string
	MaxChainLength                                   int
}

type Parent struct {
	ID             string
	CheckpointsRaw []byte
	ToLSN          uint64
}

type Plan struct {
	Kind     string
	ChainID  string
	NewChain bool
	Seq      int
	Parent   *Parent

	// Preserve the admitted policy for Begin's transactional revalidation.
	request PlanRequest
}

type AdmissionError struct {
	Code         string
	RequiresFull bool
}

func (e *AdmissionError) Error() string {
	return "physical admission: " + e.Code + "; new full backup required"
}

func admission(code string) error { return &AdmissionError{Code: code, RequiresFull: true} }

type BeginRequest struct {
	Plan                                                                                                                    Plan
	SourceID, ServerUUID, ServerVersion, ToolVersion, InstanceID, RunID, BackupID, ObjectPath, StagingToken, WorkDir, KeyID string
	CreatedAt                                                                                                               time.Time
}

type Completion struct {
	BackupID                string
	CheckpointsRaw, InfoRaw []byte
	FromLSN, ToLSN, LastLSN uint64
	PayloadSize             int64
	PayloadSHA256           string
	MetadataSize            int64
	MetadataSHA256          string
	CompletedAt             time.Time
}

type Cleanup struct {
	ID                            int64
	BackupID, Kind, Target, RunID string
}

type Backup struct {
	ID, ChainID, ParentID, Kind, Status, InstanceID, RunID string
	Seq                                                    int
	FromLSN, ToLSN, LastLSN                                uint64
	CheckpointsRaw, InfoRaw                                []byte
	ObjectPath, KeyID, FailureCode                         string
	PayloadSize, MetadataSize                              int64
	PayloadSHA256, MetadataSHA256                          string
	LocalPresent                                           bool
	CreatedAt, CompletedAt                                 time.Time
}

func (s *Store) withTx(ctx context.Context, fn func(*storesql.Queries) error) (retErr error) {
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() {
		if retErr != nil {
			if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
				retErr = errors.Join(retErr, rollbackErr)
			}
		}
	}()
	if err = fn(storesql.New(tx)); err != nil {
		return err
	}
	return tx.Commit()
}

func text(value string) sql.NullString { return sql.NullString{String: value, Valid: true} }

// Fixed-width UTC timestamps retain chronological ordering in SQLite TEXT.
func timestamp(value time.Time) string { return value.UTC().Format("2006-01-02T15:04:05.000000000Z") }
