package store

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"strconv"

	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/physical/store/internal/storesql"
)

// Begin reserves the running record and all cleanup obligations before the
// caller creates any directories or writes any payload bytes.
func (s *Store) Begin(ctx context.Context, request BeginRequest) error {
	plan := request.Plan
	if !completeOwnership(request) {
		return errors.New("physical store: incomplete backup ownership")
	}
	if !plannedIdentityMatches(request) {
		return admission("chain_broken")
	}
	return s.withTx(ctx, func(q *storesql.Queries) error {
		if err := revalidatePlan(ctx, q, plan); err != nil {
			return err
		}
		created := timestamp(request.CreatedAt)
		if plan.NewChain {
			if err := q.InsertChain(ctx, storesql.InsertChainParams{
				ID: plan.ChainID, SourceID: request.SourceID, ServerUuid: request.ServerUUID,
				ServerVersion: request.ServerVersion, ToolVersion: request.ToolVersion, StartedAt: created,
			}); err != nil {
				return err
			}
		}
		var parent sql.NullString
		if plan.Parent != nil {
			parent = text(plan.Parent.ID)
		}
		if err := q.InsertBackup(ctx, storesql.InsertBackupParams{
			ID: request.BackupID, ChainID: plan.ChainID, Seq: int64(plan.Seq), ParentID: parent,
			Kind: "physical-" + plan.Kind, InstanceID: request.InstanceID, RunID: request.RunID,
			ObjectPath: request.ObjectPath, KeyID: request.KeyID, CreatedAt: created,
		}); err != nil {
			return err
		}
		return insertCleanup(ctx, q, request)
	})
}

func revalidatePlan(ctx context.Context, q *storesql.Queries, plan Plan) error {
	if plan.Kind == "full" {
		if !validFullPlan(plan) {
			return admission("chain_broken")
		}
		return nil
	}
	current, err := incrementalPlan(ctx, q, plan.request)
	if err != nil {
		return err
	}
	if !sameIncrementalPlan(plan, current) {
		return admission("chain_broken")
	}
	return nil
}

// Complete is the success point, after durable object publication. All record
// changes and ledger deletion commit together or remain entirely uncommitted.
func (s *Store) Complete(ctx context.Context, completion Completion) error {
	cp, err := completionCheckpoints(completion)
	if err != nil {
		return err
	}
	return s.withTx(ctx, func(q *storesql.Queries) error {
		backup, err := q.GetBackup(ctx, completion.BackupID)
		if err != nil {
			return err
		}
		if backup.Status != "running" {
			return errors.New("physical store: backup is not running")
		}
		if err = validateCompletion(ctx, q, backup, cp); err != nil {
			return err
		}
		changed, err := q.CompleteBackup(ctx, storesql.CompleteBackupParams{
			ID: completion.BackupID, FromLsn: text(strconv.FormatUint(cp.FromLSN, 10)),
			ToLsn: text(strconv.FormatUint(cp.ToLSN, 10)), LastLsn: text(strconv.FormatUint(cp.LastLSN, 10)),
			Checkpoints: completion.CheckpointsRaw, XtrabackupInfo: completion.InfoRaw,
			PayloadSize: sql.NullInt64{Int64: completion.PayloadSize, Valid: true}, PayloadSha256: text(completion.PayloadSHA256),
			MetadataSize: sql.NullInt64{Int64: completion.MetadataSize, Valid: true}, MetadataSha256: text(completion.MetadataSHA256),
			CompletedAt: text(timestamp(completion.CompletedAt)),
		})
		if err != nil {
			return err
		}
		if changed != 1 {
			return errors.New("physical store: backup is not running")
		}
		return q.DeleteBackupCleanup(ctx, completion.BackupID)
	})
}

func validateCompletion(ctx context.Context, q *storesql.Queries, backup storesql.Backup, cp physical.Checkpoints) error {
	if backup.Kind == "physical-full" {
		if !validFullCompletion(backup, cp) {
			return admission("chain_broken")
		}
		return nil
	}
	return validateIncrementalCompletion(ctx, q, backup, cp)
}

func validateIncrementalCompletion(ctx context.Context, q *storesql.Queries, backup storesql.Backup, cp physical.Checkpoints) error {
	if !backup.ParentID.Valid || cp.BackupType != "incremental" {
		return admission("chain_broken")
	}
	members, err := q.ChainBackups(ctx, backup.ChainID)
	if err != nil {
		return err
	}
	if len(members) < 2 || members[len(members)-1].ID != backup.ID {
		return admission("chain_broken")
	}
	prefix := members[:len(members)-1]
	if err = validateChain(prefix); err != nil {
		return err
	}
	parent := prefix[len(prefix)-1]
	if parent.ID != backup.ParentID.String || parent.Seq+1 != backup.Seq || parent.ToLsn.String != strconv.FormatUint(cp.FromLSN, 10) {
		return admission("chain_broken")
	}
	return nil
}

// Fail never changes a completed backup and intentionally preserves its ledger.
func (s *Store) Fail(ctx context.Context, backupID, code string) error {
	changed, err := storesql.New(s.db).FailBackup(ctx, storesql.FailBackupParams{ID: backupID, FailureCode: text(code)})
	if err != nil {
		return err
	}
	if changed != 1 {
		return errors.New("physical store: backup is not running")
	}
	return nil
}

func insertCleanup(ctx context.Context, q *storesql.Queries, request BeginRequest) error {
	for _, target := range []struct{ kind, path string }{
		{"staging", request.StagingToken}, {"workdir", request.WorkDir}, {"object", request.ObjectPath},
	} {
		if err := q.InsertCleanup(ctx, storesql.InsertCleanupParams{
			BackupID: request.BackupID, InstanceID: request.InstanceID, RunID: request.RunID,
			Kind: target.kind, Target: target.path, CreatedAt: timestamp(request.CreatedAt),
		}); err != nil {
			return err
		}
	}
	return nil
}

func validFullPlan(plan Plan) bool {
	return plan.NewChain && plan.Seq == 1 && plan.Parent == nil && plan.ChainID != ""
}

func sameIncrementalPlan(plan, current Plan) bool {
	return plan.Kind == "incremental" && !plan.NewChain && plan.ChainID == current.ChainID && plan.Seq == current.Seq &&
		plan.Parent != nil && plan.Parent.ID == current.Parent.ID && plan.Parent.ToLSN == current.Parent.ToLSN &&
		bytes.Equal(plan.Parent.CheckpointsRaw, current.Parent.CheckpointsRaw)
}

func completionCheckpoints(completion Completion) (physical.Checkpoints, error) {
	cp, err := physical.ParseCheckpoints(completion.CheckpointsRaw)
	if err != nil || cp.FromLSN != completion.FromLSN || cp.ToLSN != completion.ToLSN || cp.LastLSN != completion.LastLSN {
		return physical.Checkpoints{}, admission("chain_broken")
	}
	if !completeMetadata(completion) {
		return physical.Checkpoints{}, errors.New("physical store: incomplete completion metadata")
	}
	return cp, nil
}

func completeMetadata(completion Completion) bool {
	return len(completion.InfoRaw) != 0 && !completion.CompletedAt.IsZero() && completion.PayloadSize >= 0 &&
		completion.MetadataSize >= 0 && completion.PayloadSHA256 != "" && completion.MetadataSHA256 != ""
}

func validFullCompletion(backup storesql.Backup, cp physical.Checkpoints) bool {
	return backup.Seq == 1 && !backup.ParentID.Valid && cp.FromLSN == 0 && cp.BackupType == "full-backuped"
}

func completeOwnership(request BeginRequest) bool {
	return request.BackupID != "" && request.InstanceID != "" && request.RunID != "" &&
		request.ObjectPath != "" && request.StagingToken != "" && request.WorkDir != "" && !request.CreatedAt.IsZero()
}

func plannedIdentityMatches(request BeginRequest) bool {
	planned := request.Plan.request
	return planned.SourceID == request.SourceID && planned.ServerUUID == request.ServerUUID &&
		planned.ServerVersion == request.ServerVersion && planned.ToolVersion == request.ToolVersion && planned.Kind == request.Plan.Kind
}
