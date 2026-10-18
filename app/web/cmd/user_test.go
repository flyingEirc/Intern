package cmd

import (
	"bytes"
	"database/sql"
	"encoding/json/v2"
	"io"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

func executeAdmin(t *testing.T, dir, input string, extra ...string) (string, error) {
	t.Helper()
	var output bytes.Buffer
	command := New(&output, io.Discard)
	command.SetIn(strings.NewReader(input))
	args := append([]string{"user", "create-admin", "--metadata-dir", dir}, extra...)
	command.SetArgs(args)
	err := command.ExecuteContext(t.Context())
	return output.String(), err
}

func TestCreateAdminWhileServeOwnsDatabase(t *testing.T) {
	cleanConfigEnv(t)
	dir := filepath.Join(t.TempDir(), "metadata")
	database, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	output, err := executeAdmin(t, dir, "command-private-password\r\nignored-second-line", "--username", "Admin", "--password-stdin")
	if err != nil {
		t.Fatal(err)
	}
	var created struct {
		ID string `json:"id"`
	}
	if err = json.Unmarshal([]byte(output), &created); err != nil || created.ID == "" {
		t.Fatal("created user ID missing")
	}
	assertCreatedAdmin(t, database, created.ID)
	if _, err = executeAdmin(t, dir, "command-private-password\n", "--username", "ADMIN", "--password-stdin"); err == nil {
		t.Fatal("duplicate username accepted")
	}
	assertSystemAudit(t, database)
	if strings.Contains(output, "command-private-password") || strings.Contains(output, "$argon2id$") {
		t.Fatal("command output leaked secret")
	}
}

func assertCreatedAdmin(t *testing.T, database *store.Store, id string) {
	t.Helper()
	user, err := database.User(t.Context(), id)
	if err != nil || user.Role != "admin" {
		t.Fatalf("admin not persisted: %v", err)
	}
	passwords, err := auth.NewPasswords(auth.PasswordParams{Memory: 64, Time: 1, Parallelism: 1})
	if err != nil {
		t.Fatal(err)
	}
	valid, err := passwords.Verify("command-private-password", user.PasswordHash)
	if err != nil || !valid {
		t.Fatal("stdin password line was not read correctly")
	}
}

func assertSystemAudit(t *testing.T, database *store.Store) {
	t.Helper()
	entries, err := database.AuditLogs(t.Context(), 0, 10)
	if err != nil || len(entries) != 1 || entries[0].ActorType != "system" || entries[0].Result != "success" {
		t.Fatalf("create-admin audit: %v", err)
	}
}

func TestCreateAdminRejectsUnsafeInputsAndOldSchema(t *testing.T) {
	cleanConfigEnv(t)
	dir := filepath.Join(t.TempDir(), "metadata")
	database, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	defer database.Close()
	for _, test := range []struct {
		name, input, message string
		flags                []string
	}{
		{"non terminal", "command-private-password", "--password-stdin", []string{"--username", "admin"}},
		{"short", "short\n", "12–256", []string{"--username", "admin", "--password-stdin"}},
		{"username", "command-private-password\n", "username", []string{"--username", "ab", "--password-stdin"}},
		{"plaintext flag", "", "unknown flag", []string{"--username", "admin", "--password", "command-private-password"}},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, commandErr := executeAdmin(t, dir, test.input, test.flags...); commandErr == nil || !strings.Contains(commandErr.Error(), test.message) {
				t.Fatalf("rejection=%v", commandErr)
			}
		})
	}
	db, err := sql.Open("sqlite", filepath.Join(dir, "intern.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if _, err = db.ExecContext(t.Context(), "DELETE FROM web_goose_db_version WHERE version_id = 3"); err != nil {
		t.Fatal(err)
	}
	if _, err = executeAdmin(t, dir, "command-private-password\n", "--username", "admin", "--password-stdin"); err == nil || !strings.Contains(err.Error(), "run serve or migrate up") {
		t.Fatalf("old schema accepted: %v", err)
	}
}

func TestSessionConfigurationPrecedenceAndDefaults(t *testing.T) {
	cleanConfigEnv(t)
	config, err := loadConfig(t)
	if err != nil || config.Session != auth.DefaultSessionConfig() {
		t.Fatalf("session defaults=%+v, %v", config.Session, err)
	}
	t.Setenv("INTERN_SESSION_IDLE_TIMEOUT", "10m")
	t.Setenv("INTERN_SESSION_LIFETIME", "2h")
	config, err = loadConfig(t)
	if err != nil || config.Session.IdleTimeout != 10*time.Minute || config.Session.Lifetime != 2*time.Hour {
		t.Fatalf("session environment=%+v, %v", config.Session, err)
	}
	config, err = loadConfig(t, "--session-idle-timeout", "20m", "--session-lifetime", "3h")
	if err != nil || config.Session.IdleTimeout != 20*time.Minute || config.Session.Lifetime != 3*time.Hour {
		t.Fatalf("session flags=%+v, %v", config.Session, err)
	}
}
