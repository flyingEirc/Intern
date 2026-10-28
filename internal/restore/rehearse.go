package restore

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net"
	"path/filepath"
	"strings"
	"time"

	driver "github.com/go-sql-driver/mysql"

	"github.com/flyingEirc/Intern/internal/physical"
)

const (
	rehearsalReadyTimeout    = 120 * time.Second
	rehearsalShutdownTimeout = 60 * time.Second
	rehearsalRowLimit        = 1000
)

// The verify_sql limits belong to the engine: the API layer admits or refuses a
// request with exactly these bounds, so a request that is accepted is always
// admissible here. W5 reads both constants for its OpenAPI document.
const (
	// MaxVerifySQLStatements is the largest number of statements one restore may request.
	MaxVerifySQLStatements = 16
	// MaxVerifySQLBytes is the longest single statement the rehearsal will run.
	MaxVerifySQLBytes = 4096
)

const userSchemasQuery = `SELECT schema_name FROM information_schema.schemata
WHERE schema_name NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
ORDER BY schema_name`

const innodbTablesQuery = `SELECT table_schema, table_name FROM information_schema.tables
WHERE engine = 'InnoDB' AND table_schema NOT IN ('mysql', 'information_schema', 'performance_schema', 'sys')
ORDER BY table_schema, table_name`

// ValidateVerifySQL admits only one bounded SELECT statement per entry. It is
// the single authority for verify_sql: Execute refuses a request that fails it,
// and the API layer refuses the same statements before a row is ever written.
// The driver also refuses multi-statements, so the server never sees two.
func ValidateVerifySQL(queries []string) error {
	if len(queries) > MaxVerifySQLStatements {
		return fmt.Errorf("restore: too many verify statements: %d, at most %d", len(queries), MaxVerifySQLStatements)
	}
	for _, query := range queries {
		if err := validateVerifyStatement(query); err != nil {
			return err
		}
	}
	return nil
}

func validateVerifyStatement(query string) error {
	if query == "" || len(query) > MaxVerifySQLBytes || strings.ContainsRune(query, 0) {
		return fmt.Errorf("restore: invalid verify statement: empty, longer than %d bytes, or containing a NUL", MaxVerifySQLBytes)
	}
	body := strings.TrimSuffix(strings.TrimSpace(query), ";")
	if body == "" || strings.Contains(body, ";") {
		return errors.New("restore: verify statements must be a single statement")
	}
	lower := strings.ToLower(strings.TrimLeft(body, " \t\r\n("))
	if !strings.HasPrefix(lower, "select") {
		return errors.New("restore: verify statements must be SELECT")
	}
	if len(lower) > 6 && !isStatementSpace(lower[6]) {
		return errors.New("restore: verify statements must be SELECT")
	}
	return nil
}

func isStatementSpace(c byte) bool {
	return c == ' ' || c == '\t' || c == '\r' || c == '\n' || c == '('
}

// rehearse starts an isolated server on the prepared work copy, runs the fixed
// checks and the requested read-only queries, then shuts it down. The export is
// already published, so a failed rehearsal is reported, never turned into a
// failed restore.
func (e *execution) rehearse(ctx context.Context) *VerifyResult {
	if !e.req.Rehearse {
		return nil
	}
	// The rehearsing phase begins here and nowhere else: a restore that was not
	// asked to rehearse ends at exporting, so its progress and its persisted phase
	// never name a stage that never ran. The phase is reported even when the server
	// then fails to start, because the attempt is what the phase describes.
	e.enter(PhaseRehearsing)
	result := &VerifyResult{Status: "failed"}
	process, err := physical.StartMysqld(ctx, physical.MysqldOptions{
		Binary: e.opts.MysqldPath, DataDir: filepath.Join(e.work, "data"),
		Socket: e.rehearsalSocket(), PIDFile: filepath.Join(e.work, "mysqld.pid"), LogError: filepath.Join(e.work, "mysqld.err"),
	})
	if err != nil {
		result.FailureCode = startFailureCode(err)
		return result
	}
	result.Started = true
	e.report(PhaseRehearsing, map[string]any{"step": "started", "pid": process.PID()})
	db, err := e.openRehearsal(ctx, process)
	if err != nil {
		result.FailureCode = "not_ready"
		if killErr := e.killRehearsal(process); killErr != nil {
			result.FailureCode = "shutdown_failed"
		}
		return result
	}
	e.verify(ctx, db, process, result)
	return result
}

func startFailureCode(err error) string {
	if errors.Is(err, physical.ErrRehearsalRoot) {
		return "root_refused"
	}
	return "not_started"
}

func (e *execution) rehearsalSocket() string { return filepath.Join(e.work, "mysqld.sock") }

// verify runs the checks, then shuts the rehearsal server down. A check failure
// is still reported after a clean shutdown.
func (e *execution) verify(ctx context.Context, db *sql.DB, process *physical.MysqldProcess, result *VerifyResult) {
	checkCode := e.runChecks(ctx, db, result)
	shutdownErr := e.stopRehearsal(ctx, process, db)
	// The handle is always released. A server that shut down cleanly has already
	// dropped its unix socket, so the driver close of that connection reports the
	// dead socket rather than a failed check; the process wait in stopRehearsal is
	// the authoritative outcome, and the pooled resources are released either way.
	_ = db.Close() //nolint:errcheck // the exited server closed the socket first.
	result.FailureCode = firstFailure(checkCode, outcomeCode("shutdown_failed", shutdownErr))
	if result.FailureCode == "" {
		result.Status = "passed"
	}
}

func outcomeCode(code string, err error) string {
	if err != nil {
		return code
	}
	return ""
}

func firstFailure(codes ...string) string {
	for _, code := range codes {
		if code != "" {
			return code
		}
	}
	return ""
}

func (e *execution) openRehearsal(ctx context.Context, process *physical.MysqldProcess) (*sql.DB, error) {
	if err := e.waitRehearsalSocket(ctx, process); err != nil {
		return nil, err
	}
	cfg := driver.NewConfig()
	cfg.Net, cfg.Addr, cfg.User = "unix", e.rehearsalSocket(), "root"
	cfg.Timeout, cfg.ReadTimeout, cfg.WriteTimeout = 5*time.Second, 120*time.Second, 120*time.Second
	cfg.MultiStatements = false
	cfg.InterpolateParams = false
	// The rehearsal server closes the socket while it answers SHUTDOWN, so the
	// pooled connection reports a broken pipe on its way out. That line is noise:
	// stopRehearsal waits for the process itself, and the connection is scoped to
	// this config, so only the rehearsal handle loses the driver's stderr logging.
	cfg.Logger = discardDriverLogger{}
	connector, err := driver.NewConnector(cfg)
	if err != nil {
		return nil, err
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	if err = db.PingContext(ctx); err != nil {
		return nil, errors.Join(err, db.Close())
	}
	return db, nil
}

// discardDriverLogger is the per-connection go-sql-driver logger of the
// rehearsal handle. It never touches the process-wide default logger.
type discardDriverLogger struct{}

func (discardDriverLogger) Print(_ ...any) {}

func (e *execution) waitRehearsalSocket(ctx context.Context, process *physical.MysqldProcess) error {
	deadline := time.Now().Add(rehearsalReadyTimeout)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		select {
		case <-process.Exited():
			return errors.New("restore: rehearsal server exited before it was ready")
		default:
		}
		conn, err := net.DialTimeout("unix", e.rehearsalSocket(), 500*time.Millisecond)
		if err == nil {
			if closeErr := conn.Close(); closeErr != nil {
				return closeErr
			}
			return nil
		}
		if time.Now().After(deadline) {
			return errors.New("restore: rehearsal server did not become ready")
		}
		time.Sleep(200 * time.Millisecond)
	}
}

func (e *execution) killRehearsal(process *physical.MysqldProcess) error {
	killErr := process.Kill()
	waitCtx, cancel := context.WithTimeout(context.Background(), rehearsalShutdownTimeout)
	defer cancel()
	return errors.Join(killErr, process.WaitExit(waitCtx))
}

func (e *execution) stopRehearsal(ctx context.Context, process *physical.MysqldProcess, db *sql.DB) error {
	// The server normally closes the connection before answering, so a SHUTDOWN
	// error is expected. It is kept only as context if the process then fails to
	// exit; the bounded wait below is the authoritative outcome.
	var shutdownErr error
	if _, shutdownErr = db.ExecContext(ctx, "SHUTDOWN"); shutdownErr != nil {
		shutdownErr = errors.New("restore: rehearsal shutdown request did not complete")
	}
	shutdownCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), rehearsalReadyTimeout+rehearsalShutdownTimeout)
	defer cancel()
	waitCtx, stop := context.WithTimeout(shutdownCtx, rehearsalShutdownTimeout)
	defer stop()
	err := process.WaitExit(waitCtx)
	switch {
	case errors.Is(err, context.DeadlineExceeded) || errors.Is(err, context.Canceled):
		return errors.Join(errors.New("restore: rehearsal server did not shut down in time"), e.killRehearsal(process))
	case err != nil:
		return errors.Join(errors.New("restore: rehearsal server did not exit cleanly"), shutdownErr)
	default:
		return nil
	}
}

// runChecks performs the fixed checks and returns the first failing code.
func (e *execution) runChecks(ctx context.Context, db *sql.DB, result *VerifyResult) string {
	if err := e.checkSystemTables(ctx, db, result); err != nil {
		return "system_tables"
	}
	if err := e.checkSchemas(ctx, db, result); err != nil {
		return "schema_stats"
	}
	if err := e.checkTables(ctx, db, result); err != nil {
		return "check_table"
	}
	if err := e.runQueries(ctx, db, result); err != nil {
		return "verify_sql"
	}
	return ""
}

func (e *execution) checkSystemTables(ctx context.Context, db *sql.DB, result *VerifyResult) error {
	var users int
	if err := db.QueryRowContext(ctx, "SELECT COUNT(*) FROM mysql.user").Scan(&users); err != nil {
		return err
	}
	result.SystemTablesReadable = true
	return nil
}

func (e *execution) checkSchemas(ctx context.Context, db *sql.DB, result *VerifyResult) error {
	schemas, err := listUserSchemas(ctx, db)
	if err != nil {
		return err
	}
	for _, schema := range schemas {
		stat, err := e.schemaStat(ctx, db, schema)
		if err != nil {
			return err
		}
		result.Schemas = append(result.Schemas, stat)
	}
	return nil
}

func listUserSchemas(ctx context.Context, db *sql.DB) ([]string, error) {
	rows, err := db.QueryContext(ctx, userSchemasQuery)
	if err != nil {
		return nil, err
	}
	schemas, err := scanStrings(rows, "schema")
	return schemas, errors.Join(err, rows.Close())
}

func (e *execution) schemaStat(ctx context.Context, db *sql.DB, schema string) (SchemaStat, error) {
	tables, err := e.schemaTables(ctx, db, schema)
	if err != nil {
		return SchemaStat{}, err
	}
	stat := SchemaStat{Name: schema, Tables: len(tables)}
	for _, table := range tables {
		var count int64
		query := "SELECT COUNT(*) FROM " + quoteIdentifier(schema) + "." + quoteIdentifier(table)
		if err := db.QueryRowContext(ctx, query).Scan(&count); err != nil {
			return SchemaStat{}, fmt.Errorf("restore: cannot count rows of %s.%s: %w", schema, table, err)
		}
		stat.Rows += count
	}
	return stat, nil
}

func (e *execution) schemaTables(ctx context.Context, db *sql.DB, schema string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT table_name FROM information_schema.tables WHERE table_schema = ? ORDER BY table_name", schema)
	if err != nil {
		return nil, err
	}
	tables, err := scanStrings(rows, "table")
	return tables, errors.Join(err, rows.Close())
}

func scanStrings(rows *sql.Rows, label string) ([]string, error) {
	values := make([]string, 0, 4)
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return nil, fmt.Errorf("restore: cannot read %s list: %w", label, err)
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (e *execution) checkTables(ctx context.Context, db *sql.DB, result *VerifyResult) error {
	tables, err := listInnoDBTables(ctx, db)
	if err != nil {
		return err
	}
	for _, table := range tables {
		status, err := e.checkTable(ctx, db, table)
		result.CheckTable = append(result.CheckTable, CheckStat{Table: table.qualified(), Status: status})
		if err != nil {
			return err
		}
	}
	return nil
}

type qualifiedTable struct{ schema, table string }

func (t qualifiedTable) qualified() string { return t.schema + "." + t.table }

func listInnoDBTables(ctx context.Context, db *sql.DB) ([]qualifiedTable, error) {
	rows, err := db.QueryContext(ctx, innodbTablesQuery)
	if err != nil {
		return nil, err
	}
	tables := make([]qualifiedTable, 0, 8)
	for rows.Next() {
		var item qualifiedTable
		if err = rows.Scan(&item.schema, &item.table); err != nil {
			break
		}
		tables = append(tables, item)
	}
	err = errors.Join(err, rows.Err(), rows.Close())
	return tables, err
}

func (e *execution) checkTable(ctx context.Context, db *sql.DB, item qualifiedTable) (string, error) {
	var table, op, messageType, message string
	query := "CHECK TABLE " + quoteIdentifier(item.schema) + "." + quoteIdentifier(item.table)
	err := db.QueryRowContext(ctx, query).Scan(&table, &op, &messageType, &message)
	if err != nil {
		return "error", err
	}
	if !strings.EqualFold(message, "OK") {
		return message, errors.New("restore: CHECK TABLE failed")
	}
	return message, nil
}

func (e *execution) runQueries(ctx context.Context, db *sql.DB, result *VerifyResult) error {
	for _, statement := range e.req.VerifySQL {
		item := e.runQuery(ctx, db, statement)
		result.Queries = append(result.Queries, item)
		if item.ErrorCode != "" {
			return fmt.Errorf("restore: verify statement failed: %s", item.ErrorCode)
		}
	}
	return nil
}

func (e *execution) runQuery(ctx context.Context, db *sql.DB, statement string) QueryResult {
	item := QueryResult{SQL: statement}
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		item.ErrorCode = "transaction_failed"
		return item
	}
	item.Columns, item.Rows, item.ErrorCode = queryInTransaction(ctx, tx, statement)
	if item.ErrorCode == "" {
		if err = tx.Commit(); err != nil {
			item.ErrorCode = "commit_failed"
		}
	}
	if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) && item.ErrorCode == "" {
		item.ErrorCode = "rollback_failed"
	}
	return item
}

func queryInTransaction(ctx context.Context, tx *sql.Tx, statement string) ([]string, [][]string, string) {
	rows, err := tx.QueryContext(ctx, statement)
	if err != nil {
		return nil, nil, "query_failed"
	}
	columns, recorded, code := scanQueryRows(rows)
	if closeErr := rows.Close(); closeErr != nil && code == "" {
		code = "close_failed"
	}
	return columns, recorded, code
}

func scanQueryRows(rows *sql.Rows) ([]string, [][]string, string) {
	columns, err := rows.Columns()
	if err != nil {
		return nil, nil, "columns_failed"
	}
	values := make([]any, len(columns))
	pointers := make([]any, len(columns))
	for index := range values {
		pointers[index] = &values[index]
	}
	recorded := make([][]string, 0, 8)
	for rows.Next() {
		if len(recorded) >= rehearsalRowLimit {
			return columns, recorded, "rows_truncated"
		}
		if err = rows.Scan(pointers...); err != nil {
			return columns, recorded, "scan_failed"
		}
		record := make([]string, len(columns))
		for index, value := range values {
			record[index] = stringify(value)
		}
		recorded = append(recorded, record)
	}
	if err = rows.Err(); err != nil {
		return columns, recorded, "scan_failed"
	}
	return columns, recorded, ""
}

func stringify(value any) string {
	switch typed := value.(type) {
	case nil:
		return "NULL"
	case []byte:
		return string(typed)
	case string:
		return typed
	default:
		return fmt.Sprint(typed)
	}
}

func quoteIdentifier(name string) string {
	return "`" + strings.ReplaceAll(name, "`", "``") + "`"
}
