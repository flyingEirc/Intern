package cmd

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestDatabaseURLSourcesAndPrivateSecretFile(t *testing.T) {
	path := writeDatabaseURLFile(t, configTestDatabaseURL+"\n")
	requireDatabaseURL(t, configuration{DatabaseURLFile: path}, configTestDatabaseURL)
	requireDatabaseURLRefused(t, configuration{DatabaseURL: configTestDatabaseURL, DatabaseURLFile: path})
	requireDatabaseURLRefused(t, configuration{})
	link := filepath.Join(filepath.Dir(path), "link")
	if err := os.Symlink(path, link); err != nil {
		t.Fatal(err)
	}
	requireDatabaseURLRefused(t, configuration{DatabaseURLFile: link})
	requireInvalidSecretsRejected(t, path)
}

func requireInvalidSecretsRejected(t *testing.T, path string) {
	t.Helper()
	for _, content := range []string{"", "postgres://one\npostgres://two", strings.Repeat("x", 4097)} {
		if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
			t.Fatal(err)
		}
		requireDatabaseURLRefused(t, configuration{DatabaseURLFile: path})
	}
	if err := os.Chmod(path, 0o644); err != nil {
		t.Fatal(err)
	}
	requireDatabaseURLRefused(t, configuration{DatabaseURLFile: path})
}

func writeDatabaseURLFile(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "database_url")
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
	return path
}

func requireDatabaseURL(t *testing.T, c configuration, want string) {
	t.Helper()
	value, err := c.databaseURL()
	if err != nil || value != want {
		t.Fatalf("database URL = %q, %v", value, err)
	}
}

func requireDatabaseURLRefused(t *testing.T, c configuration) {
	t.Helper()
	if _, err := c.databaseURL(); err == nil {
		t.Fatal("invalid database URL source accepted")
	}
}
