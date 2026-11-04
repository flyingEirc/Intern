package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
)

func cleanConfigEnv(t *testing.T) {
	t.Helper()
	keys := []string{
		"INTERN_CONFIG", "INTERN_HOST", "INTERN_PORT", "INTERN_SOCKET", "INTERN_USERNAME", "INTERN_PASSWORD", "INTERN_PASSWORD_ENV", "INTERN_DB", "INTERN_TLS_MODE", "INTERN_TLS_CA",
		"INTERN_DIRECTORY", "INTERN_STATE_DIRECTORY", "INTERN_PROJECT", "INTERN_COMPRESS", "INTERN_ENCRYPT", "INTERN_KEY_FILE", "INTERN_MAX_RESTORE_BYTES",
		"INTERN_SCHEDULE_FULL_AT", "INTERN_SCHEDULE_INCREMENTAL_EVERY", "INTERN_SCHEDULE_TIMEZONE",
		"INTERN_CONNECTION_PASSWORD", "INTERN_CONNECTION_PASSWORD_ENV",
	}
	for _, key := range keys {
		value, existed := os.LookupEnv(key)
		t.Cleanup(func() {
			if existed {
				os.Setenv(key, value)
			} else {
				os.Unsetenv(key)
			}
		})
		os.Unsetenv(key)
	}
}

func execute(t *testing.T, out io.Writer, args ...string) error {
	t.Helper()
	command := New(out, io.Discard)
	command.SetArgs(args)
	return command.ExecuteContext(t.Context())
}

// TestConfigDirectoryRejected pins that a directory passed as the configuration
// path is a configuration error, not an empty configuration that runs the command.
func TestConfigDirectoryRejected(t *testing.T) {
	for _, useEnv := range []bool{false, true} {
		name := "flag"
		if useEnv {
			name = "env"
		}
		t.Run(name, func(t *testing.T) {
			cleanConfigEnv(t)
			configDir := t.TempDir()
			backupDir := filepath.Join(t.TempDir(), "backups")
			stateDir := filepath.Join(t.TempDir(), "state")
			args := []string{"backup", "list", "--directory", backupDir, "--state-directory", stateDir}
			if useEnv {
				t.Setenv("INTERN_CONFIG", configDir)
			} else {
				args = append(args, "--config", configDir)
			}
			err := execute(t, io.Discard, args...)
			want := fmt.Sprintf("cannot read YAML configuration %q: not a regular file", configDir)
			if err == nil || err.Error() != want {
				t.Fatalf("error = %v, want %q", err, want)
			}
			assertNotCreated(t, backupDir, stateDir)
		})
	}
}

func TestConfigFileLoads(t *testing.T) {
	cleanConfigEnv(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	stateDir := filepath.Join(t.TempDir(), "state")
	path := filepath.Join(t.TempDir(), "backup.yaml")
	contents := fmt.Sprintf("backup:\n  directory: %s\n  state_directory: %s\n  project: yaml\n", backupDir, stateDir)
	if err := os.WriteFile(path, []byte(contents), 0o600); err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	if err := execute(t, &out, "backup", "list", "--config", path); err != nil {
		t.Fatal(err)
	}
	if out.String() != "[]\n" {
		t.Fatalf("output = %q, want %q", out.String(), "[]\n")
	}
	for _, dir := range []string{backupDir, stateDir} {
		if _, err := os.Stat(dir); err != nil {
			t.Fatalf("configured directory %q was not used: %v", dir, err)
		}
	}
}

func TestMissingConfigFileKeepsExistingError(t *testing.T) {
	cleanConfigEnv(t)
	missing := filepath.Join(t.TempDir(), "missing.yaml")
	backupDir := filepath.Join(t.TempDir(), "backups")
	stateDir := filepath.Join(t.TempDir(), "state")
	err := execute(t, io.Discard, "backup", "list", "--config", missing, "--directory", backupDir, "--state-directory", stateDir)
	want := fmt.Sprintf("cannot open YAML configuration %q", missing)
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	assertNotCreated(t, backupDir, stateDir)
}

func assertNotCreated(t *testing.T, paths ...string) {
	t.Helper()
	for _, path := range paths {
		if _, err := os.Lstat(path); !os.IsNotExist(err) {
			t.Fatalf("command ran and created %q: %v", path, err)
		}
	}
}
