package server

import (
	"context"
	"errors"
	"net/http"
	"regexp"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// RestoreControl is the restore dispatch surface the HTTP layer may call.
// Reading restore rows and logs is a plain store read; only plan, execute and
// cancel reach the dispatcher.
type RestoreControl interface {
	PlanRestore(ctx context.Context, backupID string) (store.RestorePlan, error)
	ExecuteRestore(ctx context.Context, in dispatch.RestoreExecute) (dispatch.RestoreOutcome, dispatch.RestoreDecision)
	RequestRestoreCancel(ctx context.Context, restoreID string) error
	RestoreConfigured() bool
}

// restorePlanDigest is the exact plan digest shape: lowercase sha256 hex.
var restorePlanDigest = regexp.MustCompile(`^[0-9a-f]{64}$`)

// maxVerifySQLStatements bounds the read-only verification statements one
// restore may carry. Each is executed separately in a read-only transaction.
const maxVerifySQLStatements = 20

type restoreHandlers struct {
	database       *store.Store
	control        RestoreControl
	authentication *auth.Service
	passwords      *auth.PasswordLimiter
	logger         logger
}

type restorePlanRequest struct {
	BackupID string `json:"backup_id"`
}

type restoreExecuteRequest struct {
	BackupID   string   `json:"backup_id"`
	PlanDigest string   `json:"plan_digest"`
	Password   string   `json:"password"`
	Rehearse   bool     `json:"rehearse"`
	VerifySQL  []string `json:"verify_sql"`
}

type restoreAccepted struct {
	RestoreID string `json:"restore_id"`
}

type restoresPage struct {
	Items      []store.Restore `json:"items"`
	NextBefore *string         `json:"next_before"`
}

type restoreLogsPage struct {
	Items     []store.RestoreLog `json:"items"`
	NextAfter *int64             `json:"next_after"`
	Truncated bool               `json:"truncated"`
}

// registerRestores wires the W5 restore API onto the engine. The group is only
// registered when a restore control is attached; production serve always
// attaches the dispatcher, and an unconfigured deployment still answers 503 for
// plan and execute.
func registerRestores(engine *gin.Engine, database *store.Store, control RestoreControl,
	authentication *auth.Service, permissions middleware.Authorization, lg logger,
) {
	h := restoreHandlers{
		database: database, control: control, authentication: authentication,
		passwords: auth.NewPasswordLimiter(), logger: lg,
	}
	engine.POST("/api/v1/restores/plan", permissions.RequirePermission(auth.RestoresPlan), h.plan)
	engine.POST("/api/v1/restores", permissions.RequirePermission(auth.RestoresExecute), h.execute)
	engine.POST("/api/v1/restores/:id/cancel", permissions.RequirePermission(auth.RestoresExecute), h.cancel)

	group := engine.Group("/api/v1/restores", permissions.RequirePermission(auth.RestoresRead))
	group.GET("", h.restores)
	group.GET("/:id", h.restore)
	group.GET("/:id/logs", h.restoreLogs)
}

// plan builds a fresh, read-only restore plan. It writes no restore row, no
// file and no FTP object.
func (h restoreHandlers) plan(c *gin.Context) {
	var request restorePlanRequest
	if !render.DecodeJSON(c, &request) {
		return
	}
	if _, err := parseUUID(request.BackupID); err != nil {
		validationError(c, errors.New("backup_id must be a resource ID"))
		return
	}
	middleware.AuditResource(c, "backup", request.BackupID, store.AuditDetail{})
	plan, err := h.control.PlanRestore(c.Request.Context(), request.BackupID)
	if err != nil {
		h.planError(c, err)
		return
	}
	render.JSON(c, http.StatusOK, plan)
}

// planError maps the engine's fixed refusal codes onto the frozen statuses.
func (h restoreHandlers) planError(c *gin.Context, err error) {
	var planErr *dispatch.RestorePlanError
	if !errors.As(err, &planErr) {
		internalError(c)
		return
	}
	switch planErr.Code {
	case dispatch.RestoreCodeUnconfigured:
		unavailableDependency(c)
	case dispatch.RestoreCodeBackupNotFound:
		render.ErrorRefused(c, http.StatusNotFound, render.RestoreRefused, "Backup not found", planErr.Code)
	case dispatch.RestoreCodeMatrixUnsupported:
		render.ErrorRefused(c, http.StatusUnprocessableEntity, render.RestoreRefused, "Restore refused", planErr.Code)
	default:
		render.ErrorRefused(c, http.StatusConflict, render.RestoreRefused, "Restore refused", planErr.Code)
	}
}

// execute creates one restore behind the Idempotency-Key contract. It requires
// an administrator's password re-entry, refuses a stale plan digest, and
// commits the restore row with its success audit in one transaction.
func (h restoreHandlers) execute(c *gin.Context) {
	var request restoreExecuteRequest
	if !render.DecodeJSON(c, &request) {
		return
	}
	if err := validateRestoreExecute(request); err != nil {
		validationError(c, err)
		return
	}
	key := c.GetHeader("Idempotency-Key")
	if len(key) > 128 {
		validationError(c, errors.New("Idempotency-Key must be at most 128 characters"))
		return
	}
	user := middleware.CurrentUser(c)
	middleware.AuditResource(c, "backup", request.BackupID, store.AuditDetail{})
	if !allowPasswordAttempt(c, h.passwords, user.ID) {
		return
	}
	valid, ok := verifiedCurrentPassword(c, h.authentication.Passwords, request.Password, user.PasswordHash)
	if !ok {
		return
	}
	if !valid {
		// The audit middleware records the 403 as a denied restores.execute
		// row; no restore row is created.
		render.Error(c, http.StatusForbidden, render.ConfirmationFailed, "Password confirmation failed")
		return
	}
	requestHash := dispatch.MarshalRestoreRequest(request.BackupID, request.PlanDigest, request.Rehearse, request.VerifySQL)
	outcome, decision := h.control.ExecuteRestore(c.Request.Context(), dispatch.RestoreExecute{
		BackupID: request.BackupID, PlanDigest: request.PlanDigest, Rehearse: request.Rehearse,
		VerifySQL: request.VerifySQL, IdempotencyKey: key, RequestHash: requestHash, ActorID: user.ID,
		Audit: middleware.SuccessAudit(c, "restores.execute", http.StatusAccepted, "restore", "", store.AuditDetail{}),
	})
	h.renderExecute(c, outcome, decision)
}

// validateRestoreExecute rejects a body that cannot be executed before any
// password work. verify_sql entries are single read-only SELECT statements.
func validateRestoreExecute(request restoreExecuteRequest) error {
	if _, err := parseUUID(request.BackupID); err != nil {
		return errors.New("backup_id must be a resource ID")
	}
	if !restorePlanDigest.MatchString(request.PlanDigest) {
		return errors.New("plan_digest must be 64 lowercase hexadecimal characters")
	}
	if request.Password == "" {
		return errors.New("password is required")
	}
	if len(request.VerifySQL) > maxVerifySQLStatements {
		return errors.New("verify_sql accepts at most 20 statements")
	}
	for _, statement := range request.VerifySQL {
		if err := validateVerifySQL(statement); err != nil {
			return err
		}
	}
	return nil
}

// validateVerifySQL accepts one non-empty single SELECT statement without a
// second statement separator. The engine executes it in a read-only
// transaction; this is a lexical bound, not a SQL parser.
func validateVerifySQL(statement string) error {
	trimmed := strings.TrimSpace(statement)
	if trimmed == "" || len(trimmed) > 8192 {
		return errors.New("verify_sql statements must be 1 to 8192 bytes")
	}
	if strings.Contains(strings.TrimSuffix(trimmed, ";"), ";") {
		return errors.New("verify_sql accepts one statement per entry")
	}
	lowered := strings.ToLower(strings.TrimSuffix(trimmed, ";"))
	if !strings.HasPrefix(lowered, "select") {
		return errors.New("verify_sql statements must be read-only SELECT statements")
	}
	return nil
}

// renderExecute maps the frozen execute outcomes onto the wire.
func (h restoreHandlers) renderExecute(c *gin.Context, outcome dispatch.RestoreOutcome, decision dispatch.RestoreDecision) {
	switch outcome {
	case dispatch.RestoreCreated:
		render.JSON(c, http.StatusAccepted, restoreAccepted{RestoreID: decision.RestoreID})
		// The row and its success audit committed together in the store.
		middleware.MarkAuditRecorded(c)
	case dispatch.RestoreReplayed:
		render.JSON(c, http.StatusAccepted, restoreAccepted{RestoreID: decision.RestoreID})
	case dispatch.RestoreRefused:
		render.ErrorRefused(c, http.StatusConflict, render.RestoreRefused, "Restore refused", decision.Code)
	case dispatch.RestoreRejected:
		render.ErrorRefused(c, http.StatusUnprocessableEntity, render.RestoreRefused, "Restore refused", decision.Code)
	case dispatch.RestoreNotFound:
		render.ErrorRefused(c, http.StatusNotFound, render.RestoreRefused, "Backup not found", decision.Code)
	case dispatch.RestorePlanChanged:
		render.Error(c, http.StatusConflict, render.PlanChanged, "The restore plan changed; request a fresh plan")
	case dispatch.RestoreUnavailable:
		unavailableDependency(c)
	case dispatch.RestoreValidation:
		render.Error(c, http.StatusUnprocessableEntity, render.ValidationFailed,
			"A rehearsal requires mysqld_path to be configured")
	default:
		internalError(c)
	}
}

func (h restoreHandlers) restores(c *gin.Context) {
	before, limit, ok := resourcePagination(c, "restore")
	if !ok {
		return
	}
	filter := store.RestoresFilter{Before: before}
	if value, exists := c.GetQuery("backup_id"); exists {
		if _, err := parseUUID(value); err != nil {
			validationError(c, errors.New("backup_id must be a resource ID"))
			return
		}
		filter.BackupID = value
	}
	if value, exists := c.GetQuery("status"); exists {
		if !validRestoreStatus(value) {
			validationError(c, errors.New("status must be one of queued, running, completed, failed, canceled"))
			return
		}
		filter.Status = value
	}
	filter.Limit = limit + 1
	items, err := h.database.Restores(c.Request.Context(), filter)
	if err != nil {
		internalError(c)
		return
	}
	page := restoresPage{Items: items}
	if len(items) > limit {
		page.Items = items[:limit]
		last := page.Items[limit-1].ID
		page.NextBefore = &last
	}
	render.JSON(c, http.StatusOK, page)
}

func (h restoreHandlers) restore(c *gin.Context) {
	restore, err := h.database.Restore(c.Request.Context(), c.Param("id"))
	if err != nil {
		h.restoreError(c, err)
		return
	}
	render.JSON(c, http.StatusOK, restore)
}

func (h restoreHandlers) restoreLogs(c *gin.Context) {
	restoreID := c.Param("id")
	if _, err := h.database.Restore(c.Request.Context(), restoreID); err != nil {
		h.restoreError(c, err)
		return
	}
	limit, after, ok := logWindow(c)
	if !ok {
		return
	}
	items, err := h.database.RestoreLogsAfter(c.Request.Context(), restoreID, after, limit)
	if err != nil {
		internalError(c)
		return
	}
	page := restoreLogsPage{Items: items}
	if len(items) > 0 {
		last := items[len(items)-1].Seq
		page.NextAfter = &last
		page.Truncated = h.restoreLogTruncated(c, restoreID)
	}
	render.JSON(c, http.StatusOK, page)
}

// restoreLogTruncated reports whether the newest stored row is the truncated
// marker. A read failure is reported as false: the page itself already
// succeeded.
func (h restoreHandlers) restoreLogTruncated(c *gin.Context, restoreID string) bool {
	maxSeq, err := h.database.MaxRestoreLogSeq(c.Request.Context(), restoreID)
	if err != nil || maxSeq <= 0 {
		return false
	}
	last, err := h.database.RestoreLog(c.Request.Context(), restoreID, maxSeq)
	return err == nil && last.Message == store.RestoreTruncatedMessage
}

func (h restoreHandlers) cancel(c *gin.Context) {
	id := c.Param("id")
	if _, err := h.database.Restore(c.Request.Context(), id); err != nil {
		h.restoreError(c, err)
		return
	}
	middleware.AuditResource(c, "restore", id, store.AuditDetail{})
	err := h.control.RequestRestoreCancel(c.Request.Context(), id)
	switch {
	case err == nil:
		render.JSON(c, http.StatusAccepted, restoreAccepted{RestoreID: id})
	case errors.Is(err, store.ErrConflict):
		render.Error(c, http.StatusConflict, render.Conflict, "Restore is already terminal")
	case errors.Is(err, store.ErrNotFound):
		render.Error(c, http.StatusNotFound, render.NotFound, "Restore not found")
	default:
		internalError(c)
	}
}

func (h restoreHandlers) restoreError(c *gin.Context, err error) {
	if errors.Is(err, store.ErrNotFound) {
		render.Error(c, http.StatusNotFound, render.NotFound, "Restore not found")
		return
	}
	internalError(c)
}

// validRestoreStatus reports whether value is one of the closed restore
// statuses.
func validRestoreStatus(value string) bool {
	switch value {
	case store.RestoreStatusQueued, store.RestoreStatusRunning, store.RestoreStatusCompleted,
		store.RestoreStatusFailed, store.RestoreStatusCanceled:
		return true
	default:
		return false
	}
}
