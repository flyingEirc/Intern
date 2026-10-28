package restore

import (
	"errors"
	"fmt"
)

// PlanError codes. They are part of the W5 API contract and must not be renamed
// or reused for a different refusal.
const (
	CodeBackupNotFound      = "backup_not_found"
	CodeBackupNotCompleted  = "backup_not_completed"
	CodeChainBroken         = "chain_broken"
	CodeLSNDiscontinuous    = "lsn_discontinuous"
	CodeSourceUnavailable   = "source_unavailable"
	CodeToolVersionMismatch = "tool_version_mismatch"
	CodeMatrixUnsupported   = "matrix_unsupported"
	CodeKeyUnavailable      = "key_unavailable"
	CodeInsufficientSpace   = "insufficient_space"
	CodeUnconfigured        = "unconfigured"
	CodePlanChanged         = "plan_changed"
)

// ExecError codes. They classify a refused or failed execution; a published
// export is never reported as a failure of the data, only of the rehearsal.
const (
	CodeCanceled             = "canceled"
	CodeAuthenticationFailed = "authentication_failed"
	CodeFetchFailed          = "fetch_failed"
	CodeExtractFailed        = "extract_failed"
	CodePrepareFailed        = "prepare_failed"
	CodeExportFailed         = "export_failed"
	CodeDiskFull             = "disk_full"
	CodeRehearsalUnavailable = "rehearsal_unavailable"
	CodeWorkFailed           = "work_failed"
	CodeCleanupFailed        = "cleanup_failed"
	CodeInvalidRequest       = "invalid_request"
	CodeInvalidVerifySQL     = "invalid_verify_sql"
)

// Risk codes are fixed strings. They are advisory: a plan never claims that a
// restore replaces production safely or that point-in-time recovery exists.
const (
	RiskNoAutoReplaceProduction = "no_auto_replace_production"
	RiskServerUUIDDuplicate     = "server_uuid_duplicate"
	RiskPITRUnavailable         = "pitr_unavailable"
	RiskRehearsalUnavailable    = "rehearsal_unavailable"
)

// PlanError carries a fixed machine code and the refused backup.
type PlanError struct {
	Code     string
	BackupID string
}

func (e *PlanError) Error() string {
	return fmt.Sprintf("restore: plan refused: %s", e.Code)
}

func planRefusal(code, backupID string) error { return &PlanError{Code: code, BackupID: backupID} }

// ExecError reports a refused or failed execution with its exact side effects.
type ExecError struct {
	Code        string
	Phase       string
	SideEffects SideEffects
	cause       error
}

func (e *ExecError) Error() string {
	return fmt.Sprintf("restore: execution failed: %s during %s", e.Code, e.Phase)
}

// Unwrap preserves cancellation and I/O classification for callers without
// exposing tool diagnostics; the cause is never rendered by Error.
func (e *ExecError) Unwrap() error { return e.cause }

func execFailure(code string, phase Phase, side SideEffects, cause error) error {
	return &ExecError{Code: code, Phase: string(phase), SideEffects: side, cause: cause}
}

// IsPlanError reports whether err is a refusal that W5 maps to 422/404/409.
func IsPlanError(err error) bool {
	var refusal *PlanError
	return errors.As(err, &refusal)
}
