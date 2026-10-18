package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strings"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

type crossOriginDeniedKey struct{}

// CrossOriginDeniedRequest marks only requests rejected by the outer standard
// library protection. The marker is not derived from a client-controlled header.
func CrossOriginDeniedRequest(r *http.Request) *http.Request {
	return r.WithContext(context.WithValue(r.Context(), crossOriginDeniedKey{}, true))
}

// CrossOriginRejection runs after request logging and before session loading.
// The outer deny handler dispatches directly here. Using the same engine obtains
// Gin's exact route template and trusted-proxy ClientIP without duplicating its
// router or proxy parsing, and aborts before any endpoint or session store.
func CrossOriginRejection(database *store.Store, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		if c.Request.Context().Value(crossOriginDeniedKey{}) != true {
			c.Next()
			return
		}
		render.Error(c, http.StatusForbidden, render.CrossOriginRejected, "Cross-origin request rejected")
		if !isAPIPath(c.Request.URL.Path) || !IsMutation(c.Request.Method) {
			return
		}
		route := c.FullPath()
		if route == "" {
			route = "unmatched"
		}
		status := int64(http.StatusForbidden)
		writeAudit(c.Request.Context(), database, logger, store.AuditLog{
			ActorType: "anonymous", Action: "security.cross_origin_rejected", Result: "denied", Status: &status,
			RequestID: c.Writer.Header().Get(render.RequestIDHeader), IP: c.ClientIP(),
			Detail: store.AuditDetail{Method: c.Request.Method, Route: route},
		})
	}
}

func isAPIPath(path string) bool { return path == "/api/v1" || strings.HasPrefix(path, "/api/v1/") }
