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
	"slices"
	"strings"
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
	replicas      []replicaHandle
	replicaCursor int
	closers       []func() error
	// replicatorFailed records (local, replica) pairs whose last lazy open
	// failed, so the replication worker retries them on the next step.
	replicatorFailed map[string]struct{}
}

// replicaHandle is one Replicator for a distinct (local, replica) repository pair.
type replicaHandle struct {
	localID, replicaID string
	replicator         *replication.Replicator
}

const replicatorUnavailable = "replicator_unavailable"

func pairKey(localID, replicaID string) string {
	return localID + "\x00" + replicaID
}

// replicatorFor returns one replicator of this FTP repository, nil when this
// instance does not serve it.
func (d *Dispatcher) replicatorFor(repositoryID string) *replication.Replicator {
	d.mu.Lock()
	defer d.mu.Unlock()
	for _, slot := range d.replicas {
		if slot.replicaID == repositoryID {
			return slot.replicator
		}
	}
	return nil
}

// replicatorForPair returns the replicator of one policy's (local, replica) pair.
func (d *Dispatcher) replicatorForPair(localID, replicaID string) *replication.Replicator {
	d.mu.Lock()
	defer d.mu.Unlock()
	return d.replicatorForPairLocked(localID, replicaID)
}

func (d *Dispatcher) replicatorForPairLocked(localID, replicaID string) *replication.Replicator {
	for _, slot := range d.replicas {
		if slot.localID == localID && slot.replicaID == replicaID {
			return slot.replicator
		}
	}
	return nil
}

func (d *Dispatcher) replicaSnapshot() []replicaHandle {
	d.mu.Lock()
	defer d.mu.Unlock()
	return slices.Clone(d.replicas)
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
	d := &Dispatcher{
		config: config, logger: logger, now: now, cancel: cancel, done: make(chan struct{}),
		replicatorFailed: map[string]struct{}{},
	}
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
		log := runLogWrite(eventInterrupted, nil)
		if failErr := d.config.Database.FinishRunFailed(ctx, run.ID, store.RunErrorInterrupted, "interrupted", entry, log); failErr != nil {
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
	seen := map[string]struct{}{}
	for _, slot := range d.replicaSnapshot() {
		if _, ok := seen[slot.replicaID]; ok {
			continue
		}
		seen[slot.replicaID] = struct{}{}
		if err := slot.replicator.Recover(ctx); err != nil {
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
		if repository.Kind != store.RepositoryLocal || repository.Local == nil {
			continue
		}
		opened, openErr := d.openLocal(repository)
		if openErr != nil {
			closeAll(recovered)
			return nil, openErr
		}
		recovered = append(recovered, opened)
	}
	if openErr := d.openPolicyReplicators(ctx); openErr != nil {
		closeAll(recovered)
		d.stop()
		return nil, openErr
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

// openPolicyReplicators creates one Replicator per distinct
// (local_repository_id, replica_repository_id) pair among policies that have a
// replica, using that policy's local repository root. FTP repositories that are
// not attached to a policy are not opened, so serve starts when the newest
// repository is FTP.
func (d *Dispatcher) openPolicyReplicators(ctx context.Context) error {
	if !d.config.Settings.Credentials() {
		d.logger.WarnContext(ctx, "replication disabled: the KEK is not configured so the FTP credential cannot be opened")
		return nil
	}
	policies, err := d.config.Database.Policies(ctx, "", 1_000_000)
	if err != nil {
		return fmt.Errorf("dispatch: policies: %w", err)
	}
	seen := map[string]struct{}{}
	for _, policy := range policies {
		if policy.ReplicaRepositoryID == nil {
			continue
		}
		key := policy.LocalRepositoryID + "\x00" + *policy.ReplicaRepositoryID
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if openErr := d.openPair(ctx, policy.LocalRepositoryID, *policy.ReplicaRepositoryID); openErr != nil {
			return openErr
		}
	}
	return nil
}

// openPair installs the replicator of one (local, replica) pair. A successful
// install always runs the durable catch-up, because an install is the moment the
// pair becomes usable: at Start, after a lazy open, and after a retried reopen.
// The catch-up is an idempotent insert per completed backup, so an unreachable
// remote only delays the copy and never loses it.
func (d *Dispatcher) openPair(ctx context.Context, localID, replicaID string) error {
	if d.replicatorForPair(localID, replicaID) != nil {
		d.clearReplicatorFailed(localID, replicaID)
		return nil
	}
	localRow, replicaRow, err := d.loadPairRepositories(ctx, localID, replicaID)
	if err != nil {
		return err
	}
	replicator, closer, err := d.openReplicator(ctx, replicaRow, localRow, localRow.Local.Root)
	if err != nil {
		return err
	}
	d.installReplicator(localID, replicaID, replicator, closer)
	d.catchUpReplication(ctx, localID, replicaID, replicator)
	return nil
}

func (d *Dispatcher) loadPairRepositories(ctx context.Context, localID, replicaID string) (store.Repository, store.Repository, error) {
	localRow, err := d.config.Database.Repository(ctx, localID)
	if err != nil {
		return store.Repository{}, store.Repository{}, fmt.Errorf("dispatch: local repository %s: %w", localID, err)
	}
	if localRow.Kind != store.RepositoryLocal || localRow.Local == nil {
		return store.Repository{}, store.Repository{}, fmt.Errorf("dispatch: repository %s is not a local repository", localID)
	}
	replicaRow, err := d.config.Database.Repository(ctx, replicaID)
	if err != nil {
		return store.Repository{}, store.Repository{}, fmt.Errorf("dispatch: replica repository %s: %w", replicaID, err)
	}
	if replicaRow.Kind != store.RepositoryFTP || replicaRow.FTP == nil {
		return store.Repository{}, store.Repository{}, fmt.Errorf("dispatch: repository %s is not an FTP replica", replicaID)
	}
	return localRow, replicaRow, nil
}

func (d *Dispatcher) installReplicator(localID, replicaID string, replicator *replication.Replicator, closer func() error) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.replicatorForPairLocked(localID, replicaID) != nil {
		if closer != nil {
			_ = closer() //nolint:errcheck // duplicate open; the live handle stays
		}
		delete(d.replicatorFailed, pairKey(localID, replicaID))
		return
	}
	d.replicas = append(d.replicas, replicaHandle{
		localID: localID, replicaID: replicaID, replicator: replicator,
	})
	if closer != nil {
		d.closers = append(d.closers, closer)
	}
	delete(d.replicatorFailed, pairKey(localID, replicaID))
}

// ensurePolicyReplicator returns the replicator for a policy's (local, replica)
// pair, opening it — and running the pair's catch-up — when this process has not
// seen the pair yet. It is called from the completion path only: a trigger never
// dials the remote. Open failures are logged, retried by the replication worker,
// and never fail the backup itself; the backup they leave unqueued is picked up
// by the catch-up that runs when the pair is finally installed.
func (d *Dispatcher) ensurePolicyReplicator(ctx context.Context, policy store.Policy, runID string) *replication.Replicator {
	if policy.ReplicaRepositoryID == nil {
		return nil
	}
	localID := policy.LocalRepositoryID
	replicaID := *policy.ReplicaRepositoryID
	if existing := d.replicatorForPair(localID, replicaID); existing != nil {
		return existing
	}
	if err := d.openPair(ctx, localID, replicaID); err != nil {
		d.markReplicatorFailed(localID, replicaID)
		d.logReplicatorUnavailable(ctx, runID, replicaID, err)
		return nil
	}
	return d.replicatorForPair(localID, replicaID)
}

// catchUpReplication queues every completed, locally present backup of every
// source this pair serves. EnqueueCompleted is an idempotent insert per backup,
// so running it at each install is what makes a replica durable across an
// unavailable remote: a backup completed while the pair was down is queued by
// the catch-up instead of being lost, and FIFO order is preserved because the
// insert order is the creation order.
func (d *Dispatcher) catchUpReplication(ctx context.Context, localID, replicaID string, replicator *replication.Replicator) {
	sources, err := d.pairSources(ctx, localID, replicaID)
	if err != nil {
		d.logger.ErrorContext(ctx, "cannot list the pair's sources for the replication catch-up",
			"local_repository_id", localID, "repository_id", replicaID, "error", err)
		return
	}
	for _, sourceID := range sources {
		if err = replicator.EnqueueCompleted(ctx, sourceID); err != nil {
			d.logger.ErrorContext(ctx, "cannot run the replication catch-up",
				"local_repository_id", localID, "repository_id", replicaID,
				"source_id", sourceID, "error", err)
			return
		}
	}
}

// pairSources returns the distinct sources of the policies that use one pair.
func (d *Dispatcher) pairSources(ctx context.Context, localID, replicaID string) ([]string, error) {
	policies, err := d.config.Database.Policies(ctx, "", 1_000_000)
	if err != nil {
		return nil, err
	}
	seen := map[string]struct{}{}
	sources := make([]string, 0, len(policies))
	for _, policy := range policies {
		if policy.ReplicaRepositoryID == nil || *policy.ReplicaRepositoryID != replicaID ||
			policy.LocalRepositoryID != localID {
			continue
		}
		if _, ok := seen[policy.SourceID]; ok {
			continue
		}
		seen[policy.SourceID] = struct{}{}
		sources = append(sources, policy.SourceID)
	}
	return sources, nil
}

func (d *Dispatcher) markReplicatorFailed(localID, replicaID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.replicatorFailed == nil {
		d.replicatorFailed = map[string]struct{}{}
	}
	d.replicatorFailed[pairKey(localID, replicaID)] = struct{}{}
}

func (d *Dispatcher) clearReplicatorFailed(localID, replicaID string) {
	d.mu.Lock()
	defer d.mu.Unlock()
	delete(d.replicatorFailed, pairKey(localID, replicaID))
}

func (d *Dispatcher) logReplicatorUnavailable(ctx context.Context, runID, replicaID string, err error) {
	d.logger.ErrorContext(ctx, "replicator unavailable",
		"repository_id", replicaID, "code", replicatorUnavailable, "error", err)
	if runID == "" {
		return
	}
	d.logEvent(ctx, runID, eventReplicationStep, map[string]any{
		"status": "failed", "code": replicatorUnavailable, "repository_id": replicaID,
	})
}

// claimPolicyReplicators opens the pairs of policies this process has not
// installed yet, then runs their catch-up. The replication worker calls it each
// cycle, so a policy created after Start reaches a replicator even if no run ever
// completed again; the completion path is the fast route, this is the durable
// one. It dials nothing when every pair is already installed.
func (d *Dispatcher) claimPolicyReplicators(ctx context.Context) {
	if !d.config.Settings.Credentials() {
		return
	}
	policies, err := d.config.Database.Policies(ctx, "", 1_000_000)
	if err != nil {
		d.logger.ErrorContext(ctx, "cannot list policies for the replication worker", "error", err)
		return
	}
	seen := map[string]struct{}{}
	for _, policy := range policies {
		if policy.ReplicaRepositoryID == nil {
			continue
		}
		key := pairKey(policy.LocalRepositoryID, *policy.ReplicaRepositoryID)
		if _, ok := seen[key]; ok {
			continue
		}
		seen[key] = struct{}{}
		if d.replicatorForPair(policy.LocalRepositoryID, *policy.ReplicaRepositoryID) != nil {
			continue
		}
		if err = d.openPair(ctx, policy.LocalRepositoryID, *policy.ReplicaRepositoryID); err != nil {
			d.markReplicatorFailed(policy.LocalRepositoryID, *policy.ReplicaRepositoryID)
			d.logReplicatorUnavailable(ctx, "", *policy.ReplicaRepositoryID, err)
		}
	}
}

// retryFailedReplicators reopens pairs whose last open failed. A later Step
// therefore picks up a pair once the remote becomes reachable, and the reopen's
// catch-up queues every backup completed while it was down.
func (d *Dispatcher) retryFailedReplicators(ctx context.Context) {
	d.mu.Lock()
	keys := make([]string, 0, len(d.replicatorFailed))
	for key := range d.replicatorFailed {
		keys = append(keys, key)
	}
	d.mu.Unlock()
	for _, key := range keys {
		localID, replicaID, ok := strings.Cut(key, "\x00")
		if !ok {
			continue
		}
		if d.replicatorForPair(localID, replicaID) != nil {
			d.clearReplicatorFailed(localID, replicaID)
			continue
		}
		if err := d.openPair(ctx, localID, replicaID); err != nil {
			d.logReplicatorUnavailable(ctx, "", replicaID, err)
		}
	}
}

// closeAll releases every repository handle opened before a startup failure.
func closeAll(recovered []recoveredRepository) {
	for _, item := range recovered {
		item.close()
	}
}

// openReplicator opens the FTP remote and the policy's local repository. The
// replicator records both identities, so its queue is the rows of this pair
// only and a replica repository shared by two local repositories cannot make it
// read the other's backups.
func (d *Dispatcher) openReplicator(ctx context.Context, repository, localRow store.Repository, localRoot string) (*replication.Replicator, func() error, error) {
	settings := d.config.Settings
	if !settings.Credentials() {
		return nil, nil, fmt.Errorf("dispatch: FTP repository %s needs a configured KEK", repository.ID)
	}
	password, err := d.config.Database.RepositorySecret(ctx, repository.ID, settings.Sealer)
	if err != nil {
		return nil, nil, fmt.Errorf("dispatch: FTP repository %s secret: %w", repository.ID, err)
	}
	remote, remoteClose, err := openRemote(ctx, ftpConfig{
		Address: repository.FTP.Address(), Username: repository.FTP.Username, Password: string(password),
		BasePath: repository.FTP.BasePath, InstanceID: d.config.Database.InstanceID(), TLSMode: repository.TLSMode,
	})
	if err != nil {
		return nil, nil, fmt.Errorf("dispatch: FTP repository %s: %w", repository.ID, err)
	}
	if remoteClose == nil {
		remoteClose = func() error { return nil }
	}
	// Replicator.Local is the published local repository the objects are read
	// from; Remote is this FTP repository. The handle closes at Stop.
	localRepository, err := local.Open(localRoot)
	if err != nil {
		closeErr := remoteClose()
		return nil, nil, errors.Join(fmt.Errorf("dispatch: replication local repository: %w", err), closeErr)
	}
	replicator := &replication.Replicator{
		DB: d.config.Database.DB(), Store: physicalstore.New(d.config.Database.DB()),
		Local: localRepository, Remote: remote,
		RepositoryID: repository.ID, LocalRepositoryID: localRow.ID,
		InstanceID: d.config.Database.InstanceID(),
		KeyFile:    settings.KeyFile, WorkRoot: d.workRoot(),
		Now: d.now,
	}
	return replicator, func() error {
		return errors.Join(remoteClose(), localRepository.Close())
	}, nil
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
