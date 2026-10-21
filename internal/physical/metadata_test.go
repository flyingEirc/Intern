package physical

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testCheckpoints = "backup_type = full-backuped\nfrom_lsn = 0\nto_lsn = 100\nlast_lsn = 110\nflushed_lsn = 100\nredo_memory = 0\nredo_frames = 0\n"

const testInfo = "uuid = 12345678-1234-1234-1234-123456789abc\nname = \ntool_name = xtrabackup\ntool_command = --defaults-file=/private/client.cnf --backup\ntool_version = 8.4.0-7\nibbackup_version = 8.4.0-7\nserver_version = 8.4.11\nserver_flavor = MySQL Community Server - GPL\nstart_time = 2026-10-20 12:00:00\nend_time = 2026-10-20 12:00:01\nlock_time = 0\nbinlog_pos = filename 'binlog.000001', position '123'\ninnodb_from_lsn = 0\ninnodb_to_lsn = 100\npartial = N\nincremental = N\nformat = xbstream\ncompressed = N\nencrypted = N\nlock_ddl_type = OFF\nbackup_size = 1234\n"

func TestMetadataExactBytes(t *testing.T) {
	dir := t.TempDir()
	cp := []byte(strings.ReplaceAll(testCheckpoints, "\n", "\r\n"))
	info := []byte(testInfo)
	os.WriteFile(filepath.Join(dir, "xtrabackup_checkpoints"), cp, 0o600)
	os.WriteFile(filepath.Join(dir, "xtrabackup_info"), info, 0o600)
	result, err := ReadMetadata(dir)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(cp, result.CheckpointsRaw) || !bytes.Equal(info, result.InfoRaw) || result.Checkpoints.ToLSN != 100 || result.Info.ToolVersion != "8.4.0-7" || result.Info.UUID != "12345678-1234-1234-1234-123456789abc" || result.Info.Fields["server_flavor"] != "MySQL Community Server - GPL" || result.Info.Fields["lock_ddl_type"] != "OFF" || result.Info.Fields["backup_size"] != "1234" {
		t.Fatalf("metadata bytes or parsed fields changed: %#v", result)
	}
}

func TestCheckpointBoundaries(t *testing.T) {
	cases := map[string]string{
		"negative":   strings.Replace(testCheckpoints, "to_lsn = 100", "to_lsn = -1", 1),
		"plus":       strings.Replace(testCheckpoints, "to_lsn = 100", "to_lsn = +1", 1),
		"overflow":   strings.Replace(testCheckpoints, "to_lsn = 100", "to_lsn = 18446744073709551616", 1),
		"reverse":    strings.Replace(testCheckpoints, "last_lsn = 110", "last_lsn = 99", 1),
		"unknown":    testCheckpoints + "unknown = 1\n",
		"duplicate":  testCheckpoints + "to_lsn = 100\n",
		"missing":    strings.Replace(testCheckpoints, "last_lsn = 110\n", "", 1),
		"state":      strings.Replace(testCheckpoints, "full-backuped", "full-backed-up", 1),
		"full-start": strings.Replace(testCheckpoints, "from_lsn = 0", "from_lsn = 1", 1),
		"nul":        testCheckpoints + "\x00",
	}
	for name, raw := range cases {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseCheckpoints([]byte(raw)); err == nil {
				t.Fatal("malformed checkpoint accepted")
			}
		})
	}
}

func TestInfoBoundaries(t *testing.T) {
	for name, raw := range map[string]string{
		"uuid":      strings.Replace(testInfo, "12345678-1234-1234-1234-123456789abc", "invalid", 1),
		"version":   strings.Replace(testInfo, "server_version = 8.4.11", "server_version = unknown", 1),
		"duplicate": testInfo + "format = xbstream\n",
		"format":    strings.Replace(testInfo, "format = xbstream", "format = tar", 1),
		"boolean":   strings.Replace(testInfo, "incremental = N", "incremental = false", 1),
		"number":    strings.Replace(testInfo, "innodb_to_lsn = 100", "innodb_to_lsn = nope", 1),
		"unknown":   testInfo + "unrecognized = foo\n",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ParseInfo([]byte(raw)); err == nil {
				t.Fatal("malformed info accepted")
			}
		})
	}
}

func TestInfoRejectsSignedLSNsAndSizes(t *testing.T) {
	cases := []struct {
		name string
		raw  string
	}{
		{name: "lsn-sign", raw: strings.Replace(testInfo, "innodb_to_lsn = 100", "innodb_to_lsn = -1", 1)},
		{name: "from-sign", raw: strings.Replace(testInfo, "innodb_from_lsn = 0", "innodb_from_lsn = -1", 1)},
		{name: "lock-plus", raw: strings.Replace(testInfo, "lock_time = 0", "lock_time = +1", 1)},
		{name: "lock-overflow", raw: strings.Replace(testInfo, "lock_time = 0", "lock_time = 9223372036854775808", 1)},
		{name: "lock-underflow", raw: strings.Replace(testInfo, "lock_time = 0", "lock_time = -9223372036854775809", 1)},
		{name: "lock-junk", raw: strings.Replace(testInfo, "lock_time = 0", "lock_time = -1x", 1)},
		{name: "size-sign", raw: strings.Replace(testInfo, "backup_size = 1234", "backup_size = -1", 1)},
		{name: "size-plus", raw: strings.Replace(testInfo, "backup_size = 1234", "backup_size = +1", 1)},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := ParseInfo([]byte(tc.raw)); err == nil {
				t.Fatal("malformed info accepted")
			}
		})
	}
}

func TestSignedLockTimeIsInformational(t *testing.T) {
	for _, lock := range []string{"-9223372036854775808", "9223372036854775807"} {
		raw := strings.Replace(testInfo, "lock_time = 0", "lock_time = "+lock, 1)
		info, err := ParseInfo([]byte(raw))
		if err != nil {
			t.Fatalf("lock_time %s: %v", lock, err)
		}
		if info.Fields["lock_time"] != lock || info.FromLSN != 0 || info.ToLSN != 100 || info.UUID != "12345678-1234-1234-1234-123456789abc" {
			t.Fatalf("lock_time %s changed the backup interval or identity: %#v", lock, info)
		}
	}
}

func TestMetadataPreservesSignedLockTime(t *testing.T) {
	dir := t.TempDir()
	raw := strings.Replace(testInfo, "lock_time = 0", "lock_time = -7", 1)
	if err := os.WriteFile(filepath.Join(dir, "xtrabackup_checkpoints"), []byte(testCheckpoints), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "xtrabackup_info"), []byte(raw), 0o600); err != nil {
		t.Fatal(err)
	}
	result, err := ReadMetadata(dir)
	if err != nil {
		t.Fatal(err)
	}
	if string(result.InfoRaw) != raw || result.Info.Fields["lock_time"] != "-7" || result.Checkpoints.FromLSN != 0 || result.Checkpoints.ToLSN != 100 {
		t.Fatal("negative lock_time was not preserved beside the unsigned LSN interval")
	}
}

func TestMetadataRejectsMismatchAndSymlink(t *testing.T) {
	dir := t.TempDir()
	os.WriteFile(filepath.Join(dir, "xtrabackup_checkpoints"), []byte(testCheckpoints), 0o600)
	os.WriteFile(filepath.Join(dir, "xtrabackup_info"), []byte(strings.Replace(testInfo, "innodb_to_lsn = 100", "innodb_to_lsn = 99", 1)), 0o600)
	if _, err := ReadMetadata(dir); err == nil {
		t.Fatal("disagreement accepted")
	}
	os.Remove(filepath.Join(dir, "xtrabackup_info"))
	os.Symlink(filepath.Join(dir, "xtrabackup_checkpoints"), filepath.Join(dir, "xtrabackup_info"))
	if _, err := ReadMetadata(dir); err == nil {
		t.Fatal("symlink accepted")
	}
}

func TestToolVersionParsing(t *testing.T) {
	raw := "xtrabackup version 8.4.0-7 based on MySQL server 8.4.0 Linux (x86_64) (revision id: 4093cfd8)\n"
	version, err := ParseToolVersion(raw)
	if err != nil || version.Raw != raw || version.Version != "8.4.0-7" {
		t.Fatalf("version = %#v, error = %v", version, err)
	}
	absolute := "/usr/bin/" + raw
	version, err = ParseToolVersion(absolute)
	if err != nil || version.Raw != absolute || version.Version != "8.4.0-7" {
		t.Fatalf("absolute executable banner = %#v, error = %v", version, err)
	}
	for _, malformed := range []string{"8.4.0-7", raw + raw, "xtrabackup version 8.4.0\n", strings.Repeat("x", diagnosticLimit+1)} {
		if _, err := ParseToolVersion(malformed); err == nil {
			t.Fatal("invalid version banner accepted")
		}
	}
}

func TestBoundedDiagnostic(t *testing.T) {
	var output boundedOutput
	output.Write(bytes.Repeat([]byte("secret"), diagnosticLimit))
	output.Write([]byte("\ncompleted OK!\n"))
	if len(output.data) > diagnosticLimit || !output.truncated || !output.completed() {
		t.Fatal("diagnostic bound or marker handling failed")
	}
	output.Write([]byte("later failure\n"))
	if output.completed() {
		t.Fatal("nonterminal completion marker accepted")
	}
}
