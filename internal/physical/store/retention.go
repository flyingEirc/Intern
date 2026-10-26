package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/flyingEirc/Intern/internal/physical/store/internal/storesql"
)

// ErrChainPinned and ErrChainIncomplete refuse a whole-chain local delete. They
// are wrapped, so both survive errors.Is through the reservation.
var (
	ErrChainPinned     = errors.New("physical store: chain is pinned")
	ErrChainIncomplete = errors.New("physical store: chain is not fully completed")
)

// Chain is the retained chain identity with its pin flag.
type Chain struct {
	ID, SourceID, ServerUUID, ServerVersion, ToolVersion string
	Pinned                                               bool
	StartedAt, SealedAt                                  time.Time
}

// LocalDelete is one reserved object-ledger row for a whole-chain local delete.
// Completing the row is the caller's durable record that the directory tree is
// gone; the row stays until then, so an interrupted reclaim is retried.
type LocalDelete struct {
	ID       int64
	BackupID string
	Target   string
}

// Chain returns one chain record. A missing chain reports sql.ErrNoRows.
func (s *Store) Chain(ctx context.Context, chainID string) (Chain, error) {
	row, err := storesql.New(s.db).Chain(ctx, chainID)
	if err != nil {
		return Chain{}, err
	}
	return chainRecord(row)
}

// SourceChains returns every chain of one source in creation order.
func (s *Store) SourceChains(ctx context.Context, sourceID string) ([]Chain, error) {
	rows, err := storesql.New(s.db).SourceChains(ctx, sourceID)
	if err != nil {
		return nil, err
	}
	chains := make([]Chain, 0, len(rows))
	for _, row := range rows {
		chain, err := chainRecord(row)
		if err != nil {
			return nil, err
		}
		chains = append(chains, chain)
	}
	return chains, nil
}

// SetPinned is the only writer of chains.pinned. A missing chain reports
// sql.ErrNoRows and changes nothing.
func (s *Store) SetPinned(ctx context.Context, chainID string, pinned bool) error {
	value := int64(0)
	if pinned {
		value = 1
	}
	changed, err := storesql.New(s.db).SetChainPinned(ctx, storesql.SetChainPinnedParams{Pinned: value, ID: chainID})
	if err != nil {
		return err
	}
	if changed != 1 {
		return fmt.Errorf("physical store: chain not found: %w", sql.ErrNoRows)
	}
	return nil
}

// ReserveLocalDelete durably reserves the local payload of every member of one
// completed chain. It writes one object row per member object path and clears
// local_present for the chain in a single transaction, so a crash either leaves
// the chain fully local or fully reserved. It is idempotent: a rerun returns the
// rows still reserved, and a chain that already lost every payload returns no
// rows. Rows, checkpoints, and metadata are never touched.
//
// The caller establishes eligibility first. The pin check inside the transaction
// only closes the race with a concurrent pin; a reservation that already exists
// stays authorized so an interrupted reclaim can finish.
func (s *Store) ReserveLocalDelete(ctx context.Context, chainID string, at time.Time) ([]LocalDelete, error) {
	if chainID == "" || at.IsZero() {
		return nil, errors.New("physical store: incomplete local delete reservation")
	}
	if err := s.withTx(ctx, func(q *storesql.Queries) error { return reserveChain(ctx, q, chainID, timestamp(at)) }); err != nil {
		return nil, err
	}
	return s.LocalDeletePending(ctx, chainID)
}

func reserveChain(ctx context.Context, q *storesql.Queries, chainID, created string) error {
	chain, err := q.Chain(ctx, chainID)
	if err != nil {
		return err
	}
	members, err := localPayloadMembers(ctx, q, chainID)
	if err != nil {
		return err
	}
	// Nothing holds a local payload: an earlier reservation is reclaiming.
	if len(members) == 0 {
		return nil
	}
	if chain.Pinned != 0 {
		return fmt.Errorf("physical store: chain is pinned: %w", ErrChainPinned)
	}
	for _, member := range members {
		if err = q.ReserveChainObject(ctx, storesql.ReserveChainObjectParams{
			BackupID: member.ID, InstanceID: member.InstanceID, RunID: member.RunID,
			Target: member.ObjectPath, CreatedAt: created,
		}); err != nil {
			return err
		}
	}
	_, err = q.ClearChainLocalPresent(ctx, chainID)
	return err
}

// localPayloadMembers returns the members that still declare a local payload.
// An incomplete member anywhere in the chain refuses the whole reservation.
func localPayloadMembers(ctx context.Context, q *storesql.Queries, chainID string) ([]storesql.Backup, error) {
	members, err := q.ChainBackups(ctx, chainID)
	if err != nil {
		return nil, err
	}
	if len(members) == 0 {
		return nil, errors.New("physical store: chain has no payload")
	}
	present := make([]storesql.Backup, 0, len(members))
	for _, member := range members {
		if member.Status != "completed" {
			return nil, fmt.Errorf("physical store: chain is not fully completed: %w", ErrChainIncomplete)
		}
		if member.LocalPresent != 0 {
			present = append(present, member)
		}
	}
	return present, nil
}

// LocalDeletePending returns the whole-chain reservation of one chain, oldest
// row first. A chain without a reservation returns an empty slice. Those rows
// remain the authorization to finish reclaim even if the chain is later pinned.
func (s *Store) LocalDeletePending(ctx context.Context, chainID string) ([]LocalDelete, error) {
	rows, err := storesql.New(s.db).ChainObjectCleanup(ctx, chainID)
	if err != nil {
		return nil, err
	}
	deletes := make([]LocalDelete, 0, len(rows))
	for _, row := range rows {
		deletes = append(deletes, LocalDelete{ID: row.ID, BackupID: row.BackupID, Target: row.Target})
	}
	return deletes, nil
}

// LocalDeletePendingForInstance returns every committed whole-chain local-delete
// reservation for this instance, oldest row first. Recover uses it so a crash
// between reservation and reclaim does not depend on DeleteLocalChain being
// called again. A different instance's reservations are not listed.
func (s *Store) LocalDeletePendingForInstance(ctx context.Context, instanceID string) ([]LocalDelete, error) {
	rows, err := storesql.New(s.db).InstanceObjectCleanup(ctx, instanceID)
	if err != nil {
		return nil, err
	}
	deletes := make([]LocalDelete, 0, len(rows))
	for _, row := range rows {
		deletes = append(deletes, LocalDelete{ID: row.ID, BackupID: row.BackupID, Target: row.Target})
	}
	return deletes, nil
}

// LocalDeleteDone drops one reserved row. Removal is what makes a rerun skip an
// already reclaimed directory.
func (s *Store) LocalDeleteDone(ctx context.Context, id int64) error {
	return storesql.New(s.db).LocalDeleteDone(ctx, id)
}

// LocalDeleteFailed keeps the reserved row and records the attempt.
func (s *Store) LocalDeleteFailed(ctx context.Context, id int64, code string) error {
	return storesql.New(s.db).LocalDeleteFailed(ctx, storesql.LocalDeleteFailedParams{ID: id, LastErrorCode: text(code)})
}

func chainRecord(row storesql.Chain) (Chain, error) {
	chain := Chain{
		ID: row.ID, SourceID: row.SourceID, ServerUUID: row.ServerUuid,
		ServerVersion: row.ServerVersion, ToolVersion: row.ToolVersion, Pinned: row.Pinned != 0,
	}
	var err error
	if chain.StartedAt, err = time.Parse(time.RFC3339Nano, row.StartedAt); err != nil {
		return Chain{}, err
	}
	if row.SealedAt.Valid {
		chain.SealedAt, err = time.Parse(time.RFC3339Nano, row.SealedAt.String)
	}
	return chain, err
}
