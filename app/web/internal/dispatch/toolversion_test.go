package dispatch

import (
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/physical"
	"github.com/flyingEirc/Intern/internal/physical/chain"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// TestChainJobUsesTheProbedToolVersion proves the dispatch preflight path hands
// the physical chain the XtraBackup version the tool probe observed, not the
// MySQL server version. The two differ on every pinned fixture, and the chain
// compares the tool version against the metadata it wrote, so passing the server
// version makes every run fail after xtrabackup finished.
func TestChainJobUsesTheProbedToolVersion(t *testing.T) {
	q := newQueueFixture(t)
	d := q.newDispatcher(time.Now)
	repo := openQueueRepository(t, q)
	// The two versions are deliberately different, so an assertion below cannot
	// pass by reading the wrong field.
	const serverVersion, toolVersion = "8.4.11", "8.4.0-7"
	run := q.queued("tool-version-run", store.RunKindFull, store.RunTriggerManual, time.Now().UTC())
	job := d.chainJob(run, q.policy, repo, admissionFor(
		physical.Connection{Socket: "/var/run/mysqld/mysqld.sock", Username: "backup"},
		physical.Identity{UUID: preflightUUID, Version: serverVersion},
		passingOutcome(serverVersion, toolVersion),
	))
	requireObservedToolVersion(t, job, repo, q, run, serverVersion, toolVersion)
}

// preflightUUID is the server identity the fixture preflight observed.
const preflightUUID = "0f0e0d0c-0b0a-4908-8706-050403020100"

// openQueueRepository opens the fixture policy's local repository.
func openQueueRepository(t *testing.T, q *queueFixture) *local.Repository {
	t.Helper()
	repo, err := local.Open(q.repository.Local.Root)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = repo.Close() })
	return repo
}

// passingOutcome is one successful preflight observation of both versions.
func passingOutcome(serverVersion, toolVersion string) resources.PreflightOutcome {
	return resources.PreflightOutcome{
		Checks: []string{"identity", "compatibility"}, ServerUUID: preflightUUID,
		ServerVersion: serverVersion, ToolVersion: toolVersion,
	}
}

// requireObservedToolVersion requires the job to carry the probed tool version
// as its tool version and the preflight's server version as its identity.
func requireObservedToolVersion(t *testing.T, job chain.Job, repo *local.Repository,
	q *queueFixture, run store.Run, serverVersion, toolVersion string,
) {
	t.Helper()
	if serverVersion == toolVersion {
		t.Fatal("the fixture versions must differ for this test to prove anything")
	}
	if job.ToolVersion != toolVersion {
		t.Fatalf("job tool version = %q, want the probed tool version %q", job.ToolVersion, toolVersion)
	}
	if job.ToolVersion == job.Identity.Version {
		t.Fatalf("job tool version %q is the server version", job.ToolVersion)
	}
	if job.Identity.Version != serverVersion {
		t.Fatalf("job server version = %q, want %q", job.Identity.Version, serverVersion)
	}
	if job.SourceID != q.policy.SourceID || job.Kind != run.Kind || job.Store == nil || job.Repo != repo {
		t.Fatalf("job is not built from the admitted run: %+v", job)
	}
}

// TestAdmissionKeepsTheObservedToolVersion requires the admission to carry the
// tool version of the preflight it observed, including when that version is
// absent: the chain then refuses the job's empty tool version rather than
// silently substituting the server version.
func TestAdmissionKeepsTheObservedToolVersion(t *testing.T) {
	identity := physical.Identity{Version: "9.1.0"}
	admitted := admissionFor(physical.Connection{}, identity, resources.PreflightOutcome{})
	if admitted.ToolVersion != "" {
		t.Fatalf("admission tool version = %q, want the empty observation", admitted.ToolVersion)
	}
	if admitted.Identity.Version != identity.Version {
		t.Fatalf("admission server version = %q, want %q", admitted.Identity.Version, identity.Version)
	}
}
