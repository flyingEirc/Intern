package physical

import (
	"context"
	"encoding/json/v2"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const preflightTestUUID = "3b1568ba-4016-11ed-b878-0242ac120002"

func requirePreflightCode(t *testing.T, err error, want string) {
	t.Helper()
	var failure *PreflightError
	if !errors.As(err, &failure) {
		t.Fatalf("want structured %q error, got %v", want, err)
	}
	if failure.Code != want {
		t.Fatalf("want code %q, got %q (%v)", want, failure.Code, err)
	}
}

func TestAutoCNFParsing(t *testing.T) {
	valid := "# generated\r\n[auto]\r\nserver-uuid = " + preflightTestUUID + "\r\n"
	got, err := parseAutoCNF([]byte(valid))
	if err != nil || got != preflightTestUUID {
		t.Fatalf("uuid=%q err=%v", got, err)
	}
	for _, test := range []struct{ name, raw string }{
		{"empty", ""},
		{"missing_section", "server-uuid=" + preflightTestUUID},
		{"wrong_section", "[mysqld]\nserver-uuid=" + preflightTestUUID},
		{"invalid_uuid", "[auto]\nserver-uuid=arbitrary"},
		{"duplicate", "[auto]\nserver-uuid=" + preflightTestUUID + "\nserver-uuid=" + preflightTestUUID},
		{"nul", "[auto]\nserver-uuid=" + preflightTestUUID + "\x00"},
		{"oversize", strings.Repeat("#", maxAutoCNFBytes+1)},
		{"malformed", "[auto]\nserver-uuid " + preflightTestUUID},
		{"unknown_key", "[auto]\nserver-uuid=" + preflightTestUUID + "\nsecret=ignored"},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := parseAutoCNF([]byte(test.raw))
			requirePreflightCode(t, err, "auto_cnf_invalid")
		})
	}
}

func TestBackupPrivilegeBoundaries(t *testing.T) {
	static := "GRANT PROCESS, RELOAD, LOCK TABLES, REPLICATION CLIENT ON *.* TO `backup`@`localhost`"
	dynamic := "GRANT BACKUP_ADMIN ON *.* TO `backup`@`localhost`"
	for _, test := range []struct {
		name   string
		grants []string
		want   []string
	}{
		{"minimum", []string{static, dynamic}, []string{}},
		{"root", []string{"GRANT ALL PRIVILEGES ON *.* TO `root`@`localhost`", dynamic}, []string{}},
		{"all_excludes_dynamic", []string{"GRANT ALL PRIVILEGES ON *.* TO `root`@`localhost`"}, []string{"BACKUP_ADMIN"}},
		{"table_grants_not_global", []string{static, "GRANT BACKUP_ADMIN ON `private`.* TO `backup`@`localhost`"}, []string{"BACKUP_ADMIN"}},
		{"account_text_is_not_privilege", []string{static, "GRANT USAGE ON *.* TO `BACKUP_ADMIN`@`localhost`"}, []string{"BACKUP_ADMIN"}},
		{"role_not_effective_evidence", []string{static, "GRANT `BACKUP_ADMIN`@`%` TO `backup`@`localhost`"}, []string{"BACKUP_ADMIN"}},
		{"missing_static", []string{dynamic}, []string{"PROCESS", "RELOAD", "LOCK TABLES", "REPLICATION CLIENT"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			got := missingBackupPrivileges(test.grants)
			if !reflect.DeepEqual(got, test.want) {
				t.Fatalf("missing=%v want=%v", got, test.want)
			}
		})
	}
	if hasGlobalTableVisibility([]string{static, dynamic}) {
		t.Fatal("minimal backup privileges must report partial table visibility")
	}
	if !hasGlobalTableVisibility([]string{"GRANT SELECT ON *.* TO `backup`@`localhost`"}) {
		t.Fatal("global SELECT must establish visibility")
	}
	if hasGlobalTableVisibility([]string{
		"GRANT SELECT ON *.* TO `backup`@`localhost`",
		"REVOKE SELECT ON `private`.* FROM `backup`@`localhost`",
	}) {
		t.Fatal("partial revokes must not claim complete visibility")
	}
}

func preflightVolumeFixture(t *testing.T) (string, Identity) {
	t.Helper()
	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "auto.cnf"), []byte("[auto]\nserver-uuid="+preflightTestUUID+"\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, "ibdata1"), []byte("fixture"), 0o600); err != nil {
		t.Fatal(err)
	}
	return dir, Identity{UUID: preflightTestUUID, DataDir: dir}
}

func TestVolumeIdentityAndReadability(t *testing.T) {
	dir, identity := preflightVolumeFixture(t)
	if err := checkDataVolume(t.Context(), dir, identity); err != nil {
		t.Fatal(err)
	}
	identity.UUID = "4b1568ba-4016-11ed-b878-0242ac120002"
	requirePreflightCode(t, checkDataVolume(t.Context(), dir, identity), "uuid_mismatch")
	identity.UUID = preflightTestUUID
	identity.DataDir = t.TempDir()
	requirePreflightCode(t, checkDataVolume(t.Context(), dir, identity), "datadir_mismatch")
	missing := filepath.Join(t.TempDir(), "missing")
	requirePreflightCode(t, checkDataVolume(t.Context(), missing, Identity{DataDir: missing}), "datadir_unreadable")
}

func TestVolumeRejectsSymlinkAndMissingAutoCNF(t *testing.T) {
	dir, identity := preflightVolumeFixture(t)
	if err := os.Symlink(filepath.Join(dir, "ibdata1"), filepath.Join(dir, "linked.ibd")); err != nil {
		t.Fatal(err)
	}
	requirePreflightCode(t, checkDataVolume(t.Context(), dir, identity), "datadir_symlink_unsupported")
	if err := os.Remove(filepath.Join(dir, "auto.cnf")); err != nil {
		t.Fatal(err)
	}
	requirePreflightCode(t, checkDataVolume(t.Context(), dir, identity), "auto_cnf_unreadable")
}

func TestSourceDatafileBoundaries(t *testing.T) {
	dir, _ := preflightVolumeFixture(t)
	if err := checkSourceDatafile(dir, "ibdata1"); err != nil {
		t.Fatal(err)
	}
	requirePreflightCode(t, checkSourceDatafile(dir, "../outside.ibd"), "external_tablespace_unsupported")
	requirePreflightCode(t, checkSourceDatafile(dir, "missing.ibd"), "datadir_unreadable")
}

func TestKeyringFilesCannotEnableUnsupportedEncryption(t *testing.T) {
	requirePreflightCode(t, checkKeyringMaterials(t.Context(), true, nil), "keyring_required")
	if err := checkKeyringMaterials(t.Context(), false, nil); err != nil {
		t.Fatal(err)
	}
	key := filepath.Join(t.TempDir(), "sensitive-keyring-material")
	requirePreflightCode(t, checkKeyringMaterials(t.Context(), true, []string{key}), "keyring_unreadable")
	if err := os.WriteFile(key, []byte("not-a-real-keyring"), 0o600); err != nil {
		t.Fatal(err)
	}
	requirePreflightCode(t, checkKeyringMaterials(t.Context(), true, []string{key}), "keyring_unsupported")
	link := key + "-link"
	if err := os.Symlink(key, link); err != nil {
		t.Fatal(err)
	}
	requirePreflightCode(t, checkKeyringMaterials(t.Context(), true, []string{link}), "keyring_unreadable")
}

func TestCapacityAdmissionBoundaries(t *testing.T) {
	for _, test := range []struct {
		name               string
		work, backup       volumeSpace
		minWork, minBackup uint64
		code               string
	}{
		{"separate_equal", volumeSpace{10, 1}, volumeSpace{20, 2}, 10, 20, ""},
		{"work_short", volumeSpace{9, 1}, volumeSpace{20, 2}, 10, 20, "space_insufficient"},
		{"backup_short", volumeSpace{10, 1}, volumeSpace{19, 2}, 10, 20, "space_insufficient"},
		{"shared_equal", volumeSpace{30, 1}, volumeSpace{30, 1}, 10, 20, ""},
		{"shared_double_count", volumeSpace{20, 1}, volumeSpace{20, 1}, 10, 20, "space_insufficient"},
		{"shared_changed_space", volumeSpace{30, 1}, volumeSpace{29, 1}, 10, 20, "space_insufficient"},
		{"overflow", volumeSpace{math.MaxUint64, 1}, volumeSpace{math.MaxUint64, 1}, math.MaxUint64, 1, "space_insufficient"},
		{"zero_policy", volumeSpace{10, 1}, volumeSpace{10, 2}, 0, 1, "capacity_policy_required"},
	} {
		t.Run(test.name, func(t *testing.T) {
			err := admitVolumeSpace(test.work, test.backup, test.minWork, test.minBackup)
			if test.code != "" {
				requirePreflightCode(t, err, test.code)
			} else if err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestStorageProbeCleanupAndOverlap(t *testing.T) {
	root := t.TempDir()
	if err := probeStorageWrite(t.Context(), root); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(root)
	if err != nil || len(entries) != 0 {
		t.Fatalf("probe left files: entries=%v err=%v", entries, err)
	}
	requirePreflightCode(t, probeStorageWrite(t.Context(), filepath.Join(root, "missing")), "storage_unwritable")
	dir, _ := preflightVolumeFixture(t)
	opts := PreflightOptions{Connection: Connection{DataDir: dir}, WorkRoot: dir, BackupRoot: root}
	_, err = preflightStorageRoots(opts)
	requirePreflightCode(t, err, "storage_overlap")
	opts.WorkRoot = root
	_, err = preflightStorageRoots(opts)
	requirePreflightCode(t, err, "storage_overlap")
}

func TestPreflightCancellationAndSecretSafeErrors(t *testing.T) {
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	dir, identity := preflightVolumeFixture(t)
	err := checkDataVolume(ctx, dir, identity)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("lost cancellation: %v", err)
	}
	secret := "credential-that-must-not-appear"
	path := filepath.Join(t.TempDir(), secret)
	err = checkDataVolume(t.Context(), path, Identity{DataDir: path})
	var failure *PreflightError
	if !errors.As(err, &failure) {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), secret) || strings.Contains(failure.Error(), secret) || failure.Unwrap() != nil {
		t.Fatalf("diagnostic leaked private error detail: %s", encoded)
	}
}

func TestPreflightRealConnectionFailureIsSecretSafe(t *testing.T) {
	secret := "private-connection-value"
	opts := PreflightOptions{
		Connection: Connection{
			Socket:  filepath.Join(t.TempDir(), secret),
			DataDir: t.TempDir(), Username: secret, Password: secret,
		},
		Binary:   "must-not-be-invoked",
		WorkRoot: t.TempDir(), BackupRoot: t.TempDir(),
		MinWorkBytes: 1, MinBackupBytes: 1,
	}
	_, err := Preflight(t.Context(), opts)
	requirePreflightCode(t, err, "identity_query_failed")
	if strings.Contains(err.Error(), secret) {
		t.Fatal("connection failure leaked a secret")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	_, err = Preflight(ctx, opts)
	requirePreflightCode(t, err, "canceled")
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation not preserved: %v", err)
	}
}
