package restore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"

	"github.com/flyingEirc/Intern/internal/fsutil"
	"github.com/flyingEirc/Intern/internal/physical"
)

var restoreIDPattern = regexp.MustCompile(`^[0-9a-f]{32}$`)

// Execute re-plans the restore point, refuses when the request does not match
// that fresh plan, and only then touches the filesystem. Any failure before the
// export is published removes exactly this restore's work directory and pending
// export. A rehearsal failure after publication is reported in Result.Verify and
// does not remove the export.
func Execute(ctx context.Context, opts Options, req Request, progress Progress) (Result, error) {
	if err := validateRequest(opts, req); err != nil {
		return Result{}, err
	}
	fresh, err := BuildPlan(ctx, opts, req.Plan.TargetBackupID)
	if err != nil {
		return Result{}, err
	}
	if fresh.Digest != req.Plan.Digest {
		return Result{}, planRefusal(CodePlanChanged, req.Plan.TargetBackupID)
	}
	execution := &execution{
		opts: opts, plan: fresh, req: req, progress: progress,
		work:    filepath.Join(opts.WorkRoot, "restore-"+req.RestoreID),
		pending: filepath.Join(opts.ExportRoot, ".pending-"+req.RestoreID),
		export:  filepath.Join(opts.ExportRoot, req.RestoreID),
	}
	return execution.run(ctx)
}

func validateRequest(opts Options, req Request) error {
	if !restoreIDPattern.MatchString(req.RestoreID) || req.Plan.TargetBackupID == "" {
		return execFailure(CodeInvalidRequest, PhaseFetching, SideEffects{}, errors.New("restore: invalid restore id or target"))
	}
	if req.Rehearse && opts.MysqldPath == "" {
		return execFailure(CodeRehearsalUnavailable, PhaseRehearsing, SideEffects{}, errors.New("restore: rehearsal requires a mysqld binary"))
	}
	if req.Rehearse && os.Geteuid() == 0 {
		return execFailure(CodeRehearsalUnavailable, PhaseRehearsing, SideEffects{}, physical.ErrRehearsalRoot)
	}
	if err := validateVerifySQL(req.VerifySQL); err != nil {
		return execFailure(CodeInvalidVerifySQL, PhaseRehearsing, SideEffects{}, err)
	}
	return nil
}

type stageError struct {
	code string
	err  error
}

func (e *stageError) Error() string { return e.err.Error() }
func (e *stageError) Unwrap() error { return e.err }

func stage(code string, err error) error { return &stageError{code: code, err: err} }

type execution struct {
	opts     Options
	plan     Plan
	req      Request
	progress Progress

	work    string
	pending string
	export  string

	phase          Phase
	side           SideEffects
	result         Result
	prepareArgs    [][]string
	exportBytes    int64
	workCreated    bool
	pendingCreated bool
	published      bool
}

func (e *execution) run(ctx context.Context) (Result, error) {
	if err := e.createRoots(); err != nil {
		return Result{}, execFailure(e.failureCode(ctx, CodeWorkFailed, err), PhaseFetching, e.side, err)
	}
	for _, step := range []struct {
		code string
		fn   func(context.Context) error
	}{
		{"", e.fetchAndAuthenticate},
		{"", e.extractAll},
		{"", e.prepareChain},
		{CodePrepareFailed, func(context.Context) error { return e.renamePrepared() }},
		{"", e.publish},
	} {
		if err := step.fn(ctx); err != nil {
			return e.abort(ctx, step.code, err)
		}
	}
	return e.succeed(ctx)
}

func (e *execution) succeed(ctx context.Context) (Result, error) {
	e.published = true
	e.side.ExportPublished = true
	e.side.PendingExportRemoved = true
	e.result = Result{
		ExportPath: e.export, ExportBytes: e.exportBytes,
		PreparedToLSN: e.plan.TargetToLSN, PrepareArgs: e.prepareArgs,
	}
	e.enter(PhaseRehearsing)
	e.result.Verify = e.rehearse(ctx)
	e.cleanupWork()
	return e.result, nil
}

func (e *execution) enter(phase Phase) {
	e.phase = phase
	if e.progress != nil {
		e.progress(phase, map[string]any{"phase": string(phase)})
	}
}

func (e *execution) report(phase Phase, data map[string]any) {
	if e.progress != nil {
		e.progress(phase, data)
	}
}

func (e *execution) createRoots() error {
	if err := validatePrivateWorkRoot(e.opts.WorkRoot); err != nil {
		return err
	}
	if err := CheckExportRoot(e.opts.ExportRoot, append([]string{e.opts.WorkRoot}, e.opts.RestrictedRoots...)...); err != nil {
		return err
	}
	if err := validateKeyOutsideExport(e.opts.KeyFile, e.opts.ExportRoot); err != nil {
		return err
	}
	for _, path := range []string{e.work, e.pending, e.export} {
		if _, err := os.Lstat(path); err == nil {
			return errors.New("restore: a restore-owned path already exists")
		} else if !errors.Is(err, os.ErrNotExist) {
			return errors.New("restore: cannot inspect restore-owned paths")
		}
	}
	if err := os.Mkdir(e.work, 0o700); err != nil {
		return errors.New("restore: cannot create the private work directory")
	}
	e.workCreated = true
	return nil
}

// failureCode prefers cancellation and real ENOSPC over the stage code, so a
// canceled or out-of-space run is never reported as a generic stage failure.
func (e *execution) failureCode(ctx context.Context, code string, err error) string {
	if ctx.Err() != nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return CodeCanceled
	}
	if errors.Is(err, syscall.ENOSPC) || errors.Is(err, syscall.EDQUOT) {
		return CodeDiskFull
	}
	var staged *stageError
	if errors.As(err, &staged) {
		return staged.code
	}
	if code != "" {
		return code
	}
	return CodeWorkFailed
}

func (e *execution) abort(ctx context.Context, code string, cause error) (Result, error) {
	cleanupErr := e.cleanupFailure()
	if cleanupErr != nil {
		code = CodeCleanupFailed
	}
	return Result{}, execFailure(e.failureCode(ctx, code, cause), e.phase, e.side, errors.Join(cause, cleanupErr))
}

// cleanupFailure removes exactly this restore's work directory and pending
// export. It never scans a prefix and never touches a published export.
func (e *execution) cleanupFailure() error {
	workErr := removeExactDirectory(e.work, e.opts.WorkRoot)
	e.side.WorkDirRemoved = workErr == nil
	e.workCreated = false
	pendingErr := removeExactDirectory(e.pending, e.opts.ExportRoot)
	e.side.PendingExportRemoved = pendingErr == nil
	e.pendingCreated = false
	return errors.Join(workErr, pendingErr)
}

func (e *execution) cleanupWork() {
	if err := removeExactDirectory(e.work, e.opts.WorkRoot); err != nil {
		return
	}
	e.side.WorkDirRemoved = true
	e.workCreated = false
}

// removeExactDirectory removes one directory entry by exact name and syncs its
// parent. An absent entry is still synced so a retried recovery is durable.
func removeExactDirectory(path, parent string) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return syncDirectory(parent)
	}
	if err != nil {
		return err
	}
	if !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return errors.New("restore: refusing to remove a non-directory restore target")
	}
	if err = os.RemoveAll(path); err != nil {
		return err
	}
	return syncDirectory(parent)
}

func syncDirectory(path string) (retErr error) {
	dir, err := os.Open(path) // #nosec G304 -- path is a validated private restore or export root.
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, dir)
	return dir.Sync()
}

func (e *execution) fetchAndAuthenticate(ctx context.Context) error {
	for _, link := range e.plan.Links {
		dir := filepath.Join(e.work, "fetch", strconv.Itoa(link.Seq))
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return stage(CodeFetchFailed, err)
		}
		if err := e.fetchLink(ctx, link, dir); err != nil {
			return err
		}
		if err := e.authenticateLink(ctx, link, dir); err != nil {
			return err
		}
	}
	return nil
}

func (e *execution) fetchLink(ctx context.Context, link PlanLink, dir string) error {
	e.phase = PhaseFetching
	e.report(PhaseFetching, map[string]any{"backup_id": link.BackupID, "seq": link.Seq, "source": link.Source})
	var err error
	switch link.Source {
	case "local":
		err = e.copyLocal(ctx, link, dir)
	case "ftp":
		err = e.download(ctx, link, dir)
	default:
		err = errors.New("restore: plan link has no source")
	}
	if err != nil {
		return stage(CodeFetchFailed, err)
	}
	return nil
}

func (e *execution) authenticateLink(ctx context.Context, link PlanLink, dir string) error {
	e.phase = PhaseAuthenticating
	e.report(PhaseAuthenticating, map[string]any{"backup_id": link.BackupID, "seq": link.Seq})
	if err := e.authenticate(ctx, link, dir); err != nil {
		return stage(CodeAuthenticationFailed, err)
	}
	return nil
}

func (e *execution) copyLocal(ctx context.Context, link PlanLink, dir string) error {
	repository, err := e.opts.Locator.Local(ctx, link.BackupID)
	if err != nil {
		return err
	}
	if repository == nil {
		return errors.New("restore: the local repository is no longer available")
	}
	for _, member := range memberOrder {
		reader, err := repository.Open(link.objectPath + "/" + member)
		if err != nil {
			return errors.New("restore: a local member disappeared")
		}
		copyErr := writeFetched(filepath.Join(dir, member), reader)
		closeErr := reader.Close()
		if err = errors.Join(copyErr, closeErr); err != nil {
			return err
		}
	}
	return nil
}

func writeFetched(path string, reader io.Reader) (retErr error) {
	file, err := fsutil.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, file)
	if _, err = io.Copy(file, reader); err != nil {
		return err
	}
	return file.Sync()
}

func (e *execution) download(ctx context.Context, link PlanLink, dir string) (retErr error) {
	ref, err := e.findReplica(ctx, link)
	if err != nil {
		return err
	}
	remote, err := openReplica(ctx, e.opts, ref)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, remote)
	record, err := e.opts.Replication.Replica(ctx, link.BackupID, ref.RepositoryID)
	if err != nil {
		return errors.New("restore: the verified replica row disappeared")
	}
	if record.Status != "verified" {
		return errors.New("restore: the replica is no longer verified")
	}
	for _, member := range memberOrder {
		recorded, ok := record.ReplicaMemberByName(member)
		if !ok || recorded.Size <= 0 {
			return errors.New("restore: a replica member is not recorded")
		}
		if err = e.downloadMember(ctx, remote, link, member, filepath.Join(dir, member), recorded.Size); err != nil {
			return err
		}
	}
	return nil
}

func (e *execution) findReplica(ctx context.Context, link PlanLink) (ReplicaRef, error) {
	refs, err := e.opts.Locator.Replicas(ctx, link.BackupID)
	if err != nil {
		return ReplicaRef{}, err
	}
	for _, ref := range refs {
		if ref.RepositoryID == link.RepositoryID {
			return ref, nil
		}
	}
	return ReplicaRef{}, errors.New("restore: the planned replica is no longer available")
}

const maxFetchAttempts = 5

// downloadMember resumes one immutable ciphertext at its current offset. Every
// attempt opens a new session and asks the server for that byte offset; a short
// read is never success, and the artifact authentication is still required.
func (e *execution) downloadMember(ctx context.Context, remote remoteReader, link PlanLink, member, path string, expected int64) (retErr error) {
	file, err := fsutil.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, file)
	key := link.objectPath + "/" + member
	var offset int64
	for attempt := 1; attempt <= maxFetchAttempts; attempt++ {
		written, attemptErr := e.fetchAttempt(ctx, remote, file, key, attempt, offset, expected)
		offset += written
		if offset > expected {
			return errors.New("restore: fetched member exceeds its recorded size")
		}
		if attemptErr == nil && offset == expected {
			return file.Sync()
		}
		if ctx.Err() != nil {
			return ctx.Err()
		}
		e.report(PhaseFetching, map[string]any{"backup_id": link.BackupID, "member": member, "attempt": attempt})
	}
	return errors.New("restore: a replica member did not download completely")
}

// fetchAttempt copies one bounded attempt into the open file and reports how many
// bytes it added. A nil error means the attempt consumed to the recorded size.
func (e *execution) fetchAttempt(ctx context.Context, remote remoteReader, file *os.File, key string, attempt int, offset, expected int64) (int64, error) {
	reader, err := remote.Open(ctx, key, offset)
	if err != nil {
		return 0, err
	}
	source, injected := e.faultedSource(attempt, key, offset, reader)
	written, copyErr := io.Copy(file, io.LimitReader(source, expected-offset+1))
	closeErr := reader.Close()
	err = errors.Join(copyErr, closeErr, injected)
	if err == nil && offset+written != expected {
		return written, errors.New("restore: the replica sent fewer bytes than recorded")
	}
	return written, err
}

func (e *execution) faultedSource(attempt int, key string, offset int64, reader io.Reader) (io.Reader, error) {
	if e.opts.fetchFault == nil {
		return reader, nil
	}
	after, faultErr := e.opts.fetchFault(attempt, key, offset)
	if faultErr == nil || after <= 0 {
		return reader, nil
	}
	return io.LimitReader(reader, after), faultErr
}

func (e *execution) extractAll(ctx context.Context) error {
	for index, link := range e.plan.Links {
		dir := filepath.Join(e.work, strconv.Itoa(index))
		if err := os.Mkdir(dir, 0o700); err != nil {
			return stage(CodeExtractFailed, err)
		}
		e.phase = PhaseExtracting
		e.report(PhaseExtracting, map[string]any{"backup_id": link.BackupID, "seq": link.Seq})
		if err := e.extract(ctx, link, dir); err != nil {
			return stage(CodeExtractFailed, err)
		}
		if err := os.RemoveAll(filepath.Join(e.work, "fetch", strconv.Itoa(link.Seq))); err != nil {
			return stage(CodeExtractFailed, err)
		}
	}
	return nil
}

func (e *execution) extract(ctx context.Context, link PlanLink, dir string) error {
	fetched := filepath.Join(e.work, "fetch", strconv.Itoa(link.Seq))
	reader, writer := io.Pipe()
	authErr := make(chan error, 1)
	go func() {
		_, err := artifactRead(ctx, e.opts, fetched, writer, e.expandLimit(), true)
		authErr <- errors.Join(err, writer.CloseWithError(err))
	}()
	extractErr := physical.ExtractInto(ctx, physical.ExtractIntoOptions{
		Binary: e.opts.XbstreamPath, Directory: dir, Parallel: e.opts.Parallel,
	}, reader)
	var closeErr error
	if extractErr != nil {
		closeErr = reader.CloseWithError(extractErr)
	} else {
		closeErr = reader.Close()
	}
	if err := <-authErr; err != nil || closeErr != nil {
		return errors.Join(err, closeErr)
	}
	return extractErr
}

func (e *execution) expandLimit() int64 {
	factor := int64(e.opts.ExpansionFactor)
	if factor < 1 {
		factor = DefaultExpansionFactor
	}
	limit := int64(64 << 20)
	for _, link := range e.plan.Links {
		if link.PayloadSize > 0 && link.PayloadSize <= (1<<62)/factor {
			limit = max(limit, link.PayloadSize*factor)
		}
	}
	return limit
}
