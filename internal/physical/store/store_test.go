package store

import (
	"bytes"
	"database/sql"
	"errors"
	"fmt"
	"math"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/internal/pgtest"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	db := pgtest.Open(t)
	if err := Migrate(t.Context(), db); err != nil {
		t.Fatal(err)
	}
	return New(db)
}

func planRequest(kind string) PlanRequest {
	return PlanRequest{SourceID: "source", ServerUUID: "server", ServerVersion: "8.4.11", ToolVersion: "8.4.0-7", Kind: kind, MaxChainLength: 3}
}

func beginRequest(plan Plan, id string) BeginRequest {
	return BeginRequest{
		Plan: plan, SourceID: "source", ServerUUID: "server", ServerVersion: "8.4.11", ToolVersion: "8.4.0-7",
		InstanceID: "instance", RunID: id, BackupID: id, ObjectPath: "instance/source/" + plan.ChainID + "/" + id,
		StagingToken: id, WorkDir: "/work/run-" + id, KeyID: "key", CreatedAt: time.Now(),
	}
}

func mustBegin(t *testing.T, s *Store, kind, id string) BeginRequest {
	t.Helper()
	plan, err := s.Plan(t.Context(), planRequest(kind))
	if err != nil {
		t.Fatal(err)
	}
	request := beginRequest(plan, id)
	if err = s.Begin(t.Context(), request); err != nil {
		t.Fatal(err)
	}
	return request
}

func completion(id, kind string, from, to uint64) Completion {
	return Completion{
		BackupID: id, FromLSN: from, ToLSN: to, LastLSN: to,
		CheckpointsRaw: []byte(fmt.Sprintf("backup_type = %s\nfrom_lsn = %d\nto_lsn = %d\nlast_lsn = %d\n", kind, from, to, to)),
		InfoRaw:        []byte("tool_name = xtrabackup\n"), PayloadSize: 512, PayloadSHA256: "payload-digest",
		MetadataSize: 128, MetadataSHA256: "metadata-digest", CompletedAt: time.Now(),
	}
}

func mustComplete(t *testing.T, s *Store, value Completion) {
	t.Helper()
	if err := s.Complete(t.Context(), value); err != nil {
		t.Fatal(err)
	}
}

func assertAdmission(t *testing.T, err error, code string) {
	t.Helper()
	var admission *AdmissionError
	if !errors.As(err, &admission) || admission.Code != code || !admission.RequiresFull {
		t.Fatalf("admission = %v; want %s requiring full", err, code)
	}
}

func execSQL(t *testing.T, s *Store, query string, args ...any) {
	t.Helper()
	if _, err := s.db.ExecContext(t.Context(), query, args...); err != nil {
		t.Fatal(err)
	}
}

func TestPhysicalChainLifecycle(t *testing.T) {
	s := testStore(t)
	full := mustBegin(t, s, "full", "full")
	pending, err := s.PendingCleanup(t.Context(), "instance", nil)
	if err != nil || len(pending) != 3 {
		t.Fatalf("running ownership: %+v %v", pending, err)
	}
	want := completion("full", "full-backuped", 0, 100)
	mustComplete(t, s, want)
	assertCompletedRecord(t, s, want)
	assertCleanupCount(t, s, "instance", nil, 0)
	completeTwoIncrements(t, s, full.Plan.ChainID)
	members, err := s.ChainBackups(t.Context(), full.Plan.ChainID)
	if err != nil || len(members) != 3 || members[2].ParentID != "inc1" || members[2].ToLSN != 300 {
		t.Fatalf("chain records: %+v %v", members, err)
	}
	_, err = s.Plan(t.Context(), planRequest("incremental"))
	assertAdmission(t, err, "chain_length_reached")
	if err = s.Fail(t.Context(), "full", "late_cancel"); err == nil {
		t.Fatal("completed backup changed to failed")
	}
	if err = s.Complete(t.Context(), want); err == nil {
		t.Fatal("completed backup completed a second time")
	}
}

func TestPhysicalAdmissionMatrix(t *testing.T) {
	for _, tc := range []struct {
		name  string
		code  string
		setup func(*testing.T, *Store, *PlanRequest)
	}{
		{"missing", "parent_missing", func(t *testing.T, s *Store, _ *PlanRequest) {
			execSQL(t, s, "DELETE FROM backups")
			execSQL(t, s, "DELETE FROM chains")
		}},
		{"running", "parent_not_completed", func(t *testing.T, s *Store, _ *PlanRequest) { execSQL(t, s, "UPDATE backups SET status = 'running'") }},
		{"failed", "parent_not_completed", func(t *testing.T, s *Store, _ *PlanRequest) { execSQL(t, s, "UPDATE backups SET status = 'failed'") }},
		{"uuid", "server_uuid_changed", func(_ *testing.T, _ *Store, r *PlanRequest) { r.ServerUUID = "replacement" }},
		{"server patch", "server_version_changed", func(_ *testing.T, _ *Store, r *PlanRequest) { r.ServerVersion = "8.4.12" }},
		{"tool patch", "tool_version_changed", func(_ *testing.T, _ *Store, r *PlanRequest) { r.ToolVersion = "8.4.0-8" }},
		{"limit", "chain_length_reached", func(_ *testing.T, _ *Store, r *PlanRequest) { r.MaxChainLength = 1 }},
		{"sequence gap", "chain_broken", func(t *testing.T, s *Store, _ *PlanRequest) { execSQL(t, s, "UPDATE backups SET seq = 2") }},
		{"raw mismatch", "chain_broken", func(t *testing.T, s *Store, _ *PlanRequest) { execSQL(t, s, "UPDATE backups SET to_lsn = '101'") }},
		{"sealed", "chain_broken", func(t *testing.T, s *Store, _ *PlanRequest) { execSQL(t, s, "UPDATE chains SET sealed_at = 'sealed'") }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := testStore(t)
			mustBegin(t, s, "full", "full")
			mustComplete(t, s, completion("full", "full-backuped", 0, 100))
			request := planRequest("incremental")
			tc.setup(t, s, &request)
			_, err := s.Plan(t.Context(), request)
			assertAdmission(t, err, tc.code)
		})
	}
}

func TestBeginRevalidatesAndRollsBack(t *testing.T) {
	s := testStore(t)
	mustBegin(t, s, "full", "full")
	mustComplete(t, s, completion("full", "full-backuped", 0, 100))
	plan, err := s.Plan(t.Context(), planRequest("incremental"))
	if err != nil {
		t.Fatal(err)
	}
	wrong := plan
	wrong.Parent = &Parent{ID: "wrong", ToLSN: plan.Parent.ToLSN, CheckpointsRaw: plan.Parent.CheckpointsRaw}
	assertAdmission(t, s.Begin(t.Context(), beginRequest(wrong, "wrong")), "chain_broken")
	if err = s.Begin(t.Context(), beginRequest(plan, "first")); err != nil {
		t.Fatal(err)
	}
	assertAdmission(t, s.Begin(t.Context(), beginRequest(plan, "racing")), "parent_not_completed")
	mustComplete(t, s, completion("first", "incremental", 100, 200))
	assertAdmission(t, s.Begin(t.Context(), beginRequest(plan, "stale")), "chain_broken")
	if _, err = s.Backup(t.Context(), "stale"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("stale plan inserted backup: %v", err)
	}
}

func TestBeginRollsBackPartialLedger(t *testing.T) {
	s := testStore(t)
	owner := mustBegin(t, s, "full", "owner")
	newPlan, err := s.Plan(t.Context(), planRequest("full"))
	if err != nil {
		t.Fatal(err)
	}
	collision := beginRequest(newPlan, "collision")
	collision.WorkDir = owner.WorkDir
	if err = s.Begin(t.Context(), collision); err == nil {
		t.Fatal("duplicate cleanup target accepted")
	}
	if _, err = s.Backup(t.Context(), "collision"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("partial running row survived rollback: %v", err)
	}
	var chains int
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM chains WHERE id = $1", newPlan.ChainID).Scan(&chains); err != nil || chains != 0 {
		t.Fatalf("partial chain survived rollback: %d %v", chains, err)
	}
	var ledger int
	if err = s.db.QueryRowContext(t.Context(), "SELECT count(*) FROM cleanup_ledger WHERE backup_id = 'collision'").Scan(&ledger); err != nil || ledger != 0 {
		t.Fatalf("partial ledger survived rollback: %d %v", ledger, err)
	}
}

func TestCompleteRequiresContinuityAndRunning(t *testing.T) {
	s := testStore(t)
	mustBegin(t, s, "full", "full")
	mustComplete(t, s, completion("full", "full-backuped", 0, 100))
	mustBegin(t, s, "incremental", "inc")
	assertAdmission(t, s.Complete(t.Context(), completion("inc", "incremental", 99, 200)), "chain_broken")
	backup, err := s.Backup(t.Context(), "inc")
	if err != nil || backup.Status != "running" || backup.LocalPresent {
		t.Fatalf("discontinuous completion changed state: %+v %v", backup, err)
	}
	pending, err := s.PendingCleanup(t.Context(), "instance", nil)
	if err != nil || len(pending) != 3 {
		t.Fatalf("failed completion lost cleanup: %+v %v", pending, err)
	}
	if err = s.Fail(t.Context(), "inc", "tool_exit"); err != nil {
		t.Fatal(err)
	}
	if err = s.Complete(t.Context(), completion("inc", "incremental", 100, 200)); err == nil {
		t.Fatal("failed backup completed")
	}
	_, err = s.Plan(t.Context(), planRequest("incremental"))
	assertAdmission(t, err, "parent_not_completed")
}

func TestUnsignedLSNRoundTrip(t *testing.T) {
	s := testStore(t)
	mustBegin(t, s, "full", "full")
	mustComplete(t, s, completion("full", "full-backuped", 0, math.MaxUint64-1))
	mustBegin(t, s, "incremental", "inc")
	mustComplete(t, s, completion("inc", "incremental", math.MaxUint64-1, math.MaxUint64))
	backup, err := s.Backup(t.Context(), "inc")
	if err != nil || backup.ToLSN != math.MaxUint64 || backup.FromLSN != math.MaxUint64-1 {
		t.Fatalf("unsigned LSN round trip: %+v %v", backup, err)
	}
}

func assertCompletedRecord(t *testing.T, s *Store, want Completion) {
	t.Helper()
	first, err := s.Backup(t.Context(), want.BackupID)
	if err != nil || first.Status != "completed" || !first.LocalPresent || first.KeyID != "key" ||
		!bytes.Equal(first.CheckpointsRaw, want.CheckpointsRaw) || !bytes.Equal(first.InfoRaw, want.InfoRaw) ||
		first.PayloadSize != want.PayloadSize || first.MetadataSHA256 != want.MetadataSHA256 {
		t.Fatalf("completed record: %+v %v", first, err)
	}
}

func completeTwoIncrements(t *testing.T, s *Store, chainID string) {
	t.Helper()
	for i := 1; i <= 2; i++ {
		id := fmt.Sprintf("inc%d", i)
		request := mustBegin(t, s, "incremental", id)
		if request.Plan.Seq != i+1 || request.Plan.ChainID != chainID || request.Plan.Parent.ToLSN != uint64(i*100) {
			t.Fatalf("incremental plan: %+v", request.Plan)
		}
		mustComplete(t, s, completion(id, "incremental", uint64(i*100), uint64((i+1)*100)))
	}
}
