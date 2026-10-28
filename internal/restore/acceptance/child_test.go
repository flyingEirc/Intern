//go:build integration

package acceptance

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/flyingEirc/Intern/internal/physical"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
	"github.com/flyingEirc/Intern/internal/restore"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// TestP4KillChildWorker runs one real restore in a separate process so the
// parent can SIGKILL its whole process tree during prepare. It never completes
// on its own when the injection works.
func TestP4KillChildWorker(t *testing.T) {
	id := os.Getenv("INTERN_P4_KILL_ID")
	ready := os.Getenv("INTERN_P4_KILL_READY")
	if id == "" || !filepath.IsAbs(ready) {
		t.Fatal("the kill child requires a restore id and a ready marker")
	}
	w := openExistingWorker(t)
	plan := w.planFor(t, w.options(), w.manifest.backup(t, 2).BackupID)
	progress := &events{onPhase: func(phase restore.Phase, _ map[string]any) {
		if phase != restore.PhasePreparing {
			return
		}
		must(t, os.WriteFile(ready, []byte("preparing"), 0o600))
	}}
	result, err := w.execute(t, w.options(), restore.Request{RestoreID: id, Plan: plan}, progress)
	must(t, err)
	if result.ExportPath != "" {
		t.Fatal("the kill child unexpectedly published an export")
	}
	t.Fatal("the kill child completed before the parent killed it")
}

// TestP4RootRefusalWorker proves the root refusal in the real image, where the
// harness execs this binary as uid 0. It never touches fixture state: the
// refusal happens before any database or filesystem work.
func TestP4RootRefusalWorker(t *testing.T) {
	if os.Geteuid() != 0 {
		t.Fatal("the root-refusal worker must run as root")
	}
	work := requiredAbsoluteEnv(t, "INTERN_P4_ROOT")
	_, err := physical.StartMysqld(t.Context(), physical.MysqldOptions{
		Binary: mysqldPath(t), DataDir: work, Socket: filepath.Join(work, "root.sock"),
		PIDFile: filepath.Join(work, "root.pid"), LogError: filepath.Join(work, "root.err"),
	})
	if !errors.Is(err, physical.ErrRehearsalRoot) {
		t.Fatalf("root rehearsal error = %v, want ErrRehearsalRoot", err)
	}
	id := strings.Repeat("e", 32)
	_, err = restore.Execute(t.Context(), restore.Options{MysqldPath: mysqldPath(t)}, restore.Request{
		RestoreID: id, Plan: restore.Plan{TargetBackupID: "unused"}, Rehearse: true,
	}, nil)
	var execErr *restore.ExecError
	if !errors.As(err, &execErr) || execErr.Code != restore.CodeRehearsalUnavailable {
		t.Fatalf("root execute error = %v, want rehearsal_unavailable", err)
	}
	t.Log("P4-A7 root_refusal mysqld=refused execute=rehearsal_unavailable")
}

// openExistingWorker binds the already produced fixture without producing again.
func openExistingWorker(t *testing.T) *worker {
	t.Helper()
	requireNative(t)
	paths := newLayout(t)
	requireFixture(t, paths)
	repo, err := local.Open(paths.repo)
	must(t, err)
	t.Cleanup(func() { must(t, repo.Close()) })
	db := openControlDB(t, paths.db)
	w := &worker{
		t: t, paths: paths, db: db, store: physicalstore.New(db), catalog: replication.NewCatalog(db), repo: repo,
		source: openSource(t), mysqld: mysqldPath(t),
	}
	w.remote = w.openFTP()
	w.manifest = loadManifest(t, paths.manifest)
	w.facts = restoreFacts{prepareArgs: map[string][][]string{}, sources: map[string]string{}}
	return w
}
