//go:build integration

package integration

import (
	"github.com/flyingEirc/IotProject/app/cli/internal/testenv"
)

func permissionsTLS(c *testenv.Case) {
	seedBasic(c)
	full := c.Backup("full")
	c.Refuse("wrong-tls-hostname", "backup", "full", "--config", "/run/secrets/source.yaml", "--host", "wrong-source")
	c.CopyUntrustedCA("/run/secrets/invalid-ca.pem")
	c.Refuse("wrong-tls-ca", "backup", "full", "--config", "/run/secrets/source.yaml", "--tls-ca", "/run/secrets/invalid-ca.pem")
	c.Refuse("tls-required", "backup", "full", "--config", "/run/secrets/source.yaml", "--tls-mode", "DISABLED")
	c.SQL(c.Source, "REVOKE RELOAD ON *.* FROM 'intern_backup'@'%'")
	c.Refuse("missing-snapshot-privilege", "backup", "full", "--config", "/run/secrets/source.yaml")
	c.SQL(c.Source, "GRANT RELOAD ON *.* TO 'intern_backup'@'%'")
	c.SQL(c.Source, "REVOKE REPLICATION SLAVE ON *.* FROM 'intern_backup'@'%'")
	c.Refuse("missing-log-privilege", "backup", "incremental", "--config", "/run/secrets/source.yaml")
	c.Assert("permission-errors-not-published", len(c.List()) == 1 && c.List()[0].ID == full.ID)
	c.Restore(full.ID, true)
	c.Assert("scoped-restore-role", rows(c, c.Target, "restored") == "1:baseline|2:old|3:delete")
}
func interruptions(c *testenv.Case) {
	seedBasic(c)
	c.Backup("full")
	c.Stage("database-stop")
	c.StopDatabase(c.Source)
	c.Refuse("stopped-database", "backup", "incremental", "--config", "/run/secrets/source.yaml")
	c.Assert("stop-not-published", len(c.List()) == 1)
	c.RestartDatabase(c.Source)
	c.Backup("full")
	c.Stage("network-disconnect")
	c.Disconnect(c.Source)
	c.Refuse("disconnected-database", "backup", "full", "--config", "/run/secrets/source.yaml")
	c.Assert("network-not-published", len(c.List()) == 2)
	c.Reconnect(c.Source)
	interruptedDump(c, "TERM")
	interruptedDump(c, "KILL")
}
func interruptedDump(c *testenv.Case, signal string) {
	c.Stage("blocked-dump-" + signal)
	before := len(c.List())
	release := c.HoldWrites(c.Source)
	pid := c.StartCLI("backup full --config /run/secrets/source.yaml")
	c.WaitForDump()
	c.Signal(pid, signal)
	c.AwaitToolsStopped()
	release()
	c.Assert("interrupted-dump-not-published-"+signal, len(c.List()) == before)
	c.Root("/bin/sh", "-c", "for path in /work/state/work-* /work/backups/.pending-*; do test ! -e \"$path\" || exit 1; done")
	c.Backup("full")
	c.Assert("restart-after-"+signal, len(c.List()) == before+1)
}
func storageFailures(c *testenv.Case) {
	seedBasic(c)
	c.SQL(c.Source, "CREATE TABLE app.large_payload(id INT PRIMARY KEY,payload LONGBLOB) ENGINE=InnoDB; INSERT INTO app.large_payload VALUES(1,REPEAT('x',2097152))")
	c.Refuse("disk-full", "backup", "full", "--config", "/run/secrets/source.yaml", "--directory", "/limited/backups", "--compress=false", "--encrypt=false")
	c.Assert("disk-full-not-published", len(c.List("--directory", "/limited/backups")) == 0)
	c.Root("chmod", "0500", "/work/backups")
	c.Refuse("unwritable-backup-directory", "backup", "full", "--config", "/run/secrets/source.yaml")
	c.Root("chmod", "0700", "/work/backups")
	c.Assert("permission-failure-not-published", len(c.List()) == 0)
	c.Backup("full")
	c.Assert("storage-recovered", len(c.List()) == 1)
	c.Root("/bin/sh", "-c", "for path in /work/state/work-* /work/backups/.pending-*; do test ! -e \"$path\" || exit 1; done")
}
func versionRefusal(c *testenv.Case) {
	seedBasic(c)
	var alternate string
	for _, profile := range c.Suite.Contract.Profiles {
		if profile.ID != c.Suite.Profile.ID {
			alternate = profile.ID
		}
	}
	profile, err := c.Suite.Contract.Profile(alternate)
	c.Must(err)
	other, err := c.NewDatabase("other-version", profile, true)
	c.Must(err)
	c.PointSource(other)
	seedBasic(c)
	c.Refuse("real-client-server-version-mismatch", "backup", "full", "--config", "/run/secrets/source.yaml")
	c.Assert("wrong-version-not-published", len(c.List()) == 0)
}
func scopeRejections(c *testenv.Case) {
	seedBasic(c)
	full := c.Backup("full")
	c.SQL(c.Source, "USE fixture; ALTER TABLE app.records ADD COLUMN marker INT NOT NULL DEFAULT 17")
	c.Refuse("unsafe-default-schema-ddl", "backup", "incremental", "--config", "/run/secrets/source.yaml")
	c.Assert("unsafe-ddl-not-published", len(c.List()) == 1 && c.List()[0].ID == full.ID)
	baseline := c.Backup("full")
	c.Restore(baseline.ID, true)
	c.Assert("new-baseline-contains-ddl", c.Scalar(c.Target, "SELECT marker FROM restored.records WHERE id=1") == "17")
	c.SQL(c.Source, "CREATE TABLE app.volatile(id INT PRIMARY KEY) ENGINE=MEMORY")
	c.Refuse("nontransactional-table", "backup", "full", "--config", "/run/secrets/source.yaml")
	c.Assert("unsupported-engine-not-published", len(c.List()) == 2)
	c.SQL(c.Source, "DROP TABLE app.volatile")
	fresh := c.Backup("full")
	c.SQL(c.Source, "USE app; INSERT INTO records VALUES(4,'later',17)")
	increment := c.Backup("incremental")
	off, err := c.NewDatabase("gtid-off", c.Suite.Profile, false)
	c.Must(err)
	c.PointTarget(off)
	c.Refuse("gtid-mode-mismatch", "restore", "--config", "/run/secrets/target.yaml", "--backup", increment.ID, "--execute")
	c.Assert("mode-mismatch-no-target", c.Scalar(c.Target, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='restored'") == "0")
	c.Assert("valid-chain-precondition", increment.Parent == fresh.ID)
}
