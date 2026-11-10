//go:build integration

package acceptance

import (
	"errors"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// a3PostgresOwnerLoss injects a real server-side disconnect only after the
// physical backup process is observed live, then verifies shutdown and recovery.
func a3PostgresOwnerLoss(t *testing.T, f *fixture) {
	f.startServeChild(t)
	policy := f.createPolicy(t, "postgres-owner-loss")
	f.enable(t, policy)
	if second, err := store.Open(t.Context(), f.dsn); !errors.Is(err, store.ErrOwnerInUse) {
		if second != nil {
			_ = second.Close()
		}
		t.Fatalf("second process owner = %v", err)
	}
	runID := f.triggerRun(t, policy, store.RunKindFull, "postgres-owner-loss-full")
	backup := f.waitXtrabackup(t, runID)
	f.requireBackupAlive(t, backup)
	var pid int
	must(t, f.view(t).QueryRowContext(t.Context(), `SELECT locks.pid FROM pg_locks locks
JOIN pg_stat_activity activity ON activity.pid = locks.pid
WHERE locks.locktype = 'advisory' AND locks.objid = 17477 AND locks.granted
AND activity.datname = current_database()`).Scan(&pid))
	var terminated bool
	must(t, f.view(t).QueryRowContext(t.Context(), "SELECT pg_terminate_backend($1)", pid).Scan(&terminated))
	if !terminated {
		t.Fatal("PostgreSQL did not terminate the owned session")
	}
	f.waitChild(t, 2*time.Minute)
	if f.childExitCode(t) == 0 {
		t.Fatal("lost owner exited successfully")
	}
	if backup.alive() {
		t.Fatal("backup tool survived permanent owner loss")
	}
	if count := f.completedBackupsForRun(t, runID); count != 0 {
		t.Fatalf("lost owner published %d completed backups", count)
	}
	f.startServeChild(t)
	f.requireKilledRunRecovered(t, runID)
	if count := f.completedBackupsForRun(t, runID); count != 0 {
		t.Fatalf("restart reported %d falsely completed backups", count)
	}
	f.assertNoSecret(t, nil)
	t.Logf("PG-P3 owner_terminated=true second_owner=refused serve_exit=nonzero tool_canceled=true completed=0 recovered=interrupted %s", backup.evidence())
}
