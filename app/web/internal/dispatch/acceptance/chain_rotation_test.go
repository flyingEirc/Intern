//go:build integration

package acceptance

import (
	"fmt"
	"testing"

	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// a2DefaultChainRotation exercises the product defaults against the real source,
// local repository and replica rather than changing the chain limit in a fixture.
func a2DefaultChainRotation(t *testing.T, f *fixture) {
	policy := f.createPolicy(t, "default-chain-rotation")
	maximum := requireDefaultChainLimit(t, f, policy)
	f.enable(t, policy)
	chain := completeDefaultChain(t, f, policy, maximum)
	requireFullChainRefusal(t, f, policy)
	admissions := f.admissionFulls(t, policy)
	if len(admissions) != 1 {
		t.Fatalf("full chain queued %d admission fulls, want exactly one", len(admissions))
	}
	newFull := completeAdmission(t, f, admissions[0].ID, chain)
	incremental := completeRotatedIncremental(t, f, policy, newFull)
	f.waitAllReplicasVerified(t, incremental.ID)
	if admissions = f.admissionFulls(t, policy); len(admissions) != 1 {
		t.Fatalf("rotation created %d admission fulls", len(admissions))
	}
	f.assertNoSecret(t, nil)
	t.Logf("PG-G2 default_max=%d completed_chain=%d admission_fulls=%d next_incremental_seq=%d replicas_verified=9",
		maximum, maximum, len(admissions), incremental.Seq)
}

func requireDefaultChainLimit(t *testing.T, f *fixture, policy string) int {
	t.Helper()
	var maximum int
	must(t, f.view(t).QueryRowContext(t.Context(),
		"SELECT max_chain_length FROM policies WHERE id = $1", policy).Scan(&maximum))
	if maximum != 7 {
		t.Fatalf("new policy max_chain_length = %d, want 7", maximum)
	}
	return maximum
}

// completeDefaultChain fills the default seven-member chain through the API.
func completeDefaultChain(t *testing.T, f *fixture, policy string, maximum int) string {
	t.Helper()
	var chain string
	for seq := 1; seq <= maximum; seq++ {
		kind := store.RunKindIncremental
		if seq == 1 {
			kind = store.RunKindFull
		}
		f.dml(t, 100+seq)
		backup := f.completeMember(t, policy, kind, fmt.Sprintf("default-chain-%d", seq))
		if seq == 1 {
			chain = backup.ChainID
		}
		if backup.Seq != seq || backup.ChainID != chain {
			t.Fatalf("default chain seq %d = %+v", seq, backup)
		}
		f.waitAllReplicasVerified(t, backup.ID)
	}
	return chain
}

func (f *fixture) completeMember(t *testing.T, policy, kind, key string) dispatch.BackupView {
	t.Helper()
	run := f.waitRun(t, f.triggerRun(t, policy, kind, key), store.RunStatusCompleted)
	if run.BackupID == nil {
		t.Fatal("completed chain member has no backup")
	}
	return f.getBackup(t, *run.BackupID)
}

func requireFullChainRefusal(t *testing.T, f *fixture, policy string) {
	t.Helper()
	refused := f.waitRun(t, f.triggerRun(t, policy, store.RunKindIncremental, "default-chain-overflow"), store.RunStatusFailed)
	if refused.ErrorCode == nil || *refused.ErrorCode != "chain_length_reached" ||
		errorClass(refused) != store.RunErrorRequiresFull {
		t.Fatalf("full chain incremental = %+v", refused)
	}
}

func completeAdmission(t *testing.T, f *fixture, admissionID, previousChain string) dispatch.BackupView {
	t.Helper()
	full := f.waitRun(t, admissionID, store.RunStatusCompleted)
	if full.BackupID == nil {
		t.Fatal("admission full has no backup")
	}
	newFull := f.getBackup(t, *full.BackupID)
	if newFull.ChainID == previousChain || newFull.Seq != 1 {
		t.Fatalf("admission did not start a new chain: %+v", newFull)
	}
	f.waitAllReplicasVerified(t, newFull.ID)
	return newFull
}

func completeRotatedIncremental(t *testing.T, f *fixture, policy string, parent dispatch.BackupView) dispatch.BackupView {
	t.Helper()
	next := f.waitRun(t, f.triggerRun(t, policy, store.RunKindIncremental, "default-chain-after-rotation"), store.RunStatusCompleted)
	if next.BackupID == nil {
		t.Fatal("post-rotation incremental has no backup")
	}
	incremental := f.getBackup(t, *next.BackupID)
	if incremental.ChainID != parent.ChainID || incremental.Seq != 2 || incremental.FromLSN != parent.ToLSN {
		t.Fatalf("post-rotation incremental is discontinuous: %+v", incremental)
	}
	return incremental
}
