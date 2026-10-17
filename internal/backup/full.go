package backup

import (
	"archive/tar"
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

var snapshotCoordinate = regexp.MustCompile(`^-- CHANGE REPLICATION SOURCE TO SOURCE_LOG_FILE='([a-zA-Z0-9_-]+\.[0-9]{6,})', SOURCE_LOG_POS=([0-9]+);$`)

func (e *Engine) Full(ctx context.Context, request string) (Result, error) {
	return e.backup(ctx, request, backupMethod{kind: "logical-full", collect: e.dump})
}

func syncDirectory(path string) error {
	f, err := fsutil.Open(path)
	if err != nil {
		return err
	}
	return errors.Join(f.Sync(), f.Close())
}
func fileDigest(path, name string) (meta File, retErr error) {
	f, err := fsutil.Open(path)
	if err != nil {
		return meta, err
	}
	defer fsutil.Close(&retErr, f)
	hash := sha256.New()
	n, err := io.Copy(hash, f)
	if err != nil {
		return meta, err
	}
	return File{Name: name, Size: n, SHA256: hex.EncodeToString(hash.Sum(nil))}, nil
}
func (e *Engine) dump(ctx context.Context, s *session, creds, work string, m *Manifest) error {
	path := filepath.Join(work, "dump.sql")
	if err := runDump(ctx, s, creds, path); err != nil {
		return err
	}
	coordinate, err := readSnapshotCoordinate(path)
	if err != nil {
		return err
	}
	m.Start = coordinate
	m.End = coordinate
	digest, err := fileDigest(path, "dump.sql")
	if err != nil {
		return err
	}
	m.Files = []File{digest}
	return nil
}
func runDump(ctx context.Context, s *session, creds, path string) (retErr error) {
	f, err := fsutil.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, f)
	// A single DB argument omits CREATE DATABASE/USE, allowing a selected target DB.
	return runTool(ctx, "mysqldump", creds, nil, f, "--single-transaction", "--source-data=2", "--quick", "--routines", "--events", "--triggers", "--hex-blob", "--no-tablespaces", "--set-gtid-purged=OFF", "--default-character-set=utf8mb4", s.cfg.DB)
}
func readSnapshotCoordinate(path string) (coordinate Coordinate, retErr error) {
	f, err := fsutil.Open(path)
	if err != nil {
		return coordinate, err
	}
	defer fsutil.Close(&retErr, f)
	scanner := bufio.NewScanner(io.LimitReader(f, 8<<20))
	scanner.Buffer(make([]byte, 4096), 1<<20)
	for scanner.Scan() {
		matches := snapshotCoordinate.FindStringSubmatch(scanner.Text())
		if matches == nil {
			continue
		}
		pos, parseErr := strconv.ParseInt(matches[2], 10, 64)
		if parseErr != nil {
			return coordinate, parseErr
		}
		return Coordinate{File: matches[1], Position: pos}, nil
	}
	return coordinate, errors.Join(errors.New("mysqldump did not provide a coordinated snapshot binlog position"), scanner.Err())
}
func writeTar(ctx context.Context, w io.Writer, dir string, files []File) (retErr error) {
	tw := tar.NewWriter(w)
	defer fsutil.Close(&retErr, tw)
	for _, meta := range files {
		if err := ctx.Err(); err != nil {
			return err
		}
		if err := tw.WriteHeader(&tar.Header{Name: meta.Name, Mode: 0600, Size: meta.Size, Typeflag: tar.TypeReg}); err != nil {
			return err
		}
		if err := copyFile(ctx, tw, filepath.Join(dir, meta.Name)); err != nil {
			return err
		}
	}
	return nil
}
func copyFile(ctx context.Context, w io.Writer, path string) (retErr error) {
	f, err := fsutil.Open(path)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, f)
	_, err = io.Copy(w, &contextReader{ctx: ctx, r: f})
	return err
}

type contextReader struct {
	ctx context.Context
	r   io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.r.Read(p)
}
