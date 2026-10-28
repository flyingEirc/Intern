package replication

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
)

// ReplicaMember is one recorded remote member of a replica. SHA256 is the local
// ciphertext digest recorded at enqueue; Size is its byte length.
type ReplicaMember struct {
	Name   string
	Size   int64
	SHA256 string
	Status string
}

// ReplicaRecord is the read-only state of one (backup, repository) replica,
// including its members in the fixed upload order.
type ReplicaRecord struct {
	BackupID          string
	RepositoryID      string
	LocalRepositoryID string
	Status            string
	Members           []ReplicaMember
}

// Catalog reads replica state without touching a queue. It performs no writes
// and holds no session; callers own the database.
type Catalog struct {
	DB *sql.DB
}

// NewCatalog binds the read-only accessor to the control-plane database.
func NewCatalog(db *sql.DB) *Catalog { return &Catalog{DB: db} }

// Replica returns one replica row and its members. A missing row reports
// sql.ErrNoRows and is not a remote-object claim.
func (c *Catalog) Replica(ctx context.Context, backupID, repositoryID string) (ReplicaRecord, error) {
	if err := c.validate(backupID, repositoryID); err != nil {
		return ReplicaRecord{}, err
	}
	record, err := c.replicaHeader(ctx, backupID, repositoryID)
	if err != nil {
		return ReplicaRecord{}, err
	}
	if record.Members, err = c.members(ctx, backupID, repositoryID); err != nil {
		return ReplicaRecord{}, err
	}
	slices.SortFunc(record.Members, func(a, b ReplicaMember) int {
		if left, right := memberIndex(a.Name), memberIndex(b.Name); left != right {
			return left - right
		}
		return strings.Compare(a.Name, b.Name)
	})
	return record, nil
}

func (c *Catalog) validate(backupID, repositoryID string) error {
	if c == nil || c.DB == nil {
		return errors.New("replication: no catalog database")
	}
	if backupID == "" || repositoryID == "" {
		return errors.New("replication: backup and repository are required")
	}
	return nil
}

func (c *Catalog) replicaHeader(ctx context.Context, backupID, repositoryID string) (ReplicaRecord, error) {
	record := ReplicaRecord{BackupID: backupID, RepositoryID: repositoryID}
	err := c.DB.QueryRowContext(ctx,
		`SELECT status, local_repository_id FROM replicas WHERE backup_id = ? AND repository_id = ?`,
		backupID, repositoryID).Scan(&record.Status, &record.LocalRepositoryID)
	if errors.Is(err, sql.ErrNoRows) {
		return ReplicaRecord{}, sql.ErrNoRows
	}
	if err != nil {
		return ReplicaRecord{}, fmt.Errorf("replication: cannot read replica: %w", err)
	}
	return record, nil
}

func (c *Catalog) members(ctx context.Context, backupID, repositoryID string) (members []ReplicaMember, retErr error) {
	rows, err := c.DB.QueryContext(ctx,
		`SELECT member, digest, size, status FROM replica_members WHERE backup_id = ? AND repository_id = ? ORDER BY member`,
		backupID, repositoryID)
	if err != nil {
		return nil, fmt.Errorf("replication: cannot read replica members: %w", err)
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	for rows.Next() {
		var member ReplicaMember
		if err = rows.Scan(&member.Name, &member.SHA256, &member.Size, &member.Status); err != nil {
			return nil, fmt.Errorf("replication: cannot read replica member: %w", err)
		}
		members = append(members, member)
	}
	if err = rows.Err(); err != nil {
		return nil, fmt.Errorf("replication: cannot read replica members: %w", err)
	}
	return members, nil
}

// memberIndex keeps the members in their fixed upload order; unknown names sort
// after the known ones in lexical order.
func memberIndex(name string) int {
	for index, member := range memberOrder {
		if member == name {
			return index
		}
	}
	return len(memberOrder) + len(name)
}

// ReplicaMemberByName returns the recorded member or false.
func (r ReplicaRecord) ReplicaMemberByName(name string) (ReplicaMember, bool) {
	for _, member := range r.Members {
		if member.Name == name {
			return member, true
		}
	}
	return ReplicaMember{}, false
}
