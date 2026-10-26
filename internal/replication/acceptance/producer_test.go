//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/physical/chain"
)

// producerSpec is one source and the member count of each of its chains. The
// first member of a chain is a full backup, the rest are incremental, and every
// chain starts with a new full backup.
type producerSpec struct {
	SourceID string
	Chains   []int
}

// producerSpecs is the fixed produced set. Every chain is real XtraBackup output
// against the real server, so no scenario ever sees a synthetic object.
var producerSpecs = []producerSpec{
	// The continuous chain A1 replicates: one full and two incrementals.
	{SourceID: "r1", Chains: []int{3}},
	// Three links of one chain, so a real gap blocks the link after it. The
	// protection-point scenario uses its own chain because a remote key can
	// belong to exactly one repository.
	{SourceID: "r1-points", Chains: []int{3}},
	{SourceID: "r1-blocked", Chains: []int{3}},
	// Two independent chains: the older one is what retention may reclaim.
	{SourceID: "r1-ret", Chains: []int{2, 2}},
	{SourceID: "r1-corrupt", Chains: []int{1}},
	// Two backups: one is tampered with, one is truncated.
	{SourceID: "r1-tamper", Chains: []int{1, 1}},
	{SourceID: "r1-kek", Chains: []int{1}},
	{SourceID: "r1-quota", Chains: []int{1}},
	{SourceID: "r1-kill", Chains: []int{1}},
	{SourceID: "r1-retry", Chains: []int{1}},
	{SourceID: "r1-backoff", Chains: []int{1}},
	{SourceID: "r1-conflict", Chains: []int{1}},
}

// produce publishes every real chain the scenarios need and records the
// authority they compare with.
func (w *worker) produce() {
	w.t.Helper()
	password := os.Getenv("INTERN_R1_PASSWORD")
	if password == "" {
		w.t.Fatal("real physical fixture credentials missing")
	}
	ctx, cancel := context.WithTimeout(w.t.Context(), 14*time.Minute)
	defer cancel()
	opts := physical.PreflightOptions{
		Connection: physical.Connection{Socket: socketPath, DataDir: dataDir, Username: "intern_bkp", Password: password},
		Binary:     "/usr/bin/xtrabackup", WorkRoot: w.paths.work, BackupRoot: w.paths.repo,
		MinWorkBytes: 1, MinBackupBytes: 1,
	}
	preflight, err := physical.Preflight(ctx, opts)
	must(w.t, err)
	source := openSource(w.t, password)
	w.t.Cleanup(func() { must(w.t, source.Close()) })
	w.manifest = producedManifest{InstanceID: instanceID, ServerVersion: preflight.Identity.Version, ToolVersion: preflight.Tool.Version}
	for _, spec := range producerSpecs {
		w.produceSource(ctx, opts, preflight, source, spec)
	}
	writeManifest(w.t, w.paths.manifest, w.manifest)
	w.t.Logf("R1-A0 produced instance=%s mysql=%s pxb=%s backups=%d chains=%d",
		w.manifest.InstanceID, w.manifest.ServerVersion, w.manifest.ToolVersion, len(w.manifest.Backups), countChains(w.manifest))
}

func countChains(manifest producedManifest) int {
	seen := make(map[string]bool)
	for _, file := range manifest.Backups {
		seen[file.ChainID] = true
	}
	return len(seen)
}

func openSource(t *testing.T, password string) *sql.DB {
	t.Helper()
	cfg := driver.NewConfig()
	cfg.Net, cfg.Addr, cfg.User, cfg.Passwd = "unix", socketPath, "intern_bkp", password
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 5*time.Second, 30*time.Second, 30*time.Second
	connector, err := driver.NewConnector(cfg)
	must(t, err)
	db := sql.OpenDB(connector)
	must(t, db.PingContext(t.Context()))
	return db
}

// produceSource publishes every chain of one source in order, recording the
// owner's view of each published backup.
func (w *worker) produceSource(ctx context.Context, opts physical.PreflightOptions, preflight physical.PreflightResult, source *sql.DB, spec producerSpec) {
	w.t.Helper()
	for _, count := range spec.Chains {
		w.produceChain(ctx, opts, preflight, source, spec.SourceID, count)
	}
}

// produceChain publishes one chain: a real full backup followed by count-1 real
// incrementals, each continuing the previous real checkpoint.
func (w *worker) produceChain(ctx context.Context, opts physical.PreflightOptions, preflight physical.PreflightResult, source *sql.DB, sourceID string, count int) {
	w.t.Helper()
	var previous chain.Result
	for index := range count {
		if index > 0 {
			advanceCheckpointPast(w.t, ctx, source, previous.Checkpoints.ToLSN)
		}
		result, digest := w.runBackup(ctx, opts, preflight, source, sourceID, index)
		if index > 0 && (result.ChainID != previous.ChainID || result.Checkpoints.FromLSN != previous.Checkpoints.ToLSN) {
			w.t.Fatal("incremental did not continue the produced chain")
		}
		w.recordProduced(ctx, result, sourceID, digest)
		w.t.Logf("R1-A0 source=%s chain=%s seq=%d backup=%s from_lsn=%d to_lsn=%d",
			sourceID, result.ChainID, result.Seq, result.BackupID, result.Checkpoints.FromLSN, result.Checkpoints.ToLSN)
		previous = result
	}
}

// runBackup runs one real XtraBackup snapshot into the repository and requires
// the source data to be unchanged by the snapshot.
func (w *worker) runBackup(ctx context.Context, opts physical.PreflightOptions, preflight physical.PreflightResult, source *sql.DB, sourceID string, index int) (chain.Result, string) {
	w.t.Helper()
	job := chain.Job{
		Store: w.store, Repo: w.repo, Binary: "/usr/bin/xtrabackup", WorkRoot: w.paths.work,
		Connection: opts.Connection, Identity: preflight.Identity, ToolVersion: preflight.Tool.Version,
		InstanceID: instanceID, SourceID: sourceID, Kind: "full", MaxChainLength: 8,
		KeyFile: w.paths.key, Parallel: 1,
	}
	if index > 0 {
		job.Kind = "incremental"
	}
	before := snapshot(w.t, ctx, source)
	result, err := chain.Run(ctx, job)
	must(w.t, err)
	if after := snapshot(w.t, ctx, source); before != after {
		w.t.Fatal("fixture data changed during the physical snapshot")
	}
	return result, before
}

// recordProduced checks the published object against the completion record and
// appends the authority the replication scenarios compare with.
func (w *worker) recordProduced(ctx context.Context, result chain.Result, sourceID, snapshotDigest string) {
	w.t.Helper()
	backup, err := w.store.Backup(ctx, result.BackupID)
	must(w.t, err)
	if backup.Status != "completed" || !backup.LocalPresent || backup.ObjectPath != result.ObjectPath {
		w.t.Fatal("published backup is not completed and locally present")
	}
	file := producedFile{
		SourceID: sourceID, BackupID: backup.ID, ChainID: backup.ChainID, Seq: backup.Seq,
		ObjectPath: backup.ObjectPath, PayloadSize: backup.PayloadSize, PayloadSHA256: backup.PayloadSHA256,
		MetadataSize: backup.MetadataSize, MetadataSHA256: backup.MetadataSHA256, SnapshotSHA256: snapshotDigest,
		FromLSN: backup.FromLSN, ToLSN: backup.ToLSN,
	}
	file.EnvelopeSHA256 = w.digestLocal(backup.ObjectPath, "envelope.json")
	file.MetaEnvelopeSHA256 = w.digestLocal(backup.ObjectPath, "metadata.envelope.json")
	// Both authenticated manifests must decode from the published object, so a
	// replication failure can never be blamed on a broken published artifact.
	w.assertAuthenticated(ctx, backup.ObjectPath)
	w.manifest.Backups = append(w.manifest.Backups, file)
}

func (w *worker) digestLocal(objectPath, member string) string {
	w.t.Helper()
	file, err := w.repo.Open(objectPath + "/" + member)
	must(w.t, err)
	defer func() { must(w.t, file.Close()) }()
	hash := sha256.New()
	_, err = io.Copy(hash, file)
	must(w.t, err)
	return hex.EncodeToString(hash.Sum(nil))
}

// assertAuthenticated decodes both v1 artifacts of one published object with the
// chain KEK and requires the metadata body to equal its authenticated manifest.
func (w *worker) assertAuthenticated(ctx context.Context, objectPath string) {
	w.t.Helper()
	dir, err := os.MkdirTemp(w.paths.work, "authenticate-")
	must(w.t, err)
	w.t.Cleanup(func() { must(w.t, os.RemoveAll(dir)) })
	for _, member := range members {
		w.copyLocal(objectPath, member, dir)
	}
	opts := artifact.ReadOptions{KeyFile: w.paths.key, Encrypt: true, WorkDirectory: dir, MaxBytes: 4 << 30}
	if _, err = artifact.ReadFiles(ctx, filepath.Join(dir, "envelope.json"), filepath.Join(dir, "payload.bin"), io.Discard, opts); err != nil {
		w.t.Fatal("published payload artifact does not authenticate")
	}
	var body bytes.Buffer
	raw, err := artifact.ReadFiles(ctx, filepath.Join(dir, "metadata.envelope.json"), filepath.Join(dir, "metadata.bin"), &body, opts)
	must(w.t, err)
	if !bytes.Equal(body.Bytes(), raw) {
		w.t.Fatal("published metadata artifact disagrees with its manifest")
	}
}

func (w *worker) copyLocal(objectPath, member, dir string) {
	w.t.Helper()
	source, err := w.repo.Open(objectPath + "/" + member)
	must(w.t, err)
	defer func() { must(w.t, source.Close()) }()
	target, err := os.Create(filepath.Join(dir, member)) //nolint:gosec // Private 0700 work directory.
	must(w.t, err)
	_, err = io.Copy(target, source)
	must(w.t, errors.Join(err, target.Close()))
}

// writeManifest atomically publishes the produced authority.
func writeManifest(t *testing.T, path string, manifest producedManifest) {
	t.Helper()
	raw, err := json.Marshal(manifest)
	must(t, err)
	temporary := path + ".tmp"
	must(t, os.WriteFile(temporary, raw, 0o600))
	must(t, os.Rename(temporary, path))
}

// snapshot digests the fixture's observable data: every record value in order
// plus the size and identity span of the write-pressure table.
func snapshot(t *testing.T, ctx context.Context, db *sql.DB) string {
	t.Helper()
	hash := sha256.New()
	rows, err := db.QueryContext(ctx, "SELECT id, CAST(value AS CHAR) FROM fixture.records ORDER BY id")
	must(t, err)
	for rows.Next() {
		var id int64
		var value string
		must(t, rows.Scan(&id, &value))
		if _, err = fmt.Fprintf(hash, "%d:%s\n", id, value); err != nil {
			must(t, err)
		}
	}
	must(t, errors.Join(rows.Err(), rows.Close()))
	var count, sum int64
	must(t, db.QueryRowContext(ctx, "SELECT COUNT(*), COALESCE(SUM(id), 0) FROM fixture.lsn_pressure").Scan(&count, &sum))
	if _, err = fmt.Fprintf(hash, "pressure:%d:%d\n", count, sum); err != nil {
		must(t, err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

// advanceCheckpointPast writes more than the redo capacity so the server moves
// its checkpoint past the parent to_lsn, as P3 does for its incremental chain.
func advanceCheckpointPast(t *testing.T, ctx context.Context, db *sql.DB, after uint64) {
	t.Helper()
	limit := redoCapacity(t, ctx, db) * 2
	payload := bytes.Repeat([]byte{'a'}, 256*1024)
	deadline := time.Now().Add(120 * time.Second)
	var written uint64
	for {
		if written < limit {
			_, err := db.ExecContext(ctx, "INSERT INTO fixture.lsn_pressure(payload) VALUES (?)", payload)
			must(t, err)
			written += uint64(len(payload))
		}
		if serverCheckpoint(t, ctx, db) > after {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("server checkpoint did not pass the parent to_lsn")
		}
		if written >= limit {
			time.Sleep(100 * time.Millisecond)
		}
	}
}

func redoCapacity(t *testing.T, ctx context.Context, db *sql.DB) uint64 {
	t.Helper()
	var value string
	must(t, db.QueryRowContext(ctx, "SELECT VARIABLE_VALUE FROM performance_schema.global_variables WHERE VARIABLE_NAME = 'innodb_redo_log_capacity'").Scan(&value))
	capacity, err := strconv.ParseUint(value, 10, 64)
	must(t, err)
	if capacity == 0 {
		t.Fatal("innodb_redo_log_capacity is zero")
	}
	return capacity
}

func serverCheckpoint(t *testing.T, ctx context.Context, db *sql.DB) uint64 {
	t.Helper()
	var raw string
	must(t, db.QueryRowContext(ctx, "SELECT storage_engines FROM performance_schema.log_status").Scan(&raw))
	var body struct {
		InnoDB struct {
			Checkpoint uint64 `json:"LSN_checkpoint"`
		} `json:"InnoDB"`
	}
	if err := json.Unmarshal([]byte(raw), &body); err != nil || body.InnoDB.Checkpoint == 0 {
		t.Fatalf("cannot read the InnoDB checkpoint: %v", err)
	}
	return body.InnoDB.Checkpoint
}
