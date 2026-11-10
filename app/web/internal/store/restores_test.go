package store

import (
	"errors"
	"strings"
	"testing"

	"github.com/flyingEirc/Intern/internal/pgtest"
)

func restoreTestStore(t *testing.T) (*Store, User) {
	t.Helper()
	s := openTestStore(t, pgtest.New(t))
	user, err := s.SetupAdmin(t.Context(), "restore-admin", "hash", SystemAudit("auth.setup", AuditDetail{Role: "admin"}))
	if err != nil {
		t.Fatal(err)
	}
	return s, user
}

func restoreTestInput(id, userID string) RestoreInput {
	plan := RestorePlan{
		BackupID: "00000000-0000-7000-8000-000000000001", ChainID: "chain-1", SourceID: "source-1",
		ServerUUID: "uuid-1", ServerVersion: "8.4.11", ToolVersion: "8.4.0-7", TargetToLSN: 500,
		Links: []RestorePlanLink{{
			BackupID: "00000000-0000-7000-8000-000000000001", Seq: 1, Kind: RunKindFull,
			FromLSN: 0, ToLSN: 500, Source: "local", RepositoryID: "repository-1",
			PayloadSize: 100, MetadataSize: 10,
		}},
		RequiredWorkBytes: 200, RequiredExportBytes: 800, AvailableWorkBytes: 1000,
		AvailableExportBytes: 1000, ExportPath: "/export/planned", Risks: []string{},
		Digest: strings.Repeat("a", 64), RehearsalAvailable: true,
	}
	return RestoreInput{
		ID: id, BackupID: plan.BackupID, Plan: plan, PlanDigest: plan.Digest,
		RequestHash: "request", RequestedBy: userID,
		Audit: SystemAudit("restores.execute", AuditDetail{}),
	}
}

// TestCreateRestoreCommitsRowAndSuccessAudit proves the insert and the success
// audit are one transaction: an audit that cannot be written leaves no row.
func TestCreateRestoreCommitsRowAndSuccessAudit(t *testing.T) {
	s, user := restoreTestStore(t)
	input := restoreTestInput("aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa", user.ID)
	created := requireCreatedRestore(t, s, input)
	if created.Status != RestoreStatusQueued || created.VerifyStatus != RestoreVerifyNotRequested {
		t.Fatalf("created restore = %+v", created)
	}
	if created.VerifySQL == nil || len(created.VerifySQL) != 0 {
		t.Fatalf("verify_sql = %#v, want an empty array", created.VerifySQL)
	}
	if created.Plan.Digest != input.PlanDigest || len(created.Plan.Links) != 1 {
		t.Fatalf("plan round trip = %+v", created.Plan)
	}
	if audits := countRestores(t, s, "restores.execute", "success"); audits != 1 {
		t.Fatalf("success audits = %d", audits)
	}
	requireRestoreRollsBackWithoutSuccessAudit(t, s, input)
}

func requireCreatedRestore(t *testing.T, s *Store, input RestoreInput) Restore {
	t.Helper()
	created, err := s.CreateRestore(t.Context(), input)
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// countRestores counts audit rows of one action and result.
func countRestores(t *testing.T, s *Store, action, result string) int {
	t.Helper()
	var count int
	if err := s.DB().QueryRowContext(t.Context(),
		`SELECT COUNT(*) FROM audit_logs WHERE action = $1 AND result = $2`, action, result).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// requireRestoreRollsBackWithoutSuccessAudit writes no audit action and proves
// the restore row is rolled back with it.
func requireRestoreRollsBackWithoutSuccessAudit(t *testing.T, s *Store, input RestoreInput) {
	t.Helper()
	blocked := input
	blocked.Audit = AuditLog{}
	blocked.ID = "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	if _, err := s.CreateRestore(t.Context(), blocked); err == nil {
		t.Fatal("a restore was created without a success audit action")
	}
	if _, err := s.Restore(t.Context(), blocked.ID); !errors.Is(err, ErrNotFound) {
		t.Fatalf("rolled-back restore read = %v", err)
	}
}

// TestRestoreIdempotencyKeyIsUnique proves a duplicate key conflicts while
// rows without a key coexist.
func TestRestoreIdempotencyKeyIsUnique(t *testing.T) {
	s, user := restoreTestStore(t)
	first := restoreTestInput("cccccccccccccccccccccccccccccccc", user.ID)
	first.IdempotencyKey = "once"
	requireCreatedRestore(t, s, first)
	second := restoreTestInput("dddddddddddddddddddddddddddddddd", user.ID)
	second.IdempotencyKey = "once"
	if _, err := s.CreateRestore(t.Context(), second); !errors.Is(err, ErrConflict) {
		t.Fatalf("duplicate key = %v, want ErrConflict", err)
	}
	for index, id := range []string{"eeeeeeeeeeeeeeeeeeeeeeeeeeeeeeee", "ffffffffffffffffffffffffffffffff"} {
		keyless := restoreTestInput(id, user.ID)
		keyless.ID = id
		requireCreatedRestore(t, s, keyless)
		_ = index
	}
	found, err := s.RestoreByIdempotencyKey(t.Context(), "once")
	if err != nil || found.ID != first.ID {
		t.Fatalf("key lookup = %+v, %v", found, err)
	}
}

// TestRestoreLogIsBounded proves the 1000-row cap with the truncated marker and
// that a later append is dropped without rewriting it.
func TestRestoreLogIsBounded(t *testing.T) {
	s, user := restoreTestStore(t)
	created := requireCreatedRestore(t, s, restoreTestInput("gggggggggggggggggggggggggggggggg", user.ID))
	requireFullRestoreLog(t, s, created.ID)
	requireRestoreAppendDroppedAfterCap(t, s, created.ID)
}

// requireFullRestoreLog fills the log to its cap and requires the final row to
// be the truncated marker.
func requireFullRestoreLog(t *testing.T, s *Store, id string) {
	t.Helper()
	for range MaxRestoreLogRows {
		if err := s.AppendRestoreLog(t.Context(), id, "info", "restore phase preparing", map[string]any{"phase": "preparing"}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.AppendRestoreLog(t.Context(), id, "info", "over-cap event", nil); err != nil {
		t.Fatal(err)
	}
	logs := requireRestoreLogs(t, s, id)
	if len(logs) != MaxRestoreLogRows {
		t.Fatalf("log rows = %d, want %d", len(logs), MaxRestoreLogRows)
	}
	last := logs[len(logs)-1]
	if last.Message != RestoreTruncatedMessage || last.Level != "warn" {
		t.Fatalf("final row = %+v", last)
	}
}

// requireRestoreAppendDroppedAfterCap proves a further append neither rewrites
// the marker nor grows the log.
func requireRestoreAppendDroppedAfterCap(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.AppendRestoreLog(t.Context(), id, "info", "after cap", nil); err != nil {
		t.Fatal(err)
	}
	logs := requireRestoreLogs(t, s, id)
	if len(logs) != MaxRestoreLogRows || logs[len(logs)-1].Message != RestoreTruncatedMessage {
		t.Fatalf("post-cap log = %d rows", len(logs))
	}
	seq, err := s.MaxRestoreLogSeq(t.Context(), id)
	if err != nil || seq != MaxRestoreLogRows {
		t.Fatalf("max seq = %d, %v", seq, err)
	}
}

func requireRestoreLogs(t *testing.T, s *Store, id string) []RestoreLog {
	t.Helper()
	logs, err := s.RestoreLogsAfter(t.Context(), id, 0, MaxRestoreLogRows+1)
	if err != nil {
		t.Fatal(err)
	}
	return logs
}

// TestRestoreTerminalWritesAndRecovery proves the terminal transitions persist
// the outcome, refuse a non-running row and list interrupted rows.
func TestRestoreTerminalWritesAndRecovery(t *testing.T) {
	s, user := restoreTestStore(t)
	created := requireCreatedRestore(t, s, restoreTestInput("hhhhhhhhhhhhhhhhhhhhhhhhhhhhhhhh", user.ID))
	requireClaimedRestore(t, s)
	requireRestorePhaseRecorded(t, s, created.ID)
	requireInterruptedList(t, s, created.ID)
	requireInterruptedRestoreFailed(t, s, created.ID)
	requireTerminalWriteRefused(t, s, created.ID)
}

func requireClaimedRestore(t *testing.T, s *Store) {
	t.Helper()
	_, claimed, err := s.ClaimRestore(t.Context())
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v", claimed, err)
	}
}

func requireRestorePhaseRecorded(t *testing.T, s *Store, id string) {
	t.Helper()
	if err := s.SetRestoreWorkDir(t.Context(), id, "/work/restore-x"); err != nil {
		t.Fatal(err)
	}
	if err := s.UpdateRestorePhase(t.Context(), id, "preparing"); err != nil {
		t.Fatal(err)
	}
	running, err := s.Restore(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if running.Phase == nil || *running.Phase != "preparing" || running.WorkDir == nil {
		t.Fatalf("running restore = %+v", running)
	}
}

func requireInterruptedList(t *testing.T, s *Store, id string) {
	t.Helper()
	interrupted, err := s.InterruptedRestores(t.Context())
	if err != nil || len(interrupted) != 1 || interrupted[0].ID != id {
		t.Fatalf("interrupted = %+v, %v", interrupted, err)
	}
}

func requireInterruptedRestoreFailed(t *testing.T, s *Store, id string) {
	t.Helper()
	effects := RestoreSideEffects{WorkDirRemoved: true, PendingExportRemoved: true}
	err := s.FailInterruptedRestore(t.Context(), id, effects, SystemAudit("restores.interrupted", AuditDetail{}),
		&RunLogWrite{Level: "error", Message: "restore interrupted"})
	if err != nil {
		t.Fatal(err)
	}
	failed, err := s.Restore(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	if failed.Status != RestoreStatusFailed || failed.ErrorClass == nil || *failed.ErrorClass != RestoreErrorInterrupted {
		t.Fatalf("failed restore = %+v", failed)
	}
	if failed.SideEffects == nil || !failed.SideEffects.WorkDirRemoved || !failed.SideEffects.PendingExportRemoved {
		t.Fatalf("side effects = %+v", failed.SideEffects)
	}
	if failed.Phase != nil {
		t.Fatalf("phase survived the terminal write: %q", *failed.Phase)
	}
}

func requireTerminalWriteRefused(t *testing.T, s *Store, id string) {
	t.Helper()
	err := s.FinishRestoreCompleted(t.Context(), id, RestoreOutcome{
		ExportPath: "/export/x", ExportBytes: 1, VerifyStatus: RestoreVerifyNotRequested,
	}, SystemAudit("restores.completed", AuditDetail{}), nil)
	if err == nil {
		t.Fatal("a failed restore accepted a completed write")
	}
}

// TestRestoreVerifyStatusTracksTheRehearsal proves the closed verify_status set
// from the store's side: a row that asked for a rehearsal starts pending (a row
// that did not stays not_requested), and a queued cancel that ended without a
// rehearsal result records not_run instead of claiming none was ever wanted.
func TestRestoreVerifyStatusTracksTheRehearsal(t *testing.T) {
	s, user := restoreTestStore(t)
	create := func(id string, rehearse bool) Restore {
		t.Helper()
		input := restoreTestInput(id, user.ID)
		input.Rehearse = rehearse
		return requireCreatedRestore(t, s, input)
	}
	if got := create("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb1", false).VerifyStatus; got != RestoreVerifyNotRequested {
		t.Fatalf("a row without a rehearsal starts %s, want not_requested", got)
	}
	if got := create("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb2", true).VerifyStatus; got != RestoreVerifyPending {
		t.Fatalf("a row that asked for a rehearsal starts %s, want pending", got)
	}
	if changed, err := s.CancelQueuedRestore(t.Context(), "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb2",
		SystemAudit("restores.canceled", AuditDetail{}), nil); err != nil || !changed {
		t.Fatalf("queued cancel = %v, %v", changed, err)
	}
	if got := requireStoredRestore(t, s, "bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb2").VerifyStatus; got != RestoreVerifyNotRun {
		t.Fatalf("a canceled rehearsal row reports %s, want not_run", got)
	}
}

// TestRestoreInterruptedRehearsalIsNotRun proves startup recovery marks a row
// whose rehearsal never ran as not_run: the interruption happens before the
// rehearsal produced anything.
func TestRestoreInterruptedRehearsalIsNotRun(t *testing.T) {
	s, user := restoreTestStore(t)
	input := restoreTestInput("bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb3", user.ID)
	input.Rehearse = true
	created := requireCreatedRestore(t, s, input)
	claimed, ok, err := s.ClaimRestore(t.Context())
	if err != nil || !ok || claimed.ID != created.ID {
		t.Fatalf("claim = %+v, %v, %v", claimed, ok, err)
	}
	if err = s.FailInterruptedRestore(t.Context(), claimed.ID, RestoreSideEffects{},
		SystemAudit("restores.interrupted", AuditDetail{}), nil); err != nil {
		t.Fatal(err)
	}
	if got := requireStoredRestore(t, s, claimed.ID).VerifyStatus; got != RestoreVerifyNotRun {
		t.Fatalf("an interrupted rehearsal row reports %s, want not_run", got)
	}
}

// requireStoredRestore reads one stored restore row.
func requireStoredRestore(t *testing.T, s *Store, id string) Restore {
	t.Helper()
	restore, err := s.Restore(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return restore
}
