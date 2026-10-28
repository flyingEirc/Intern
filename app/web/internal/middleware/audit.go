package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"sync"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// AuditTimeout is the context budget for a detached audit write after the
// request context is canceled. Client disconnects still get the full budget;
// server shutdown can shorten it via DetachedAudit.Bound. It is not a hard cap
// on a SQLite call already inside the driver: the modernc busy callback does
// not observe sqlite3_interrupt, so that call can continue for store.BusyTimeout.
// Shutdown waits for this budget plus that busy timeout before closing the database.
const AuditTimeout = 5 * time.Second

const auditRecordedKey = "intern.audit_recorded"

var auditActions = map[string]string{
	"POST /api/v1/auth/login":           "auth.login",
	"POST /api/v1/auth/logout":          "auth.logout",
	"PUT /api/v1/auth/password":         "auth.password",
	"POST /api/v1/users":                "users.create",
	"PATCH /api/v1/users/:id":           "users.update",
	"DELETE /api/v1/users/:id/sessions": "users.revoke_sessions",

	"POST /api/v1/repositories":           "repositories.create",
	"PUT /api/v1/repositories/:id":        "repositories.update",
	"DELETE /api/v1/repositories/:id":     "repositories.delete",
	"POST /api/v1/repositories/:id/probe": "repositories.probe",

	"POST /api/v1/sources":               "sources.create",
	"PUT /api/v1/sources/:id":            "sources.update",
	"DELETE /api/v1/sources/:id":         "sources.delete",
	"POST /api/v1/sources/:id/preflight": "sources.preflight",

	"POST /api/v1/policies":             "policies.create",
	"PUT /api/v1/policies/:id":          "policies.update",
	"DELETE /api/v1/policies/:id":       "policies.delete",
	"POST /api/v1/policies/:id/enable":  "policies.enable",
	"POST /api/v1/policies/:id/disable": "policies.disable",

	"POST /api/v1/policies/:id/runs":                         "runs.trigger",
	"POST /api/v1/runs/:id/cancel":                           "runs.cancel",
	"POST /api/v1/backups/:id/replicas/:repository_id/retry": "replicas.retry",

	"POST /api/v1/restores/plan":       "restores.plan",
	"POST /api/v1/restores":            "restores.execute",
	"POST /api/v1/restores/:id/cancel": "restores.cancel",
}

const auditContextKey = "intern.audit"

type auditContext struct {
	username     string
	resourceType string
	resourceID   string
	detail       store.AuditDetail
}

func AuditAction(method, route string) (string, bool) {
	action, ok := auditActions[method+" "+route]
	return action, ok
}

func IsMutation(method string) bool {
	return method == http.MethodPost || method == http.MethodPut || method == http.MethodPatch || method == http.MethodDelete
}

func LoginAttempt(c *gin.Context, username string) {
	entry := auditData(c)
	entry.username = truncateUsername(username)
}

func truncateUsername(username string) string {
	if len(username) <= 64 {
		return username
	}
	runes := []rune(username)
	return string(runes[:min(len(runes), 64)])
}

func AuditResource(c *gin.Context, kind, id string, detail store.AuditDetail) {
	entry := auditData(c)
	entry.resourceType, entry.resourceID, entry.detail = kind, id, detail
}

func auditData(c *gin.Context) *auditContext {
	if value, ok := c.Get(auditContextKey); ok {
		entry, valid := value.(*auditContext)
		if !valid {
			panic("invalid audit context")
		}
		return entry
	}
	entry := &auditContext{}
	c.Set(auditContextKey, entry)
	return entry
}

// RecordOutcome writes one audit row with a caller-chosen result and then marks
// the request as recorded. It is for operations that answer 2xx while reporting a
// failed outcome in the body, such as a completed preflight whose checks failed:
// the middleware would otherwise label that request "success". A write failure is
// logged and does not change the response.
func RecordOutcome(c *gin.Context, database *store.Store, logger *slog.Logger, result string, status int, action, resourceType, resourceID string, detail store.AuditDetail) {
	entry := auditEntry(c)
	entry.Action, entry.ResourceType, entry.ResourceID = action, resourceType, resourceID
	entry.Detail, entry.Result = detail, result
	code := int64(status)
	entry.Status = &code
	writeAudit(c.Request.Context(), database, logger, entry)
	MarkAuditRecorded(c)
}

// MarkAuditRecorded tells Audit to skip its best-effort row because the handler
// already committed that success audit in the same transaction as the mutation.
func MarkAuditRecorded(c *gin.Context) { c.Set(auditRecordedKey, true) }

func auditRecorded(c *gin.Context) bool {
	recorded, _ := c.Get(auditRecordedKey)
	return recorded == true
}

// SuccessAudit builds the success row for a security mutation. Login cookie
// issuance and session-token rotation are not included: those happen outside
// the database transaction and must not be described as audit-atomic.
func SuccessAudit(c *gin.Context, action string, status int, resourceType, resourceID string, detail store.AuditDetail) store.AuditLog {
	code := int64(status)
	entry := store.AuditLog{
		ActorType: "anonymous", Action: action, ResourceType: resourceType, ResourceID: resourceID,
		Detail: detail, IP: c.ClientIP(), Result: "success", Status: &code,
		RequestID: c.Writer.Header().Get(render.RequestIDHeader),
	}
	if _, exists := c.Get(currentUserKey); exists {
		user := CurrentUser(c)
		entry.ActorType, entry.ActorID, entry.ActorName = "user", user.ID, user.Username
	}
	return entry
}

func Audit(database *store.Store, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			if !isAPIPath(c.Request.URL.Path) || !IsMutation(c.Request.Method) || auditRecorded(c) {
				return
			}
			writeAudit(c.Request.Context(), database, logger, auditEntry(c))
		}()
		c.Next()
	}
}

func writeAudit(requestContext context.Context, database *store.Store, logger *slog.Logger, entry store.AuditLog) {
	// A disconnected client must not suppress the completed request's audit.
	// The timeout is only the context budget. A lock wait already inside
	// SQLite can outlive it; shutdown keeps the database open through that wait.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestContext), detachedTimeout(requestContext))
	defer cancel()
	if err := database.Audit(ctx, entry); err != nil {
		logger.ErrorContext(ctx, "audit write failed", "request_id", entry.RequestID, "error", err)
	}
}

// DetachedAudit caps audit writes that outlive the request context during shutdown.
type DetachedAudit struct {
	mu       sync.Mutex
	deadline time.Time
}

func NewDetachedAudit() *DetachedAudit { return &DetachedAudit{} }

type detachedAuditKey struct{}

func (d *DetachedAudit) WithContext(ctx context.Context) context.Context {
	if d == nil {
		return ctx
	}
	return context.WithValue(ctx, detachedAuditKey{}, d)
}

func DetachedAuditFrom(ctx context.Context) (*DetachedAudit, bool) {
	audit, ok := ctx.Value(detachedAuditKey{}).(*DetachedAudit)
	return audit, ok && audit != nil
}

// Bound starts the shutdown deadline for audits that have not yet called the
// driver. Audits that start after Bound see the remaining context time, at most
// AuditTimeout from this call. A SQLite call that has already entered the busy
// handler does not stop when that context expires.
func (d *DetachedAudit) Bound(limit time.Duration) {
	if d == nil {
		return
	}
	d.mu.Lock()
	d.deadline = time.Now().Add(limit)
	d.mu.Unlock()
}

func (d *DetachedAudit) timeout(max time.Duration) time.Duration {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.deadline.IsZero() {
		return max
	}
	remaining := time.Until(d.deadline)
	if remaining <= 0 {
		return 0
	}
	if remaining < max {
		return remaining
	}
	return max
}

func detachedTimeout(ctx context.Context) time.Duration {
	audit, ok := DetachedAuditFrom(ctx)
	if !ok {
		return AuditTimeout
	}
	return audit.timeout(AuditTimeout)
}

func auditEntry(c *gin.Context) store.AuditLog {
	data := auditData(c)
	action, ok := AuditAction(c.Request.Method, c.FullPath())
	if !ok {
		action = "http.unmatched"
	}
	status := int64(c.Writer.Status())
	entry := store.AuditLog{
		ActorType: "anonymous", ActorName: data.username, Action: action,
		ResourceType: data.resourceType, ResourceID: data.resourceID, Detail: data.detail,
		IP: c.ClientIP(), Status: &status, Result: auditResult(int(status)),
		RequestID: c.Writer.Header().Get(render.RequestIDHeader),
	}
	if _, exists := c.Get(currentUserKey); exists {
		user := CurrentUser(c)
		entry.ActorType, entry.ActorID, entry.ActorName = "user", user.ID, user.Username
	}
	return entry
}

func auditResult(status int) string {
	if status >= 200 && status < 300 {
		return "success"
	}
	if status == 401 || status == 403 || status == 429 {
		return "denied"
	}
	return "failed"
}
