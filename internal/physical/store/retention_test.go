package store

import (
	"database/sql"
	"errors"
	"testing"
	"time"
)

func TestSetPinnedIsTheChainPinWriter(t *testing.T) {
	s := testStore(t)
	request := mustBegin(t, s, "full", "full")
	mustComplete(t, s, completion("full", "full-backuped", 0, 100))
	chainID := request.Plan.ChainID
	assertPinned(t, s, chainID, false)
	for _, pinned := range []bool{true, false, true} {
		if err := s.SetPinned(t.Context(), chainID, pinned); err != nil {
			t.Fatal(err)
		}
		assertPinned(t, s, chainID, pinned)
	}
	if err := s.SetPinned(t.Context(), "missing", true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing chain pin: %v", err)
	}
	if err := s.SetPinned(t.Context(), "", true); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("empty chain pin: %v", err)
	}
}

func TestReserveLocalDeleteIsOneTransaction(t *testing.T) {
	s := testStore(t)
	request := mustBegin(t, s, "full", "full")
	mustComplete(t, s, completion("full", "full-backuped", 0, 100))
	mustBegin(t, s, "incremental", "inc")
	mustComplete(t, s, completion("inc", "incremental", 100, 200))
	chainID := request.Plan.ChainID
	reserved, err := s.ReserveLocalDelete(t.Context(), chainID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	assertReservedTargets(t, reserved, request.ObjectPath)
	assertLocalPresent(t, s, "full", false)
	assertLocalPresent(t, s, "inc", false)
	assertRecordSurvives(t, s, "inc")
	assertNotOrphanCleanup(t, s)
	assertReservationIsIdempotent(t, s, chainID, reserved)
}

func assertReservedTargets(t *testing.T, reserved []LocalDelete, fullTarget string) {
	t.Helper()
	if len(reserved) != 2 || reserved[0].BackupID != "full" || reserved[1].BackupID != "inc" || reserved[0].Target != fullTarget {
		t.Fatalf("reservation rows: %+v", reserved)
	}
}

// A reservation changes only local payload presence: records, checkpoints, and
// metadata stay readable for restore planning.
func assertRecordSurvives(t *testing.T, s *Store, backupID string) {
	t.Helper()
	backup, err := s.Backup(t.Context(), backupID)
	if err != nil || backup.Status != "completed" || len(backup.CheckpointsRaw) == 0 || backup.PayloadSHA256 != "payload-digest" {
		t.Fatalf("reservation changed the record: %+v %v", backup, err)
	}
}

// A completed backup's reservation must never enter the orphan cleanup path.
func assertNotOrphanCleanup(t *testing.T, s *Store) {
	t.Helper()
	pending, err := s.PendingCleanup(t.Context(), "instance", nil)
	if err != nil || len(pending) != 0 {
		t.Fatalf("reservation exposed as orphan cleanup: %+v %v", pending, err)
	}
}

func assertReservationIsIdempotent(t *testing.T, s *Store, chainID string, reserved []LocalDelete) {
	t.Helper()
	again, err := s.ReserveLocalDelete(t.Context(), chainID, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	assertSameReservation(t, reserved, again)
}

func TestReserveLocalDeleteRollsBackOnFailure(t *testing.T) {
	s := testStore(t)
	request := mustBegin(t, s, "full", "full")
	mustComplete(t, s, completion("full", "full-backuped", 0, 100))
	mustBegin(t, s, "incremental", "inc")
	mustComplete(t, s, completion("inc", "incremental", 100, 200))
	chainID := request.Plan.ChainID
	execSQL(t, s, `CREATE FUNCTION reject_presence() RETURNS trigger LANGUAGE plpgsql AS $$ BEGIN RAISE EXCEPTION 'injected presence failure'; END $$`)
	execSQL(t, s, `CREATE TRIGGER reject_presence BEFORE UPDATE OF local_present ON backups FOR EACH ROW EXECUTE FUNCTION reject_presence()`)
	if _, err := s.ReserveLocalDelete(t.Context(), chainID, time.Now()); err == nil {
		t.Fatal("reservation failure ignored")
	}
	execSQL(t, s, "DROP TRIGGER reject_presence ON backups")
	assertLocalPresent(t, s, "full", true)
	assertLocalPresent(t, s, "inc", true)
	if reserved, err := s.LocalDeletePending(t.Context(), chainID); err != nil || len(reserved) != 0 {
		t.Fatalf("partial reservation rows: %+v %v", reserved, err)
	}
}

func TestReserveLocalDeleteRefusesIneligibleInput(t *testing.T) {
	s := testStore(t)
	request := mustBegin(t, s, "full", "full")
	mustComplete(t, s, completion("full", "full-backuped", 0, 100))
	chainID := request.Plan.ChainID
	mustBegin(t, s, "incremental", "inc")
	if _, err := s.ReserveLocalDelete(t.Context(), chainID, time.Now()); err == nil {
		t.Fatal("incomplete chain reserved")
	}
	assertLocalPresent(t, s, "full", true)
	if _, err := s.ReserveLocalDelete(t.Context(), "missing", time.Now()); !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("missing chain reservation: %v", err)
	}
	if _, err := s.ReserveLocalDelete(t.Context(), chainID, time.Time{}); err == nil {
		t.Fatal("zero reservation time accepted")
	}
}

func TestLocalDeleteCompletionAndRetry(t *testing.T) {
	s := testStore(t)
	request := mustBegin(t, s, "full", "full")
	mustComplete(t, s, completion("full", "full-backuped", 0, 100))
	chainID := request.Plan.ChainID
	reserved, err := s.ReserveLocalDelete(t.Context(), chainID, time.Now())
	if err != nil || len(reserved) != 1 {
		t.Fatalf("reservation: %+v %v", reserved, err)
	}
	if err = s.LocalDeleteFailed(t.Context(), reserved[0].ID, "cleanup_failed"); err != nil {
		t.Fatal(err)
	}
	pending, err := s.LocalDeletePending(t.Context(), chainID)
	if err != nil || len(pending) != 1 {
		t.Fatalf("failed reclaim dropped the row: %+v %v", pending, err)
	}
	assertReclaimAttempt(t, s, reserved[0].ID)
	if err = s.LocalDeleteDone(t.Context(), reserved[0].ID); err != nil {
		t.Fatal(err)
	}
	if pending, err = s.LocalDeletePending(t.Context(), chainID); err != nil || len(pending) != 0 {
		t.Fatalf("completed reclaim retained the row: %+v %v", pending, err)
	}
	// The chain stays readable and stays marked absent after reclaim completes.
	if _, err = s.Chain(t.Context(), chainID); err != nil {
		t.Fatal(err)
	}
	assertLocalPresent(t, s, "full", false)
}

func TestLocalDeletePendingForInstanceListsThisInstance(t *testing.T) {
	s := testStore(t)
	request := mustBegin(t, s, "full", "full")
	mustComplete(t, s, completion("full", "full-backuped", 0, 100))
	chainID := request.Plan.ChainID
	reserved, err := s.ReserveLocalDelete(t.Context(), chainID, time.Now())
	if err != nil || len(reserved) != 1 {
		t.Fatalf("reservation: %+v %v", reserved, err)
	}
	if err = s.SetPinned(t.Context(), chainID, true); err != nil {
		t.Fatal(err)
	}
	got, err := s.LocalDeletePendingForInstance(t.Context(), "instance")
	if err != nil {
		t.Fatal(err)
	}
	assertSameReservation(t, reserved, got)
	other, err := s.LocalDeletePendingForInstance(t.Context(), "other")
	if err != nil || len(other) != 0 {
		t.Fatalf("other instance reservations: %+v %v", other, err)
	}
	assertNotOrphanCleanup(t, s)
}

func TestSourceChainsOrderAndIdentity(t *testing.T) {
	s := testStore(t)
	first := mustBegin(t, s, "full", "first")
	mustComplete(t, s, completion("first", "full-backuped", 0, 100))
	second := mustBegin(t, s, "full", "second")
	mustComplete(t, s, completion("second", "full-backuped", 0, 200))
	chains, err := s.SourceChains(t.Context(), "source")
	if err != nil || len(chains) != 2 {
		t.Fatalf("source chains: %+v %v", chains, err)
	}
	if chains[0].ID != first.Plan.ChainID || chains[1].ID != second.Plan.ChainID {
		t.Fatalf("source chain order: %+v", chains)
	}
	if chains[0].SourceID != "source" || !chains[0].SealedAt.IsZero() || chains[0].Pinned {
		t.Fatalf("chain identity: %+v", chains[0])
	}
	if other, err := s.SourceChains(t.Context(), "other"); err != nil || len(other) != 0 {
		t.Fatalf("unrelated source chains: %+v %v", other, err)
	}
}

func assertPinned(t *testing.T, s *Store, chainID string, want bool) {
	t.Helper()
	chain, err := s.Chain(t.Context(), chainID)
	if err != nil || chain.Pinned != want {
		t.Fatalf("chain pin = %+v %v; want %t", chain, err, want)
	}
}

func assertLocalPresent(t *testing.T, s *Store, backupID string, want bool) {
	t.Helper()
	backup, err := s.Backup(t.Context(), backupID)
	if err != nil || backup.LocalPresent != want {
		t.Fatalf("backup %s local_present = %t %v; want %t", backupID, backup.LocalPresent, err, want)
	}
}

func assertSameReservation(t *testing.T, want, got []LocalDelete) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("reservation changed on rerun: %+v; want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("reservation changed on rerun: %+v; want %+v", got, want)
		}
	}
}

// A failed reclaim keeps its row and records the attempt, so a rerun retries it.
func assertReclaimAttempt(t *testing.T, s *Store, id int64) {
	t.Helper()
	var attempts int64
	var code sql.NullString
	err := s.db.QueryRowContext(t.Context(), "SELECT attempts, last_error_code FROM cleanup_ledger WHERE id = $1", id).Scan(&attempts, &code)
	if err != nil {
		t.Fatal(err)
	}
	if attempts != 1 || code.String != "cleanup_failed" {
		t.Fatalf("reclaim attempt counters: %d %q", attempts, code.String)
	}
}
