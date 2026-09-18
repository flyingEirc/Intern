package backup

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/flyingEirc/IotProject/base/catalog"
	"github.com/flyingEirc/IotProject/base/fsutil"
)

func (e *Engine) Incremental(ctx context.Context, request string) (Result, error) {
	return e.backup(ctx, request, backupMethod{kind: "binlog", prepare: e.attachParent, collect: e.archiveLogs})
}

func (e *Engine) attachParent(ctx context.Context, s *session, info sourceInfo, m *Manifest) error {
	prev, err := e.store.Latest(ctx, sourceKey(e.opts.Project, info, s.cfg.DB))
	if errors.Is(err, catalog.ErrNotFound) {
		return ErrNoBaseline
	}
	if err != nil {
		return err
	}
	parent, err := parseManifest(prev.Manifest)
	if err != nil {
		return err
	}
	if filepath.Dir(prev.Path) != e.opts.Directory {
		return errors.New("previous backup is in another directory; select its directory or create a new full baseline")
	}
	if err = checkParentSource(ctx, s, info, parent); err != nil {
		return err
	}
	m.Parent = parent.ID
	m.Start = parent.End
	return nil
}
func checkParentSource(ctx context.Context, s *session, info sourceInfo, parent Manifest) error {
	if parent.SourceUUID != info.UUID || parent.Database != s.cfg.DB || parent.GTIDMode != info.GTID || parent.ServerVersion != info.Version {
		return fmt.Errorf("%w: source identity/version/GTID mode changed", ErrNewBaseline)
	}
	if parent.ObservedGTID == "" {
		return nil
	}
	var continuous int
	if err := s.conn.QueryRowContext(ctx, "SELECT GTID_SUBSET(?, @@global.gtid_executed)", parent.ObservedGTID).Scan(&continuous); err != nil {
		return dbError("check source GTID continuity", err)
	}
	if continuous != 1 {
		return fmt.Errorf("%w: source GTID history was reset", ErrNewBaseline)
	}
	return nil
}
func nextLog(a, b string) bool {
	ai := strings.LastIndexByte(a, '.')
	bi := strings.LastIndexByte(b, '.')
	if ai < 0 || bi < 0 || a[:ai] != b[:bi] {
		return false
	}
	an, err := strconv.ParseUint(a[ai+1:], 10, 64)
	if err != nil {
		return false
	}
	bn, err := strconv.ParseUint(b[bi+1:], 10, 64)
	return err == nil && an+1 == bn
}
func closedLogs(ctx context.Context, s *session, start Coordinate) ([]sourceLog, error) {
	if _, err := s.conn.ExecContext(ctx, "FLUSH BINARY LOGS"); err != nil {
		return nil, dbError("rotate binary logs (RELOAD required)", err)
	}
	logs, err := s.logs(ctx)
	if err != nil {
		return nil, err
	}
	first := -1
	for i, l := range logs {
		if l.Name == start.File {
			first = i
			break
		}
	}
	if first < 0 {
		return nil, fmt.Errorf("%w: required binlog was purged or source generation changed", ErrNewBaseline)
	}
	if first >= len(logs)-1 {
		return nil, errors.New("binlog rotation did not produce a closed file")
	}
	if start.Position > logs[first].Size {
		return nil, errors.New("saved binlog position exceeds source file size; refusing reused/truncated log")
	}
	return logs[first:], nil
}
func (e *Engine) archiveLogs(ctx context.Context, s *session, creds, work string, m *Manifest) error {
	logs, err := closedLogs(ctx, s, m.Start)
	if err != nil {
		return err
	}
	spool := filepath.Join(work, "logs")
	if err = os.Mkdir(spool, 0700); err != nil {
		return err
	}
	for i := range len(logs) - 1 {
		start := int64(4)
		if i == 0 {
			start = m.Start.Position
		}
		f, downloadErr := archiveLog(ctx, creds, spool, logs[i], logs[i+1].Name, start, m.Database)
		if downloadErr != nil {
			return downloadErr
		}
		if err = os.Rename(filepath.Join(spool, f.Name), filepath.Join(work, f.Name)); err != nil {
			return err
		}
		m.Files = append(m.Files, f)
	}
	// Position 4 is the header boundary of the new file; all archived transactions are complete.
	m.End = Coordinate{File: logs[len(logs)-1].Name, Position: 4}
	return nil
}
func archiveLog(ctx context.Context, creds, spool string, log sourceLog, next string, start int64, database string) (File, error) {
	if !nextLog(log.Name, next) {
		return File{}, errors.New("source binary log sequence has a gap")
	}
	if err := runTool(ctx, "mysqlbinlog", creds, nil, io.Discard, "--read-from-remote-server", "--raw", "--verify-binlog-checksum", "--result-file="+spool+string(os.PathSeparator), log.Name); err != nil {
		return File{}, err
	}
	path := filepath.Join(spool, log.Name)
	if err := os.Chmod(path, 0600); err != nil {
		return File{}, err
	}
	f, err := fileDigest(path, log.Name)
	if err != nil {
		return File{}, err
	}
	if f.Size != log.Size {
		return File{}, errors.New("downloaded binlog size differs from closed source file")
	}
	if err = runTool(ctx, "mysqlbinlog", "", nil, io.Discard, "--verify-binlog-checksum", "--skip-force-if-open", "--require-row-format", path); err != nil {
		return File{}, fmt.Errorf("%w: archived log violates replay requirements: %w", ErrNewBaseline, err)
	}
	if err = validateBinlog(ctx, path, start, database, database, next); err != nil {
		return File{}, fmt.Errorf("%w: %w", ErrNewBaseline, err)
	}
	return f, nil
}

type sourceLog struct {
	Name string
	Size int64
}

func (s *session) logs(ctx context.Context) (logs []sourceLog, retErr error) {
	rows, err := s.conn.QueryContext(ctx, "SHOW BINARY LOGS")
	if err != nil {
		return nil, dbError("list binary logs", err)
	}
	defer fsutil.Close(&retErr, rows)
	for rows.Next() {
		var l sourceLog
		var encrypted string
		if err = rows.Scan(&l.Name, &l.Size, &encrypted); err != nil {
			return nil, err
		}
		if !logName.MatchString(l.Name) {
			return nil, errors.New("server returned an unsafe binlog filename")
		}
		logs = append(logs, l)
	}
	return logs, rows.Err()
}
