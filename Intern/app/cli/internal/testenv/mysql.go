//go:build integration

package testenv

import (
	"bytes"
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"strings"
	"time"

	driver "github.com/go-sql-driver/mysql"
	tc "github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/mysql"
	"github.com/testcontainers/testcontainers-go/network"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/flyingEirc/IotProject/app/cli/internal/qualitygate"
)

type Database struct {
	Container            *mysql.MySQLContainer
	SQL                  *sql.DB
	Alias, UUID, Version string
	Profile              qualitygate.Profile
}

func (c *Case) NewDatabase(alias string, profile qualitygate.Profile, gtid bool) (*Database, error) {
	cert, key, err := c.CA.server(alias)
	if err != nil {
		return nil, err
	}
	config := c.databaseConfig(gtid)
	files := []tc.ContainerFile{{Reader: bytes.NewReader(c.CA.PEM), ContainerFilePath: "/tmp/intern-ca.pem", FileMode: 0600}, {Reader: bytes.NewReader(cert), ContainerFilePath: "/tmp/intern-cert.pem", FileMode: 0600}, {Reader: bytes.NewReader(key), ContainerFilePath: "/tmp/intern-key.pem", FileMode: 0600}, {Reader: strings.NewReader(config), ContainerFilePath: "/etc/mysql/conf.d/intern.cnf", FileMode: 0644}}
	volume := c.prefix + "-" + alias + "-data"
	ctr, err := mysql.Run(c.Ctx, profile.Image, mysql.WithUsername("root"), mysql.WithPassword(c.rootPassword), mysql.WithDatabase("fixture"), tc.WithEnv(map[string]string{"MYSQL_ROOT_HOST": "%"}), tc.WithFiles(files...), network.WithNetwork([]string{alias, "wrong-" + alias}, c.Network), tc.WithImagePlatform(c.Suite.Candidate.Platform), tc.WithMounts(tc.VolumeMount(volume, "/var/lib/mysql")), tc.WithEntrypoint("/bin/sh", "-c", "chown mysql:mysql /tmp/intern-ca.pem /tmp/intern-cert.pem /tmp/intern-key.pem; exec /usr/local/bin/docker-entrypoint.sh mysqld"), tc.WithCmd(), tc.WithHostConfigModifier(boundedHost), tc.WithWaitStrategy(wait.ForLog("port: 3306  MySQL Community Server").WithStartupTimeout(90*time.Second)))
	if ctr != nil {
		c.ownContainer(ctr.Container, "database-"+alias, volume)
	}
	if err != nil {
		return nil, c.SafeError(err)
	}
	db := &Database{Container: ctr, Alias: alias, Profile: profile}
	if err = c.connectDatabase(db); err != nil {
		return nil, err
	}
	if err = c.waitDatabase(db); err != nil {
		return nil, err
	}
	if err = c.provisionUsers(db); err != nil {
		return nil, err
	}
	if _, err = db.SQL.ExecContext(c.Ctx, "UPDATE performance_schema.setup_consumers SET ENABLED='YES' WHERE NAME='events_statements_history_long'"); err != nil {
		return nil, c.SafeError(err)
	}
	if err = c.recordDatabase(db, gtid); err != nil {
		return nil, err
	}
	return db, nil
}
func (c *Case) databaseConfig(gtid bool) string {
	mode := "ON"
	if !gtid {
		mode = "OFF"
	}
	return fmt.Sprintf("[mysqld]\nserver-id=%d\nlog-bin=binlog\nbinlog_format=ROW\ngtid_mode=%s\nenforce_gtid_consistency=%s\nssl-ca=/tmp/intern-ca.pem\nssl-cert=/tmp/intern-cert.pem\nssl-key=/tmp/intern-key.pem\nrequire_secure_transport=ON\nevent_scheduler=OFF\ninnodb_buffer_pool_size=67108864\n", 100+len(c.Receipt.Instances), mode, mode)
}
func (c *Case) connectDatabase(db *Database) error {
	host, err := db.Container.Host(c.Ctx)
	if err != nil {
		return err
	}
	port, err := db.Container.MappedPort(c.Ctx, "3306/tcp")
	if err != nil {
		return err
	}
	config := driver.NewConfig()
	config.Net = "tcp"
	config.Addr = net.JoinHostPort(host, port.Port())
	config.User = "root"
	config.Passwd = c.rootPassword
	config.DBName = "fixture"
	config.TLS = c.CA.client(db.Alias)
	config.MultiStatements = true
	config.InterpolateParams = true
	config.Timeout = 5 * time.Second
	config.ReadTimeout = 10 * time.Second
	config.WriteTimeout = 10 * time.Second
	connector, err := driver.NewConnector(config)
	if err != nil {
		return c.SafeError(err)
	}
	opened := sql.OpenDB(connector)
	opened.SetMaxOpenConns(4)
	db.SQL = opened
	c.addResource()
	c.T.Cleanup(func() { c.removed(opened.Close(), "SQL observer") })
	return nil
}
func (c *Case) waitDatabase(db *Database) error {
	ctx, cancel := context.WithTimeout(c.Ctx, 30*time.Second)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		var ready int
		err := db.SQL.QueryRowContext(ctx, "SELECT 1").Scan(&ready)
		if err == nil && ready == 1 {
			return nil
		}
		select {
		case <-ctx.Done():
			return errors.New("SQL readiness deadline exceeded")
		case <-ticker.C:
		}
	}
}
func (c *Case) provisionUsers(db *Database) error {
	statements := []struct {
		query string
		args  []any
	}{
		{"CREATE USER 'app_owner'@'%' IDENTIFIED BY ?", []any{c.rootPassword}},
		{"GRANT ALL PRIVILEGES ON app.* TO 'app_owner'@'%'", nil},
		{"CREATE USER 'intern_backup'@'%' IDENTIFIED BY ?", []any{c.backupPassword}},
		{"CREATE USER 'intern_restore'@'%' IDENTIFIED BY ?", []any{c.restorePassword}},
		{"GRANT SELECT, SHOW VIEW, TRIGGER, EVENT ON app.* TO 'intern_backup'@'%'", nil},
		{"GRANT RELOAD, REPLICATION CLIENT, REPLICATION SLAVE, SHOW_ROUTINE ON *.* TO 'intern_backup'@'%'", nil},
		{"GRANT BINLOG_ADMIN, SESSION_VARIABLES_ADMIN, SET_ANY_DEFINER ON *.* TO 'intern_restore'@'%'", nil},
	}
	for _, s := range statements {
		if _, err := db.SQL.ExecContext(c.Ctx, s.query, s.args...); err != nil {
			return c.SafeError(err)
		}
	}
	return c.grantRestoreSchemas(db)
}
func (c *Case) grantRestoreSchemas(db *Database) error {
	const grant = "GRANT CREATE, SELECT, INSERT, UPDATE, DELETE, ALTER, INDEX, DROP, CREATE VIEW, SHOW VIEW, TRIGGER, EVENT, CREATE ROUTINE, ALTER ROUTINE, EXECUTE, REFERENCES, LOCK TABLES ON "
	statements := []string{
		grant + "restored.* TO 'intern_restore'@'%'", grant + "app.* TO 'intern_restore'@'%'",
		grant + "protected.* TO 'intern_restore'@'%'", grant + "wrong_key.* TO 'intern_restore'@'%'",
		grant + "oversized.* TO 'intern_restore'@'%'", grant + "tampered.* TO 'intern_restore'@'%'",
		grant + "missing.* TO 'intern_restore'@'%'", grant + "duplicate.* TO 'intern_restore'@'%'",
		grant + "renamed.* TO 'intern_restore'@'%'", grant + "escape.* TO 'intern_restore'@'%'",
	}
	for _, query := range statements {
		if _, err := db.SQL.ExecContext(c.Ctx, query); err != nil {
			return c.SafeError(err)
		}
	}
	return nil
}
func (c *Case) recordDatabase(db *Database, gtid bool) error {
	var mode, format string
	var binlog int
	if err := db.SQL.QueryRowContext(c.Ctx, "SELECT @@server_uuid, @@version, @@global.gtid_mode, @@global.log_bin, @@global.binlog_format").Scan(&db.UUID, &db.Version, &mode, &binlog, &format); err != nil {
		return c.SafeError(err)
	}
	if db.Version != db.Profile.Version || binlog != 1 || format != "ROW" {
		return errors.New("real database capabilities differ from fixture contract")
	}
	if (mode == "ON") != gtid {
		return errors.New("database GTID fixture mode mismatch")
	}
	inspected, err := db.Container.Inspect(c.Ctx)
	if err != nil {
		return err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.Receipt.Instances = append(c.Receipt.Instances, qualitygate.Instance{Role: db.Alias, UUID: db.UUID, Version: db.Version, ImageID: inspected.Image})
	return nil
}
func (c *Case) SQL(db *Database, query string, args ...any) {
	c.T.Helper()
	_, err := db.SQL.ExecContext(c.Ctx, query, args...)
	c.Must(c.SafeError(err))
}
func (c *Case) Scalar(db *Database, query string, args ...any) string {
	c.T.Helper()
	var value string
	c.Must(c.SafeError(db.SQL.QueryRowContext(c.Ctx, query, args...).Scan(&value)))
	return value
}

func (c *Case) LastStatementError(db *Database) string {
	var code int
	var message string
	err := db.SQL.QueryRowContext(c.Ctx, "SELECT MYSQL_ERRNO, MESSAGE_TEXT FROM performance_schema.events_statements_history_long WHERE MYSQL_ERRNO <> 0 ORDER BY TIMER_END DESC LIMIT 1").Scan(&code, &message)
	if err != nil {
		return "no SQL error record"
	}
	return c.Redact(fmt.Sprintf("%d: %s", code, message))
}
