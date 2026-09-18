package backup

import (
	"context"
	"crypto/sha256"
	"crypto/tls"
	"crypto/x509"
	"database/sql"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"

	"github.com/flyingEirc/IotProject/base/fsutil"
)

var databaseName = regexp.MustCompile(`^[a-zA-Z0-9_][a-zA-Z0-9_$-]{0,63}$`)
var logName = regexp.MustCompile(`^[a-zA-Z0-9_-]+\.[0-9]{6,}$`)
var versionPattern = regexp.MustCompile(`\b([89])\.([0-9]+)\.([0-9]+)\b`)

type sourceInfo struct {
	UUID, Version, GTID, ObservedGTID, Charset, Collation string
	SchemaObjects                                         bool
}
type session struct {
	db   *sql.DB
	conn *sql.Conn
	cfg  Connection
}
type discardLogger struct{}

func (discardLogger) Print(...any) {}

func normalizeConnection(c Connection) (Connection, error) {
	if err := validateDatabase(c); err != nil {
		return c, err
	}
	if c.Host == "" {
		c.Host = "127.0.0.1"
	}
	if c.Port == 0 {
		c.Port = 3306
	}
	if c.Port < 1 || c.Port > 65535 {
		return c, errors.New("database port must be between 1 and 65535")
	}
	if err := resolvePassword(&c); err != nil {
		return c, err
	}
	if err := normalizeTransport(&c); err != nil {
		return c, err
	}
	return c, nil
}
func validateDatabase(c Connection) error {
	if c.Username == "" {
		return errors.New("database username is required")
	}
	if !databaseName.MatchString(c.DB) {
		return errors.New("database name must be 1–64 ASCII letters, digits, _, $, or - and cannot start with - or $")
	}
	switch strings.ToLower(c.DB) {
	case "mysql", "sys", "information_schema", "performance_schema":
		return errors.New("system databases are not supported backup/restore targets")
	}
	return nil
}
func resolvePassword(c *Connection) error {
	if c.PasswordEnv == "" || c.Password != "" {
		return nil
	}
	password, ok := os.LookupEnv(c.PasswordEnv)
	if !ok {
		return errors.New("database password environment variable is not set")
	}
	c.Password = password
	return nil
}
func normalizeTransport(c *Connection) error {
	if c.TLSMode == "" {
		c.TLSMode = "VERIFY_IDENTITY"
		if c.Socket != "" {
			c.TLSMode = "DISABLED"
		}
	}
	c.TLSMode = strings.ToUpper(c.TLSMode)
	if c.TLSMode != "DISABLED" && c.TLSMode != "VERIFY_IDENTITY" {
		return errors.New("tls-mode must be VERIFY_IDENTITY or explicitly DISABLED; unverifiable TLS is not accepted")
	}
	for _, v := range []string{c.Username, c.Password, c.Host, c.Socket, c.TLSCA} {
		if strings.ContainsRune(v, 0) {
			return errors.New("connection fields cannot contain NUL")
		}
	}
	return nil
}

func connect(ctx context.Context, c Connection) (*session, error) {
	c, err := normalizeConnection(c)
	if err != nil {
		return nil, err
	}
	cfg := mysql.NewConfig()
	cfg.User = c.Username
	cfg.Passwd = c.Password
	cfg.Logger = discardLogger{}
	cfg.Timeout = 10 * time.Second
	if c.Socket != "" {
		cfg.Net = "unix"
		cfg.Addr = c.Socket
	} else {
		cfg.Net = "tcp"
		cfg.Addr = net.JoinHostPort(c.Host, strconv.Itoa(c.Port))
	}
	if c.TLSMode == "VERIFY_IDENTITY" {
		cfg.TLS, err = connectionTLS(c)
		if err != nil {
			return nil, err
		}
	}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, errors.New("invalid MySQL connection settings")
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(ctx)
	if err != nil {
		return nil, errors.Join(dbError("connect", err), db.Close())
	}
	return &session{db: db, conn: conn, cfg: c}, nil
}
func (s *session) Close() error { return errors.Join(s.conn.Close(), s.db.Close()) }

func connectionTLS(c Connection) (cfg *tls.Config, retErr error) {
	cfg = &tls.Config{MinVersion: tls.VersionTLS12, ServerName: c.Host}
	if c.TLSCA == "" {
		return cfg, nil
	}
	f, err := fsutil.Open(c.TLSCA)
	if err != nil {
		return nil, errors.New("cannot read TLS CA file")
	}
	defer fsutil.Close(&retErr, f)
	pem, err := io.ReadAll(f)
	if err != nil {
		return nil, errors.New("cannot read TLS CA file")
	}
	roots := x509.NewCertPool()
	if !roots.AppendCertsFromPEM(pem) {
		return nil, errors.New("TLS CA file contains no certificates")
	}
	cfg.RootCAs = roots
	return cfg, nil
}
func dbError(operation string, err error) error {
	var me *mysql.MySQLError
	if errors.As(err, &me) {
		return fmt.Errorf("MySQL %s failed (server error %d; check privileges and server configuration)", operation, me.Number)
	}
	if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return err
	}
	return fmt.Errorf("MySQL %s failed: connection or protocol error", operation)
}
func (s *session) lock(ctx context.Context, role string) error {
	sum := sha256.Sum256([]byte("intern:" + role + ":" + strings.ToLower(s.cfg.DB)))
	var acquired sql.NullInt64
	if err := s.conn.QueryRowContext(ctx, "SELECT GET_LOCK(?, 0)", fmt.Sprintf("intern:%x", sum[:24])).Scan(&acquired); err != nil {
		return dbError("lock", err)
	}
	if !acquired.Valid || acquired.Int64 != 1 {
		return errors.New("another Intern operation owns this database")
	}
	return nil
}
func (s *session) info(ctx context.Context, source bool) (sourceInfo, error) {
	var info sourceInfo
	if err := s.conn.QueryRowContext(ctx, "SELECT @@server_uuid, @@version, @@global.gtid_mode, @@global.gtid_executed").Scan(&info.UUID, &info.Version, &info.GTID, &info.ObservedGTID); err != nil {
		return info, dbError("inspect server", err)
	}
	if err := validateServerVersion(info.Version); err != nil {
		return info, err
	}
	if !source {
		return info, nil
	}
	if err := s.checkBinlogMode(ctx); err != nil {
		return info, err
	}
	var err error
	if err = s.conn.QueryRowContext(ctx, "SELECT SCHEMA_NAME, DEFAULT_CHARACTER_SET_NAME, DEFAULT_COLLATION_NAME FROM information_schema.SCHEMATA WHERE SCHEMA_NAME = ?", s.cfg.DB).Scan(&s.cfg.DB, &info.Charset, &info.Collation); err != nil {
		return info, errors.New("source database not found or not accessible")
	}
	if err = s.checkTableScope(ctx); err != nil {
		return info, err
	}
	var count int
	if err = s.conn.QueryRowContext(ctx, `SELECT (SELECT COUNT(*) FROM information_schema.VIEWS WHERE TABLE_SCHEMA = ?) + (SELECT COUNT(*) FROM information_schema.ROUTINES WHERE ROUTINE_SCHEMA = ?) + (SELECT COUNT(*) FROM information_schema.EVENTS WHERE EVENT_SCHEMA = ?) + (SELECT COUNT(*) FROM information_schema.TRIGGERS WHERE TRIGGER_SCHEMA = ?)`, s.cfg.DB, s.cfg.DB, s.cfg.DB, s.cfg.DB).Scan(&count); err != nil {
		return info, dbError("inspect schema objects", err)
	}
	info.SchemaObjects = count > 0
	return info, nil
}
func validateServerVersion(version string) error {
	parts := versionPattern.FindStringSubmatch(version)
	if len(parts) == 0 || (parts[1] == "8" && parts[2] != "4") {
		return errors.New("only Oracle MySQL 8.4/9.x compatible servers are supported; MariaDB is not supported")
	}
	if strings.Contains(strings.ToLower(version), "mariadb") {
		return errors.New("MariaDB is not supported")
	}
	return nil
}

func (s *session) checkBinlogMode(ctx context.Context) (retErr error) {
	var logBin int
	if err := s.conn.QueryRowContext(ctx, "SELECT @@global.log_bin").Scan(&logBin); err != nil {
		return dbError("inspect binlog", err)
	}
	if logBin != 1 {
		return errors.New("binary logging must be enabled for a coordinated logical backup chain")
	}
	rows, err := s.conn.QueryContext(ctx, "SHOW GLOBAL VARIABLES LIKE 'binlog_format'")
	if err != nil {
		return dbError("inspect binlog format", err)
	}
	defer fsutil.Close(&retErr, rows)
	for rows.Next() {
		var k, v string
		if err = rows.Scan(&k, &v); err != nil {
			return err
		}
		if v != "ROW" {
			return errors.New("ROW binlog format is required for single-database recovery")
		}
	}
	return rows.Err()
}
func (s *session) checkTableScope(ctx context.Context) error {
	var count int
	if err := s.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.TABLES WHERE TABLE_SCHEMA = ? AND TABLE_TYPE = 'BASE TABLE' AND ENGINE <> 'InnoDB'", s.cfg.DB).Scan(&count); err != nil {
		return dbError("inspect table engines", err)
	}
	if count != 0 {
		return errors.New("consistent logical backup requires all source tables to use InnoDB")
	}
	if err := s.conn.QueryRowContext(ctx, "SELECT COUNT(*) FROM information_schema.KEY_COLUMN_USAGE WHERE TABLE_SCHEMA = ? AND REFERENCED_TABLE_SCHEMA IS NOT NULL AND REFERENCED_TABLE_SCHEMA <> ?", s.cfg.DB, s.cfg.DB).Scan(&count); err != nil {
		return dbError("inspect foreign key scope", err)
	}
	if count != 0 {
		return errors.New("cross-database foreign keys require an instance-scoped backup plan")
	}
	return nil
}

func optionQuote(s string) string {
	r := strings.NewReplacer("\\", "\\\\", "\"", "\\\"", "\n", "\\n", "\r", "\\r", "\t", "\\t", "\b", "\\b")
	return "\"" + r.Replace(s) + "\""
}
func credentials(dir string, c Connection) (string, error) {
	// MySQL's Unix transport still requires the client host to be localhost.
	if c.Socket != "" {
		c.Host = "localhost"
	}
	var b strings.Builder
	b.WriteString("[client]\n")
	values := [][2]string{{"user", c.Username}, {"password", c.Password}, {"host", c.Host}, {"port", strconv.Itoa(c.Port)}, {"ssl-mode", c.TLSMode}}
	if c.Socket != "" {
		values = append(values, [2]string{"socket", c.Socket}, [2]string{"protocol", "SOCKET"})
	} else {
		values = append(values, [2]string{"protocol", "TCP"})
	}
	if c.TLSCA != "" {
		values = append(values, [2]string{"ssl-ca", c.TLSCA})
	}
	for _, kv := range values {
		b.WriteString(kv[0] + "=" + optionQuote(kv[1]) + "\n")
	}
	path := filepath.Join(dir, "client.cnf")
	if err := os.WriteFile(path, []byte(b.String()), 0600); err != nil {
		return "", err
	}
	return path, nil
}
func officialCommand(ctx context.Context, name string, args ...string) (*exec.Cmd, error) {
	var cmd *exec.Cmd
	switch name {
	case "mysql":
		cmd = exec.CommandContext(ctx, "mysql")
	case "mysqldump":
		cmd = exec.CommandContext(ctx, "mysqldump")
	case "mysqlbinlog":
		cmd = exec.CommandContext(ctx, "mysqlbinlog")
	default:
		return nil, errors.New("unsupported MySQL tool")
	}
	// Executable selection is fixed; arguments are passed as argv, never through a shell.
	cmd.Args = append(cmd.Args, args...)
	return cmd, nil
}
func toolCommand(ctx context.Context, name, credentialFile string, args ...string) (*exec.Cmd, error) {
	prefix := make([]string, 0, len(args)+2)
	if credentialFile != "" {
		prefix = append(prefix, "--defaults-file="+credentialFile, "--no-login-paths")
	} else {
		prefix = append(prefix, "--no-defaults", "--no-login-paths")
	}
	cmd, err := officialCommand(ctx, name, append(prefix, args...)...)
	if err != nil {
		return nil, err
	}
	// Do not inherit MySQL password/login overrides; credentials come only from the private option file.
	for _, v := range os.Environ() {
		if !strings.HasPrefix(v, "MYSQL_PWD=") && !strings.HasPrefix(v, "MYSQL_TEST_LOGIN_FILE=") && !strings.HasPrefix(v, "MYSQL_HOME=") {
			cmd.Env = append(cmd.Env, v)
		}
	}
	cmd.WaitDelay = 5 * time.Second
	return cmd, nil
}
func runTool(ctx context.Context, name, creds string, input io.Reader, output io.Writer, args ...string) error {
	cmd, err := toolCommand(ctx, name, creds, args...)
	if err != nil {
		return err
	}
	cmd.Stdin = input
	cmd.Stdout = output
	// Client stderr may contain SQL or credentials; never forward it to logs/errors.
	cmd.Stderr = io.Discard
	if err = cmd.Run(); err != nil {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		var ee *exec.ExitError
		if errors.As(err, &ee) {
			return fmt.Errorf("%s failed (exit %d); check tool compatibility, privileges and input integrity", name, ee.ExitCode())
		}
		return fmt.Errorf("cannot execute required MySQL tool %s", name)
	}
	return nil
}
func toolVersion(ctx context.Context, name, server string) (string, error) {
	cmd, err := officialCommand(ctx, name, "--no-defaults", "--version")
	if err != nil {
		return "", err
	}
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("required tool %s is unavailable", name)
	}
	v := versionPattern.FindStringSubmatch(string(out))
	s := versionPattern.FindStringSubmatch(server)
	if len(v) == 0 || len(s) == 0 || v[1] != s[1] || v[2] != s[2] {
		return "", fmt.Errorf("%s major/minor version must match MySQL server %s", name, server)
	}
	return strings.TrimSpace(string(out)), nil
}
