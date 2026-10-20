package physical

import (
	"context"
	"database/sql"
	"regexp"
	"strings"
)

var sourceUUIDPattern = regexp.MustCompile(`^[0-9a-fA-F]{8}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{4}-[0-9a-fA-F]{12}$`)

func readSourceIdentity(ctx context.Context, db *sql.DB, identity *Identity) error {
	err := db.QueryRowContext(ctx, `SELECT @@server_uuid, @@version, @@version_comment, @@datadir, @@innodb_page_size`).Scan(
		&identity.UUID, &identity.Version, &identity.VersionComment, &identity.DataDir, &identity.PageSize,
	)
	if err != nil {
		return preflightExternalFailure(ctx, "identity", "identity_query_failed", "Cannot read MySQL identity; check socket connectivity and account access.")
	}
	if !sourceUUIDPattern.MatchString(identity.UUID) {
		return preflightFailure("identity", "identity_invalid", "MySQL did not return a valid server UUID.")
	}
	switch identity.PageSize {
	case 4096, 8192, 16384, 32768, 65536:
		return nil
	default:
		return preflightFailure("identity", "page_size_unsupported", "The source InnoDB page size is not supported.")
	}
}

// ALL PRIVILEGES does not include dynamic privileges such as BACKUP_ADMIN.
var backupStaticPrivileges = []string{"PROCESS", "RELOAD", "LOCK TABLES", "REPLICATION CLIENT"}

func missingBackupPrivileges(grants []string) []string {
	granted := make(map[string]bool)
	for _, grant := range grants {
		collectGlobalPrivileges(grant, granted)
	}
	missing := make([]string, 0, len(backupStaticPrivileges)+1)
	for _, privilege := range backupStaticPrivileges {
		if !granted[privilege] && !granted["ALL PRIVILEGES"] {
			missing = append(missing, privilege)
		}
	}
	if !granted["BACKUP_ADMIN"] {
		missing = append(missing, "BACKUP_ADMIN")
	}
	return missing
}

func collectGlobalPrivileges(grant string, granted map[string]bool) {
	// Only the privilege clause is interpreted. Account names, role names and
	// object names cannot manufacture global grants by containing known words.
	clause, remainder, ok := strings.Cut(grant, " ON ")
	if !ok || !strings.HasPrefix(remainder, "*.* TO ") || !strings.HasPrefix(clause, "GRANT ") {
		return
	}
	for privilege := range strings.SplitSeq(strings.TrimPrefix(clause, "GRANT "), ",") {
		granted[strings.TrimSpace(privilege)] = true
	}
}

func checkBackupPrivileges(ctx context.Context, db *sql.DB, result *PreflightResult) error {
	grants, err := readGrants(ctx, db)
	if err != nil {
		return err
	}
	if missing := missingBackupPrivileges(grants); len(missing) != 0 {
		failure := preflightFailure("privileges", "privileges_missing", "Grant the listed global privileges directly to the backup account; BACKUP_ADMIN is a separate dynamic grant.")
		failure.Missing = missing
		return failure
	}
	if !hasGlobalTableVisibility(grants) {
		result.addWarning("table_inventory_partial", "The account cannot enumerate all user tables; unobserved non-InnoDB tables may require stronger locks and full copies.")
	}
	for _, probe := range []struct{ query, message string }{
		{`SELECT * FROM performance_schema.log_status`, "Grant SELECT on performance_schema.log_status and BACKUP_ADMIN; the real log-status query failed."},
		{`SELECT * FROM performance_schema.replication_group_members`, "Grant SELECT on performance_schema.replication_group_members; the real group-membership query failed."},
	} {
		if err := drainPreflightQuery(ctx, db, probe.query); err != nil {
			return preflightExternalFailure(ctx, "privileges", "privilege_probe_failed", probe.message)
		}
	}
	return nil
}

func hasGlobalTableVisibility(grants []string) bool {
	granted := make(map[string]bool)
	for _, grant := range grants {
		if strings.HasPrefix(grant, "REVOKE ") {
			return false
		}
		collectGlobalPrivileges(grant, granted)
	}
	return granted["SELECT"] || granted["ALL PRIVILEGES"]
}

func readGrants(ctx context.Context, db *sql.DB) (grants []string, retErr error) {
	rows, err := db.QueryContext(ctx, "SHOW GRANTS")
	if err != nil {
		return nil, preflightExternalFailure(ctx, "privileges", "grants_query_failed", "Cannot inspect grants for the backup account.")
	}
	defer func() {
		if err := rows.Close(); err != nil && retErr == nil {
			retErr = preflightFailure("privileges", "grants_query_failed", "Cannot finish inspecting account grants.")
		}
	}()
	for rows.Next() {
		var grant string
		if err := rows.Scan(&grant); err != nil {
			return nil, preflightFailure("privileges", "grants_query_failed", "Cannot decode account grants.")
		}
		grants = append(grants, grant)
	}
	if err := rows.Err(); err != nil {
		return nil, preflightExternalFailure(ctx, "privileges", "grants_query_failed", "Cannot finish inspecting account grants.")
	}
	return grants, nil
}

// No returned table values are logged, retained, or placed in diagnostics.
func drainPreflightQuery(ctx context.Context, db *sql.DB, query string) error {
	rows, err := db.QueryContext(ctx, query)
	if err != nil {
		return err
	}
	for rows.Next() {
	}
	if err := rows.Err(); err != nil {
		if closeErr := rows.Close(); closeErr != nil {
			return closeErr
		}
		return err
	}
	return rows.Close()
}

func checkSourceTables(ctx context.Context, db *sql.DB, result *PreflightResult) error {
	var nonInnoDB uint64
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM information_schema.tables
WHERE table_schema NOT IN ('mysql', 'sys', 'information_schema', 'performance_schema')
AND table_type = 'BASE TABLE' AND engine <> 'InnoDB'`).Scan(&nonInnoDB)
	if err != nil {
		return preflightExternalFailure(ctx, "tables", "tables_query_failed", "Cannot inspect user table engines; check global table visibility.")
	}
	if nonInnoDB != 0 {
		result.addWarning("non_innodb_tables_require_full_copy_and_locks", "Observed non-InnoDB user tables require full copying and stronger locks.")
	}
	return nil
}

func checkSourceKeyring(ctx context.Context, db *sql.DB, files []string) error {
	var encrypted uint64
	var redo, undo bool
	err := db.QueryRowContext(ctx, `SELECT
(SELECT COUNT(*) FROM information_schema.innodb_tablespaces WHERE encryption = 'Y'),
@@innodb_redo_log_encrypt, @@innodb_undo_log_encrypt`).Scan(&encrypted, &redo, &undo)
	if err != nil {
		return preflightExternalFailure(ctx, "keyring", "encryption_query_failed", "Cannot inspect tablespace, redo and undo encryption; check PROCESS privilege.")
	}
	if err := drainPreflightQuery(ctx, db, `SELECT STATUS_KEY, STATUS_VALUE FROM performance_schema.keyring_component_status`); err != nil {
		return preflightExternalFailure(ctx, "keyring", "keyring_query_failed", "Cannot inspect keyring component status; grant SELECT on performance_schema.keyring_component_status.")
	}
	return checkKeyringMaterials(ctx, encrypted != 0 || redo || undo, files)
}

func checkPageTracking(ctx context.Context, db *sql.DB, requested bool, result *PreflightResult) error {
	if !requested {
		return nil
	}
	var count uint64
	err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM mysql.component WHERE component_urn = 'file://component_mysqlbackup'`).Scan(&count)
	if err != nil {
		return preflightExternalFailure(ctx, "page_tracking", "page_tracking_query_failed", "Cannot inspect page tracking; grant SELECT on mysql.component.")
	}
	if count != 1 {
		return preflightFailure("page_tracking", "page_tracking_unavailable", "Install component_mysqlbackup before requesting page tracking, or disable page tracking.")
	}
	var startLSN uint64
	if err := db.QueryRowContext(ctx, `SELECT mysqlbackup_page_track_get_start_lsn()`).Scan(&startLSN); err != nil {
		return preflightExternalFailure(ctx, "page_tracking", "page_tracking_unavailable", "The page tracking component is registered but its read-only LSN probe failed; check component health and BACKUP_ADMIN.")
	}
	if startLSN == 0 {
		result.addWarning("page_tracking_starts_with_next_full_backup", "Page tracking is installed but not started; the next full backup will start tracking.")
	}
	return nil
}
