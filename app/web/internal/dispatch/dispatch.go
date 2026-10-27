// Package dispatch owns the persistent run queue: the cron scheduler, the
// backup worker, the replication worker and the startup recovery. It runs
// beside the HTTP server on the single control-plane database connection.
//
// Ownership rules frozen in the W4 contract:
//
//   - One scheduler loop computes due policies with the stored next_full_at /
//     next_incremental_at columns and advances both after every fire.
//   - One backup worker claims runs with an UPDATE ... RETURNING; full runs
//     order before incremental ones, then scheduled_at. Two backups never run
//     at once.
//   - One replication worker steps the per-repository queues round-robin at
//     concurrency 1; the replicas table stays the only replication queue.
//   - Shutdown cancels the run contexts, waits a bounded 45s for the workers,
//     and only then lets serve close the database. A worker still running
//     after that reports an error the next start's recovery handles.
package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/physical/chain"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// Timing bounds. shutdownWait must exceed chain.Complete's 15s WithoutCancel
// commit window, so a finishing run commits before the database closes.
const (
	schedulerTick     = 30 * time.Second
	shutdownWait      = 45 * time.Second
	replicationPoll   = time.Second
	workerPoll        = time.Second
	claimRetry        = time.Second
	runLogCapDeadline = 10 * time.Second
)

// Config wires the dispatcher to the process capabilities. Everything except
// the database and logger is optional: without the KEK, work root or
// XtraBackup executable the serve process still starts and runs are created
// and then fail visibly with the unavailable error class.
type Config struct {
	Database *store.Store
	Settings resources.Settings
	Logger   *slog.Logger
	// Now injects the clock. Tests freeze time with it; production leaves it
	// nil for the real clock.
	Now func() time.Time
	// Preflight is the resources service the fresh per-run preflight uses.
	Preflight *resources.Service
}

// Dispatcher owns the run queue for one serve process.
type Dispatcher struct {
	config Config
	logger *slog.Logger
	now    func() time.Time
	// cancel stops the loops and every in-flight run context.
	cancel context.CancelFunc
	// done closes when all workers have returned.
	done chan struct{}

	mu sync.Mutex
	// runningCancel holds the context cancel of the current backup, so a
	// cancel request reaches the in-flight chain.Run. runningRunID identifies
	// the run that cancel belongs to.
	runningCancel context.CancelFunc
	runningRunID  string
	replicas      []*replication.Replicator
	closers       []func() error
}

// replicatorFor returns the replicator of one repository, nil when this
// instance does not serve that FTP repository.
func (d *Dispatcher) replicatorFor(repositoryID string) *replication.Replicator {
	for _, replicator := range d.replicas {
		if replicator.RepositoryID == repositoryID {
			return replicator
		}
	}
	return nil
}

// ErrWorkersStillRunning reports that a worker did not finish inside the
// bounded shutdown window. The next start's recovery reclaims whatever the
// interrupted run left behind; this error never describes a completed backup.
var ErrWorkersStillRunning = errors.New("dispatch: workers still running after the bounded shutdown wait; the next start's recovery reclaims their state")

// Start launches recovery, the scheduler, the backup worker and the replication
// worker on the caller's context. The caller must later call Stop exactly once.
func Start(ctx context.Context, config Config) (*Dispatcher, error) {
	if config.Database == nil {
		return nil, errors.New("dispatch: database is required")
	}
	logger := config.Logger
	if logger == nil {
		logger = slog.Default()
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	ctx, cancel := context.WithCancel(ctx)
	d := &Dispatcher{config: config, logger: logger, now: now, cancel: cancel, done: make(chan struct{})}
	if err := d.recover(ctx); err != nil {
		cancel()
		return nil, err
	}
	go d.loop(ctx)
	return d, nil
}

// recover implements the frozen startup order: interrupted control-plane runs
// fail first, then the physical chain recovery runs for each local repository
// with no active runs, then each FTP repository's replication recovery. It all
// happens before the dispatcher loops start.
func (d *Dispatcher) recover(ctx context.Context) error {
	if err := d.failInterruptedRuns(ctx); err != nil {
		return err
	}
	recovered, err := d.openRepositories(ctx)
	if err != nil {
		return err
	}
	if err := d.recoverPhysical(ctx, recovered); err != nil {
		return err
	}
	return d.recoverReplication(ctx)
}

// failInterruptedRuns is the first frozen recovery step: every row a previous
// process left running becomes failed/interrupted, so its slot is never
// re-executed.
func (d *Dispatcher) failInterruptedRuns(ctx context.Context) error {
	runs, err := d.config.Database.InterruptedRuns(ctx)
	if err != nil {
		return fmt.Errorf("dispatch: interrupted runs: %w", err)
	}
	for _, run := range runs {
		entry := store.SystemAudit("runs.interrupted", store.AuditDetail{})
		if failErr := d.config.Database.FinishRunFailed(ctx, run.ID, store.RunErrorInterrupted, "interrupted", entry); failErr != nil {
			return fmt.Errorf("dispatch: interrupt run %s: %w", run.ID, failErr)
		}
		d.logger.InfoContext(ctx, "interrupted run failed at startup",
			"run_id", run.ID, "policy_id", run.PolicyID, "kind", run.Kind, "trigger", run.Trigger)
	}
	return nil
}

// recoverPhysical reclaims the physical state of every local repository.
//
// Decision 9: an unconfigured deployment still serves. Physical recovery needs
// an absolute work root, so without one there is no in-flight state this process
// could have owned; nothing is reclaimed silently because the interrupted
// control-plane rows were already failed and every later run fails visibly.
func (d *Dispatcher) recoverPhysical(ctx context.Context, recovered []recoveredRepository) error {
	if d.workRoot() == "" {
		if len(recovered) > 0 {
			d.logger.WarnContext(ctx, "physical recovery skipped: the work root is not configured; the next configured start reclaims its state")
		}
		return nil
	}
	for _, item := range recovered {
		if err := chain.Recover(ctx, item.physical, item.repo, d.workRoot(), d.config.Database.InstanceID(), nil); err != nil {
			item.close()
			return fmt.Errorf("dispatch: physical recovery: %w", err)
		}
	}
	return nil
}

// recoverReplication reclaims each FTP repository's interrupted copies.
func (d *Dispatcher) recoverReplication(ctx context.Context) error {
	for _, replicator := range d.replicas {
		if err := replicator.Recover(ctx); err != nil {
			return fmt.Errorf("dispatch: replication recovery: %w", err)
		}
	}
	return nil
}

// openRepositories opens every local repository and every FTP repository this
// instance serves. The FTP repositories also become the replication workers'
// replicators. A broken repository is a startup failure, not a silent skip.
func (d *Dispatcher) openRepositories(ctx context.Context) ([]recoveredRepository, error) {
	repositories, err := d.config.Database.Repositories(ctx, "", 1_000_000)
	if err != nil {
		return nil, fmt.Errorf("dispatch: repositories: %w", err)
	}
	var recovered []recoveredRepository
	for _, repository := range repositories {
		switch {
		case repository.Kind == store.RepositoryLocal && repository.Local != nil:
			opened, openErr := d.openLocal(repository)
			if openErr != nil {
				closeAll(recovered)
				return nil, openErr
			}
			recovered = append(recovered, opened)
		case repository.Kind == store.RepositoryFTP && repository.FTP != nil:
			if openErr := d.openRemote(ctx, repository, recovered); openErr != nil {
				closeAll(recovered)
				return nil, openErr
			}
		}
	}
	return recovered, nil
}

// openLocal opens one local repository and records its physical handle.
func (d *Dispatcher) openLocal(repository store.Repository) (recoveredRepository, error) {
	repo, err := local.Open(repository.Local.Root)
	if err != nil {
		return recoveredRepository{}, fmt.Errorf("dispatch: local repository %s: %w", repository.ID, err)
	}
	return recoveredRepository{
		id: repository.ID, repo: repo,
		physical: physicalstore.New(d.config.Database.DB()),
		closeFn:  repo.Close,
	}, nil
}

// openRemote opens one FTP repository's replicator and records its handle. A
// deployment without the KEK cannot open a sealed FTP credential at all, so the
// repository is skipped with a warning instead of aborting serve (decision 9).
// No backup can complete in that state either: every run fails with error_class
// unavailable before the replication handoff.
func (d *Dispatcher) openRemote(ctx context.Context, repository store.Repository, _ []recoveredRepository) error {
	if !d.config.Settings.Credentials() {
		d.logger.WarnContext(ctx, "replication disabled: the KEK is not configured so the FTP credential cannot be opened",
			"repository_id", repository.ID)
		return nil
	}
	replicator, closer, err := d.openReplicator(ctx, repository)
	if err != nil {
		return err
	}
	d.replicas = append(d.replicas, replicator)
	if closer != nil {
		d.closers = append(d.closers, closer)
	}
	return nil
}

// closeAll releases every repository handle opened before a startup failure.
func closeAll(recovered []recoveredRepository) {
	for _, item := range recovered {
		item.close()
	}
}

func (d *Dispatcher) openReplicator(ctx context.Context, repository store.Repository) (*replication.Replicator, func() error, error) {
	settings := d.config.Settings
	if !settings.Credentials() {
		return nil, nil, fmt.Errorf("dispatch: FTP repository %s needs a configured KEK", repository.ID)
	}
	password, err := d.config.Database.RepositorySecret(ctx, repository.ID, settings.Sealer)
	if err != nil {
		return nil, nil, fmt.Errorf("dispatch: FTP repository %s secret: %w", repository.ID, err)
	}
	repo, err := ftpOpen(ctx, ftpConfig{
		Address: repository.FTP.Address(), Username: repository.FTP.Username, Password: string(password),
		BasePath: repository.FTP.BasePath, InstanceID: d.config.Database.InstanceID(), TLSMode: repository.TLSMode,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("dispatch: FTP repository %s: %w", repository.ID, err)
	}
	// Replicator.Local is the published local repository the objects are read
	// from; Remote is this FTP repository. The handle closes at Stop.
	localRepository, err := local.Open(d.localRoot())
	if err != nil {
		closeErr := repo.Close()
		return nil, nil, errors.Join(fmt.Errorf("dispatch: replication local repository: %w", err), closeErr)
	}
	replicator := &replication.Replicator{
		DB: d.config.Database.DB(), Store: physicalstore.New(d.config.Database.DB()),
		Local: localRepository, Remote: replication.NewFTPRemote(repo),
		RepositoryID: repository.ID, InstanceID: d.config.Database.InstanceID(),
		KeyFile: settings.KeyFile, WorkRoot: d.workRoot(),
		Now: d.now,
	}
	return replicator, func() error {
		return errors.Join(repo.Close(), localRepository.Close())
	}, nil
}

// localRoot returns the root of the first local repository, which owns the
// objects replication copies. A deployment without one has no replication.
func (d *Dispatcher) localRoot() string {
	repositories, err := d.config.Database.Repositories(context.Background(), "", 1)
	if err != nil || len(repositories) == 0 || repositories[0].Local == nil {
		return ""
	}
	return repositories[0].Local.Root
}

func (d *Dispatcher) workRoot() string { return d.config.Settings.WorkRoot }

type recoveredRepository struct {
	id       string
	repo     *local.Repository
	physical *physicalstore.Store
	closeFn  func() error
}

func (r recoveredRepository) close() {
	if r.closeFn != nil {
		// A repository close failure on the startup-error path has no better
		// destination than the caller's error; the handle is already unusable.
		_ = r.closeFn() //nolint:errcheck // best-effort cleanup before returning the startup error
	}
}

// loop runs the scheduler, backup worker and replication worker until ctx is
// canceled. One goroutine per channel, concurrency frozen by the contract.
func (d *Dispatcher) loop(ctx context.Context) {
	defer close(d.done)
	var workers sync.WaitGroup
	workers.Add(3)
	go func() {
		defer workers.Done()
		d.scheduler(ctx)
	}()
	go func() {
		defer workers.Done()
		d.backupWorker(ctx)
	}()
	go func() {
		defer workers.Done()
		d.replicationWorker(ctx)
	}()
	workers.Wait()
}

// Stop implements the frozen shutdown order: cancel the run contexts, wait the
// bounded 45s for the workers, then return so serve can close the database.
func (d *Dispatcher) Stop() error {
	d.cancel()
	select {
	case <-d.done:
		d.stop()
		return nil
	case <-time.After(shutdownWait):
		go func() {
			<-d.done
			d.stop()
		}()
		return ErrWorkersStillRunning
	}
}
