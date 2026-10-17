//go:build integration

package testenv

import (
	"encoding/json/v2"

	tcexec "github.com/testcontainers/testcontainers-go/exec"
)

func (c *Case) ConfigurationChecks() {
	c.T.Helper()
	original := c.Read("/run/secrets/source.yaml")
	config := c.configuration(c.Source, false)
	config.Connection.Password = "intentionally-wrong"
	data, err := json.Marshal(config)
	c.Must(err)
	c.Copy("/run/secrets/source.yaml", data)
	environment := []string{"INTERN_CONNECTION_PASSWORD_ENV=GATE_BACKUP_PASSWORD", "GATE_BACKUP_PASSWORD=" + c.backupPassword, "INTERN_BACKUP_ENCRYPT=false", "INTERN_BACKUP_COMPRESS=false"}
	c.CLIWithEnvironment("env-secret-overrides-yaml", environment, "backup", "full", "--config", "/run/secrets/source.yaml")
	c.Copy("/run/secrets/source.yaml", original)
	c.CLIWithEnvironment("flag-overrides-env", []string{"INTERN_CONNECTION_USERNAME=absent_user"}, "backup", "full", "--config", "/run/secrets/source.yaml", "--username", "intern_backup", "--compress=false", "--encrypt=false")
	c.Root("chmod", "0644", "/run/secrets/source.yaml")
	c.Refuse("public-password-config", "backup", "full", "--config", "/run/secrets/source.yaml")
	c.Root("chmod", "0600", "/run/secrets/source.yaml")
}
func (c *Case) CLIWithEnvironment(id string, environment []string, args ...string) {
	c.T.Helper()
	argv := append([]string{"timeout", "--signal=TERM", "--kill-after=5s", "90s", "/usr/local/bin/intern"}, args...)
	output, code, err := execContainer(c.Ctx, c.Runner, "10001", argv, tcexec.WithEnv(environment))
	c.Must(c.SafeError(err))
	if code != 0 {
		c.T.Fatalf("%s: %s", id, c.Redact(output))
	}
	c.Assert(id, code == 0)
}
