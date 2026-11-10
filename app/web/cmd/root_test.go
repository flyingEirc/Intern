package cmd

import (
	"bytes"
	"encoding/json/v2"
	"io"
	"os"
	"path/filepath"
	"testing"

	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/pgtest"
)

func executeMigration(t *testing.T, dir, subcommand string) store.Status {
	t.Helper()
	var output bytes.Buffer
	command := New(&output, io.Discard)
	command.SetArgs([]string{"migrate", subcommand, "--metadata-dir", dir})
	if err := command.ExecuteContext(t.Context()); err != nil {
		t.Fatal(err)
	}
	var status store.Status
	if err := json.Unmarshal(output.Bytes(), &status); err != nil {
		t.Fatal(err)
	}
	return status
}

func TestMigrateStatusBeforeInitialization(t *testing.T) {
	cleanConfigEnv(t)
	t.Setenv("INTERN_DATABASE_URL", pgtest.New(t))
	dir := filepath.Join(t.TempDir(), "metadata")
	latest := latestWebVersion(t)
	before := executeMigration(t, dir, "status")
	if before.CurrentVersion != 0 || before.LatestVersion != latest ||
		before.PendingMigrations != int(latest) || before.InstanceID != "" {
		t.Fatalf("initial status: %+v", before)
	}
	if _, err := os.Stat(dir); !os.IsNotExist(err) {
		t.Fatalf("status created metadata directory: %v", err)
	}
}

func TestMigrateCommands(t *testing.T) {
	cleanConfigEnv(t)
	t.Setenv("INTERN_DATABASE_URL", pgtest.New(t))
	dir := filepath.Join(t.TempDir(), "metadata")
	latest := latestWebVersion(t)
	after := executeMigration(t, dir, "up")
	if after.CurrentVersion != latest || after.PendingMigrations != 0 || after.InstanceID == "" {
		t.Fatalf("migrated status: %+v", after)
	}
	repeated := executeMigration(t, dir, "up")
	if after != repeated {
		t.Fatalf("repeated migration changed status: %+v != %+v", after, repeated)
	}
	database, err := store.Open(t.Context(), os.Getenv("INTERN_DATABASE_URL"))
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	if active := executeMigration(t, dir, "status"); active != after {
		t.Fatalf("active database status: %+v", active)
	}
	command := New(io.Discard, io.Discard)
	command.SetArgs([]string{"migrate", "up", "--metadata-dir", dir})
	if err = command.ExecuteContext(t.Context()); err == nil {
		t.Fatal("migrate up bypassed ownership lock")
	}
}

// latestWebVersion is the newest embedded web migration version, so a new
// migration does not leave these expectations pinned to a stale number.
func latestWebVersion(t *testing.T) int64 {
	t.Helper()
	latest, err := store.LatestVersion()
	if err != nil {
		t.Fatal(err)
	}
	return latest
}
