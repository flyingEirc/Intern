package cmd

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// cleanConfigEnv isolates a test from the machine that runs it. The loader reads
// every INTERN_ variable and counts an empty one as set, so all of them are
// removed instead of a list that can drift from the configuration keys. The user
// directories that supply the default backup and state paths move into a
// temporary home, so no default can reach the real one.
func cleanConfigEnv(t *testing.T) {
	t.Helper()
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if !strings.HasPrefix(key, "INTERN_") {
			continue
		}
		t.Setenv(key, "") // Registers the restore of the original value.
		if err := os.Unsetenv(key); err != nil {
			t.Fatal(err)
		}
	}
	home := t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	t.Setenv("XDG_CONFIG_HOME", filepath.Join(home, ".config"))
	t.Setenv("AppData", filepath.Join(home, "AppData"))
}

// TestCleanConfigEnvIsolatesTheMachine pins what every configuration test relies
// on: no ambient INTERN_ variable reaches the loader, and the default directories
// resolve inside a temporary home.
func TestCleanConfigEnvIsolatesTheMachine(t *testing.T) {
	// Variables the loader really reads; the empty one counts as set as well.
	t.Setenv("INTERN_BACKUP_DIRECTORY", "/ambient/backups")
	t.Setenv("INTERN_CONNECTION_USERNAME", "ambient")
	t.Setenv("INTERN_BACKUP_PROJECT", "")
	realHome, _ := os.UserHomeDir() // An unset HOME is a case the sandbox repairs, not a test error.
	cleanConfigEnv(t)
	for _, entry := range os.Environ() {
		if strings.HasPrefix(entry, "INTERN_") {
			t.Errorf("ambient variable survived: %s", entry)
		}
	}
	home, err := os.UserHomeDir()
	if err != nil {
		t.Fatal(err)
	}
	configDir, err := os.UserConfigDir()
	if err != nil {
		t.Fatal(err)
	}
	if home == realHome || !strings.HasPrefix(configDir, home) {
		t.Errorf("home = %q, config directory = %q; want a temporary home that is not %q", home, configDir, realHome)
	}
}

func execute(t *testing.T, out io.Writer, args ...string) error {
	t.Helper()
	command := New(out, io.Discard)
	command.SetArgs(args)
	return command.ExecuteContext(t.Context())
}

// configDeadline bounds a command that must fail before it reads the configuration.
const configDeadline = 5 * time.Second

// executeBounded fails the test instead of hanging when the command blocks, as
// opening a FIFO configuration in blocking mode does.
func executeBounded(t *testing.T, args ...string) error {
	t.Helper()
	errCh := make(chan error, 1)
	go func() { errCh <- execute(t, io.Discard, args...) }()
	select {
	case err := <-errCh:
		return err
	case <-time.After(configDeadline):
		t.Fatalf("command still blocked after %v", configDeadline)
		return nil
	}
}

// TestConfigNonRegularRejected pins that a configuration path that is not a
// regular file is a configuration error, not an empty configuration that runs the
// command. The FIFO has no writer, so opening it in blocking mode never returns.
func TestConfigNonRegularRejected(t *testing.T) {
	kinds := []struct {
		name string
		path func(*testing.T) string
	}{
		{"directory", func(t *testing.T) string { return t.TempDir() }},
		{"fifo", configFIFO},
	}
	for _, kind := range kinds {
		for _, useEnv := range []bool{false, true} {
			source := "flag"
			if useEnv {
				source = "env"
			}
			t.Run(kind.name+" "+source, func(t *testing.T) {
				assertNonRegularRejected(t, kind.path(t), useEnv)
			})
		}
	}
}

// assertNonRegularRejected selects configPath by flag or by INTERN_CONFIG and
// expects the not-a-regular-file error before the command touches its directories.
func assertNonRegularRejected(t *testing.T, configPath string, useEnv bool) {
	t.Helper()
	cleanConfigEnv(t)
	backupDir := filepath.Join(t.TempDir(), "backups")
	stateDir := filepath.Join(t.TempDir(), "state")
	args := []string{"backup", "list", "--directory", backupDir, "--state-directory", stateDir}
	if useEnv {
		t.Setenv("INTERN_CONFIG", configPath)
	} else {
		args = append(args, "--config", configPath)
	}
	err := executeBounded(t, args...)
	want := fmt.Sprintf("cannot read YAML configuration %q: not a regular file", configPath)
	if err == nil || err.Error() != want {
		t.Fatalf("error = %v, want %q", err, want)
	}
	assertNotCreated(t, backupDir, stateDir)
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
