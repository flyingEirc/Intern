package physical

import (
	"bytes"
	"context"
	"errors"
	"io"
	"maps"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/flyingEirc/Intern/internal/pipeline"
)

// BackupSource is single-use. Configure its public fields before Run, and do not
// mutate them while running. Preflight must admit the source before invocation.
// The downstream writer must honor cancellation; Go cannot interrupt arbitrary
// user code blocked inside Write. The source never closes the caller's writer.
type BackupSource struct {
	Binary            string
	WorkRoot          string
	Connection        Connection
	ParentCheckpoints []byte
	Parallel          int
	Throttle          int
	PageTracking      bool
	Timeout           time.Duration

	mu       sync.Mutex
	started  bool
	metadata Metadata
	result   error
	complete bool
}

var _ pipeline.Source = (*BackupSource)(nil)

// Metadata returns independent copies only after a fully successful Run.
func (s *BackupSource) Metadata() (Metadata, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if !s.complete {
		return Metadata{}, errors.New("physical: backup metadata is not available")
	}
	if s.result != nil {
		return Metadata{}, s.result
	}
	result := s.metadata
	result.CheckpointsRaw = bytes.Clone(result.CheckpointsRaw)
	result.InfoRaw = bytes.Clone(result.InfoRaw)
	result.Info.Fields = maps.Clone(result.Info.Fields)
	return result, nil
}

// Run streams xbstream stdout and requires exit success, completion marker and
// agreeing native metadata before declaring the source complete.
func (s *BackupSource) Run(ctx context.Context, w io.Writer) (err error) {
	if err = s.begin(); err != nil {
		return err
	}
	var metadata Metadata
	defer func() {
		s.mu.Lock()
		defer s.mu.Unlock()
		s.metadata, s.result, s.complete = metadata, err, true
	}()
	if err = s.validate(w); err != nil {
		return err
	}
	ctx, cancel := toolContext(ctx, s.Timeout)
	defer cancel()
	dir, err := newWorkDir(s.WorkRoot)
	if err != nil {
		return err
	}
	defer func() { err = errors.Join(err, removeWorkDir(dir)) }()
	args, err := s.arguments(dir)
	if err != nil {
		return err
	}
	var diagnostic boundedOutput
	if err = runProcess(ctx, processSpec{binary: s.Binary, args: args, cleanup: dir, stdout: w, stderr: &diagnostic}); err != nil {
		return err
	}
	if !diagnostic.completed() {
		return errors.New("physical: xtrabackup did not report completed OK")
	}
	metadata, err = ReadMetadata(filepath.Join(dir, "lsn"))
	if err != nil {
		return err
	}
	if err = s.validateResult(metadata); err != nil {
		return err
	}
	if cause := context.Cause(ctx); cause != nil {
		return processCancellation{cause: cause}
	}
	return nil
}

func (s *BackupSource) begin() error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.started {
		return errors.New("physical: backup source is single-use")
	}
	s.started = true
	return nil
}

func (s *BackupSource) validate(w io.Writer) error {
	if !s.hasRequiredInputs(w) {
		return errors.New("physical: binary, writer, username, absolute socket and datadir are required")
	}
	if s.Parallel < 0 || s.Throttle < 0 || s.Timeout < 0 {
		return errors.New("physical: parallel, throttle and timeout must not be negative")
	}
	if strings.IndexByte(s.Connection.Username, 0) >= 0 || strings.IndexByte(s.Connection.Password, 0) >= 0 {
		return errors.New("physical: credentials contain an unsupported NUL byte")
	}
	return nil
}

func (s *BackupSource) hasRequiredInputs(w io.Writer) bool {
	return s.Binary != "" && w != nil && s.Connection.Username != "" &&
		filepath.IsAbs(s.Connection.Socket) && filepath.IsAbs(s.Connection.DataDir)
}

func (s *BackupSource) arguments(dir string) ([]string, error) {
	credential := filepath.Join(dir, "client.cnf")
	body := "[client]\nuser=" + optionQuote(s.Connection.Username) + "\npassword=" + optionQuote(s.Connection.Password) + "\n"
	if err := os.WriteFile(credential, []byte(body), 0o600); err != nil {
		return nil, errors.New("physical: cannot create private credential file")
	}
	for _, child := range []string{"xb", "lsn"} {
		if err := os.Mkdir(filepath.Join(dir, child), 0o700); err != nil {
			return nil, errors.New("physical: cannot create private tool directory")
		}
	}
	parallel := max(s.Parallel, 1)
	args := []string{"--defaults-file=" + credential, "--backup", "--stream=xbstream", "--socket=" + s.Connection.Socket, "--datadir=" + s.Connection.DataDir, "--target-dir=" + filepath.Join(dir, "xb"), "--extra-lsndir=" + filepath.Join(dir, "lsn"), "--parallel=" + strconv.Itoa(parallel)}
	if s.Throttle > 0 {
		args = append(args, "--throttle="+strconv.Itoa(s.Throttle))
	}
	if s.PageTracking {
		args = append(args, "--page-tracking")
	}
	return s.incrementalArguments(args)
}

func (s *BackupSource) incrementalArguments(args []string) ([]string, error) {
	if len(s.ParentCheckpoints) == 0 {
		return args, nil
	}
	cp, err := ParseCheckpoints(s.ParentCheckpoints)
	if err != nil {
		return nil, err
	}
	if cp.BackupType != "full-backuped" && cp.BackupType != "incremental" {
		return nil, errors.New("physical: incremental parent must be an unprepared backup")
	}
	return append(args, "--incremental-lsn="+strconv.FormatUint(cp.ToLSN, 10)), nil
}

func (s *BackupSource) validateResult(metadata Metadata) error {
	cp := metadata.Checkpoints
	if metadata.Info.Fields["format"] != "xbstream" {
		return errors.New("physical: tool metadata does not describe xbstream")
	}
	if len(s.ParentCheckpoints) == 0 {
		if cp.BackupType != "full-backuped" || cp.FromLSN != 0 {
			return errors.New("physical: tool did not produce an unprepared full backup")
		}
		return nil
	}
	parent, err := ParseCheckpoints(s.ParentCheckpoints)
	if err != nil {
		return err
	}
	if cp.BackupType != "incremental" || cp.FromLSN != parent.ToLSN {
		return errors.New("physical: incremental checkpoint does not continue its parent")
	}
	return nil
}

func optionQuote(value string) string {
	replacer := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n", "\r", "\\r", "\t", "\\t", "\b", "\\b")
	return "\"" + replacer.Replace(value) + "\""
}

func newWorkDir(root string) (string, error) {
	if !filepath.IsAbs(root) {
		return "", errors.New("physical: work root must be absolute")
	}
	dir, err := os.MkdirTemp(root, "physical-run-")
	if err != nil {
		return "", errors.New("physical: cannot create private work directory")
	}
	return dir, nil
}

func removeWorkDir(dir string) error {
	if err := os.RemoveAll(dir); err != nil {
		return errors.New("physical: private work cleanup failed; operator cleanup required")
	}
	return nil
}
