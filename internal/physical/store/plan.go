package store

import (
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"strconv"

	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/physical/store/internal/storesql"
)

func (s *Store) Plan(ctx context.Context, request PlanRequest) (Plan, error) {
	if request.SourceID == "" || request.ServerUUID == "" || request.ServerVersion == "" || request.ToolVersion == "" {
		return Plan{}, errors.New("physical store: incomplete source identity")
	}
	if request.Kind == "full" {
		var id [16]byte
		if _, err := rand.Read(id[:]); err != nil {
			return Plan{}, err
		}
		return Plan{Kind: "full", ChainID: hex.EncodeToString(id[:]), NewChain: true, Seq: 1, request: request}, nil
	}
	if request.Kind != "incremental" || request.MaxChainLength < 1 {
		return Plan{}, errors.New("physical store: invalid backup kind or chain limit")
	}
	var plan Plan
	// Keep the chain identity and all members in one consistent snapshot.
	err := s.withTx(ctx, func(q *storesql.Queries) error {
		var err error
		plan, err = incrementalPlan(ctx, q, request)
		return err
	})
	return plan, err
}

func incrementalPlan(ctx context.Context, q *storesql.Queries, request PlanRequest) (Plan, error) {
	chain, err := q.LatestChain(ctx, request.SourceID)
	if errors.Is(err, sql.ErrNoRows) {
		return Plan{}, admission("parent_missing")
	}
	if err != nil {
		return Plan{}, err
	}
	if err = validateChainIdentity(chain, request); err != nil {
		return Plan{}, err
	}
	backups, err := q.ChainBackups(ctx, chain.ID)
	if err != nil {
		return Plan{}, err
	}
	if len(backups) == 0 {
		return Plan{}, admission("parent_missing")
	}
	if backups[len(backups)-1].Status != "completed" {
		return Plan{}, admission("parent_not_completed")
	}
	if err = validateChain(backups); err != nil {
		return Plan{}, err
	}
	if len(backups) >= request.MaxChainLength {
		return Plan{}, admission("chain_length_reached")
	}
	parent := backups[len(backups)-1]
	lsn, err := strconv.ParseUint(parent.ToLsn.String, 10, 64)
	if err != nil {
		return Plan{}, admission("chain_broken")
	}
	return Plan{
		Kind: "incremental", ChainID: chain.ID, Seq: len(backups) + 1,
		Parent: &Parent{ID: parent.ID, CheckpointsRaw: parent.Checkpoints, ToLSN: lsn}, request: request,
	}, nil
}

func validateChain(backups []storesql.Backup) error {
	var parentID string
	var parentLSN uint64
	for i, backup := range backups {
		cp, err := physical.ParseCheckpoints(backup.Checkpoints)
		if err != nil || backup.Status != "completed" || backup.Seq != int64(i+1) || cp.FromLSN != parentLSN {
			return admission("chain_broken")
		}
		if !checkpointRecordMatches(backup, cp) || !chainLinkMatches(backup, cp, parentID) {
			return admission("chain_broken")
		}
		parentID, parentLSN = backup.ID, cp.ToLSN
	}
	return nil
}

func validateChainIdentity(chain storesql.Chain, request PlanRequest) error {
	if chain.ServerUuid != request.ServerUUID {
		return admission("server_uuid_changed")
	}
	if chain.ServerVersion != request.ServerVersion {
		return admission("server_version_changed")
	}
	if chain.ToolVersion != request.ToolVersion {
		return admission("tool_version_changed")
	}
	if chain.SealedAt.Valid {
		return admission("chain_broken")
	}
	return nil
}

func checkpointRecordMatches(backup storesql.Backup, cp physical.Checkpoints) bool {
	return backup.FromLsn == text(strconv.FormatUint(cp.FromLSN, 10)) &&
		backup.ToLsn == text(strconv.FormatUint(cp.ToLSN, 10)) &&
		backup.LastLsn == text(strconv.FormatUint(cp.LastLSN, 10))
}

func chainLinkMatches(backup storesql.Backup, cp physical.Checkpoints, parentID string) bool {
	if parentID == "" {
		return backup.Kind == "physical-full" && !backup.ParentID.Valid && cp.BackupType == "full-backuped"
	}
	return backup.Kind == "physical-incremental" && backup.ParentID == text(parentID) && cp.BackupType == "incremental"
}
