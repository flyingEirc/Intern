package physical

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"unicode/utf8"
)

const metadataLimit = 1024 * 1024

var uuidPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func metadataFields(raw []byte, allowed string) (map[string]string, error) {
	if !validMetadataEncoding(raw) {
		return nil, errors.New("physical: invalid metadata encoding or size")
	}
	fields := make(map[string]string)
	for _, line := range strings.Split(string(raw), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		key, value, ok := strings.Cut(line, "=")
		key = strings.TrimSpace(key)
		if !ok || key == "" || !strings.Contains(allowed, "|"+key+"|") {
			return nil, errors.New("physical: unsupported metadata field or format")
		}
		if _, duplicate := fields[key]; duplicate {
			return nil, errors.New("physical: duplicate metadata field")
		}
		fields[key] = strings.TrimSpace(value)
	}
	return fields, nil
}

func validMetadataEncoding(raw []byte) bool {
	return len(raw) > 0 && len(raw) <= metadataLimit && utf8.Valid(raw) && bytes.IndexByte(raw, 0) < 0
}

func decimalField(fields map[string]string, key string) (uint64, error) {
	value := fields[key]
	if value == "" || strings.IndexFunc(value, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return 0, fmt.Errorf("physical: invalid metadata integer %s", key)
	}
	n, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		return 0, fmt.Errorf("physical: metadata integer %s overflows", key)
	}
	return n, nil
}

// signedDecimalField accepts the native signed range of an informational field.
// A leading plus is not part of that rendering. LSN and size fields stay unsigned.
func signedDecimalField(fields map[string]string, key string) error {
	value := fields[key]
	if value == "" || value[0] == '+' {
		return fmt.Errorf("physical: invalid metadata integer %s", key)
	}
	body := value
	if body[0] == '-' {
		body = body[1:]
	}
	if body == "" || strings.IndexFunc(body, func(r rune) bool { return r < '0' || r > '9' }) >= 0 {
		return fmt.Errorf("physical: invalid metadata integer %s", key)
	}
	if _, err := strconv.ParseInt(value, 10, 64); err != nil {
		return fmt.Errorf("physical: metadata integer %s overflows", key)
	}
	return nil
}

// ParseCheckpoints rejects unknown states, malformed numbers and reversed LSNs.
func ParseCheckpoints(raw []byte) (Checkpoints, error) {
	fields, err := metadataFields(raw, "|backup_type|from_lsn|to_lsn|last_lsn|flushed_lsn|compact|recover_binlog_info|redo_memory|redo_frames|")
	if err != nil {
		return Checkpoints{}, err
	}
	cp := Checkpoints{BackupType: fields["backup_type"]}
	switch cp.BackupType {
	case "full-backuped", "incremental", "full-prepared", "log-applied":
	default:
		return Checkpoints{}, errors.New("physical: unsupported checkpoint backup_type")
	}
	for key, dest := range map[string]*uint64{"from_lsn": &cp.FromLSN, "to_lsn": &cp.ToLSN, "last_lsn": &cp.LastLSN} {
		*dest, err = decimalField(fields, key)
		if err != nil {
			return Checkpoints{}, err
		}
	}
	if err := validateCheckpointNumbers(fields, cp); err != nil {
		return Checkpoints{}, err
	}
	return cp, nil
}

func validateCheckpointNumbers(fields map[string]string, cp Checkpoints) error {
	if cp.ToLSN < cp.FromLSN || cp.LastLSN < cp.ToLSN || cp.ToLSN == 0 {
		return errors.New("physical: invalid checkpoint LSN ordering")
	}
	if cp.BackupType == "full-backuped" && cp.FromLSN != 0 {
		return errors.New("physical: full backup has nonzero from_lsn")
	}
	for _, key := range []string{"flushed_lsn", "compact", "recover_binlog_info", "redo_memory", "redo_frames"} {
		if _, exists := fields[key]; exists {
			if _, err := decimalField(fields, key); err != nil {
				return err
			}
		}
	}
	return nil
}

// ParseInfo parses the native key/value format, preserving all recognized fields.
// lock_time is the tool's signed informational duration and may be negative
// when the clock steps backward. LSN and size fields remain unsigned.
func ParseInfo(raw []byte) (Info, error) {
	fields, err := metadataFields(raw, "|uuid|name|tool_name|tool_command|tool_version|ibbackup_version|server_version|server_flavor|start_time|end_time|lock_time|binlog_pos|innodb_from_lsn|innodb_to_lsn|partial|incremental|format|compact|compressed|encrypted|lock_ddl_type|backup_size|uncompressed_backup_size|")
	if err != nil {
		return Info{}, err
	}
	info := Info{UUID: fields["uuid"], ToolName: fields["tool_name"], ToolVersion: fields["tool_version"], ServerVersion: fields["server_version"], Fields: fields}
	if !validInfoIdentity(info) {
		return Info{}, errors.New("physical: invalid metadata identity or version")
	}
	if err = validateInfoFlags(fields); err != nil {
		return Info{}, err
	}
	info.Incremental = fields["incremental"] == "Y"
	info.FromLSN, err = decimalField(fields, "innodb_from_lsn")
	if err != nil {
		return Info{}, err
	}
	info.ToLSN, err = decimalField(fields, "innodb_to_lsn")
	if err != nil || info.ToLSN == 0 || info.FromLSN > info.ToLSN {
		return Info{}, errors.New("physical: invalid info LSN interval")
	}
	return info, nil
}

func validInfoIdentity(info Info) bool {
	return uuidPattern.MatchString(info.UUID) && info.ToolName == "xtrabackup" &&
		nativeVersionPattern.MatchString(info.ToolVersion) && serverVersionPattern.MatchString(info.ServerVersion)
}

func validateInfoFlags(fields map[string]string) error {
	for _, key := range []string{"partial", "incremental", "compact", "encrypted"} {
		value, exists := fields[key]
		if (exists || key == "incremental") && value != "Y" && value != "N" {
			return errors.New("physical: invalid metadata boolean")
		}
	}
	if fields["format"] != "xbstream" && fields["format"] != "file" {
		return errors.New("physical: unsupported metadata stream format")
	}
	return validateInfoOptionalFields(fields)
}

func validateInfoOptionalFields(fields map[string]string) error {
	if _, exists := fields["lock_time"]; exists {
		if err := signedDecimalField(fields, "lock_time"); err != nil {
			return err
		}
	}
	if err := validateInfoSizes(fields); err != nil {
		return err
	}
	if value, exists := fields["ibbackup_version"]; exists && value != fields["tool_version"] {
		return errors.New("physical: native backup tool versions disagree")
	}
	if value, exists := fields["compressed"]; exists && value != "N" && value != "compressed" {
		return errors.New("physical: unsupported native compression format")
	}
	return nil
}

func validateInfoSizes(fields map[string]string) error {
	for _, key := range []string{"backup_size", "uncompressed_backup_size"} {
		if _, exists := fields[key]; exists {
			if _, err := decimalField(fields, key); err != nil {
				return err
			}
		}
	}
	return nil
}

// ReadMetadata reads the --extra-lsndir xtrabackup_checkpoints and
// xtrabackup_info files without rewriting them. Those bytes are authoritative.
func ReadMetadata(dir string) (Metadata, error) {
	var result Metadata
	var err error
	result.CheckpointsRaw, err = readMetadataFile(filepath.Join(dir, "xtrabackup_checkpoints"))
	if err != nil {
		return Metadata{}, err
	}
	result.InfoRaw, err = readMetadataFile(filepath.Join(dir, "xtrabackup_info"))
	if err != nil {
		return Metadata{}, err
	}
	result.Checkpoints, err = ParseCheckpoints(result.CheckpointsRaw)
	if err != nil {
		return Metadata{}, err
	}
	result.Info, err = ParseInfo(result.InfoRaw)
	if err != nil {
		return Metadata{}, err
	}
	if result.Checkpoints.FromLSN != result.Info.FromLSN || result.Checkpoints.ToLSN != result.Info.ToLSN || (result.Checkpoints.BackupType == "incremental") != result.Info.Incremental {
		return Metadata{}, errors.New("physical: native metadata disagrees")
	}
	return result, nil
}

func readMetadataFile(path string) ([]byte, error) {
	stat, err := os.Lstat(path)
	if err != nil || !stat.Mode().IsRegular() || stat.Size() > metadataLimit {
		return nil, errors.New("physical: metadata must be a readable bounded regular file")
	}
	// #nosec G304 -- fixed native filenames under the caller-selected work directory.
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.New("physical: cannot open metadata file")
	}
	raw, readErr := io.ReadAll(io.LimitReader(f, metadataLimit+1))
	closeErr := f.Close()
	if readErr != nil || closeErr != nil || len(raw) > metadataLimit {
		return nil, errors.New("physical: cannot read bounded metadata file")
	}
	return raw, nil
}
