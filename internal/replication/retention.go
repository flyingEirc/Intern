// Local retention for completed physical chains. Retention is chain-level only.
// A new local-delete reservation is allowed only when every member is verified
// on the remote repository, the chain is not pinned, and a newer chain of the
// same source is fully verified. A committed reservation is the authorization
// to finish reclaim: a later pin, or any other loss of eligibility, does not
// stop it. Pin only refuses a new reservation. Nothing here runs automatically.
package replication

import (
	"context"
	"errors"
	"time"

	"github.com/flyingEirc/Intern/internal/physical/store"
)

// Retention reasons are fixed, safe codes a caller may persist and display.
const (
	// RetentionPinned refuses a chain a user pinned.
	RetentionPinned = "pinned"
	// RetentionIncomplete refuses a chain with a member that is not completed.
	RetentionIncomplete = "incomplete"
	// RetentionUnverified refuses a chain that is not fully verified on this
	// repository.
	RetentionUnverified = "unverified"
	// RetentionLatest refuses the newest chain of a source.
	RetentionLatest = "latest"
	// RetentionNoNewerVerified refuses a chain without a newer fully verified
	// chain, so the verified prefix always keeps its newest chain.
	RetentionNoNewerVerified = "no_newer_verified"
)

// IneligibleError reports why one chain may not lose its local payload.
type IneligibleError struct{ ChainID, Reason string }

func (e *IneligibleError) Error() string {
	return "replication retention: chain " + e.ChainID + " is not eligible: " + e.Reason
}

// Eligible reports whether one whole chain may start a new local-payload
// reservation. It reads state only and never deletes anything. A chain that
// already has a committed reservation is finished by DeleteLocalChain or Recover
// even if Eligible would now refuse it.
func (r *Replicator) Eligible(ctx context.Context, chainID string) (bool, string, error) {
	ok, reason, err := r.eligibility(ctx, chainID)
	return ok, reason, err
}

func (r *Replicator) eligibility(ctx context.Context, chainID string) (bool, string, error) {
	chain, err := r.Store.Chain(ctx, chainID)
	if err != nil {
		return false, "", err
	}
	if chain.Pinned {
		return false, RetentionPinned, nil
	}
	members, err := r.Store.ChainBackups(ctx, chainID)
	if err != nil {
		return false, "", err
	}
	if !allCompleted(members) {
		return false, RetentionIncomplete, nil
	}
	chains, err := r.Store.SourceChains(ctx, chain.SourceID)
	if err != nil {
		return false, "", err
	}
	index := chainIndex(chains, chainID)
	if index < 0 {
		return false, "", errors.New("replication retention: chain is not listed for its source")
	}
	verified, err := r.verifiedWith(ctx, chains)
	if err != nil {
		return false, "", err
	}
	if !verified[chainID] {
		return false, RetentionUnverified, nil
	}
	return newerVerified(chains[index+1:], verified)
}

// newerVerified requires a strictly newer chain that is itself fully verified.
// An empty newer set is the source's latest chain.
func newerVerified(newer []store.Chain, verified map[string]bool) (bool, string, error) {
	if len(newer) == 0 {
		return false, RetentionLatest, nil
	}
	for _, chain := range newer {
		if verified[chain.ID] {
			return true, "", nil
		}
	}
	return false, RetentionNoNewerVerified, nil
}

func chainIndex(chains []store.Chain, chainID string) int {
	for i, chain := range chains {
		if chain.ID == chainID {
			return i
		}
	}
	return -1
}

// verifiedWith returns which chains of one ordered source list are fully
// verified on this repository.
func (r *Replicator) verifiedWith(ctx context.Context, chains []store.Chain) (map[string]bool, error) {
	members := make(map[string][]store.Backup, len(chains))
	ids := make([]string, 0, len(chains))
	for _, chain := range chains {
		backups, err := r.Store.ChainBackups(ctx, chain.ID)
		if err != nil {
			return nil, err
		}
		members[chain.ID] = backups
		for _, backup := range backups {
			ids = append(ids, backup.ID)
		}
	}
	statuses, err := r.ReplicaStatuses(ctx, ids)
	if err != nil {
		return nil, err
	}
	verified := make(map[string]bool, len(chains))
	for id, backups := range members {
		verified[id] = allVerified(backups, statuses)
	}
	return verified, nil
}

func allCompleted(members []store.Backup) bool {
	if len(members) == 0 {
		return false
	}
	for _, member := range members {
		if member.Status != "completed" {
			return false
		}
	}
	return true
}

func allVerified(members []store.Backup, statuses map[string]string) bool {
	if !allCompleted(members) {
		return false
	}
	for _, member := range members {
		if statuses[member.ID] != replicaVerified {
			return false
		}
	}
	return true
}

// DeleteLocalChain removes the local payload directories of one chain. The
// rows, checkpoints, and metadata stay, and no other chain is touched.
//
// Eligibility is required only to create a reservation. If the chain already
// has pending local-delete rows, those rows are reclaimed without checking
// eligibility again. Otherwise this checks eligibility, reserves, then reclaims.
//
// Every object path is reserved in the physical cleanup ledger, together with
// local_present=0, before a directory is touched. A crash therefore leaves either
// a fully local chain or a durable reservation, and running this again or Recover
// reclaims whatever is left, so a repeated or resumed call is safe.
func (r *Replicator) DeleteLocalChain(ctx context.Context, chainID string) error {
	reserved, err := r.Store.LocalDeletePending(ctx, chainID)
	if err != nil {
		return err
	}
	if len(reserved) > 0 {
		return r.reclaimReserved(ctx, reserved)
	}
	eligible, reason, err := r.eligibility(ctx, chainID)
	if err != nil {
		return err
	}
	if !eligible {
		return &IneligibleError{ChainID: chainID, Reason: reason}
	}
	reserved, err = r.Store.ReserveLocalDelete(ctx, chainID, r.retentionNow())
	if err != nil {
		return err
	}
	return r.reclaimReserved(ctx, reserved)
}

// recoverLocalDeletes finishes every pending local-delete reservation for this
// instance. A committed reservation stays authorized after a later pin.
func (r *Replicator) recoverLocalDeletes(ctx context.Context) error {
	reserved, err := r.Store.LocalDeletePendingForInstance(ctx, r.InstanceID)
	if err != nil {
		return err
	}
	return r.reclaimReserved(ctx, reserved)
}

// reclaimReserved finishes one reservation. A row is removed only after its
// directory is durably gone, so a failure keeps the remaining work recorded.
func (r *Replicator) reclaimReserved(ctx context.Context, reserved []store.LocalDelete) error {
	var result error
	for _, row := range reserved {
		if err := ctx.Err(); err != nil {
			return errors.Join(result, err)
		}
		if err := r.Local.ReclaimPublished(row.Target); err != nil {
			result = errors.Join(result, err, r.Store.LocalDeleteFailed(ctx, row.ID, "cleanup_failed"))
			continue
		}
		result = errors.Join(result, r.Store.LocalDeleteDone(ctx, row.ID))
	}
	return result
}

func (r *Replicator) retentionNow() time.Time {
	if r.Now != nil {
		return r.Now()
	}
	return time.Now()
}

const replicaVerified = "verified"
