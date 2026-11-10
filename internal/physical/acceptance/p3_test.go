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
	"runtime"
	"syscall"
	"testing"

	driver "github.com/go-sql-driver/mysql"

	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/pgtest"
	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/physical/chain"
	"github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

type p3Fixture struct {
	job      chain.Job
	db       *sql.DB
	source   *sql.DB
	repoRoot string
}

func newP3Fixture(t *testing.T) p3Fixture {
	t.Helper()
	pgtest.RequireDSN(t)
	if runtime.GOOS != "linux" || runtime.GOARCH != "amd64" {
		t.Fatal("P3 requires native Linux amd64")
	}
	opts := volumeOptions(t)
	opts.WorkRoot = filepath.Join(opts.WorkRoot, "work")
	opts.BackupRoot = filepath.Join(opts.BackupRoot, "repository")
	must(t, os.Mkdir(opts.WorkRoot, 0o700))
	must(t, os.Mkdir(opts.BackupRoot, 0o700))
	preflight, err := physical.Preflight(t.Context(), opts)
	must(t, err)
	requireLiveSource(t, preflight, linuxAMD64Candidate(t))
	repo, err := local.Open(opts.BackupRoot)
	must(t, err)
	t.Cleanup(func() { must(t, repo.Close()) })
	key := filepath.Join(t.TempDir(), "kek.json")
	must(t, artifact.GenerateKey(key))
	db := pgtest.Open(t)
	must(t, store.Migrate(t.Context(), db))
	cfg := driver.NewConfig()
	cfg.Net, cfg.Addr, cfg.User, cfg.Passwd = "unix", opts.Connection.Socket, opts.Connection.Username, opts.Connection.Password
	connector, err := driver.NewConnector(cfg)
	must(t, err)
	source := sql.OpenDB(connector)
	t.Cleanup(func() { must(t, source.Close()) })
	return p3Fixture{job: chain.Job{Store: store.New(db), Repo: repo, Binary: opts.Binary, WorkRoot: opts.WorkRoot, Connection: opts.Connection, Identity: preflight.Identity, ToolVersion: preflight.Tool.Version, InstanceID: "p3-instance", SourceID: "p3-source", Kind: "full", MaxChainLength: 8, KeyFile: key, Parallel: 1}, db: db, source: source, repoRoot: opts.BackupRoot}
}

func TestP3RealWorker(t *testing.T) {
	fixture := newP3Fixture(t)
	var previous chain.Result
	var parent string
	for index := range 3 {
		if index > 0 {
			advanceCheckpointPast(t, t.Context(), fixture.source, previous.Checkpoints.ToLSN)
			fixture.job.Kind = "incremental"
		}
		snapshot := t.TempDir()
		fixture.job.Binary = extraLSNDirWrapper(t, snapshot)
		fixture.job = preflightP3Job(t, fixture.job, fixture.repoRoot)
		before := p3Snapshot(t, fixture.source)
		result, err := chain.Run(t.Context(), fixture.job)
		must(t, err)
		if after := p3Snapshot(t, fixture.source); before != after {
			t.Fatal("fixture changed during physical snapshot")
		}
		backup, err := fixture.job.Store.Backup(t.Context(), result.BackupID)
		must(t, err)
		metadata, err := physical.ReadMetadata(snapshot)
		must(t, err)
		assertP3ToolBytes(t, backup, metadata, parent, index+1)
		assertP3Continuity(t, result, previous, index)
		assertP3ChainIdentity(t, fixture, backup.ChainID)
		assertP3Object(t, fixture, backup)
		assertP3Ledger(t, fixture.db, backup.ID, 0)
		assertWorkGone(t, fixture.job.WorkRoot)
		t.Logf("P3-A1 backup=%d chain=%s backup_id=%s from_lsn=%d to_lsn=%d raw_tool_bytes=exact authenticated_eof=true snapshot_sha256=%s", index, result.ChainID, result.BackupID, result.Checkpoints.FromLSN, result.Checkpoints.ToLSN, before)
		previous, parent = result, result.BackupID
	}
	t.Run("admission", func(t *testing.T) { p3Admission(t, fixture, previous) })
	t.Run("faults", func(t *testing.T) { p3Faults(t, fixture) })
}

func preflightP3Job(t *testing.T, job chain.Job, repoRoot string) chain.Job {
	t.Helper()
	result, err := physical.Preflight(t.Context(), physical.PreflightOptions{Connection: job.Connection, Binary: "/usr/bin/xtrabackup", WorkRoot: job.WorkRoot, BackupRoot: repoRoot, MinWorkBytes: 1, MinBackupBytes: 1, PageTracking: job.PageTracking})
	must(t, err)
	job.Identity, job.ToolVersion = result.Identity, result.Tool.Version
	return job
}

func assertP3ChainIdentity(t *testing.T, fixture p3Fixture, chainID string) {
	t.Helper()
	var source, uuid, serverVersion, toolVersion string
	must(t, fixture.db.QueryRowContext(t.Context(), "SELECT source_id,server_uuid,server_version,tool_version FROM chains WHERE id=$1", chainID).Scan(&source, &uuid, &serverVersion, &toolVersion))
	want := [4]string{fixture.job.SourceID, fixture.job.Identity.UUID, fixture.job.Identity.Version, fixture.job.ToolVersion}
	if [4]string{source, uuid, serverVersion, toolVersion} != want {
		t.Fatal("stored chain identity differs from real preflight")
	}
}

func assertP3ToolBytes(t *testing.T, backup store.Backup, metadata physical.Metadata, parent string, seq int) {
	t.Helper()
	if backup.Status != "completed" || !backup.LocalPresent || backup.ParentID != parent || backup.Seq != seq {
		t.Fatal("completed backup does not preserve chain identity")
	}
	if !bytes.Equal(backup.CheckpointsRaw, metadata.CheckpointsRaw) || !bytes.Equal(backup.InfoRaw, metadata.InfoRaw) {
		t.Fatal("completed backup does not preserve tool bytes")
	}
}

func assertP3Continuity(t *testing.T, result, previous chain.Result, index int) {
	t.Helper()
	if result.Checkpoints.FromLSN != previous.Checkpoints.ToLSN || result.Checkpoints.ToLSN <= result.Checkpoints.FromLSN {
		t.Fatal("published LSN chain is discontinuous")
	}
	if index > 0 && result.ChainID != previous.ChainID {
		t.Fatal("incremental changed chain identity")
	}
}

func p3Snapshot(t *testing.T, db *sql.DB) string {
	t.Helper()
	hash := sha256.New()
	for _, query := range []string{"SELECT id, CAST(value AS CHAR) FROM fixture.records ORDER BY id", "SELECT id, SHA2(payload,256) FROM fixture.lsn_pressure ORDER BY id"} {
		rows, err := db.QueryContext(t.Context(), query)
		must(t, err)
		for rows.Next() {
			var id int64
			var value string
			must(t, rows.Scan(&id, &value))
			_, err = fmt.Fprintf(hash, "%d:%s\n", id, value)
			must(t, err)
		}
		must(t, rows.Err())
		must(t, rows.Close())
		_, err = hash.Write([]byte{0})
		must(t, err)
	}
	return hex.EncodeToString(hash.Sum(nil))
}

func assertP3Object(t *testing.T, fixture p3Fixture, backup store.Backup) {
	t.Helper()
	dir := filepath.Join(fixture.repoRoot, filepath.FromSlash(backup.ObjectPath))
	opts := artifact.ReadOptions{KeyFile: fixture.job.KeyFile, Encrypt: true, WorkDirectory: t.TempDir(), MaxBytes: 4 << 30}
	identity, err := artifact.ReadFiles(t.Context(), filepath.Join(dir, "envelope.json"), filepath.Join(dir, "payload.bin"), io.Discard, opts)
	must(t, err)
	var id p3Identity
	must(t, json.Unmarshal(identity, &id))
	assertP3Identity(t, id, fixture.job, backup, "xbstream")
	var body bytes.Buffer
	raw, err := artifact.ReadFiles(t.Context(), filepath.Join(dir, "metadata.envelope.json"), filepath.Join(dir, "metadata.bin"), &body, opts)
	must(t, err)
	if !bytes.Equal(body.Bytes(), raw) {
		t.Fatal("metadata payload and authenticated manifest disagree")
	}
	assertP3Metadata(t, raw, fixture.job, backup)
	assertP3Ciphertext(t, dir, backup)
	assertP3Secrets(t, fixture.job, identity, body.Bytes(), backup.InfoRaw)
}

type p3Identity struct {
	Version       int    `json:"version"`
	Role          string `json:"role"`
	InstanceID    string `json:"instance_id"`
	SourceID      string `json:"source_id"`
	BackupID      string `json:"backup_id"`
	ChainID       string `json:"chain_id"`
	ParentID      string `json:"parent_id,omitempty"`
	Seq           int    `json:"seq"`
	Kind          string `json:"kind"`
	ServerUUID    string `json:"server_uuid"`
	ServerVersion string `json:"server_version"`
	ToolVersion   string `json:"tool_version"`
}

func assertP3Identity(t *testing.T, got p3Identity, job chain.Job, backup store.Backup, role string) {
	t.Helper()
	want := p3Identity{Version: 1, Role: role, InstanceID: job.InstanceID, SourceID: job.SourceID, BackupID: backup.ID, ChainID: backup.ChainID, ParentID: backup.ParentID, Seq: backup.Seq, Kind: job.Kind, ServerUUID: job.Identity.UUID, ServerVersion: job.Identity.Version, ToolVersion: job.ToolVersion}
	if got != want {
		t.Fatal("authenticated identity differs from real source or backup")
	}
}

func assertP3Metadata(t *testing.T, raw []byte, job chain.Job, backup store.Backup) {
	t.Helper()
	var metadata struct {
		Identity      p3Identity `json:"identity"`
		Checkpoints   []byte     `json:"checkpoints"`
		Info          []byte     `json:"xtrabackup_info"`
		PayloadSize   int64      `json:"payload_size"`
		PayloadSHA256 string     `json:"payload_sha256"`
	}
	must(t, json.Unmarshal(raw, &metadata))
	assertP3Identity(t, metadata.Identity, job, backup, "tool-metadata")
	if !bytes.Equal(metadata.Checkpoints, backup.CheckpointsRaw) || !bytes.Equal(metadata.Info, backup.InfoRaw) {
		t.Fatal("authenticated metadata does not bind stored tool bytes")
	}
	if metadata.PayloadSize != backup.PayloadSize || metadata.PayloadSHA256 != backup.PayloadSHA256 {
		t.Fatal("authenticated metadata does not bind payload")
	}
}

func assertP3Ciphertext(t *testing.T, dir string, backup store.Backup) {
	t.Helper()
	for _, item := range []struct {
		name, digest string
		size         int64
	}{{"payload.bin", backup.PayloadSHA256, backup.PayloadSize}, {"metadata.bin", backup.MetadataSHA256, backup.MetadataSize}} {
		file, openErr := os.Open(filepath.Join(dir, item.name))
		must(t, openErr)
		hash := sha256.New()
		size, copyErr := io.Copy(hash, file)
		must(t, copyErr)
		must(t, file.Close())
		if size != item.size || hex.EncodeToString(hash.Sum(nil)) != item.digest {
			t.Fatal("stored ciphertext size or SHA256 mismatch")
		}
	}
}

func assertP3Secrets(t *testing.T, job chain.Job, values ...[]byte) {
	t.Helper()
	for _, raw := range values {
		if bytes.Contains(raw, []byte(job.Connection.Password)) || bytes.Contains(raw, []byte(job.KeyFile)) {
			t.Fatal("secret or key path leaked into physical manifest")
		}
	}
}

func assertP3Ledger(t *testing.T, db *sql.DB, backupID string, want int) {
	t.Helper()
	var count int
	must(t, db.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM cleanup_ledger WHERE backup_id=$1", backupID).Scan(&count))
	if count != want {
		t.Fatalf("cleanup ledger count=%d want=%d", count, want)
	}
}

func p3Admission(t *testing.T, fixture p3Fixture, latest chain.Result) {
	t.Helper()
	base := store.PlanRequest{SourceID: fixture.job.SourceID, ServerUUID: fixture.job.Identity.UUID, ServerVersion: fixture.job.Identity.Version, ToolVersion: fixture.job.ToolVersion, Kind: "incremental", MaxChainLength: 8}
	for _, code := range []string{"parent_missing", "server_uuid_changed", "server_version_changed", "tool_version_changed", "chain_length_reached"} {
		request := base
		switch code {
		case "parent_missing":
			request.SourceID = "missing-source"
		case "server_uuid_changed":
			request.ServerUUID = "00000000-0000-0000-0000-000000000000"
		case "server_version_changed":
			request.ServerVersion = "8.4.12"
		case "tool_version_changed":
			request.ToolVersion = "8.4.0-8"
		case "chain_length_reached":
			request.MaxChainLength = 3
		}
		_, err := fixture.job.Store.Plan(t.Context(), request)
		assertP3Admission(t, err, code)
		t.Logf("P3-A3 rejected=%s requires_full=true", code)
	}
	backup, err := fixture.job.Store.Backup(t.Context(), latest.BackupID)
	must(t, err)
	_, err = fixture.db.ExecContext(t.Context(), "UPDATE backups SET status='running' WHERE id=$1", backup.ID)
	must(t, err)
	_, err = fixture.job.Store.Plan(t.Context(), base)
	assertP3Admission(t, err, "parent_not_completed")
	_, err = fixture.db.ExecContext(t.Context(), "UPDATE backups SET status='completed' WHERE id=$1", backup.ID)
	must(t, err)
	t.Log("P3-A3 rejected=parent_not_completed requires_full=true")
	_, err = fixture.db.ExecContext(t.Context(), "UPDATE backups SET parent_id=NULL WHERE id=$1", backup.ID)
	must(t, err)
	_, err = fixture.job.Store.Plan(t.Context(), base)
	assertP3Admission(t, err, "chain_broken")
	_, err = fixture.db.ExecContext(t.Context(), "UPDATE backups SET parent_id=$1 WHERE id=$2", backup.ParentID, backup.ID)
	must(t, err)
	t.Log("P3-A3 rejected=chain_broken requires_full=true")
}

func assertP3Admission(t *testing.T, err error, code string) {
	t.Helper()
	var admission *store.AdmissionError
	if !errors.As(err, &admission) || !admission.RequiresFull || admission.Code != code {
		t.Fatalf("admission=%v want=%s requires_full", err, code)
	}
}

func p3Faults(t *testing.T, fixture p3Fixture) {
	t.Helper()
	for _, fault := range []string{"nonzero", "cancel", "short_write", "close", "fsync", "metadata", "publish", "completion", "late_cancel"} {
		t.Run(fault, func(t *testing.T) { p3Fault(t, fixture, fault) })
	}
}

func p3Fault(t *testing.T, fixture p3Fixture, fault string) {
	t.Helper()
	job := preflightP3Job(t, fixture.job, fixture.repoRoot)
	job.Kind, job.SourceID, job.Binary = "full", "fault-"+fault, "/usr/bin/xtrabackup"
	fixture.job = job
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	injection := p3FaultInjection{t: t, db: fixture.db, fault: fault, cancel: cancel}
	marker := filepath.Join(t.TempDir(), "nonzero-ran")
	if fault == "nonzero" {
		job.Binary = filepath.Join(t.TempDir(), "xtrabackup")
		must(t, os.WriteFile(job.Binary, []byte("#!/bin/sh\nprintf reached > '"+marker+"'\nprintf 'partial-stream'\nexit 23\n"), 0o700))
	}
	result, err := chain.RunWithFault(ctx, job, injection.observe)
	if fault == "completion" {
		_, dropErr := fixture.db.ExecContext(t.Context(), "DROP TRIGGER p3_reject_complete ON backups")
		must(t, dropErr)
	}
	if fault == "nonzero" {
		data, readErr := os.ReadFile(marker)
		must(t, readErr)
		injection.reached = string(data) == "reached"
	}
	if !injection.reached {
		t.Fatalf("fault %s was not reached: %v", fault, err)
	}
	backup, lookupErr := job.Store.Backup(t.Context(), result.BackupID)
	must(t, lookupErr)
	if fault == "late_cancel" {
		assertP3LateCancel(t, ctx, fixture, backup, err)
		return
	}
	assertP3FaultError(t, fault, err)
	assertP3FailedState(t, fixture, backup, fault)
	assertP3FailedCleanup(t, fixture, backup)
	t.Logf("P3-A2 fault=%s completed=false parent=refused ledger=empty pending=gone work=gone", fault)
}

func assertP3FaultError(t *testing.T, fault string, err error) {
	t.Helper()
	if err == nil {
		t.Fatal("injected failure returned success")
	}
	var want error
	switch fault {
	case "cancel":
		want = context.Canceled
	case "short_write":
		want = io.ErrShortWrite
	case "close", "fsync", "metadata", "publish":
		want = syscall.EIO
	}
	if want != nil && !errors.Is(err, want) {
		t.Fatalf("fault %s lost its injected cause: %v", fault, err)
	}
}

type p3FaultInjection struct {
	t       *testing.T
	db      *sql.DB
	fault   string
	cancel  context.CancelFunc
	reached bool
}

func (f *p3FaultInjection) observe(point string) error {
	switch f.fault + "/" + point {
	case "cancel/write":
		f.reached = true
		f.cancel()
		return context.Canceled
	case "short_write/write":
		f.reached = true
		return io.ErrShortWrite
	case "close/close", "fsync/fsync", "metadata/metadata", "publish/published":
		f.reached = true
		return syscall.EIO
	case "completion/before_complete":
		f.reached = true
		_, err := f.db.ExecContext(f.t.Context(), `CREATE OR REPLACE FUNCTION p3_reject_complete() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected completion failure'; END $$`)
		must(f.t, err)
		_, err = f.db.ExecContext(f.t.Context(), `CREATE TRIGGER p3_reject_complete BEFORE UPDATE OF status ON backups FOR EACH ROW WHEN (NEW.status='completed') EXECUTE FUNCTION p3_reject_complete()`)
		must(f.t, err)
	case "late_cancel/published":
		f.reached = true
		f.cancel()
	}
	return nil
}

func assertP3LateCancel(t *testing.T, ctx context.Context, fixture p3Fixture, backup store.Backup, err error) {
	t.Helper()
	must(t, err)
	if backup.Status != "completed" || ctx.Err() == nil {
		t.Fatal("late cancellation revoked completed backup")
	}
	assertP3Object(t, fixture, backup)
	assertP3Ledger(t, fixture.db, backup.ID, 0)
	assertWorkGone(t, fixture.job.WorkRoot)
	t.Log("P3-A2 fault=late_cancel completed=true object=intact")
}

func assertP3FailedState(t *testing.T, fixture p3Fixture, backup store.Backup, fault string) {
	t.Helper()
	if backup.Status == "completed" || backup.LocalPresent {
		t.Fatal("fault produced a completed backup")
	}
	if fault == "publish" || fault == "completion" {
		if backup.Status != "running" {
			t.Fatal("uncertain publication must retain running state")
		}
		assertP3Ledger(t, fixture.db, backup.ID, 3)
		if _, statErr := os.Stat(filepath.Join(fixture.repoRoot, backup.ObjectPath, "payload.bin")); statErr != nil {
			t.Fatal("published object disappeared before recovery")
		}
		return
	}
	if backup.Status != "failed" {
		t.Fatal("pre-publication failure did not fail the row")
	}
	assertP3Ledger(t, fixture.db, backup.ID, 0)
}

func assertP3FailedCleanup(t *testing.T, fixture p3Fixture, backup store.Backup) {
	t.Helper()
	job := fixture.job
	_, planErr := job.Store.Plan(t.Context(), store.PlanRequest{SourceID: job.SourceID, ServerUUID: job.Identity.UUID, ServerVersion: job.Identity.Version, ToolVersion: job.ToolVersion, Kind: "incremental", MaxChainLength: 8})
	assertP3Admission(t, planErr, "parent_not_completed")
	must(t, chain.Recover(t.Context(), job.Store, job.Repo, job.WorkRoot, job.InstanceID, nil))
	assertP3Ledger(t, fixture.db, backup.ID, 0)
	assertWorkGone(t, job.WorkRoot)
	for _, target := range []string{".pending-" + backup.RunID, backup.ObjectPath} {
		if _, statErr := os.Stat(filepath.Join(fixture.repoRoot, target)); !errors.Is(statErr, os.ErrNotExist) {
			t.Fatal("failed staging or published directory survived cleanup")
		}
	}
}
