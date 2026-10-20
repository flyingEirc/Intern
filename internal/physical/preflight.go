package physical

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	mysql "github.com/go-sql-driver/mysql"
)

// PreflightOptions describes the mounted, local-socket source and capacity policy.
// Capacity thresholds are required, in bytes, and are not backup size estimates.
// KeyringFiles are inspected but cannot enable an unsupported keyring workflow.
type PreflightOptions struct {
	Connection     Connection
	Binary         string
	WorkRoot       string
	BackupRoot     string
	MinWorkBytes   uint64
	MinBackupBytes uint64
	PageTracking   bool
	KeyringFiles   []string
}

// PreflightResult contains observations, not a guarantee against later changes.
// Callers must rerun preflight immediately before every backup.
type PreflightResult struct {
	Identity       Identity
	Tool           ToolVersion
	Warnings       []string
	WarningDetails []PreflightWarning
	Compatibility  CompatibilityEntry
	Checks         []string
}

// PreflightWarning gives W3 a stable code and a secret-safe explanation.
type PreflightWarning struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (r *PreflightResult) addWarning(code, message string) {
	r.Warnings = append(r.Warnings, code)
	r.WarningDetails = append(r.WarningDetails, PreflightWarning{Code: code, Message: message})
}

// PreflightError exposes only fixed, actionable diagnostics. It never wraps SQL,
// filesystem or process errors, which can contain passwords, DSNs or key paths.
// Missing contains only known privilege names, never raw SHOW GRANTS output.
type PreflightError struct {
	Check   string   `json:"check"`
	Code    string   `json:"code"`
	Message string   `json:"message"`
	Missing []string `json:"missing,omitempty"`
	cause   error
}

func (e *PreflightError) Error() string { return e.Message }

// Unwrap preserves cancellation only; arbitrary external diagnostics stay private.
func (e *PreflightError) Unwrap() error { return e.cause }

func preflightFailure(check, code, message string) *PreflightError {
	return &PreflightError{Check: check, Code: code, Message: message}
}

func preflightExternalFailure(ctx context.Context, check, code, message string) *PreflightError {
	failure := preflightFailure(check, code, message)
	if err := ctx.Err(); err != nil {
		failure.Code = "canceled"
		failure.Message = "Preflight was canceled or its deadline expired."
		failure.cause = err
	}
	return failure
}

// Preflight executes real SQL and filesystem probes using the current OS identity.
// It neither installs components nor changes server configuration or key material.
func Preflight(ctx context.Context, opts PreflightOptions) (result PreflightResult, retErr error) {
	if err := validatePreflightOptions(opts); err != nil {
		return result, err
	}
	if err := ctx.Err(); err != nil {
		return result, preflightExternalFailure(ctx, "connection", "connection_failed", "Cannot connect to MySQL.")
	}
	db, err := openPreflightDB(opts.Connection)
	if err != nil {
		return result, err
	}
	defer func() {
		if closeErr := db.Close(); closeErr != nil {
			retErr = errors.Join(retErr, preflightFailure("connection", "close_failed", "Failed to close the preflight database connection."))
		}
	}()
	if err = readSourceIdentity(ctx, db, &result.Identity); err != nil {
		return result, err
	}
	result.Checks = append(result.Checks, "identity")
	result.Tool, err = ProbeVersion(ctx, opts.Binary)
	if err != nil {
		return result, preflightExternalFailure(ctx, "tool", "tool_probe_failed", "Cannot identify XtraBackup; check the configured executable and its permissions.")
	}
	result.Compatibility, err = matchingCompatibility(result.Identity, result.Tool, runtime.GOOS+"/"+runtime.GOARCH)
	if err != nil {
		return result, err
	}
	result.Checks = append(result.Checks, "compatibility")
	if !result.Compatibility.Verified {
		result.addWarning("compatibility_candidate_unverified", "This pinned combination is a candidate, not a verified supported combination.")
	}
	return runPreflightChecks(ctx, db, opts, result)
}

func runPreflightChecks(ctx context.Context, db *sql.DB, opts PreflightOptions, result PreflightResult) (PreflightResult, error) {
	checks := []struct {
		name string
		run  func() error
	}{
		{"volume", func() error { return checkDataVolume(ctx, opts.Connection.DataDir, result.Identity) }},
		{"privileges", func() error { return checkBackupPrivileges(ctx, db, &result) }},
		{"datafiles", func() error { return checkSourceDatafiles(ctx, db, opts.Connection.DataDir) }},
		{"tables", func() error { return checkSourceTables(ctx, db, &result) }},
		{"keyring", func() error { return checkSourceKeyring(ctx, db, opts.KeyringFiles) }},
		{"page_tracking", func() error { return checkPageTracking(ctx, db, opts.PageTracking, &result) }},
		{"capacity", func() error { return checkPreflightStorage(ctx, opts) }},
	}
	for _, check := range checks {
		if err := ctx.Err(); err != nil {
			return result, preflightExternalFailure(ctx, check.name, "canceled", "Preflight was canceled.")
		}
		if err := check.run(); err != nil {
			return result, err
		}
		result.Checks = append(result.Checks, check.name)
	}
	if err := ctx.Err(); err != nil {
		return result, preflightExternalFailure(ctx, "preflight", "canceled", "Preflight was canceled.")
	}
	return result, nil
}

func validatePreflightOptions(opts PreflightOptions) error {
	if opts.Connection.Username == "" || opts.Binary == "" {
		return preflightFailure("configuration", "configuration_invalid", "A database username and XtraBackup executable are required.")
	}
	for _, path := range []string{opts.Connection.Socket, opts.Connection.DataDir, opts.WorkRoot, opts.BackupRoot} {
		if !filepath.IsAbs(path) || strings.ContainsRune(path, 0) {
			return preflightFailure("configuration", "path_invalid", "Socket, datadir, work and backup paths must be absolute paths without NUL bytes.")
		}
	}
	if opts.MinWorkBytes == 0 || opts.MinBackupBytes == 0 {
		return preflightFailure("capacity", "capacity_policy_required", "Set positive minimum free-byte thresholds for both work and backup volumes.")
	}
	return nil
}

type preflightDiscardLogger struct{}

func (preflightDiscardLogger) Print(...any) {}

func openPreflightDB(conn Connection) (*sql.DB, error) {
	cfg := mysql.NewConfig()
	cfg.Net = "unix"
	cfg.Addr = conn.Socket
	cfg.User = conn.Username
	cfg.Passwd = conn.Password
	cfg.Timeout = 10 * time.Second
	cfg.ReadTimeout = 30 * time.Second
	cfg.WriteTimeout = 10 * time.Second
	cfg.Logger = preflightDiscardLogger{}
	connector, err := mysql.NewConnector(cfg)
	if err != nil {
		return nil, preflightFailure("connection", "connection_invalid", "The MySQL connection configuration is invalid.")
	}
	db := sql.OpenDB(connector)
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	return db, nil
}
