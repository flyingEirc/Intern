//go:build integration

package integration

import (
	"fmt"
	"strings"

	"github.com/flyingEirc/IotProject/app/cli/internal/testenv"
)

func codecMatrix(c *testenv.Case) {
	seedBasic(c)
	variants := []struct {
		name              string
		compress, encrypt bool
	}{{"plain", false, false}, {"encrypted", false, true}, {"compressed", true, false}, {"both", true, true}}
	for index, variant := range variants {
		c.Stage("codec-" + variant.name)
		if index > 0 {
			target, err := c.NewDatabase(fmt.Sprintf("target-%d", index), c.Suite.Profile, true)
			c.Must(err)
			c.PointTarget(target)
		}
		full := c.Backup("full", fmt.Sprintf("--compress=%t", variant.compress), fmt.Sprintf("--encrypt=%t", variant.encrypt))
		c.Restore(full.ID, true)
		c.Assert("codec-"+variant.name, rows(c, c.Target, "restored") == "1:baseline|2:old|3:delete")
	}
}
func checkpointChain(c *testenv.Case) {
	seedBasic(c)
	full := c.Backup("full")
	c.SQL(c.Source, "USE app; INSERT INTO records VALUES(4,'first'); UPDATE records SET value='updated' WHERE id=2; DELETE FROM records WHERE id=3")
	first := c.Backup("incremental", "--compress=false", "--encrypt=false")
	c.SQL(c.Source, "USE app; FLUSH BINARY LOGS; INSERT INTO records VALUES(5,'second'); UPDATE records SET value='final' WHERE id=1")
	second := c.Backup("incremental")
	c.Assert("parent-chain", first.Parent == full.ID && second.Parent == first.ID)
	c.SQL(c.Source, "USE app; DELETE FROM records WHERE id=1; INSERT INTO records VALUES(6,'unwanted')")
	c.Assert("source-past-checkpoint", rows(c, c.Source, "app") == "2:updated|4:first|5:second|6:unwanted")
	c.StopDatabase(c.Source)
	plan := c.Restore(second.ID, false)
	c.Assert("plan-does-not-write", !plan.Executed && c.Scalar(c.Target, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='restored'") == "0")
	c.Restore(second.ID, true)
	c.Assert("restored-S2-not-S3", rows(c, c.Target, "restored") == "1:final|2:updated|4:first|5:second")
	earlier, err := c.NewDatabase("earlier", c.Suite.Profile, true)
	c.Must(err)
	c.PointTarget(earlier)
	c.Restore(first.ID, true)
	c.Assert("restored-S1", rows(c, c.Target, "restored") == "1:baseline|2:updated|4:first")
}
func recoveryGuards(c *testenv.Case) {
	seedBasic(c)
	full := c.Backup("full")
	c.SQL(c.Source, "USE app; INSERT INTO records VALUES(4,'first')")
	increment := c.Backup("incremental")
	refuseRestore := func(id, name string, flags ...string) {
		args := make([]string, 0, 8+len(flags))
		args = append(args, "restore", "--config", "/run/secrets/target.yaml", "--backup", id, "--execute", "--db", name)
		c.Refuse("reject-"+name, append(args, flags...)...)
		c.Assert("unchanged-"+name, c.Scalar(c.Target, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME=?", name) == "0")
	}
	c.OK("keygen", "--key-file", "/run/secrets/wrong-key")
	refuseRestore(full.ID, "wrong_key", "--key-file", "/run/secrets/wrong-key")
	refuseRestore(full.ID, "oversized", "--max-restore-bytes", "100")
	path := "/work/backups/" + full.ID + "/payload.bin"
	original := c.Read(path)
	c.Copy(path, original[:len(original)-1])
	refuseRestore(full.ID, "tampered")
	c.Copy(path, original)
	original[len(original)/2] ^= 1
	c.Copy(path, original)
	refuseRestore(full.ID, "tampered")
	original[len(original)/2] ^= 1
	c.Copy(path, original)
	otherPayload := c.Read("/work/backups/" + increment.ID + "/payload.bin")
	c.Copy(path, otherPayload)
	refuseRestore(full.ID, "tampered")
	c.Copy(path, original)
	parent := "/work/backups/" + full.ID
	c.Root("mv", parent, parent+"-held")
	refuseRestore(increment.ID, "missing")
	c.Root("mv", parent+"-held", parent)
	c.Root("mv", parent, "/work/held-artifact")
	c.Root("ln", "-s", "/work/held-artifact", parent)
	refuseRestore(full.ID, "escape")
	c.Root("rm", parent)
	c.Root("mv", "/work/held-artifact", parent)
	c.TargetConfiguration(c.Source, "/run/secrets/source-as-target.yaml")
	c.Refuse("source-uuid", "restore", "--config", "/run/secrets/source-as-target.yaml", "--backup", full.ID, "--execute")
	c.Assert("source-empty-target-preserved", c.Scalar(c.Source, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='restored'") == "0")
	c.Assert("source-guard-no-damage", rows(c, c.Source, "app") == "1:baseline|2:old|3:delete|4:first")
	c.SQL(c.Target, "CREATE DATABASE protected; CREATE TABLE protected.sentinel(id INT PRIMARY KEY); INSERT INTO protected.sentinel VALUES(7)")
	c.Refuse("nonempty-target", "restore", "--config", "/run/secrets/target.yaml", "--backup", full.ID, "--db", "protected", "--execute")
	c.Assert("nonempty-preserved", c.Scalar(c.Target, "SELECT id FROM protected.sentinel") == "7")
	c.Restore(increment.ID, true)
	refuseRestore(increment.ID, "duplicate")
}
func sourceDiscontinuity(c *testenv.Case) {
	seedBasic(c)
	full := c.Backup("full")
	c.SQL(c.Source, "FLUSH BINARY LOGS")
	current := c.LastLog(c.Source)
	// The server-produced log name is validated before it is used in this administrative statement.
	if !strings.HasPrefix(current, "binlog.") {
		c.T.Fatal("unexpected binlog fixture name")
	}
	c.SQL(c.Source, "PURGE BINARY LOGS TO ?", current)
	c.Refuse("purged-source", "backup", "incremental", "--config", "/run/secrets/source.yaml")
	c.Assert("purge-not-published", len(c.List()) == 1 && c.List()[0].ID == full.ID)
	fresh := c.Backup("full")
	c.SQL(c.Source, "RESET BINARY LOGS AND GTIDS")
	c.Refuse("reset-gtid", "backup", "incremental", "--config", "/run/secrets/source.yaml")
	c.Assert("reset-not-published", len(c.List()) == 2 && c.List()[0].ID == fresh.ID)
	replacement, err := c.NewDatabase("replacement", c.Suite.Profile, true)
	c.Must(err)
	c.PointSource(replacement)
	seedBasic(c)
	c.Refuse("new-source-identity", "backup", "incremental", "--config", "/run/secrets/source.yaml")
	c.Assert("identity-not-joined", len(c.List()) == 2)
}
