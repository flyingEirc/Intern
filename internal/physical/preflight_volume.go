package physical

import (
	"context"
	"errors"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/flyingEirc/Intern/internal/fsutil"
)

const maxAutoCNFBytes = 64 * 1024

func checkDataVolume(ctx context.Context, directory string, identity Identity, socket string) (retErr error) {
	if filepath.Clean(directory) != filepath.Clean(identity.DataDir) {
		return preflightFailure("volume", "datadir_mismatch", "Mount the data volume at the same absolute path returned by MySQL datadir.")
	}
	root, err := os.OpenRoot(directory)
	if err != nil {
		return preflightExternalFailure(ctx, "volume", "datadir_unreadable", "Cannot open the data directory; check the mount and runtime UID/GID.")
	}
	defer func() {
		if closeErr := root.Close(); closeErr != nil {
			retErr = errors.Join(retErr, preflightFailure("volume", "datadir_unreadable", "Cannot close the data-volume inspection handle."))
		}
	}()
	if err = checkVolumeUUID(root, identity.UUID); err != nil {
		return err
	}
	err = fs.WalkDir(root.FS(), ".", func(path string, entry fs.DirEntry, walkErr error) error {
		return checkVolumeEntry(ctx, root, path, socket, entry, walkErr)
	})
	if err != nil {
		return err
	}
	return nil
}

func checkVolumeUUID(root *os.Root, expected string) error {
	info, err := root.Lstat("auto.cnf")
	if err != nil || !info.Mode().IsRegular() {
		return preflightFailure("volume", "auto_cnf_unreadable", "auto.cnf must be a readable regular file in the mounted data volume.")
	}
	file, err := root.Open("auto.cnf")
	if err != nil {
		return preflightFailure("volume", "auto_cnf_unreadable", "Cannot read auto.cnf from the mounted data volume; check mount ownership and permissions.")
	}
	raw, readErr := io.ReadAll(io.LimitReader(file, maxAutoCNFBytes+1))
	closeErr := file.Close()
	if readErr != nil || closeErr != nil {
		return preflightFailure("volume", "auto_cnf_unreadable", "Cannot read auto.cnf from the mounted data volume.")
	}
	uuid, err := parseAutoCNF(raw)
	if err != nil {
		return err
	}
	if !strings.EqualFold(uuid, expected) {
		return preflightFailure("volume", "uuid_mismatch", "The mounted data volume belongs to a different server UUID; correct the mount before backing up.")
	}
	return nil
}

func parseAutoCNF(raw []byte) (string, error) {
	text := string(raw)
	if len(raw) > maxAutoCNFBytes || strings.ContainsRune(text, 0) {
		return "", invalidAutoCNF()
	}
	var section, uuid string
	for line := range strings.SplitSeq(text, "\n") {
		line = strings.TrimSpace(line)
		if ignorableAutoCNFLine(line) {
			continue
		}
		if strings.HasPrefix(line, "[") && strings.HasSuffix(line, "]") {
			section = line
			continue
		}
		value, err := autoCNFValue(section, line, uuid)
		if err != nil {
			return "", err
		}
		uuid = value
	}
	if !sourceUUIDPattern.MatchString(uuid) {
		return "", invalidAutoCNF()
	}
	return uuid, nil
}

func ignorableAutoCNFLine(line string) bool {
	return line == "" || strings.HasPrefix(line, "#") || strings.HasPrefix(line, ";")
}

func autoCNFValue(section, line, previous string) (string, error) {
	key, value, ok := strings.Cut(line, "=")
	if !ok || section != "[auto]" || strings.TrimSpace(key) != "server-uuid" || previous != "" {
		return "", invalidAutoCNF()
	}
	return strings.TrimSpace(value), nil
}

func invalidAutoCNF() error {
	return preflightFailure("volume", "auto_cnf_invalid", "auto.cnf must contain one valid server-uuid in its auto section.")
}

func checkVolumeEntry(ctx context.Context, root *os.Root, path, socket string, entry fs.DirEntry, walkErr error) error {
	if ctx.Err() != nil {
		return preflightExternalFailure(ctx, "volume", "canceled", "Preflight was canceled.")
	}
	if errors.Is(walkErr, fs.ErrNotExist) {
		return nil
	}
	if walkErr != nil {
		return preflightFailure("volume", "datadir_unreadable", "Cannot enumerate the data volume; check read and directory search permissions for the runtime UID/GID.")
	}
	if entry.Type()&os.ModeSymlink != 0 {
		if isSocketAlias(root, path, socket) {
			return nil
		}
		return preflightFailure("volume", "datadir_symlink_unsupported", "Symlinked data files or directories are outside the admitted volume layout; mount a self-contained datadir.")
	}
	// Unix sockets may live in datadir; they are not physical backup payloads.
	if entry.Type()&os.ModeSocket != 0 {
		return nil
	}
	if !entry.IsDir() && !entry.Type().IsRegular() {
		return preflightFailure("volume", "datadir_file_unsupported", "The data directory contains a non-regular file that cannot be safely inspected.")
	}
	return checkReadableVolumeEntry(root, path)
}

func checkReadableVolumeEntry(root *os.Root, path string) error {
	file, err := root.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return preflightFailure("volume", "datadir_unreadable", "Cannot open a data-volume entry; check file readability and directory search permissions for the runtime UID/GID.")
	}
	if err := file.Close(); err != nil {
		return preflightFailure("volume", "datadir_unreadable", "Cannot finish inspecting data-volume readability.")
	}
	return nil
}

func checkKeyringMaterials(ctx context.Context, encrypted bool, files []string) error {
	if encrypted && len(files) == 0 {
		return preflightFailure("keyring", "keyring_required", "Encrypted tablespaces, redo or undo require independently protected historical keyring material; encrypted sources are not enabled by this runner.")
	}
	for _, path := range files {
		if ctx.Err() != nil {
			return preflightExternalFailure(ctx, "keyring", "canceled", "Preflight was canceled.")
		}
		if err := inspectKeyringFile(path); err != nil {
			return err
		}
	}
	if encrypted || len(files) != 0 {
		return preflightFailure("keyring", "keyring_unsupported", "Readable keyring files do not prove recoverability. This runner has no admitted keyring backup/prepare transport; encrypted sources remain unsupported.")
	}
	return nil
}

func inspectKeyringFile(path string) error {
	if !filepath.IsAbs(path) {
		return preflightFailure("keyring", "keyring_unreadable", "Keyring materials must be readable regular files at absolute paths.")
	}
	info, err := os.Lstat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() == 0 {
		return preflightFailure("keyring", "keyring_unreadable", "A required keyring material is missing, empty, symlinked or not a regular file.")
	}
	file, err := fsutil.Open(path)
	if err != nil {
		return preflightFailure("keyring", "keyring_unreadable", "Cannot open a required keyring material with the runtime UID/GID.")
	}
	if err := file.Close(); err != nil {
		return preflightFailure("keyring", "keyring_unreadable", "Cannot finish inspecting keyring material readability.")
	}
	return nil
}

func canonicalDirectory(path string) (string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", err
	}
	if !info.IsDir() {
		return "", errors.New("not a directory")
	}
	return canonical, nil
}

func pathsOverlap(left, right string) bool {
	return pathWithin(left, right) || pathWithin(right, left)
}

func pathWithin(root, path string) bool {
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != ".." && !strings.HasPrefix(relative, ".."+string(filepath.Separator))
}

func isSocketAlias(root *os.Root, path, socket string) bool {
	if socket == "" {
		return false
	}
	target, err := root.Readlink(path)
	if err != nil {
		return false
	}
	if !filepath.IsAbs(target) {
		target = filepath.Join(root.Name(), filepath.Dir(path), target)
	}
	resolved, err := filepath.EvalSymlinks(target)
	if err != nil {
		return false
	}
	expected, err := filepath.EvalSymlinks(socket)
	if err != nil || resolved != expected {
		return false
	}
	info, err := os.Stat(resolved)
	return err == nil && info.Mode()&os.ModeSocket != 0
}
