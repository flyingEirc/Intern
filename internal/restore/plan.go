package restore

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"math"
	"path/filepath"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// BuildPlan authenticates the catalog state, not the payload bytes: it never
// writes the database, any file or the FTP replica. Execute re-runs it and
// refuses when the digest changed.
func BuildPlan(ctx context.Context, opts Options, backupID string) (Plan, error) {
	if err := validateOptions(opts, backupID); err != nil {
		return Plan{}, err
	}
	snapshot, err := resolveChain(ctx, opts, backupID)
	if err != nil {
		return Plan{}, err
	}
	// A bad export root is the more specific refusal than a tool problem, so it
	// is checked before the tool identity: one request must never report
	// tool_version_mismatch while the export root is unusable.
	if err = validateExportConfiguration(opts, backupID); err != nil {
		return Plan{}, err
	}
	if err = validatePlanIdentity(ctx, opts, snapshot, backupID); err != nil {
		return Plan{}, err
	}
	links, err := planLinks(ctx, opts, snapshot.links)
	if err != nil {
		return Plan{}, err
	}
	plan := newPlan(snapshot, links, opts)
	if err = estimateSpace(opts, &plan); err != nil {
		return Plan{}, err
	}
	if plan.Digest, err = planDigest(plan); err != nil {
		return Plan{}, err
	}
	return plan, nil
}

func newPlan(snapshot chainSnapshot, links []PlanLink, opts Options) Plan {
	return Plan{
		TargetBackupID: snapshot.target.ID,
		ChainID:        snapshot.chain.ID,
		SourceID:       snapshot.chain.SourceID,
		ServerUUID:     snapshot.chain.ServerUUID,
		ServerVersion:  snapshot.chain.ServerVersion,
		ToolVersion:    snapshot.chain.ToolVersion,
		TargetToLSN:    snapshot.target.ToLSN,
		Links:          links,
		ExportPath:     filepath.Join(opts.ExportRoot, "{restore_id}"),
		Risks:          risks(opts),
	}
}

func validateOptions(opts Options, backupID string) error {
	if opts.Store == nil || opts.Locator == nil || backupID == "" {
		return planRefusal(CodeUnconfigured, backupID)
	}
	if opts.KeyFile == "" || opts.WorkRoot == "" || opts.ExportRoot == "" || opts.XtrabackupPath == "" || opts.XbstreamPath == "" {
		return planRefusal(CodeUnconfigured, backupID)
	}
	return nil
}

// validatePlanIdentity checks the KEK, the prepare tool and the admission
// matrix before any source is inspected, so those refusals have fixed codes.
// A tool that cannot be probed at all is a configuration fault
// ("unconfigured"), never a version disagreement: only a probe that answers
// with a version the stored chain did not produce is "tool_version_mismatch",
// and only a version pair outside the admission matrix is
// "matrix_unsupported".
func validatePlanIdentity(ctx context.Context, opts Options, snapshot chainSnapshot, backupID string) error {
	if err := checkKeyIdentity(opts, snapshot.links); err != nil {
		return planRefusal(CodeKeyUnavailable, backupID)
	}
	if !configuredTools(opts) {
		return planRefusal(CodeUnconfigured, backupID)
	}
	tool, err := opts.toolVersion(ctx)
	if err != nil {
		return planRefusal(CodeUnconfigured, backupID)
	}
	if err = physical.CheckPrepareVersion(snapshot.chain.ToolVersion, tool.Version); err != nil {
		return planRefusal(CodeToolVersionMismatch, backupID)
	}
	if err = physical.CheckCompatibility(chainIdentity(snapshot.chain), tool, "linux/amd64"); err != nil {
		return planRefusal(CodeMatrixUnsupported, backupID)
	}
	return nil
}

func checkKeyIdentity(opts Options, links []store.Backup) error {
	keyID, err := artifact.KeyID(opts.KeyFile)
	if err != nil {
		return err
	}
	for _, link := range links {
		if link.KeyID != keyID {
			return errors.New("restore: a chain member was sealed with another key")
		}
	}
	return nil
}

func configuredTools(opts Options) bool {
	for _, path := range []string{opts.XtrabackupPath, opts.XbstreamPath} {
		if err := validateExecutable(path, "tool"); err != nil {
			return false
		}
	}
	if opts.MysqldPath != "" {
		return validateExecutable(opts.MysqldPath, "mysqld") == nil
	}
	return true
}

func validateExportConfiguration(opts Options, backupID string) error {
	if err := CheckExportRoot(opts.ExportRoot, append([]string{opts.WorkRoot}, opts.RestrictedRoots...)...); err != nil {
		return planRefusal(CodeUnconfigured, backupID)
	}
	if err := validateKeyOutsideExport(opts.KeyFile, opts.ExportRoot); err != nil {
		return planRefusal(CodeKeyUnavailable, backupID)
	}
	return nil
}

func (o Options) toolVersion(ctx context.Context) (physical.ToolVersion, error) {
	if o.probeTool != nil {
		return o.probeTool(ctx, o.XtrabackupPath)
	}
	return physical.ProbeVersion(ctx, o.XtrabackupPath)
}

func chainIdentity(chain store.Chain) physical.Identity {
	// Chains are admitted only from MySQL Community Server, so the distribution
	// comment is fixed; the exact versions are the stored ones.
	return physical.Identity{UUID: chain.ServerUUID, Version: chain.ServerVersion, VersionComment: "MySQL Community Server - GPL"}
}

func risks(opts Options) []string {
	fixed := []string{RiskNoAutoReplaceProduction, RiskServerUUIDDuplicate, RiskPITRUnavailable}
	if opts.MysqldPath == "" {
		fixed = append(fixed, RiskRehearsalUnavailable)
	}
	return fixed
}

type chainSnapshot struct {
	target store.Backup
	chain  store.Chain
	links  []store.Backup
}

func resolveChain(ctx context.Context, opts Options, backupID string) (chainSnapshot, error) {
	target, err := loadTargetBackup(ctx, opts, backupID)
	if err != nil {
		return chainSnapshot{}, err
	}
	chain, err := opts.Store.Chain(ctx, target.ChainID)
	if errors.Is(err, sql.ErrNoRows) {
		return chainSnapshot{}, planRefusal(CodeChainBroken, backupID)
	}
	if err != nil {
		return chainSnapshot{}, fmt.Errorf("restore: cannot read the chain row: %w", err)
	}
	rows, err := opts.Store.ChainBackups(ctx, target.ChainID)
	if err != nil {
		return chainSnapshot{}, fmt.Errorf("restore: cannot read the chain: %w", err)
	}
	if target.Seq < 1 || len(rows) < target.Seq || rows[target.Seq-1].ID != target.ID {
		return chainSnapshot{}, planRefusal(CodeChainBroken, backupID)
	}
	links := rows[:target.Seq]
	if err = validateLinks(links, backupID); err != nil {
		return chainSnapshot{}, err
	}
	return chainSnapshot{target: target, chain: chain, links: links}, nil
}

func loadTargetBackup(ctx context.Context, opts Options, backupID string) (store.Backup, error) {
	target, err := opts.Store.Backup(ctx, backupID)
	if errors.Is(err, sql.ErrNoRows) {
		return store.Backup{}, planRefusal(CodeBackupNotFound, backupID)
	}
	if err != nil {
		return store.Backup{}, fmt.Errorf("restore: cannot read the backup row: %w", err)
	}
	if target.Status != "completed" {
		return store.Backup{}, planRefusal(CodeBackupNotCompleted, backupID)
	}
	return target, nil
}

func validateLinks(links []store.Backup, backupID string) error {
	for index := range links {
		if err := validateLink(links, index, backupID); err != nil {
			return err
		}
	}
	return nil
}

func validateLink(links []store.Backup, index int, backupID string) error {
	link := links[index]
	if link.Seq != index+1 {
		return planRefusal(CodeChainBroken, backupID)
	}
	if link.Status != "completed" {
		if link.ID == links[len(links)-1].ID {
			return planRefusal(CodeBackupNotCompleted, backupID)
		}
		return planRefusal(CodeChainBroken, backupID)
	}
	if !completeBackupFacts(link) {
		return planRefusal(CodeChainBroken, backupID)
	}
	if index == 0 {
		return validateFullLink(link, backupID)
	}
	return validateIncrementalLink(link, links[index-1], backupID)
}

func completeBackupFacts(link store.Backup) bool {
	return link.PayloadSize > 0 && link.MetadataSize > 0 &&
		link.PayloadSHA256 != "" && link.MetadataSHA256 != "" && link.ToLSN != 0
}

func validateFullLink(link store.Backup, backupID string) error {
	if link.Kind != "physical-full" || link.FromLSN != 0 {
		return planRefusal(CodeChainBroken, backupID)
	}
	return nil
}

func validateIncrementalLink(link, parent store.Backup, backupID string) error {
	if link.Kind != "physical-incremental" {
		return planRefusal(CodeChainBroken, backupID)
	}
	if link.ParentID != parent.ID {
		return planRefusal(CodeChainBroken, backupID)
	}
	if link.FromLSN != parent.ToLSN || link.ToLSN <= link.FromLSN {
		return planRefusal(CodeLSNDiscontinuous, backupID)
	}
	return nil
}

func planLinks(ctx context.Context, opts Options, backups []store.Backup) ([]PlanLink, error) {
	links := make([]PlanLink, 0, len(backups))
	for _, backup := range backups {
		link, ok, err := chooseSource(ctx, opts, backup)
		if err != nil {
			return nil, err
		}
		if !ok {
			return nil, planRefusal(CodeSourceUnavailable, backup.ID)
		}
		links = append(links, link)
	}
	return links, nil
}

func chooseSource(ctx context.Context, opts Options, backup store.Backup) (PlanLink, bool, error) {
	base := PlanLink{
		BackupID: backup.ID, Seq: backup.Seq, Kind: backup.Kind,
		FromLSN: backup.FromLSN, ToLSN: backup.ToLSN,
		PayloadSize: backup.PayloadSize, MetadataSize: backup.MetadataSize,
		objectPath: backup.ObjectPath,
	}
	repository, err := opts.Locator.Local(ctx, backup.ID)
	if err != nil {
		return PlanLink{}, false, fmt.Errorf("restore: cannot resolve the local repository: %w", err)
	}
	if repository != nil && backup.LocalPresent && localMembersMatch(repository, backup) {
		base.Source = "local"
		return base, true, nil
	}
	ref, ok, err := chooseReplica(ctx, opts, backup)
	if err != nil || !ok {
		return PlanLink{}, false, err
	}
	base.Source = "ftp"
	base.RepositoryID = ref.RepositoryID
	return base, true, nil
}

func chooseReplica(ctx context.Context, opts Options, backup store.Backup) (ReplicaRef, bool, error) {
	refs, err := opts.Locator.Replicas(ctx, backup.ID)
	if err != nil {
		return ReplicaRef{}, false, fmt.Errorf("restore: cannot resolve replicas: %w", err)
	}
	if opts.Replication == nil {
		return ReplicaRef{}, false, nil
	}
	for _, ref := range refs {
		if verifiedReplica(ctx, opts, backup, ref) {
			return ref, true, nil
		}
	}
	return ReplicaRef{}, false, nil
}

func localMembersMatch(repository *local.Repository, backup store.Backup) bool {
	sizes := map[string]int64{memberPayload: backup.PayloadSize, memberMetadata: backup.MetadataSize}
	for _, member := range memberOrder {
		size, err := repository.Size(backup.ObjectPath + "/" + member)
		if err != nil || size <= 0 {
			return false
		}
		if expected, ok := sizes[member]; ok && size != expected {
			return false
		}
	}
	return true
}

func verifiedReplica(ctx context.Context, opts Options, backup store.Backup, ref ReplicaRef) bool {
	if ref.RepositoryID == "" || ref.Open == nil {
		return false
	}
	record, ok := verifiedReplicaRecord(ctx, opts, backup, ref)
	if !ok {
		return false
	}
	remote, err := openReplica(ctx, opts, ref)
	if err != nil {
		return false
	}
	matches := replicaSizesMatch(ctx, remote, backup, record)
	// A session that cannot be closed cleanly is not a usable source.
	return matches && remote.Close() == nil
}

func verifiedReplicaRecord(ctx context.Context, opts Options, backup store.Backup, ref ReplicaRef) (replication.ReplicaRecord, bool) {
	record, err := opts.Replication.Replica(ctx, backup.ID, ref.RepositoryID)
	if err != nil {
		return replication.ReplicaRecord{}, false
	}
	if record.Status != "verified" || len(record.Members) != len(memberOrder) {
		return replication.ReplicaRecord{}, false
	}
	return record, true
}

func replicaSizesMatch(ctx context.Context, remote remoteReader, backup store.Backup, record replication.ReplicaRecord) bool {
	for _, member := range memberOrder {
		recorded, ok := record.ReplicaMemberByName(member)
		if !ok || recorded.Size <= 0 {
			return false
		}
		size, err := remote.Size(ctx, backup.ObjectPath+"/"+member)
		if err != nil || size != recorded.Size {
			return false
		}
	}
	return true
}

func openReplica(ctx context.Context, opts Options, ref ReplicaRef) (remoteReader, error) {
	if opts.remote != nil {
		return opts.remote(ctx, ref)
	}
	return ref.Open(ctx)
}

func estimateSpace(opts Options, plan *Plan) error {
	factor := int64(opts.ExpansionFactor)
	if factor == 0 {
		factor = DefaultExpansionFactor
	}
	work, export, code := spaceRequirements(plan.Links, factor)
	if code != "" {
		return planRefusal(code, plan.TargetBackupID)
	}
	workSpace, err := physical.QuerySpace(opts.WorkRoot)
	if err != nil {
		return fmt.Errorf("restore: cannot inspect work-root capacity: %w", err)
	}
	exportSpace, err := physical.QuerySpace(opts.ExportRoot)
	if err != nil {
		return fmt.Errorf("restore: cannot inspect export-root capacity: %w", err)
	}
	plan.RequiredWorkBytes, plan.RequiredExportBytes = work, export
	plan.AvailableWorkBytes, plan.AvailableExportBytes = workSpace.AvailableBytes, exportSpace.AvailableBytes
	if workSpace.AvailableBytes < work || exportSpace.AvailableBytes < export {
		return planRefusal(CodeInsufficientSpace, plan.TargetBackupID)
	}
	if workSpace.Device == exportSpace.Device && workSpace.AvailableBytes < work+export {
		return planRefusal(CodeInsufficientSpace, plan.TargetBackupID)
	}
	return nil
}

// spaceRequirements returns a fixed refusal code when the estimate cannot be
// represented or the factor is invalid.
func spaceRequirements(links []PlanLink, factor int64) (int64, int64, string) {
	if factor < 1 {
		return 0, 0, CodeUnconfigured
	}
	var work, export int64
	for _, link := range links {
		payload := link.PayloadSize
		if payload > math.MaxInt64/(2+factor) || work > math.MaxInt64-payload*(2+factor) {
			return 0, 0, CodeInsufficientSpace
		}
		work += payload * (2 + factor)
		if payload > math.MaxInt64/factor || export > math.MaxInt64-payload*factor {
			return 0, 0, CodeInsufficientSpace
		}
		export += payload * factor
	}
	return work, export, ""
}

// digestPlan is the canonical digest input. Free-space observations, the export
// placeholder and advisory risks are excluded so a plan stays stable while
// capacity changes.
type digestPlan struct {
	Version        int        `json:"version"`
	TargetBackupID string     `json:"target_backup_id"`
	ChainID        string     `json:"chain_id"`
	SourceID       string     `json:"source_id"`
	ServerUUID     string     `json:"server_uuid"`
	ServerVersion  string     `json:"server_version"`
	ToolVersion    string     `json:"tool_version"`
	TargetToLSN    uint64     `json:"target_to_lsn"`
	Links          []PlanLink `json:"links"`
}

func planDigest(plan Plan) (string, error) {
	raw, err := json.Marshal(digestPlan{
		Version: 1, TargetBackupID: plan.TargetBackupID, ChainID: plan.ChainID,
		SourceID: plan.SourceID, ServerUUID: plan.ServerUUID, ServerVersion: plan.ServerVersion,
		ToolVersion: plan.ToolVersion, TargetToLSN: plan.TargetToLSN, Links: plan.Links,
	})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(raw)
	return hex.EncodeToString(sum[:]), nil
}
