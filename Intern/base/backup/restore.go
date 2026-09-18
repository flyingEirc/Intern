package backup

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/flyingEirc/IotProject/base/catalog"
	"github.com/flyingEirc/IotProject/base/fsutil"
)

type preparedBackup struct {
	manifest  Manifest
	directory string
}

func (e *Engine) Restore(ctx context.Context, request RestoreRequest) (plan RestorePlan, retErr error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !artifactID.MatchString(request.BackupID) {
		return plan, errors.New("--backup must be a published backup ID from backup list")
	}
	target, info, err := openRestoreTarget(ctx, request.Target)
	if err != nil {
		return plan, err
	}
	defer fsutil.Close(&retErr, target)
	id, work, err := e.beginRestore(ctx, target, info)
	if err != nil {
		return plan, err
	}
	defer e.finishRestore(id, work, &plan, &retErr)
	if err = os.Mkdir(work, 0700); err != nil {
		return plan, err
	}
	chain, expanded, err := e.prepareChain(ctx, work, request.BackupID)
	if err != nil {
		return plan, err
	}
	plan = restorePlan(request.BackupID, target, info, chain, expanded)
	if err = validateRestoreTarget(ctx, target, info, chain); err != nil {
		return plan, err
	}
	if err = validateReplay(ctx, target, chain); err != nil {
		return plan, err
	}
	if !request.Execute {
		return plan, nil
	}
	if err = executeRestore(ctx, target, chain, work); err != nil {
		return plan, err
	}
	plan.Executed = true
	return plan, nil
}
func openRestoreTarget(ctx context.Context, c Connection) (target *session, info sourceInfo, retErr error) {
	s, err := connect(ctx, c)
	if err != nil {
		return nil, info, err
	}
	defer func() {
		if retErr != nil {
			fsutil.Close(&retErr, s)
		}
	}()
	if err = s.lock(ctx, "restore"); err != nil {
		return nil, info, err
	}
	info, err = s.info(ctx, false)
	if err != nil {
		return nil, info, err
	}
	return s, info, nil
}
func (e *Engine) beginRestore(ctx context.Context, target *session, info sourceInfo) (string, string, error) {
	id := requestID("restore", "restore", "")
	work := filepath.Join(e.opts.StateDirectory, "work-"+id)
	// Persist cleanup responsibility even for interrupted authentication/plan generation.
	_, err := e.store.Begin(ctx, catalog.Record{ID: id, Source: "restore:" + info.UUID + ":" + target.cfg.DB, Kind: "restore", Path: filepath.Join(e.opts.Directory, id), CreatedAt: time.Now().UTC()})
	return id, work, err
}
func (e *Engine) finishRestore(id, work string, plan *RestorePlan, retErr *error) {
	*retErr = errors.Join(*retErr, os.RemoveAll(work))
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if *retErr != nil {
		*retErr = errors.Join(*retErr, e.store.Fail(ctx, id))
		return
	}
	data, err := json.Marshal(plan)
	if err == nil {
		err = e.store.Complete(ctx, id, data)
	}
	*retErr = errors.Join(*retErr, err)
}
func restorePlan(id string, target *session, info sourceInfo, chain []preparedBackup, expanded int64) RestorePlan {
	base := chain[0].manifest
	last := chain[len(chain)-1].manifest
	plan := RestorePlan{BackupID: id, SourceUUID: base.SourceUUID, SourceDatabase: base.Database, TargetUUID: info.UUID, TargetDatabase: target.cfg.DB, Stop: last.End, ExpandedBytes: expanded, Artifacts: make([]string, 0, len(chain)), Warning: "Restore requires an isolated empty target and object-creation privileges; binlog replay additionally requires BINLOG_ADMIN/REPLICATION_APPLIER and GTID_NEXT privileges. Grants are not prevalidated. Failure can leave partial SQL effects; discard the failed target. No concurrent DDL during backup or application writes during restore."}
	for _, b := range chain {
		plan.Artifacts = append(plan.Artifacts, b.manifest.ID)
	}
	return plan
}
func validateRestoreTarget(ctx context.Context, target *session, info sourceInfo, chain []preparedBackup) error {
	if err := validateTargetIdentity(info, target.cfg.DB, chain[0].manifest, len(chain) > 1); err != nil {
		return err
	}
	if _, err := toolVersion(ctx, "mysql", info.Version); err != nil {
		return err
	}
	var count int
	if err := target.conn.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ?) + (SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ?) + (SELECT COUNT(*) FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ?)`, target.cfg.DB, target.cfg.DB, target.cfg.DB).Scan(&count); err != nil {
		return dbError("inspect target objects", err)
	}
	if count != 0 {
		return errors.New("restore target database is not empty; refusing to overwrite existing objects")
	}
	return nil
}
func validateTargetIdentity(info sourceInfo, db string, base Manifest, hasLogs bool) error {
	if hasLogs && base.GTIDMode != info.GTID {
		return errors.New("source and restore target GTID modes must match for binlog replay")
	}
	if info.UUID == base.SourceUUID {
		return errors.New("refusing restore to source server UUID, even under a different database name")
	}
	if !sameVersion(base.ServerVersion, info.Version) {
		return errors.New("cross-major/minor MySQL restore is not supported")
	}
	if base.SchemaObjects && db != base.Database {
		return errors.New("backup contains views/routines/events/triggers: preserve the source database name on the isolated target; arbitrary stored SQL schema rewriting is not safe")
	}
	return nil
}
func sameVersion(a, b string) bool {
	x := versionPattern.FindStringSubmatch(a)
	y := versionPattern.FindStringSubmatch(b)
	return len(x) > 0 && len(y) > 0 && x[1] == y[1] && x[2] == y[2]
}
func validateReplay(ctx context.Context, target *session, chain []preparedBackup) error {
	if len(chain) == 1 {
		return nil
	}
	base := chain[0].manifest
	if _, err := toolVersion(ctx, "mysqlbinlog", base.ServerVersion); err != nil {
		return err
	}
	var executed string
	if err := target.conn.QueryRowContext(ctx, "SELECT @@global.gtid_executed").Scan(&executed); err != nil {
		return dbError("inspect target GTID history", err)
	}
	// A previously replayed source GTID would be silently skipped by MySQL.
	if strings.Contains(strings.ToLower(executed), strings.ToLower(base.SourceUUID)) {
		return errors.New("target GTID history already contains this source UUID; use a fresh isolated instance")
	}
	for _, b := range chain[1:] {
		if err := validateArchivedBackup(ctx, b, base.Database, target.cfg.DB); err != nil {
			return err
		}
	}
	return nil
}
func validateArchivedBackup(ctx context.Context, b preparedBackup, source, target string) error {
	for j, file := range b.manifest.Files {
		start, next := logBoundary(b.manifest, j)
		path := filepath.Join(b.directory, file.Name)
		if err := runTool(ctx, "mysqlbinlog", "", nil, io.Discard, "--verify-binlog-checksum", "--skip-force-if-open", "--require-row-format", path); err != nil {
			return err
		}
		if err := validateBinlog(ctx, path, start, source, target, next); err != nil {
			return err
		}
	}
	return nil
}
func executeRestore(ctx context.Context, target *session, chain []preparedBackup, work string) error {
	creds, err := credentials(work, target.cfg)
	if err != nil {
		return err
	}
	base := chain[0].manifest
	// No mutation occurs before all objects and all logs pass validation.
	if _, err = target.conn.ExecContext(ctx, "CREATE DATABASE IF NOT EXISTS `"+target.cfg.DB+"` CHARACTER SET `"+base.Charset+"` COLLATE `"+base.Collation+"`"); err != nil {
		return dbError("create target database", err)
	}
	reader, writer := io.Pipe()
	done := make(chan error, 1)
	go func() {
		copyErr := writeRecoverySQL(ctx, writer, chain, target.cfg.DB, work)
		done <- errors.Join(copyErr, writer.CloseWithError(copyErr))
	}()
	err = runTool(ctx, "mysql", creds, reader, io.Discard, "--binary-mode", "--default-character-set=utf8mb4", "--database="+target.cfg.DB)
	closeErr := reader.CloseWithError(err)
	if err = errors.Join(err, closeErr, <-done); err != nil {
		return fmt.Errorf("restore failed; target may be partially modified and must remain isolated: %w", err)
	}
	return nil
}
func writeRecoverySQL(ctx context.Context, w io.Writer, chain []preparedBackup, targetDB, work string) error {
	if err := copyFile(ctx, w, filepath.Join(chain[0].directory, "dump.sql")); err != nil {
		return err
	}
	if len(chain) == 1 {
		return nil
	}
	if _, err := io.WriteString(w, "\n"); err != nil {
		return err
	}
	base := chain[0].manifest
	args := []string{"--verify-binlog-checksum", "--skip-force-if-open", "--require-row-format", "--database=" + targetDB, "--start-position=" + strconv.FormatInt(base.End.Position, 10), "--local-load=" + work}
	if base.Database != targetDB {
		args = append(args, "--rewrite-db="+base.Database+"->"+targetDB)
	}
	// One mysqlbinlog invocation and one mysql session preserve state across every log file.
	for _, b := range chain[1:] {
		for _, f := range b.manifest.Files {
			args = append(args, filepath.Join(b.directory, f.Name))
		}
	}
	return runTool(ctx, "mysqlbinlog", "", nil, w, args...)
}
