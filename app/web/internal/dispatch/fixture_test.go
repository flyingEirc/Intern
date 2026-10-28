package dispatch

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/artifact"
	"github.com/flyingEirc/Intern/internal/credential"
)

// testSocketPath returns an absolute socket path nothing listens on. The real
// MySQL driver reports the failed identity query, which is the reachable
// preflight failure on a host without a mounted MySQL server.
func testSocketPath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "absent.sock")
}

// fixture owns one real control-plane database, one KEK and the repository
// and source rows the dispatcher needs. sealer and seeding always work; runtime
// is what the dispatcher receives, and it is zero for the unconfigured
// deployment tests.
type fixture struct {
	t        *testing.T
	database *store.Store
	dir      string
	keyFile  string
	sealer   *credential.Sealer
	runtime  resources.Settings
	logs     *bytes.Buffer
}

// newFixture opens a real SQLite control plane under an exclusive lock and
// prepares a KEK outside the metadata directory.
func newFixture(t *testing.T) *fixture {
	t.Helper()
	f := &fixture{t: t, dir: filepath.Join(t.TempDir(), "metadata"), logs: &bytes.Buffer{}}
	f.database = f.openStore()
	// One cleanup closes whichever connection is current. It is registered
	// before any dispatcher, so cleanups (LIFO) stop workers first and only
	// then close the database.
	t.Cleanup(func() { _ = f.database.Close() })
	keys := t.TempDir()
	f.keyFile = filepath.Join(keys, "kek.json")
	if err := artifact.GenerateKey(f.keyFile); err != nil {
		t.Fatal(err)
	}
	sealer, err := credential.Open(f.keyFile, f.dir)
	if err != nil {
		t.Fatal(err)
	}
	f.sealer = sealer
	f.runtime = resources.Settings{
		Sealer: sealer, KeyFile: f.keyFile, MetadataDir: f.dir,
		WorkRoot: f.workRoot(), XtrabackupPath: "/usr/bin/false",
		MinWorkBytes: 1, MinBackupBytes: 1,
	}
	return f
}

func (f *fixture) openStore() *store.Store {
	f.t.Helper()
	database, err := store.Open(f.t.Context(), f.dir)
	if err != nil {
		f.t.Fatal(err)
	}
	return database
}

// closeStore releases the SQLite lock so the same directory can be reopened,
// which is the restart the recovery and idempotency tests exercise.
func (f *fixture) closeStore() {
	f.t.Helper()
	if err := f.database.Close(); err != nil {
		f.t.Fatal(err)
	}
}

// reopen is a process restart at the database boundary: the row survives, the
// connection does not.
func (f *fixture) reopen() {
	f.t.Helper()
	f.closeStore()
	f.database = f.openStore()
	f.t.Cleanup(func() { _ = f.database.Close() })
}

// workRoot is a private absolute scratch root with mode 0700.
func (f *fixture) workRoot() string {
	f.t.Helper()
	root := filepath.Join(f.t.TempDir(), "work")
	if err := os.Mkdir(root, 0o700); err != nil {
		f.t.Fatal(err)
	}
	return root
}

// unconfigured turns the runtime into the decision-9 deployment: no KEK, work
// root or XtraBackup executable. Rows may still exist from a configured start.
func (f *fixture) unconfigured() { f.runtime = resources.Settings{} }

// repository creates one local repository row and its directory.
func (f *fixture) repository(name string) store.Repository {
	f.t.Helper()
	root := filepath.Join(f.t.TempDir(), name)
	if err := os.Mkdir(root, 0o700); err != nil {
		f.t.Fatal(err)
	}
	created, err := f.database.CreateRepository(f.t.Context(), store.RepositoryInput{
		Name: name, Kind: store.RepositoryLocal, TLSMode: store.TLSModeNone, Root: root,
	}, nil, store.SystemAudit("repositories.create", store.AuditDetail{}))
	if err != nil {
		f.t.Fatal(err)
	}
	return created
}

// source creates one MySQL source row with a sealed password. The socket is
// unreachable, which is the failure the physical preflight reports here.
func (f *fixture) source(name, socket string) store.Source {
	f.t.Helper()
	created, err := f.database.CreateSource(f.t.Context(), store.SourceInput{
		Name: name, Socket: socket, Datadir: "/var/lib/mysql", Username: "backup",
		Secret: []byte("{{SECRET_dispatch_source}}"),
	}, f.sealer, store.SystemAudit("sources.create", store.AuditDetail{}))
	if err != nil {
		f.t.Fatal(err)
	}
	return created
}

// policy creates one disabled policy and optionally enables it through the
// store's preflight-gated path.
func (f *fixture) policy(name string, sourceID, repositoryID string, enabled bool) store.Policy {
	f.t.Helper()
	created, err := f.database.CreatePolicy(f.t.Context(), store.PolicyInput{
		Name: name, SourceID: sourceID, LocalRepositoryID: repositoryID,
		FullCron: "0 * * * *", IncrementalCron: "30 * * * *", Timezone: "UTC",
		MaxChainLength: 3, Parallel: 1, ZstdLevel: 3,
	}, store.SystemAudit("policies.create", store.AuditDetail{}))
	if err != nil {
		f.t.Fatal(err)
	}
	if !enabled {
		return created
	}
	record := &store.PreflightRecord{OK: true, Checks: []string{"identity"}, Warnings: []store.PreflightWarning{}}
	enabledPolicy, err := f.database.EnablePolicy(f.t.Context(), created.ID, true, record,
		store.SystemAudit("policies.enable", store.AuditDetail{}))
	if err != nil {
		f.t.Fatal(err)
	}
	return enabledPolicy
}

// ftpRepository creates one FTP repository row with a sealed password. Tests
// that open a replicator replace openRemote so this never dials.
func (f *fixture) ftpRepository(name string) store.Repository {
	f.t.Helper()
	created, err := f.database.CreateRepository(f.t.Context(), store.RepositoryInput{
		Name: name, Kind: store.RepositoryFTP, TLSMode: store.TLSModePlain,
		FTP:    &store.FTPRepositoryConfig{Host: "127.0.0.1", Port: 21, BasePath: "/backups", Username: "backup"},
		Secret: []byte("{{SECRET_dispatch_ftp}}"),
	}, f.sealer, store.SystemAudit("repositories.create", store.AuditDetail{PlaintextFTP: true}))
	if err != nil {
		f.t.Fatal(err)
	}
	return created
}

// replicaPolicy creates a disabled policy that points at a replica repository.
func (f *fixture) replicaPolicy(name, sourceID, localID, replicaID string) store.Policy {
	f.t.Helper()
	created, err := f.database.CreatePolicy(f.t.Context(), store.PolicyInput{
		Name: name, SourceID: sourceID, LocalRepositoryID: localID, ReplicaRepositoryID: &replicaID,
		FullCron: "0 * * * *", IncrementalCron: "30 * * * *", Timezone: "UTC",
		MaxChainLength: 3, Parallel: 1, ZstdLevel: 3,
	}, store.SystemAudit("policies.create", store.AuditDetail{}))
	if err != nil {
		f.t.Fatal(err)
	}
	return created
}

// schedule stamps the persisted next trigger times.
func (f *fixture) schedule(policyID string, full, incremental time.Time) {
	f.t.Helper()
	if err := f.database.InitializeSchedule(f.t.Context(), policyID, full, incremental); err != nil {
		f.t.Fatal(err)
	}
}

// policySchedule reads back the persisted next trigger times.
func (f *fixture) policySchedule(policyID string) (*time.Time, *time.Time) {
	f.t.Helper()
	full, incremental, err := f.database.PolicySchedule(f.t.Context(), policyID)
	if err != nil {
		f.t.Fatal(err)
	}
	return full, incremental
}

// runs lists a policy's runs newest first.
func (f *fixture) runs(policyID string) []store.Run {
	f.t.Helper()
	runs, err := f.database.Runs(f.t.Context(), store.RunsFilter{PolicyID: policyID, Limit: 200})
	if err != nil {
		f.t.Fatal(err)
	}
	return runs
}

// run reads one run.
func (f *fixture) run(id string) store.Run {
	f.t.Helper()
	run, err := f.database.Run(f.t.Context(), id)
	if err != nil {
		f.t.Fatal(err)
	}
	return run
}

// events lists one run's full log in sequence order.
func (f *fixture) events(runID string) []store.RunLog {
	f.t.Helper()
	logs, err := f.database.RunLogsAfter(f.t.Context(), runID, 0, store.MaxRunLogRows+1)
	if err != nil {
		f.t.Fatal(err)
	}
	return logs
}

// eventNames maps one run's log to its event names.
func (f *fixture) eventNames(runID string) []string {
	f.t.Helper()
	logs := f.events(runID)
	names := make([]string, 0, len(logs))
	for _, row := range logs {
		names = append(names, row.Event)
	}
	return names
}

// systemAuditCount counts the audit rows the dispatcher wrote for one action.
// These rows never pass through HTTP middleware, so a request-scoped count
// would not see them.
func (f *fixture) systemAuditCount(action string) int {
	f.t.Helper()
	var count int
	query := `SELECT COUNT(*) FROM audit_logs WHERE actor_type = 'system' AND action = ? AND result = 'success'`
	if err := f.database.DB().QueryRowContext(f.t.Context(), query, action).Scan(&count); err != nil {
		f.t.Fatal(err)
	}
	return count
}

// newDispatcher builds the real worker set without starting it, so a test can
// drive one loop iteration (tick, claim) against a controlled clock.
func (f *fixture) newDispatcher(now func() time.Time) *Dispatcher {
	f.t.Helper()
	_, cancel := context.WithCancel(context.Background())
	d := &Dispatcher{
		config: Config{
			Database: f.database, Settings: f.runtime, Logger: f.logger(),
			Preflight: &resources.Service{Store: f.database, Settings: f.runtime},
		},
		logger: f.logger(), now: now, cancel: cancel, done: make(chan struct{}),
		replicatorFailed: map[string]struct{}{},
	}
	f.t.Cleanup(cancel)
	return d
}

// start launches the real dispatcher, including startup recovery, on a context
// that the fixture cancels before the database closes.
func (f *fixture) start(now func() time.Time) *Dispatcher {
	f.t.Helper()
	d, err := Start(f.t.Context(), Config{
		Database: f.database, Settings: f.runtime, Logger: f.logger(),
		Now: now, Preflight: &resources.Service{Store: f.database, Settings: f.runtime},
	})
	if err != nil {
		f.t.Fatal(err)
	}
	f.t.Cleanup(func() { _ = d.Stop() })
	return d
}

// logger keeps the dispatcher's own records for the secret scans.
func (f *fixture) logger() *slog.Logger {
	return slog.New(slog.NewJSONHandler(f.logs, nil))
}

// waitFor polls cond until it holds or the deadline passes.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// waitForRunState polls one run until it reaches status with the given class.
func (f *fixture) waitForRunState(id, status, class string) store.Run {
	f.t.Helper()
	var last store.Run
	waitFor(f.t, "run "+id+" to reach "+status, func() bool {
		last = f.run(id)
		if last.Status != status {
			return false
		}
		return class == "" || (last.ErrorClass != nil && *last.ErrorClass == class)
	})
	return last
}
