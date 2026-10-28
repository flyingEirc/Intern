//go:build integration

package acceptance

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	client "github.com/jlaffaye/ftp"

	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
	"github.com/flyingEirc/Intern/internal/restore"
)

var errInjectedDisconnect = errors.New("injected transport disconnect")

// a3FTP removes every local payload, proves the plan stays FTP-only and does not
// write the replica, then restores both targets from FTP. The first download is
// interrupted mid-stream through the documented integration hook so the second
// attempt must resume at the recorded offset.
func a3FTP(t *testing.T, w *worker) {
	for _, file := range w.manifest.Backups {
		must(t, w.repo.ReclaimPublished(file.ObjectPath))
	}
	opts := w.options()
	first := w.planFor(t, opts, w.manifest.backup(t, 1).BackupID)
	second := w.planFor(t, opts, w.manifest.backup(t, 2).BackupID)
	if sourcesOf(first) != "ftp,ftp" || sourcesOf(second) != "ftp,ftp,ftp" {
		t.Fatalf("FTP plan sources = %s / %s", sourcesOf(first), sourcesOf(second))
	}
	assertFTPPlanReadOnly(t, w, opts, second)
	w.logf("P4-A3 plan target=inc1 sources=%s verified_only=true ftp_writes=0", sourcesOf(first))
	w.logf("P4-A1 ftp_plan_readonly writes=0 remote_state=unchanged")
	restoreFTPResume(t, w, opts, first)
	restoreFTPPlain(t, w, opts, second)
	w.logf("P4-A3 local_payloads=removed replicas=3 verified=true")
}

// assertFTPPlanReadOnly re-plans against the real replica and proves the whole
// remote namespace is byte-identical afterwards.
func assertFTPPlanReadOnly(t *testing.T, w *worker, opts restore.Options, plan restore.Plan) {
	t.Helper()
	remoteBefore := w.remoteDigest(t)
	if _, err := restore.BuildPlan(t.Context(), opts, plan.TargetBackupID); err != nil {
		t.Fatalf("FTP plan: %v", err)
	}
	if remoteBefore != w.remoteDigest(t) {
		t.Fatal("planning from FTP wrote the replica")
	}
	for _, link := range plan.Links {
		if link.RepositoryID != ftpRepoID {
			t.Fatal("an FTP link does not name its replica repository")
		}
	}
}

func restoreFTPResume(t *testing.T, w *worker, opts restore.Options, plan restore.Plan) {
	t.Helper()
	file := w.manifest.backup(t, 1)
	request := restore.Request{
		RestoreID: randomRestoreID(t), Plan: plan, Rehearse: true,
		VerifySQL: []string{"SELECT COUNT(*) FROM fixture.records", "SELECT MAX(id) FROM fixture.records"},
	}
	var offsets []int64
	injected := false
	fault := func(_ int, _ string, offset int64) (int64, error) {
		offsets = append(offsets, offset)
		if injected {
			return 0, nil
		}
		injected = true
		return 64 << 10, errInjectedDisconnect
	}
	progress := &events{}
	result, err := restore.ExecuteWithFetchFault(t.Context(), opts, request, progress.report, fault)
	if err != nil {
		w.logf("FTP restore failed:\n%s", failureTree(err))
		t.Fatalf("FTP restore: %v", err)
	}
	resumed := maxOffset(offsets)
	if !injected || resumed != 64<<10 {
		t.Fatalf("resume offset = %d (injected=%v), want the injected 65536", resumed, injected)
	}
	w.assertRestored(t, file, request, result)
	w.assertRehearsal(t, file, result.Verify)
	if progress.pgidOK {
		supervisorEvidence = true
	}
	w.facts.prepareArgs[request.RestoreID] = result.PrepareArgs
	w.logf("P4-A3 restore=inc1 source=ftp injection=disconnect rest_offset=%d attempts=%d authenticated_eof=true export=%s expected_snapshot=%s",
		resumed, len(offsets), request.RestoreID, file.SnapshotSHA256)
}

func maxOffset(offsets []int64) int64 {
	var highest int64
	for _, offset := range offsets {
		highest = max(highest, offset)
	}
	return highest
}

func restoreFTPPlain(t *testing.T, w *worker, opts restore.Options, plan restore.Plan) {
	t.Helper()
	file := w.manifest.backup(t, 2)
	request := restore.Request{
		RestoreID: randomRestoreID(t), Plan: plan, Rehearse: true,
		VerifySQL: []string{"SELECT COUNT(*) FROM fixture.records", "SELECT MAX(id) FROM fixture.records"},
	}
	result, err := w.execute(t, opts, request, &events{})
	if err != nil {
		w.logf("FTP restore inc2 failed:\n%s", failureTree(err))
		t.Fatalf("FTP restore inc2: %v", err)
	}
	w.assertRestored(t, file, request, result)
	w.assertRehearsal(t, file, result.Verify)
	w.facts.prepareArgs[request.RestoreID] = result.PrepareArgs
	w.logf("P4-A3 restore=inc2 source=ftp export=%s expected_snapshot=%s rows=%d max_id=%d",
		request.RestoreID, file.SnapshotSHA256, file.RecordsRows, file.MaxRecordID)
}

// cloneControl copies the checkpointed database so a rejection mutation cannot
// leak into the next case.
func (w *worker) cloneControl(t *testing.T) (*sql.DB, *physicalstore.Store, *replication.Catalog, workerLocator) {
	t.Helper()
	_, err := w.db.ExecContext(t.Context(), "PRAGMA wal_checkpoint(TRUNCATE)")
	must(t, err)
	target := filepath.Join(t.TempDir(), "clone.db")
	source, err := os.ReadFile(w.paths.db)
	must(t, err)
	must(t, os.WriteFile(target, source, 0o600))
	db := openControlDB(t, target)
	return db, physicalstore.New(db), replication.NewCatalog(db), workerLocator{w: w, db: db}
}

type rejectCase struct {
	name   string
	code   string
	stage  string
	mutate func(*testing.T, *worker, *rejection) (restore.Options, string, *restore.Request)
}

// rejection carries the per-case state a mutation may need.
type rejection struct {
	db    *sql.DB
	store *physicalstore.Store
}

func a4Reject(t *testing.T, w *worker) {
	must(t, os.WriteFile(w.toolLog(), nil, 0o600))
	t.Setenv("INTERN_P4_TOOL_LOG", w.toolLog())
	for _, test := range rejectionCases() {
		t.Run(test.name, func(t *testing.T) { runRejection(t, w, test) })
	}
	w.a4FTP(t)
}

func runRejection(t *testing.T, w *worker, test rejectCase) {
	t.Helper()
	must(t, os.WriteFile(w.toolLog(), nil, 0o600))
	db, store, catalog, locator := w.cloneControl(t)
	opts, backupID, executeRequest := test.mutate(t, w, &rejection{db: db, store: store})
	opts.Store, opts.Replication, opts.Locator = store, catalog, locator
	before := strings.Join(entryNames(t, w.paths.export), ",")
	if executeRequest != nil {
		_, err := restore.Execute(t.Context(), opts, *executeRequest, nil)
		w.assertRejection(t, test, err)
	} else {
		_, err := restore.BuildPlan(t.Context(), opts, backupID)
		w.assertRejection(t, test, err)
	}
	if before != strings.Join(entryNames(t, w.paths.export), ",") {
		t.Fatal("the rejection changed the export root")
	}
	for _, entry := range entryNames(t, w.paths.work) {
		if strings.HasPrefix(entry, "restore-") {
			t.Fatal("the rejection left a work directory")
		}
	}
	assertNoPrepare(t, w)
	w.logf("P4-A4 rejected=%s code=%s stage=%s export_unchanged=true work_gone=true prepare_calls=0",
		test.name, test.code, test.stage)
}

func assertNoPrepare(t *testing.T, w *worker) {
	t.Helper()
	lines, err := os.ReadFile(w.toolLog())
	must(t, err)
	if prepareCalls(string(lines)) != 0 {
		t.Fatalf("the rejection invoked prepare: %s", lines)
	}
}

func rejectionCases() []rejectCase {
	return []rejectCase{
		{
			name: "backup_not_found", code: restore.CodeBackupNotFound, stage: "plan",
			mutate: func(t *testing.T, w *worker, _ *rejection) (restore.Options, string, *restore.Request) {
				return w.rejectOptions(), strings.Repeat("f", 32), nil
			},
		},
		{
			name: "backup_not_completed", code: restore.CodeBackupNotCompleted, stage: "plan",
			mutate: func(t *testing.T, w *worker, r *rejection) (restore.Options, string, *restore.Request) {
				must(t, execSQL(r.db, "UPDATE backups SET status = 'failed' WHERE id = ?", w.manifest.backup(t, 1).BackupID))
				return w.rejectOptions(), w.manifest.backup(t, 1).BackupID, nil
			},
		},
		{
			name: "chain_broken_missing_middle", code: restore.CodeChainBroken, stage: "plan",
			mutate: func(t *testing.T, w *worker, r *rejection) (restore.Options, string, *restore.Request) {
				must(t, execSQL(r.db, "UPDATE backups SET seq = 9 WHERE id = ?", w.manifest.backup(t, 1).BackupID))
				return w.rejectOptions(), w.manifest.backup(t, 2).BackupID, nil
			},
		},
		{
			name: "chain_broken_wrong_parent", code: restore.CodeChainBroken, stage: "plan",
			mutate: func(t *testing.T, w *worker, r *rejection) (restore.Options, string, *restore.Request) {
				must(t, execSQL(r.db, "UPDATE backups SET parent_id = ? WHERE id = ?", w.manifest.backup(t, 0).BackupID, w.manifest.backup(t, 2).BackupID))
				return w.rejectOptions(), w.manifest.backup(t, 2).BackupID, nil
			},
		},
		{
			name: "lsn_discontinuous", code: restore.CodeLSNDiscontinuous, stage: "plan",
			mutate: func(t *testing.T, w *worker, r *rejection) (restore.Options, string, *restore.Request) {
				must(t, execSQL(r.db, "UPDATE backups SET from_lsn = '999' WHERE id = ?", w.manifest.backup(t, 1).BackupID))
				return w.rejectOptions(), w.manifest.backup(t, 2).BackupID, nil
			},
		},
		{
			name: "key_unavailable", code: restore.CodeKeyUnavailable, stage: "plan",
			mutate: func(t *testing.T, w *worker, _ *rejection) (restore.Options, string, *restore.Request) {
				opts := w.rejectOptions()
				opts.KeyFile = w.paths.wrongKey
				return opts, w.manifest.backup(t, 1).BackupID, nil
			},
		},
		{
			name: "tool_version_mismatch", code: restore.CodeToolVersionMismatch, stage: "plan",
			mutate: func(t *testing.T, w *worker, r *rejection) (restore.Options, string, *restore.Request) {
				must(t, execSQL(r.db, "UPDATE chains SET tool_version = '8.4.0-8' WHERE id = ?", w.manifest.backup(t, 1).ChainID))
				return w.rejectOptions(), w.manifest.backup(t, 1).BackupID, nil
			},
		},
		{
			name: "matrix_unsupported", code: restore.CodeMatrixUnsupported, stage: "plan",
			mutate: func(t *testing.T, w *worker, r *rejection) (restore.Options, string, *restore.Request) {
				must(t, execSQL(r.db, "UPDATE chains SET server_version = '8.4.12' WHERE id = ?", w.manifest.backup(t, 1).ChainID))
				return w.rejectOptions(), w.manifest.backup(t, 1).BackupID, nil
			},
		},
		{
			name: "insufficient_space", code: restore.CodeInsufficientSpace, stage: "plan",
			mutate: func(t *testing.T, w *worker, _ *rejection) (restore.Options, string, *restore.Request) {
				opts := w.rejectOptions()
				opts.ExportRoot = w.paths.smallExport
				return opts, w.manifest.backup(t, 1).BackupID, nil
			},
		},
		{
			name: "source_unavailable_local_truncated", code: restore.CodeSourceUnavailable, stage: "plan",
			mutate: func(t *testing.T, w *worker, _ *rejection) (restore.Options, string, *restore.Request) {
				payload := filepath.Join(w.paths.repo, filepath.FromSlash(w.rejectBackup.ObjectPath), "payload.bin")
				must(t, os.Truncate(payload, 16))
				return w.rejectOptions(), w.rejectBackup.BackupID, nil
			},
		},
		{
			name: "source_unavailable_replica_unverified", code: restore.CodeSourceUnavailable, stage: "plan",
			mutate: func(t *testing.T, w *worker, r *rejection) (restore.Options, string, *restore.Request) {
				must(t, execSQL(r.db, "UPDATE replicas SET status = 'uploaded' WHERE backup_id = ? AND repository_id = ?", w.manifest.backup(t, 1).BackupID, ftpRepoID))
				return w.rejectOptions(), w.manifest.backup(t, 1).BackupID, nil
			},
		},
		{
			name: "unconfigured_export_symlink", code: restore.CodeUnconfigured, stage: "plan",
			mutate: func(t *testing.T, w *worker, _ *rejection) (restore.Options, string, *restore.Request) {
				opts := w.rejectOptions()
				opts.ExportRoot = filepath.Join(w.paths.root, "reject-export-symlink")
				if _, err := os.Lstat(opts.ExportRoot); errors.Is(err, os.ErrNotExist) {
					must(t, os.Symlink(w.paths.export, opts.ExportRoot))
				}
				return opts, w.manifest.backup(t, 1).BackupID, nil
			},
		},
	}
}

// assertRejection requires the fixed code of one refusal. When an execution
// refusal carries the wrong code it first reports the whole withheld cause
// chain, so a native run names the refusing sub-step instead of only the code.
func (w *worker) assertRejection(t *testing.T, test rejectCase, err error) {
	t.Helper()
	if err == nil {
		t.Fatalf("%s was accepted", test.name)
	}
	if test.stage == "execute" {
		var execErr *restore.ExecError
		if !errors.As(err, &execErr) || execErr.Code != test.code {
			w.logf("%s execute diagnosis:\n%s", test.name, failureTree(err))
			t.Fatalf("%s execute code = %v, want %s", test.name, err, test.code)
		}
		side := execErr.SideEffects
		if side.ExportPublished || !side.WorkDirRemoved || !side.PendingExportRemoved {
			t.Fatalf("%s side effects = %+v", test.name, side)
		}
		return
	}
	var refusal *restore.PlanError
	if !errors.As(err, &refusal) || refusal.Code != test.code {
		t.Fatalf("%s plan code = %v, want %s", test.name, err, test.code)
	}
}

func prepareCalls(log string) int {
	count := 0
	for _, line := range strings.Split(log, "\n") {
		if strings.Contains(line, "--prepare") {
			count++
		}
	}
	return count
}

// a4FTP covers the two remote-content rejections plus one execute-time
// authentication failure. Each mutates only bytes of one immutable replica
// member and restores them afterwards, so later cases see the verified object.
func (w *worker) a4FTP(t *testing.T) {
	t.Helper()
	file := w.manifest.backup(t, 1)
	key := file.ObjectPath + "/" + "payload.bin"
	original := w.readRemote(t, key)
	digestBefore := w.remoteDigest(t)
	w.rejectRemoteContent(t, key, original, file.BackupID)
	if digestBefore != w.remoteDigest(t) {
		t.Fatal("the rejection cases did not restore the replica bytes")
	}
}

// rejectRemoteContent runs the tampered and truncated cases and restores the
// original member bytes afterwards.
func (w *worker) rejectRemoteContent(t *testing.T, key string, original []byte, backupID string) {
	t.Helper()
	tampered := bytes.Clone(original)
	if len(tampered) < 128 {
		t.Fatal("replica payload is too small to tamper with")
	}
	tampered[len(tampered)/2] ^= 0xff
	w.storeRemote(t, key, tampered)
	w.rejectPlanCase(t, "authentication_failed_ftp_tampered", restore.CodeAuthenticationFailed, "execute", backupID)
	w.storeRemote(t, key, original)
	w.storeRemote(t, key, original[:len(original)/2])
	w.rejectPlanCase(t, "source_unavailable_ftp_truncated", restore.CodeSourceUnavailable, "plan", backupID)
	w.storeRemote(t, key, original)
}

func (w *worker) rejectPlanCase(t *testing.T, name, code, stage, backupID string) {
	t.Helper()
	before := strings.Join(entryNames(t, w.paths.export), ",")
	opts := w.rejectOptions()
	if stage == "execute" {
		plan := w.planFor(t, opts, backupID)
		request := restore.Request{RestoreID: randomRestoreID(t), Plan: plan}
		_, err := restore.Execute(t.Context(), opts, request, nil)
		w.assertRejection(t, rejectCase{name: name, code: code, stage: stage}, err)
	} else {
		_, err := restore.BuildPlan(t.Context(), opts, backupID)
		w.assertRejection(t, rejectCase{name: name, code: code, stage: stage}, err)
	}
	if before != strings.Join(entryNames(t, w.paths.export), ",") {
		t.Fatal("the rejection changed the export root")
	}
	assertNoPrepare(t, w)
	w.logf("P4-A4 rejected=%s code=%s stage=%s export_unchanged=true work_gone=true prepare_calls=0", name, code, stage)
}

func execSQL(db *sql.DB, query string, args ...any) error {
	_, err := db.ExecContext(context.Background(), query, args...)
	return err
}

func (w *worker) readRemote(t *testing.T, key string) []byte {
	t.Helper()
	reader, err := w.remote.Open(t.Context(), key, 0)
	must(t, err)
	body, readErr := io.ReadAll(reader)
	must(t, readErr)
	must(t, reader.Close())
	return body
}

// storeRemote overwrites one published member with a raw FTP client, which is
// how a corrupted or truncated object is observed by the engine. Only the
// fixture account is used.
func (w *worker) storeRemote(t *testing.T, key string, body []byte) {
	t.Helper()
	conn, err := client.Dial(os.Getenv("INTERN_P4_FTP_ADDRESS"))
	must(t, err)
	must(t, conn.Login("normal", os.Getenv("INTERN_P4_FTP_PASSWORD")))
	must(t, conn.Stor("/"+key, bytes.NewReader(body)))
	must(t, conn.Quit())
}
