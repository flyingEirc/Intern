//go:build integration

package integration

import (
	"testing"

	"github.com/flyingEirc/Intern/app/cli/internal/testenv"
)

func TestMySQL(t *testing.T) {
	suite := testenv.NewSuite(t)
	cases := []struct {
		name string
		run  func(*testenv.Case)
	}{
		{"full_objects", fullObjects},
		{"empty_database", emptyDatabase},
		{"codec_matrix", codecMatrix},
		{"checkpoint_chain", checkpointChain},
		{"recovery_guards", recoveryGuards},
		{"source_discontinuity", sourceDiscontinuity},
		{"permissions_tls", permissionsTLS},
		{"interruptions", interruptions},
		{"storage_failures", storageFailures},
		{"version_refusal", versionRefusal},
		{"scope_rejections", scopeRejections},
		{"scheduler", scheduler},
		{"lifecycle", lifecycle},
		{"configuration", configuration},
	}
	for _, scenario := range cases {
		t.Run(scenario.name, func(t *testing.T) { t.Parallel(); c := suite.NewCase(t, scenario.name); scenario.run(c); c.Verified() })
	}
}

func seedBasic(c *testenv.Case) {
	c.SQL(c.Source, "CREATE DATABASE app CHARACTER SET utf8mb4 COLLATE utf8mb4_bin")
	c.SQL(c.Source, "CREATE TABLE app.records(id INT PRIMARY KEY AUTO_INCREMENT,value VARCHAR(120) NOT NULL,UNIQUE KEY value_unique(value)) ENGINE=InnoDB")
	c.SQL(c.Source, "INSERT INTO app.records VALUES(1,'baseline'),(2,'old'),(3,'delete')")
}

func rows(c *testenv.Case, db *testenv.Database, schema string) string {
	c.T.Helper()
	// Schema names below are fixed fixture identifiers, never values supplied to the product.
	query := "SELECT COALESCE(GROUP_CONCAT(CONCAT(id,':',value) ORDER BY id SEPARATOR '|'),'') FROM `" + schema + "`.records"
	return c.Scalar(db, query)
}

func fullObjects(c *testenv.Case) {
	seedBasic(c)
	c.Stage("schema-fixture")
	c.SQL(c.Source, "CREATE TABLE app.typed(id INT PRIMARY KEY,amount DECIMAL(20,6),nullable TEXT NULL,payload VARBINARY(32),event_time DATETIME(6),utf8_text VARCHAR(64)) ENGINE=InnoDB")
	c.SQL(c.Source, "INSERT INTO app.typed VALUES(1,123456789.123456,NULL,X'0001FEFF','2026-09-19 12:34:56.123456','备份Ωé'),(2,-0.000001,'',X'','2026-09-19 00:00:00.000001','')")
	c.SQL(c.Source, "CREATE TABLE app.child(id INT PRIMARY KEY,record_id INT,CONSTRAINT child_record FOREIGN KEY(record_id) REFERENCES app.records(id)) ENGINE=InnoDB")
	c.SQL(c.Source, "INSERT INTO app.child VALUES(1,1)")
	c.SQL(c.Source, "CREATE DEFINER='app_owner'@'%' VIEW app.records_view AS SELECT id,value FROM app.records")
	c.SQL(c.Source, "CREATE DEFINER='app_owner'@'%' PROCEDURE app.record_count() SELECT COUNT(*) AS n FROM app.records")
	c.SQL(c.Source, "CREATE DEFINER='app_owner'@'%' TRIGGER app.upper_value BEFORE INSERT ON app.records FOR EACH ROW SET NEW.value=UPPER(NEW.value)")
	c.SQL(c.Source, "CREATE DEFINER='app_owner'@'%' EVENT app.daily_event ON SCHEDULE EVERY 1 DAY DISABLE DO UPDATE app.records SET value=value WHERE id=1")
	c.Stage("logical-full")
	full := c.Backup("full")
	// With binary logging enabled and log_bin_trust_function_creators=OFF,
	// MySQL requires SUPER for trigger creation. Do not weaken that server policy.
	c.Refuse("stored-object-role-missing-super", "restore", "--config", "/run/secrets/target.yaml", "--backup", full.ID, "--db", "app", "--execute")
	freshTarget, err := c.NewDatabase("object-target", c.Suite.Profile, true)
	c.Must(err)
	c.PointTarget(freshTarget)
	c.SQL(c.Target, "GRANT SUPER ON *.* TO 'intern_restore'@'%'")
	c.Restore(full.ID, true, "--db", "app")
	assertFullObjects(c)
}

func assertFullObjects(c *testenv.Case) {
	c.Assert("baseline-rows", rows(c, c.Target, "app") == "1:baseline|2:old|3:delete")
	c.Assert("typed-values", c.Scalar(c.Target, "SELECT CONCAT(CAST(amount AS CHAR),'|',IFNULL(nullable,'NULL'),'|',HEX(payload),'|',DATE_FORMAT(event_time,'%Y-%m-%d %H:%i:%s.%f'),'|',utf8_text) FROM app.typed WHERE id=1") == "123456789.123456|NULL|0001FEFF|2026-09-19 12:34:56.123456|备份Ωé")
	c.Assert("negative-decimal-empty", c.Scalar(c.Target, "SELECT CONCAT(CAST(amount AS CHAR),'|',LENGTH(nullable),'|',LENGTH(payload)) FROM app.typed WHERE id=2") == "-0.000001|0|0")
	c.Assert("view-behavior", c.Scalar(c.Target, "SELECT value FROM app.records_view WHERE id=1") == "baseline")
	c.Assert("procedure-behavior", c.Scalar(c.Target, "CALL app.record_count()") == "3")
	c.Assert("disabled-event", c.Scalar(c.Target, "SELECT STATUS FROM information_schema.EVENTS WHERE EVENT_SCHEMA='app' AND EVENT_NAME='daily_event'") == "DISABLED")
	c.Assert("database-collation", c.Scalar(c.Target, "SELECT DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='app'") == "utf8mb4_bin")
	c.SQL(c.Target, "INSERT INTO app.records(value) VALUES('mixedCase')")
	c.Assert("trigger-and-auto-increment", c.Scalar(c.Target, "SELECT value FROM app.records WHERE id=4") == "MIXEDCASE")
	_, err := c.Target.SQL.ExecContext(c.Ctx, "INSERT INTO app.child VALUES(2,999)")
	c.Assert("foreign-key-enforced", err != nil)
	c.Assert("source-unchanged", rows(c, c.Source, "app") == "1:baseline|2:old|3:delete")
}

func emptyDatabase(c *testenv.Case) {
	c.SQL(c.Source, "CREATE DATABASE app")
	full := c.Backup("full")
	plan := c.Restore(full.ID, false)
	c.Assert("plan-only-no-schema", !plan.Executed && c.Scalar(c.Target, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='restored'") == "0")
	c.Restore(full.ID, true)
	c.Assert("empty-restored", c.Scalar(c.Target, "SELECT COUNT(*) FROM information_schema.SCHEMATA WHERE SCHEMA_NAME='restored'") == "1" && c.Scalar(c.Target, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA='restored'") == "0")
}
