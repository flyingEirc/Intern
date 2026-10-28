package restore

import (
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

// Export directory and file modes are part of the published contract: an
// operator mounts <export>/<id>/data into a MySQL container, whose server user
// must own and write the files.
const (
	exportDirMode  = 0o750 // #nosec G302 -- export directories are 0750 by contract.
	exportFileMode = 0o640 // #nosec G302 -- export files are 0640 by contract.
)

// publish copies the prepared data directory into a private pending directory,
// syncs it, and then renames it to the final export path without replacing an
// existing entry. The export is never a partially copied tree.
func (e *execution) publish(ctx context.Context) error {
	if err := os.Mkdir(e.pending, 0o700); err != nil {
		return stage(CodeExportFailed, errors.New("restore: cannot create the pending export"))
	}
	e.pendingCreated = true
	copied, err := copyTree(ctx, filepath.Join(e.work, "data"), filepath.Join(e.pending, "data"))
	if err != nil {
		return stage(CodeExportFailed, err)
	}
	if err = syncDirectory(e.pending); err != nil {
		return stage(CodeExportFailed, err)
	}
	if err = renameNoReplace(e.pending, e.export); err != nil {
		if errors.Is(err, os.ErrExist) {
			return stage(CodeExportFailed, errors.New("restore: the export path already exists"))
		}
		return stage(CodeExportFailed, err)
	}
	e.pendingCreated = false
	e.exportBytes = copied
	if err = syncDirectory(e.opts.ExportRoot); err != nil {
		return stage(CodeExportFailed, err)
	}
	return nil
}

// copyTree copies a prepared datadir with fixed modes and syncs every file and
// directory before returning. The owner stays the current uid/gid; no chown is
// attempted.
func copyTree(ctx context.Context, source, target string) (int64, error) {
	info, err := os.Lstat(source)
	if err != nil || !info.IsDir() || info.Mode()&os.ModeSymlink != 0 {
		return 0, errors.New("restore: the prepared data directory is missing")
	}
	if err = os.Mkdir(target, exportDirMode); err != nil {
		return 0, err
	}
	if err = os.Chmod(target, exportDirMode); err != nil {
		return 0, err
	}
	entries, err := os.ReadDir(source)
	if err != nil {
		return 0, err
	}
	var total int64
	for _, entry := range entries {
		if err = ctx.Err(); err != nil {
			return total, err
		}
		copied, err := copyEntry(ctx, filepath.Join(source, entry.Name()), filepath.Join(target, entry.Name()))
		total += copied
		if err != nil {
			return total, err
		}
	}
	return total, syncDirectory(target)
}

func copyEntry(ctx context.Context, source, target string) (int64, error) {
	info, err := os.Lstat(source)
	if err != nil {
		return 0, err
	}
	switch {
	case info.IsDir() && info.Mode()&os.ModeSymlink == 0:
		return copyTree(ctx, source, target)
	case info.Mode().IsRegular():
		return copyFile(ctx, source, target)
	default:
		return 0, errors.New("restore: the prepared datadir contains an unsupported entry")
	}
}

func copyFile(ctx context.Context, source, target string) (written int64, retErr error) {
	src, err := fsutil.Open(source)
	if err != nil {
		return 0, err
	}
	defer fsutil.Close(&retErr, src)
	dst, err := fsutil.OpenFile(target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, exportFileMode)
	if err != nil {
		return 0, err
	}
	defer fsutil.Close(&retErr, dst)
	if written, err = io.Copy(dst, &contextReader{ctx: ctx, reader: src}); err != nil {
		return written, err
	}
	if err = dst.Sync(); err != nil {
		return written, err
	}
	return written, os.Chmod(target, exportFileMode)
}

type contextReader struct {
	ctx    context.Context
	reader io.Reader
}

func (r *contextReader) Read(p []byte) (int, error) {
	if err := r.ctx.Err(); err != nil {
		return 0, err
	}
	return r.reader.Read(p)
}
