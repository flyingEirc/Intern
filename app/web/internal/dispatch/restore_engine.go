package dispatch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"path/filepath"
	"sync"

	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	physicalstore "github.com/flyingEirc/Intern/internal/physical/store"
	"github.com/flyingEirc/Intern/internal/replication"
	"github.com/flyingEirc/Intern/internal/restore"
	"github.com/flyingEirc/Intern/internal/storage/ftp"
	"github.com/flyingEirc/Intern/internal/storage/local"
)

// NewRestoreEngine builds the production restore engine from the deployment
// settings. A deployment missing any restore input returns (nil, nil): the plan
// and execute APIs then answer 503 and a queued restore fails visibly as
// unavailable. A configured deployment whose export root violates the frozen
// location rules is a startup error, not a per-request failure.
func NewRestoreEngine(database *store.Store, settings resources.Settings, logger *slog.Logger) (RestoreEngine, error) {
	if !settings.Restore() {
		return nil, nil
	}
	if logger == nil {
		logger = slog.Default()
	}
	forbidden := []string{settings.MetadataDir}
	if err := restore.CheckExportRoot(settings.ExportRoot, forbidden...); err != nil {
		return nil, fmt.Errorf("restore export root: %w", err)
	}
	return &RestoreEngineAdapter{
		database: database, settings: settings, logger: logger,
		locator: &restoreLocator{database: database, settings: settings, locals: map[string]*local.Repository{}},
	}, nil
}

// RestoreEngineAdapter wraps internal/restore with the W5 wire and store
// types. It owns the locator's cached local repositories and closes them when
// the dispatcher stops.
type RestoreEngineAdapter struct {
	database *store.Store
	settings resources.Settings
	logger   *slog.Logger
	locator  *restoreLocator
}

// Plan builds a fresh plan and converts it to the persisted view. The plan is
// read-only: no restore row, file or FTP write.
func (e *RestoreEngineAdapter) Plan(ctx context.Context, backupID string) (store.RestorePlan, error) {
	options, err := e.options(ctx)
	if err != nil {
		return store.RestorePlan{}, err
	}
	plan, err := restore.BuildPlan(ctx, options, backupID)
	if err != nil {
		return store.RestorePlan{}, restoreEngineError(err)
	}
	return restorePlanView(plan, e.settings.Rehearsal()), nil
}

// Execute runs one restore through the engine and reports the exact cleanup
// observation. A published export is never removed; a rehearsal that ran and
// failed is reported through the verify fields, not as an execution error.
func (e *RestoreEngineAdapter) Execute(ctx context.Context, req RestoreRequest, progress RestoreProgress) (RestoreResult, error) {
	options, err := e.options(ctx)
	if err != nil {
		return RestoreResult{}, &RestoreExecError{Code: "work_failed", Phase: "fetching"}
	}
	engineProgress := func(phase restore.Phase, data map[string]any) {
		if progress != nil {
			progress(string(phase), data)
		}
	}
	result, err := restore.Execute(ctx, options, restore.Request{
		RestoreID: req.RestoreID,
		Plan:      restore.Plan{TargetBackupID: req.Plan.BackupID, Digest: req.Plan.Digest},
		Rehearse:  req.Rehearse,
		VerifySQL: req.VerifySQL,
	}, engineProgress)
	if err != nil {
		return RestoreResult{}, restoreExecutionFailure(err, options, req.RestoreID)
	}
	effects := store.RestoreSideEffects{
		WorkDirRemoved:       !pathExists(filepath.Join(options.WorkRoot, "restore-"+req.RestoreID)),
		PendingExportRemoved: !pathExists(filepath.Join(options.ExportRoot, ".pending-"+req.RestoreID)),
		ExportPublished:      pathExists(result.ExportPath),
	}
	verifyStatus, verifyResult := restoreVerify(result.Verify, req.Rehearse)
	return RestoreResult{
		ExportPath: result.ExportPath, ExportBytes: result.ExportBytes,
		SideEffects: effects, VerifyStatus: verifyStatus, VerifyResult: verifyResult,
	}, nil
}

// Recover reclaims the exact targets of the given restore ids and nothing else.
func (e *RestoreEngineAdapter) Recover(ctx context.Context, restoreIDs []string) error {
	return restore.Recover(ctx, e.settings.WorkRoot, e.settings.ExportRoot, restoreIDs)
}

// Close releases the cached local repository handles.
func (e *RestoreEngineAdapter) Close() error {
	return e.locator.Close()
}

// options resolves one engine call. Local repository roots are added to the
// restricted set so the engine refuses an export root inside any of them; they
// live in the database and can change between calls.
func (e *RestoreEngineAdapter) options(ctx context.Context) (restore.Options, error) {
	roots, err := e.localRoots(ctx)
	if err != nil {
		return restore.Options{}, err
	}
	restricted := make([]string, 0, len(roots)+1)
	restricted = append(restricted, e.settings.MetadataDir)
	restricted = append(restricted, roots...)
	expansion := e.settings.RestoreExpansionFactor
	if expansion <= 0 {
		expansion = restore.DefaultExpansionFactor
	}
	return restore.Options{
		Store:           physicalstore.New(e.database.DB()),
		Replication:     replication.NewCatalog(e.database.DB()),
		Locator:         e.locator,
		KeyFile:         e.settings.KeyFile,
		WorkRoot:        e.settings.WorkRoot,
		ExportRoot:      e.settings.ExportRoot,
		XtrabackupPath:  e.settings.XtrabackupPath,
		XbstreamPath:    e.settings.XbstreamPath,
		MysqldPath:      e.settings.MysqldPath,
		RestrictedRoots: restricted,
		ExpansionFactor: expansion,
		Parallel:        1,
		UseMemory:       e.settings.RestoreUseMemory,
	}, nil
}

// localRoots lists every configured local repository root. The repository list
// is paged to its end: an export root must be checked against every local
// repository, so a deployment with more rows than one page cannot leave a root
// unexamined.
func (e *RestoreEngineAdapter) localRoots(ctx context.Context) ([]string, error) {
	var roots []string
	before := ""
	for {
		repositories, err := e.database.Repositories(ctx, before, repositoryPageSize)
		if err != nil {
			return nil, err
		}
		for _, repository := range repositories {
			if repository.Kind == store.RepositoryLocal && repository.Local != nil && repository.Local.Root != "" {
				roots = append(roots, repository.Local.Root)
			}
		}
		if len(repositories) < repositoryPageSize {
			return roots, nil
		}
		before = repositories[len(repositories)-1].ID
	}
}

// repositoryPageSize is one page of the repository list; localRoots keeps reading
// until a short page ends the table.
const repositoryPageSize = 200

// restorePlanView converts the engine plan to the persisted wire view.
func restorePlanView(plan restore.Plan, rehearsalAvailable bool) store.RestorePlan {
	links := make([]store.RestorePlanLink, 0, len(plan.Links))
	for _, link := range plan.Links {
		links = append(links, store.RestorePlanLink{
			BackupID: link.BackupID, Seq: int64(link.Seq), Kind: link.Kind,
			FromLSN: link.FromLSN, ToLSN: link.ToLSN, Source: link.Source,
			RepositoryID: link.RepositoryID, PayloadSize: link.PayloadSize,
			MetadataSize: link.MetadataSize,
		})
	}
	risks := plan.Risks
	if risks == nil {
		risks = []string{}
	}
	return store.RestorePlan{
		BackupID: plan.TargetBackupID, ChainID: plan.ChainID, SourceID: plan.SourceID,
		ServerUUID: plan.ServerUUID, ServerVersion: plan.ServerVersion, ToolVersion: plan.ToolVersion,
		TargetToLSN: plan.TargetToLSN, Links: links,
		RequiredWorkBytes: plan.RequiredWorkBytes, RequiredExportBytes: plan.RequiredExportBytes,
		AvailableWorkBytes: plan.AvailableWorkBytes, AvailableExportBytes: plan.AvailableExportBytes,
		ExportPath: plan.ExportPath, Risks: risks, Digest: plan.Digest,
		RehearsalAvailable: rehearsalAvailable,
	}
}

// restoreVerify converts the optional rehearsal outcome. The rehearsal result is
// stored and returned as-is; the status is separate from the restore status. A
// successful execution that asked for a rehearsal but produced no result is
// not_run, never not_requested: the row must not claim a rehearsal was never
// wanted when it was wanted and did not happen.
func restoreVerify(verify *restore.VerifyResult, rehearse bool) (string, any) {
	if verify == nil {
		return store.VerifyStatusWithoutResult(rehearse), nil
	}
	status := verify.Status
	if status != store.RestoreVerifyPassed && status != store.RestoreVerifyFailed {
		status = store.RestoreVerifyFailed
	}
	return status, verify
}

// restoreExecutionFailure converts one engine execution error into the W5 seam.
// A typed *restore.ExecError already carries the engine's own cleanup
// observation and is returned unchanged. An untyped or context error observed
// nothing, so the two exact targets and the published export are read from the
// filesystem here, the same way the success path reads them: side_effects must
// never report "nothing removed" for a cleanup nobody watched.
func restoreExecutionFailure(err error, options restore.Options, restoreID string) error {
	failure := restoreEngineError(err)
	var engineExec *restore.ExecError
	if errors.As(err, &engineExec) {
		return failure
	}
	var execErr *RestoreExecError
	if !errors.As(failure, &execErr) {
		return failure
	}
	execErr.SideEffects = store.RestoreSideEffects{
		WorkDirRemoved:       !pathExists(filepath.Join(options.WorkRoot, "restore-"+restoreID)),
		PendingExportRemoved: !pathExists(filepath.Join(options.ExportRoot, ".pending-"+restoreID)),
		ExportPublished:      pathExists(filepath.Join(options.ExportRoot, restoreID)),
	}
	return execErr
}

// restoreEngineError converts the engine's typed errors onto the W5 seam.
func restoreEngineError(err error) error {
	var planErr *restore.PlanError
	if errors.As(err, &planErr) {
		return &RestorePlanError{Code: planErr.Code, BackupID: planErr.BackupID}
	}
	var execErr *restore.ExecError
	if errors.As(err, &execErr) {
		return &RestoreExecError{
			Code: execErr.Code, Phase: execErr.Phase,
			SideEffects: store.RestoreSideEffects{
				WorkDirRemoved:       execErr.SideEffects.WorkDirRemoved,
				PendingExportRemoved: execErr.SideEffects.PendingExportRemoved,
				ExportPublished:      execErr.SideEffects.ExportPublished,
			},
		}
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return &RestoreExecError{Code: "canceled"}
	}
	return &RestoreExecError{Code: "work_failed"}
}

// restoreLocator resolves where a backup's ciphertext can be read. Local
// repositories are opened once and cached; the engine only reads from them, and
// the dispatcher closes the handles at shutdown. FTP sessions are opened per
// link by the engine and closed by it.
type restoreLocator struct {
	database *store.Store
	settings resources.Settings

	mu     sync.Mutex
	locals map[string]*local.Repository
	closed bool
}

// Local returns the local repository that holds backupID's object: the run that
// published the backup snapshotted the repository it ran on.
func (l *restoreLocator) Local(ctx context.Context, backupID string) (*local.Repository, error) {
	run, err := l.database.RunByBackupID(ctx, backupID)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	if run.LocalRepositoryID == nil || *run.LocalRepositoryID == "" {
		return nil, nil
	}
	repository, err := l.database.Repository(ctx, *run.LocalRepositoryID)
	if err != nil {
		if errors.Is(err, store.ErrNotFound) {
			return nil, nil
		}
		return nil, err
	}
	if repository.Kind != store.RepositoryLocal || repository.Local == nil {
		return nil, fmt.Errorf("restore: repository %s is not a local repository", repository.ID)
	}
	return l.openLocal(repository.ID, repository.Local.Root)
}

func (l *restoreLocator) openLocal(id, root string) (*local.Repository, error) {
	key := id + "\x00" + root
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil, errors.New("restore: locator is closed")
	}
	if repository, ok := l.locals[key]; ok {
		return repository, nil
	}
	repository, err := local.Open(root)
	if err != nil {
		return nil, err
	}
	l.locals[key] = repository
	return repository, nil
}

// Replicas lists the FTP replicas of one backup, verified rows first. The
// engine rechecks the status and every member size itself.
func (l *restoreLocator) Replicas(ctx context.Context, backupID string) (refs []restore.ReplicaRef, retErr error) {
	rows, err := l.database.DB().QueryContext(ctx,
		`SELECT r.repository_id FROM replicas r JOIN repositories repo ON repo.id = r.repository_id
WHERE r.backup_id = ? AND repo.kind = 'ftp'
ORDER BY CASE r.status WHEN 'verified' THEN 0 ELSE 1 END, r.repository_id`, backupID)
	if err != nil {
		return nil, err
	}
	defer func() { retErr = errors.Join(retErr, rows.Close()) }()
	refs = make([]restore.ReplicaRef, 0, 2)
	for rows.Next() {
		var repositoryID string
		if err = rows.Scan(&repositoryID); err != nil {
			return nil, err
		}
		id := repositoryID
		refs = append(refs, restore.ReplicaRef{
			RepositoryID: id,
			Open: func(ctx context.Context) (*ftp.Repository, error) {
				return l.openReplica(ctx, id)
			},
		})
	}
	if err = rows.Err(); err != nil {
		return nil, err
	}
	return refs, retErr
}

// openReplica unseals the FTP credential and opens one private session. The
// engine closes it after the link that used it.
func (l *restoreLocator) openReplica(ctx context.Context, repositoryID string) (*ftp.Repository, error) {
	repository, err := l.database.Repository(ctx, repositoryID)
	if err != nil {
		return nil, err
	}
	if repository.Kind != store.RepositoryFTP || repository.FTP == nil {
		return nil, fmt.Errorf("restore: repository %s is not an FTP repository", repositoryID)
	}
	if !l.settings.Credentials() {
		return nil, errors.New("restore: FTP credentials need a configured KEK")
	}
	password, err := l.database.RepositorySecret(ctx, repositoryID, l.settings.Sealer)
	if err != nil {
		return nil, err
	}
	return ftp.Open(ctx, ftp.Config{
		Address: repository.FTP.Address(), Username: repository.FTP.Username, Password: string(password),
		BasePath: repository.FTP.BasePath, InstanceID: l.database.InstanceID(), TLSMode: repository.TLSMode,
	})
}

// Close releases every cached local repository handle exactly once.
func (l *restoreLocator) Close() error {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.closed {
		return nil
	}
	l.closed = true
	var err error
	for key, repository := range l.locals {
		err = errors.Join(err, repository.Close())
		delete(l.locals, key)
	}
	return err
}
