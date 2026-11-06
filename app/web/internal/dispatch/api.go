package dispatch

import (
	"context"
	"database/sql"
	"encoding/json/v2"
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
)

// TriggerOutcome is one manual trigger result.
type TriggerOutcome int

// Manual trigger outcomes. created and replayed answer 202; conflict answers
// 409; unavailable answers 503 for the manual path (decision 9) while cron
// runs are still created and then fail visibly.
const (
	TriggerCreated TriggerOutcome = iota
	TriggerReplayed
	TriggerConflict
	TriggerNotFound
	TriggerUnavailable
	TriggerInternal
)

// CreatedRun is the 202 body.
type CreatedRun struct {
	RunID string `json:"run_id"`
}

// TriggerRun implements the frozen manual contract: idempotent replay of the
// same key and body is answered before the disabled-policy conflict, so a
// replay still returns 202 with the original run id after the policy is
// disabled. A new key against a disabled policy is a 409 conflict, a different
// body is a 409 conflict, and an unconfigured runtime answers 503 immediately
// (cron runs are recorded as failed instead).
func (d *Dispatcher) TriggerRun(ctx context.Context, policyID, kind, key, requestHash, actorID string) (TriggerOutcome, any) {
	policy, err := d.config.Database.Policy(ctx, policyID)
	if errors.Is(err, store.ErrNotFound) {
		return TriggerNotFound, nil
	}
	if err != nil {
		d.logger.ErrorContext(ctx, "cannot load policy", "policy_id", policyID, "error", err)
		return TriggerInternal, nil
	}
	if outcome, payload, found := d.replay(ctx, policyID, key, requestHash); found {
		return outcome, payload
	}
	if !policy.Enabled {
		return TriggerConflict, nil
	}
	if !d.config.Settings.Preflight() {
		// Decision 9: manual trigger answers 503 immediately; the cron path
		// records failed runs so the gap stays visible.
		return TriggerUnavailable, nil
	}
	run, err := d.config.Database.CreateRun(ctx, store.RunInput{
		PolicyID: policy.ID, SourceID: policy.SourceID, Kind: kind, Trigger: store.RunTriggerManual,
		IdempotencyKey: key, RequestHash: MarshalTriggerBody(kind), ScheduledAt: d.now(), ActorID: &actorID,
	})
	if errors.Is(err, store.ErrConflict) {
		// Lost an idempotency race: answer with the winner when its body matches.
		if outcome, payload, found := d.replay(ctx, policyID, key, MarshalTriggerBody(kind)); found {
			return outcome, payload
		}
		return TriggerConflict, nil
	}
	if err != nil {
		d.logger.ErrorContext(ctx, "cannot create run", "policy_id", policyID, "error", err)
		return TriggerInternal, nil
	}
	// No replicator is opened here: the trigger request never dials the remote.
	// The completion path enqueues, and the replication worker opens the pair
	// (with its catch-up) if completion could not.
	d.logEvent(ctx, run.ID, eventQueued, nil)
	return TriggerCreated, CreatedRun{RunID: run.ID}
}

// replay answers an existing idempotency key: the same policy and body replay
// the stored run, anything else is a conflict, and a read failure is internal.
// found reports whether the key already decided the outcome.
func (d *Dispatcher) replay(ctx context.Context, policyID, key, requestHash string) (TriggerOutcome, any, bool) {
	existing, err := d.config.Database.RunByIdempotencyKey(ctx, key)
	switch {
	case errors.Is(err, store.ErrNotFound):
		return TriggerCreated, nil, false
	case err != nil:
		d.logger.ErrorContext(ctx, "cannot read idempotent run", "key", key, "error", err)
		return TriggerInternal, nil, true
	case existing.PolicyID != policyID || existing.RequestHash != requestHash:
		return TriggerConflict, nil, true
	default:
		return TriggerReplayed, CreatedRun{RunID: existing.ID}, true
	}
}

// MarshalTriggerBody is the canonical request hash input, exported so the
// server can hash the body before the dispatcher call.
func MarshalTriggerBody(kind string) string {
	encoded, err := json.Marshal(triggerBody{Kind: kind})
	if err != nil {
		return kind
	}
	return string(encoded)
}

type triggerBody struct {
	Kind string `json:"kind"`
}

// Protection is the split RPO wire shape: the local completed point and the
// per-repository verified remote points. Uploaded is never treated as verified.
type Protection struct {
	Local  *PointView  `json:"local"`
	Remote []PointView `json:"remote"`
}

// PointView is one protection point.
type PointView struct {
	RepositoryID string `json:"repository_id,omitempty"`
	BackupID     string `json:"backup_id"`
	ChainID      string `json:"chain_id"`
	At           string `json:"at"`
}

// ReplicaView is one replica row on the wire.
type ReplicaView struct {
	BackupID      string  `json:"backup_id"`
	RepositoryID  string  `json:"repository_id"`
	Status        string  `json:"status"`
	Attempts      int     `json:"attempts"`
	NextAttemptAt string  `json:"next_attempt_at"`
	UploadedAt    *string `json:"uploaded_at"`
	VerifiedAt    *string `json:"verified_at"`
	AlertSince    *string `json:"alert_since"`
	LastErrorCode string  `json:"last_error_code"`
	LastReplyCode int     `json:"last_reply_code"`
}

// ReplicaFilter bounds the replica list.
type ReplicaFilter struct {
	SourceID, RepositoryID, Status string
	Limit                          int
}

// BackupView is one backup with its replicas per repository.
type BackupView struct {
	ID            string        `json:"id"`
	ChainID       string        `json:"chain_id"`
	ParentID      *string       `json:"parent_id"`
	Kind          string        `json:"kind"`
	Status        string        `json:"status"`
	Seq           int           `json:"seq"`
	FromLSN       string        `json:"from_lsn"`
	ToLSN         string        `json:"to_lsn"`
	LastLSN       string        `json:"last_lsn"`
	ObjectPath    string        `json:"object_path"`
	PayloadSize   int64         `json:"payload_size"`
	PayloadSHA256 string        `json:"payload_sha256"`
	LocalPresent  bool          `json:"local_present"`
	CompletedAt   *string       `json:"completed_at"`
	Replicas      []ReplicaView `json:"replicas"`
}

// ChainView is one chain with its members.
type ChainView struct {
	ID         string       `json:"id"`
	SourceID   string       `json:"source_id"`
	ServerUUID string       `json:"server_uuid"`
	StartedAt  string       `json:"started_at"`
	SealedAt   *string      `json:"sealed_at"`
	Pinned     bool         `json:"pinned"`
	Members    []BackupView `json:"members"`
}

// Protection reads the local and remote protection points for one source.
//
// The remote point is computed per replica repository, over the union of that
// source's replica rows on the repository across every pair that replicates to
// it: one FTP repository shared by two local repositories has one queue per
// pair, and each queue only proves the copies of its own local repository. The
// union is ordered by chain position and the point is the last item of its
// longest prefix under two rules: a row that is present but not verified ends
// the prefix, so a point is never claimed across a copy this repository has
// not proven; a seq gap skips only the remainder of the chain it falls in and
// the scan continues with the next chain, because nothing in production ever
// closes that gap and a permanently incomplete chain must not pin the point
// for every later one. The point is therefore the latest member of the last
// fully verified chain that is on the repository from seq 1 with no gap - a
// verified incremental whose base never reached the repository is skipped with
// the rest of its chain, and the preceding complete chain stays the point. A
// pair that stops producing cannot pin the point, and verification time is
// never compared: it says when a copy was proven, not where the copy sits in
// the chain.
func (d *Dispatcher) Protection(ctx context.Context, sourceID string) (Protection, error) {
	local, err := replication.LocalPoint(ctx, physicalstore.New(d.config.Database.DB()), sourceID)
	if err != nil {
		return Protection{}, err
	}
	protection := Protection{Remote: []PointView{}}
	if local != nil {
		protection.Local = &PointView{BackupID: local.BackupID, ChainID: local.ChainID, At: store.ScheduleStamp(local.At)}
	}
	remote, err := d.remotePoints(ctx, sourceID)
	if err != nil {
		return Protection{}, err
	}
	protection.Remote = remote
	return protection, nil
}

// remotePoints derives one point per installed replica repository, in the order
// the pairs were installed. A repository that has proven nothing for the source
// has no point and is omitted.
func (d *Dispatcher) remotePoints(ctx context.Context, sourceID string) ([]PointView, error) {
	seen := make(map[string]struct{}, len(d.replicaSnapshot()))
	views := make([]PointView, 0, len(d.replicaSnapshot()))
	for _, slot := range d.replicaSnapshot() {
		if _, ok := seen[slot.replicaID]; ok {
			continue
		}
		seen[slot.replicaID] = struct{}{}
		point, err := replication.RepositoryRemotePoint(ctx, d.config.Database.DB(), slot.replicaID, sourceID)
		if err != nil {
			return nil, err
		}
		if point == nil {
			continue
		}
		views = append(views, PointView{
			RepositoryID: slot.replicaID, BackupID: point.BackupID,
			ChainID: point.ChainID, At: store.ScheduleStamp(point.At),
		})
	}
	return views, nil
}

// Replicas lists replica rows over every installed pair. Each pair holds its
// own queue for a replica repository, so the page is the union of all pairs,
// not one arbitrary pair's queue.
func (d *Dispatcher) Replicas(ctx context.Context, filter ReplicaFilter) ([]ReplicaView, error) {
	limit := filter.Limit
	if limit <= 0 {
		limit = 100
	}
	views := []ReplicaView{}
	for _, slot := range d.replicaSnapshot() {
		if filter.RepositoryID != "" && slot.replicaID != filter.RepositoryID {
			continue
		}
		extended, err := appendReplicas(ctx, slot.replicator, filter, views, limit)
		if err != nil {
			return nil, err
		}
		views = extended
		if len(views) >= limit {
			return views, nil
		}
	}
	return views, nil
}

// appendReplicas forwards the matching rows of one pair, stopping at the
// caller's limit so no queue is read beyond what the page needs.
func appendReplicas(ctx context.Context, replicator *replication.Replicator, filter ReplicaFilter,
	views []ReplicaView, limit int,
) ([]ReplicaView, error) {
	rows, err := replicator.ListAll(ctx)
	if err != nil {
		return nil, err
	}
	for _, row := range rows {
		if filter.SourceID != "" && row.SourceID != filter.SourceID {
			continue
		}
		if filter.Status != "" && row.Status != filter.Status {
			continue
		}
		if len(views) >= limit {
			return views, nil
		}
		views = append(views, replicaView(row, replicator.RepositoryID))
	}
	return views, nil
}

func replicaView(row replication.Replica, repositoryID string) ReplicaView {
	view := ReplicaView{
		BackupID: row.BackupID, RepositoryID: repositoryID, Status: row.Status,
		Attempts: row.Attempts, LastErrorCode: row.LastErrorCode, LastReplyCode: row.LastReplyCode,
	}
	if row.NextAttemptAt != nil {
		view.NextAttemptAt = store.ScheduleStamp(*row.NextAttemptAt)
	}
	view.UploadedAt = stampPointer(row.UploadedAt)
	view.VerifiedAt = stampPointer(row.VerifiedAt)
	view.AlertSince = stampPointer(row.AlertSince)
	return view
}

func stampPointer(value *time.Time) *string {
	if value == nil || value.IsZero() {
		return nil
	}
	text := store.ScheduleStamp(*value)
	return &text
}

// ReplicaRetry implements the manual retry: the backup's pair is resolved first,
// because a replica repository shared by several local repositories has one
// queue per pair and only the owning pair can find the row.
func (d *Dispatcher) ReplicaRetry(ctx context.Context, backupID, repositoryID string) error {
	replicator, err := d.pairForBackup(ctx, backupID, repositoryID)
	if err != nil {
		return err
	}
	if err := replicator.Retry(ctx, backupID); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return store.ErrNotFound
		}
		return err
	}
	return nil
}

// pairForBackup resolves the (local, replica) pair whose queue holds the replica
// row of one backup, from that row's own local_repository_id. The replica row is
// the binding: it names the queue that owns the copy, and every enqueue path
// writes it - including the predecessor pull, which queues a chain base whose
// producing run named no replica repository at all, and a pair the policy moved
// to later. Resolving through the run's snapshot instead answers 404 for exactly
// the row an operator most needs to retry, and the queue then stays blocked
// behind its exhausted predecessor with no API able to repair it.
//
// A row bound to a pair this process never installed - one whose policy moved on
// and whose completed-run snapshot is gone - also reports store.ErrNotFound, which
// the API answers as a 404: no replicator holds the FTP handle that row needs.
func (d *Dispatcher) pairForBackup(ctx context.Context, backupID, repositoryID string) (*replication.Replicator, error) {
	localID, err := d.config.Database.ReplicaLocalRepository(ctx, backupID, repositoryID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, store.ErrNotFound
		}
		return nil, err
	}
	replicator := d.replicatorForPair(localID, repositoryID)
	if replicator == nil {
		return nil, store.ErrNotFound
	}
	return replicator, nil
}

// BackupView reads one backup with its per-repository replicas. The pairs are the
// ones whose replica rows hold this backup - the replica row is the binding, the
// same one the replica list, the repository point and the manual retry read - so
// a copy queued by the predecessor pull is shown even though its producing run
// named no replica repository. Each pair's queue only describes its own
// (local, replica) copy, so a replica repository shared by two local repositories
// answers through both pairs.
func (d *Dispatcher) BackupView(ctx context.Context, backupID string) (*BackupView, error) {
	handle := physicalstore.New(d.config.Database.DB())
	backup, err := handle.Backup(ctx, backupID)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, store.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	view := backupView(backup)
	view.Replicas = []ReplicaView{}
	holders, err := d.config.Database.ReplicaLocalRepositories(ctx, backupID)
	if err != nil {
		return nil, err
	}
	holds := make(map[string]struct{}, len(holders))
	for _, localID := range holders {
		holds[localID] = struct{}{}
	}
	for _, slot := range d.replicaSnapshot() {
		if _, ok := holds[slot.localID]; !ok {
			continue
		}
		replica, err := slot.replicator.Replica(ctx, backupID)
		if errors.Is(err, sql.ErrNoRows) {
			continue
		}
		if err != nil {
			return nil, err
		}
		view.Replicas = append(view.Replicas, replicaView(replica, slot.replicaID))
	}
	return &view, nil
}

// ChainViews reads every chain of one source with their members.
func (d *Dispatcher) ChainViews(ctx context.Context, sourceID string) ([]ChainView, error) {
	storeHandle := physicalstore.New(d.config.Database.DB())
	chains, err := storeHandle.SourceChains(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	views := make([]ChainView, 0, len(chains))
	for _, chain := range chains {
		members, err := storeHandle.ChainBackups(ctx, chain.ID)
		if err != nil {
			return nil, err
		}
		view := ChainView{
			ID: chain.ID, SourceID: chain.SourceID, ServerUUID: chain.ServerUUID,
			StartedAt: store.ScheduleStamp(chain.StartedAt), Pinned: chain.Pinned,
			Members: make([]BackupView, 0, len(members)),
		}
		view.SealedAt = optionalStamp(chain.SealedAt)
		for _, member := range members {
			backup := backupView(member)
			backup.Replicas = []ReplicaView{}
			view.Members = append(view.Members, backup)
		}
		views = append(views, view)
	}
	return views, nil
}

// SourceExists reports whether the control plane knows this source id.
func (d *Dispatcher) SourceExists(ctx context.Context, sourceID string) (bool, error) {
	if _, err := d.config.Database.Source(ctx, sourceID); err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return false, nil
		}
		return false, err
	}
	return true, nil
}

func backupView(backup physicalstore.Backup) BackupView {
	view := BackupView{
		ID: backup.ID, ChainID: backup.ChainID, ParentID: optionalString(backup.ParentID), Kind: backup.Kind,
		Status: backup.Status, Seq: backup.Seq, ObjectPath: backup.ObjectPath,
		PayloadSize: backup.PayloadSize, PayloadSHA256: backup.PayloadSHA256,
		LocalPresent: backup.LocalPresent, CompletedAt: optionalStamp(backup.CompletedAt),
	}
	if backup.FromLSN != 0 {
		view.FromLSN = strconv.FormatUint(backup.FromLSN, 10)
	}
	if backup.ToLSN != 0 {
		view.ToLSN = strconv.FormatUint(backup.ToLSN, 10)
	}
	if backup.LastLSN != 0 {
		view.LastLSN = strconv.FormatUint(backup.LastLSN, 10)
	}
	return view
}

// optionalString renders an unset text field as JSON null. The pointer is the
// parameter's own copy, so the caller may reuse its value afterwards.
func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

// optionalStamp renders a zero time as JSON null and any other time in the
// UTC stamp the rest of the API uses.
func optionalStamp(value time.Time) *string {
	if value.IsZero() {
		return nil
	}
	text := store.ScheduleStamp(value)
	return &text
}

var _ = fmt.Sprintf
