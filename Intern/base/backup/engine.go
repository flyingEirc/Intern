package backup

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/gofrs/flock"

	"github.com/flyingEirc/IotProject/base/artifact"
	"github.com/flyingEirc/IotProject/base/catalog"
	"github.com/flyingEirc/IotProject/base/fsutil"
)

var ErrNoBaseline = errors.New("no logical full baseline for this source; run backup full first")
var ErrNewBaseline = errors.New("backup chain cannot continue; create a new logical full baseline")

type Engine struct {
	opts  Options
	store *catalog.Store
	lock  *flock.Flock
	mu    sync.Mutex
}
type backupPaths struct{ final, pending, work string }
type backupCollector func(context.Context, *session, string, string, *Manifest) error
type backupMethod struct {
	kind    string
	prepare func(context.Context, *session, sourceInfo, *Manifest) error
	collect backupCollector
}

func privateDirectory(path string) (string, error) {
	if path == "" {
		return "", errors.New("backup and state directories must be specified")
	}
	abs, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	if err = os.MkdirAll(abs, 0700); err != nil {
		return "", err
	}
	st, err := os.Lstat(abs)
	if err != nil {
		return "", err
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 || st.Mode().Perm()&0077 != 0 {
		return "", fmt.Errorf("directory %s must be a real private directory (chmod 700)", abs)
	}
	return filepath.EvalSymlinks(abs)
}
func inside(root, path string) bool {
	rel, err := filepath.Rel(root, path)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(os.PathSeparator))
}
func normalizeOptions(opts Options) (Options, error) {
	var err error
	opts.Directory, err = privateDirectory(opts.Directory)
	if err != nil {
		return opts, err
	}
	opts.StateDirectory, err = privateDirectory(opts.StateDirectory)
	if err != nil {
		return opts, err
	}
	if inside(opts.Directory, opts.StateDirectory) || inside(opts.StateDirectory, opts.Directory) {
		return opts, errors.New("state and backup directories must be separate, non-nested directories")
	}
	if opts.Project == "" {
		opts.Project = "default"
	}
	if opts.MaxRestoreBytes == 0 {
		opts.MaxRestoreBytes = 100 << 30
	}
	if opts.MaxRestoreBytes < 1 || opts.MaxRestoreBytes > 1<<60 {
		return opts, errors.New("max-restore-bytes must be positive and at most 1 EiB")
	}
	opts.KeyFile, err = resolveKeyLocation(opts)
	return opts, err
}
func resolveKeyLocation(opts Options) (string, error) {
	if opts.KeyFile == "" {
		return "", nil
	}
	key, err := filepath.Abs(opts.KeyFile)
	if err != nil {
		return "", err
	}
	real, resolveErr := filepath.EvalSymlinks(key)
	if resolveErr == nil {
		key = real
	}
	if err = artifact.ValidateKeyLocation(key, opts.Directory, opts.StateDirectory); err != nil {
		return "", err
	}
	return key, nil
}
func lockBackups(dir string) (*flock.Flock, error) {
	lock := flock.New(filepath.Join(dir, ".intern.lock"))
	ok, err := lock.TryLock()
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	if !ok {
		return nil, errors.Join(errors.New("backup directory is in use by another Intern process"), lock.Close())
	}
	return lock, nil
}
func Open(ctx context.Context, opts Options) (*Engine, error) {
	opts, err := normalizeOptions(opts)
	if err != nil {
		return nil, err
	}
	lock, err := lockBackups(opts.Directory)
	if err != nil {
		return nil, err
	}
	store, err := catalog.Open(ctx, opts.StateDirectory)
	if err != nil {
		return nil, errors.Join(err, lock.Close())
	}
	e := &Engine{opts: opts, store: store, lock: lock}
	if err = e.recoverPending(ctx); err != nil {
		return nil, errors.Join(err, e.Close())
	}
	return e, nil
}
func (e *Engine) Close() error { return errors.Join(e.store.Close(), e.lock.Close()) }
func (e *Engine) paths(id string) backupPaths {
	return backupPaths{final: filepath.Join(e.opts.Directory, id), pending: filepath.Join(e.opts.Directory, ".pending-"+id), work: filepath.Join(e.opts.StateDirectory, "work-"+id)}
}
func (e *Engine) recoverPending(ctx context.Context) error {
	pending, err := e.store.Pending(ctx)
	if err != nil {
		return err
	}
	for _, r := range pending {
		paths := e.paths(r.ID)
		if !artifactID.MatchString(r.ID) || r.Path != paths.final {
			continue
		}
		if err = removeBackupPaths(paths); err != nil {
			return err
		}
		if r.Status == "running" {
			if err = e.store.Fail(ctx, r.ID); err != nil {
				return err
			}
		}
	}
	return nil
}
func removeBackupPaths(p backupPaths) error {
	return errors.Join(os.RemoveAll(p.final), os.RemoveAll(p.pending), os.RemoveAll(p.work))
}
func (e *Engine) List(ctx context.Context) ([]Result, error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	records, err := e.store.List(ctx)
	if err != nil {
		return nil, err
	}
	results := make([]Result, 0, len(records))
	for _, r := range records {
		if r.Kind == "restore" {
			continue
		}
		m, parseErr := parseManifest(r.Manifest)
		if parseErr != nil {
			return nil, parseErr
		}
		if m.Project == e.opts.Project && filepath.Dir(r.Path) == e.opts.Directory {
			results = append(results, result(m, r.Path))
		}
	}
	return results, nil
}

func (e *Engine) backup(ctx context.Context, request string, method backupMethod) (res Result, retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	s, err := connect(ctx, e.opts.Connection)
	if err != nil {
		return res, err
	}
	defer fsutil.Close(&retErr, s)
	if err = s.lock(ctx, "source"); err != nil {
		return res, err
	}
	info, err := s.info(ctx, true)
	if err != nil {
		return res, err
	}
	m, err := e.describeBackup(ctx, s, info, method.kind, request)
	if err != nil {
		return res, err
	}
	cached, found, err := e.completedBackup(ctx, m.ID)
	if err != nil || found {
		return cached, err
	}
	if err = e.prepareManifest(ctx, s, info, &m, method); err != nil {
		return res, err
	}
	return e.executeBackup(ctx, s, &m, sourceKey(e.opts.Project, info, s.cfg.DB), method.collect)
}
func (e *Engine) describeBackup(ctx context.Context, s *session, info sourceInfo, kind, request string) (Manifest, error) {
	tool := "mysqldump"
	if kind == "binlog" {
		tool = "mysqlbinlog"
	}
	version, err := toolVersion(ctx, tool, info.Version)
	if err != nil {
		return Manifest{}, err
	}
	id := requestID(sourceKey(e.opts.Project, info, s.cfg.DB), kind, request)
	return Manifest{Version: 1, ID: id, Project: e.opts.Project, Kind: kind, SourceUUID: info.UUID, Database: s.cfg.DB, Charset: info.Charset, Collation: info.Collation, ServerVersion: info.Version, ToolVersion: version, GTIDMode: info.GTID, ObservedGTID: info.ObservedGTID, CreatedAt: time.Now().UTC(), SchemaObjects: info.SchemaObjects, Encrypted: e.opts.Encrypt}, nil
}
func (e *Engine) completedBackup(ctx context.Context, id string) (Result, bool, error) {
	record, err := e.store.Get(ctx, id)
	if errors.Is(err, catalog.ErrNotFound) {
		return Result{}, false, nil
	}
	if err != nil {
		return Result{}, false, err
	}
	m, err := parseManifest(record.Manifest)
	if err != nil {
		return Result{}, false, err
	}
	return result(m, record.Path), true, nil
}
func (e *Engine) prepareManifest(ctx context.Context, s *session, info sourceInfo, m *Manifest, method backupMethod) error {
	if m.Encrypted {
		keyID, err := artifact.KeyID(e.opts.KeyFile)
		if err != nil {
			return err
		}
		m.KeyID = keyID
	}
	if method.prepare != nil {
		return method.prepare(ctx, s, info, m)
	}
	return nil
}
func (e *Engine) executeBackup(ctx context.Context, s *session, m *Manifest, source string, collect backupCollector) (res Result, retErr error) {
	paths := e.paths(m.ID)
	if err := e.beginBackup(ctx, *m, source, paths.final); err != nil {
		return res, err
	}
	defer e.cleanupBackup(m.ID, paths, &retErr)
	if err := os.Mkdir(paths.work, 0700); err != nil {
		return res, err
	}
	if err := os.Mkdir(paths.pending, 0700); err != nil {
		return res, err
	}
	if err := collectBackup(ctx, s, paths.work, m, collect); err != nil {
		return res, err
	}
	if err := checkSourceIdentity(ctx, s, m.SourceUUID); err != nil {
		return res, err
	}
	if err := e.publishBackup(ctx, paths, *m); err != nil {
		return res, err
	}
	return result(*m, paths.final), nil
}
func (e *Engine) beginBackup(ctx context.Context, m Manifest, source, path string) error {
	started, err := e.store.Begin(ctx, catalog.Record{ID: m.ID, Source: source, Kind: m.Kind, Parent: m.Parent, Path: path, CreatedAt: m.CreatedAt})
	if err != nil {
		return err
	}
	if !started {
		return errors.New("backup request already completed; retry listing")
	}
	return nil
}
func (e *Engine) cleanupBackup(id string, p backupPaths, retErr *error) {
	cleanupErr := os.RemoveAll(p.work)
	if *retErr != nil {
		cleanupErr = errors.Join(cleanupErr, os.RemoveAll(p.pending), os.RemoveAll(p.final))
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		cleanupErr = errors.Join(cleanupErr, e.store.Fail(ctx, id))
	}
	*retErr = errors.Join(*retErr, cleanupErr)
}
func collectBackup(ctx context.Context, s *session, work string, m *Manifest, collect backupCollector) error {
	creds, err := credentials(work, s.cfg)
	if err != nil {
		return err
	}
	return collect(ctx, s, creds, work, m)
}
func checkSourceIdentity(ctx context.Context, s *session, uuid string) error {
	// A reconnect/failover must not join source generations.
	after, err := s.info(ctx, false)
	if err != nil {
		return err
	}
	if after.UUID != uuid {
		return errors.New("source UUID changed during backup")
	}
	return nil
}
func (e *Engine) publishBackup(ctx context.Context, p backupPaths, m Manifest) error {
	metadata, err := json.Marshal(m)
	if err != nil {
		return err
	}
	if err = e.encodeBackup(ctx, p, m.Files, metadata); err != nil {
		return err
	}
	if err = syncDirectory(p.pending); err != nil {
		return err
	}
	if err = os.Rename(p.pending, p.final); err != nil {
		return err
	}
	if err = syncDirectory(e.opts.Directory); err != nil {
		return err
	}
	if err = os.RemoveAll(p.work); err != nil {
		return err
	}
	return e.store.Complete(ctx, m.ID, metadata)
}
func (e *Engine) encodeBackup(ctx context.Context, p backupPaths, files []File, metadata []byte) error {
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		copyErr := writeTar(ctx, writer, p.work, files)
		done <- errors.Join(copyErr, writer.CloseWithError(copyErr))
	}()
	err := artifact.Write(ctx, p.pending, reader, metadata, artifact.Options{Compress: e.opts.Compress, Encrypt: e.opts.Encrypt, KeyFile: e.opts.KeyFile})
	closeErr := reader.CloseWithError(err)
	return errors.Join(err, closeErr, <-done, ctx.Err())
}
