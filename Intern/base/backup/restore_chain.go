package backup

import (
	"archive/tar"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"

	"github.com/flyingEirc/IotProject/base/artifact"
	"github.com/flyingEirc/IotProject/base/catalog"
	"github.com/flyingEirc/IotProject/base/fsutil"
)

func (e *Engine) prepareChain(ctx context.Context, work, id string) ([]preparedBackup, int64, error) {
	var chain []preparedBackup
	seen := map[string]bool{}
	total := int64(0)
	for id != "" {
		if !artifactID.MatchString(id) || seen[id] || len(chain) >= 10000 {
			return nil, 0, errors.New("invalid, cyclic, or excessive backup chain")
		}
		seen[id] = true
		b, n, err := e.prepareMember(ctx, work, id, e.opts.MaxRestoreBytes-total)
		if err != nil {
			return nil, 0, err
		}
		total += n
		chain = append(chain, b)
		id = b.manifest.Parent
	}
	slices.Reverse(chain)
	if err := validateChain(chain); err != nil {
		return nil, 0, err
	}
	return chain, total, nil
}
func (e *Engine) prepareMember(ctx context.Context, work, id string, limit int64) (b preparedBackup, n int64, retErr error) {
	record, err := e.store.Get(ctx, id)
	if err != nil {
		return b, 0, fmt.Errorf("backup chain member %s is not published: %w", id, err)
	}
	if err = e.validateArtifactPath(record); err != nil {
		return b, 0, err
	}
	b.directory = filepath.Join(work, id)
	if err = os.Mkdir(b.directory, 0700); err != nil {
		return b, 0, err
	}
	path := filepath.Join(b.directory, "payload.tar")
	b.manifest, n, err = e.unpackMember(ctx, record, b.directory, work, path, limit)
	if err != nil {
		return b, 0, err
	}
	if err = os.Remove(path); err != nil {
		return b, 0, err
	}
	return b, n, nil
}
func (e *Engine) validateArtifactPath(record catalog.Record) error {
	if record.Path != filepath.Join(e.opts.Directory, record.ID) {
		return errors.New("backup chain member is outside selected backup directory")
	}
	st, err := os.Lstat(record.Path)
	if err != nil {
		return errors.New("backup artifact directory is missing or unsafe")
	}
	if !st.IsDir() || st.Mode()&os.ModeSymlink != 0 {
		return errors.New("backup artifact directory is missing or unsafe")
	}
	return nil
}
func (e *Engine) unpackMember(ctx context.Context, record catalog.Record, dir, work, path string, limit int64) (m Manifest, n int64, retErr error) {
	out, err := fsutil.OpenFile(path, os.O_RDWR|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return m, 0, err
	}
	defer fsutil.Close(&retErr, out)
	expected, err := parseManifest(record.Manifest)
	if err != nil {
		return m, 0, err
	}
	metadata, err := artifact.Read(ctx, record.Path, out, artifact.ReadOptions{KeyFile: e.opts.KeyFile, WorkDirectory: work, MaxBytes: limit, Encrypt: expected.Encrypted})
	if err != nil {
		return m, 0, err
	}
	m, err = e.manifestForRecord(metadata, record)
	if err != nil {
		return m, 0, err
	}
	if _, err = out.Seek(0, io.SeekStart); err != nil {
		return m, 0, err
	}
	n, err = extractTar(ctx, out, dir, m.Files, limit)
	return m, n, err
}
func (e *Engine) manifestForRecord(metadata []byte, record catalog.Record) (Manifest, error) {
	// SQLite is an index, not an alternative unauthenticated recovery manifest.
	if string(metadata) != string(record.Manifest) {
		return Manifest{}, errors.New("catalog and authenticated manifest disagree")
	}
	m, err := parseManifest(metadata)
	if err != nil {
		return m, err
	}
	if m.ID != record.ID || m.Project != e.opts.Project || m.Kind != record.Kind || m.Parent != record.Parent {
		return m, errors.New("artifact identity/chain does not match selected catalog")
	}
	return m, nil
}
func validateChain(chain []preparedBackup) error {
	if len(chain) == 0 || chain[0].manifest.Kind != "logical-full" {
		return errors.New("backup chain lacks a logical full baseline")
	}
	for i := 1; i < len(chain); i++ {
		prev, m := chain[i-1].manifest, chain[i].manifest
		if !validChainLink(prev, m) {
			return errors.New("backup chain identity or binlog coordinates are discontinuous")
		}
		if err := validateLogSequence(m); err != nil {
			return err
		}
	}
	return nil
}
func validChainLink(prev, m Manifest) bool {
	return m.Kind == "binlog" && m.Parent == prev.ID && m.Start == prev.End && m.SourceUUID == prev.SourceUUID && m.Database == prev.Database && m.ServerVersion == prev.ServerVersion && m.GTIDMode == prev.GTIDMode
}
func validateLogSequence(m Manifest) error {
	if m.Files[0].Name != m.Start.File || m.Start.Position > m.Files[0].Size || m.End.Position != 4 {
		return errors.New("invalid incremental start/end range")
	}
	for j, f := range m.Files {
		_, next := logBoundary(m, j)
		if !logName.MatchString(f.Name) || !nextLog(f.Name, next) {
			return errors.New("missing or out-of-order binlog in backup chain")
		}
	}
	return nil
}
func logBoundary(m Manifest, index int) (int64, string) {
	start := int64(4)
	if index == 0 {
		start = m.Start.Position
	}
	next := m.End.File
	if index+1 < len(m.Files) {
		next = m.Files[index+1].Name
	}
	return start, next
}
func extractTar(ctx context.Context, r io.Reader, dir string, files []File, limit int64) (int64, error) {
	tr := tar.NewReader(r)
	total := int64(0)
	seen := map[string]bool{}
	for _, meta := range files {
		if err := validateArchiveMember(meta, seen, limit-total); err != nil {
			return 0, err
		}
		seen[meta.Name] = true
		if err := extractMember(ctx, tr, dir, meta); err != nil {
			return 0, err
		}
		total += meta.Size
	}
	if _, err := tr.Next(); !errors.Is(err, io.EOF) {
		return 0, errors.New("archive contains unexpected extra entries")
	}
	return total, nil
}
func validateArchiveMember(meta File, seen map[string]bool, remaining int64) error {
	if filepath.Base(meta.Name) != meta.Name || (meta.Name != "dump.sql" && !logName.MatchString(meta.Name)) {
		return errors.New("unsafe or oversized artifact file manifest")
	}
	if seen[meta.Name] || meta.Size < 0 || meta.Size > remaining {
		return errors.New("unsafe or oversized artifact file manifest")
	}
	return nil
}
func extractMember(ctx context.Context, tr *tar.Reader, dir string, meta File) (retErr error) {
	h, err := tr.Next()
	if err != nil {
		return errors.New("missing artifact archive member")
	}
	if h.Typeflag != tar.TypeReg || h.Name != meta.Name || h.Size != meta.Size {
		return errors.New("artifact tar member does not match manifest")
	}
	out, err := fsutil.OpenFile(filepath.Join(dir, meta.Name), os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0600)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, out)
	hash := sha256.New()
	n, err := io.Copy(io.MultiWriter(out, hash), &contextReader{ctx: ctx, r: tr})
	if err != nil {
		return err
	}
	if n != meta.Size || hex.EncodeToString(hash.Sum(nil)) != meta.SHA256 {
		return errors.New("artifact member digest mismatch")
	}
	return nil
}
