package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

var auditActions = map[string]string{
	"POST /api/v1/auth/login":           "auth.login",
	"POST /api/v1/auth/logout":          "auth.logout",
	"PUT /api/v1/auth/password":         "auth.password",
	"POST /api/v1/users":                "users.create",
	"PATCH /api/v1/users/:id":           "users.update",
	"DELETE /api/v1/users/:id/sessions": "users.revoke_sessions",
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

func Audit(database *store.Store, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		c.Next()
		if !isAPIPath(c.Request.URL.Path) || !IsMutation(c.Request.Method) {
			return
		}
		entry := auditEntry(c)
		writeAudit(c.Request.Context(), database, logger, entry)
	}
}

func writeAudit(requestContext context.Context, database *store.Store, logger *slog.Logger, entry store.AuditLog) {
	// A disconnected client must not suppress the completed request's audit.
	ctx, cancel := context.WithTimeout(context.WithoutCancel(requestContext), 5*time.Second)
	defer cancel()
	if err := database.Audit(ctx, entry); err != nil {
		logger.ErrorContext(ctx, "audit write failed", "request_id", entry.RequestID, "error", err)
	}
}

func auditEntry(c *gin.Context) store.AuditLog {
	data := auditData(c)
	action, ok := AuditAction(c.Request.Method, c.FullPath())
	if !ok {
		action = "http.unmatched"
	}
	status := int64(c.Writer.Status())
	entry := store.AuditLog{ActorType: "anonymous", ActorName: data.username, Action: action,
		ResourceType: data.resourceType, ResourceID: data.resourceID, Detail: data.detail,
		IP: c.ClientIP(), Status: &status, Result: auditResult(int(status)),
		RequestID: c.Writer.Header().Get(render.RequestIDHeader)}
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
