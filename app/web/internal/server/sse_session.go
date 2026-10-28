package server

import (
	"net/http"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
)

// sseSession authenticates the streaming route group without the 8 MiB
// buffered response writer. The Session middleware has already loaded the scs
// session into the request context and skipped its buffer for this path, so
// this handler performs exactly the shared authenticated load — user, disabled
// and epoch checks, the same code path as every JSON route — plus the
// runs:read permission check, all before any header byte. Failures are
// ordinary JSON 401/403/500 responses; after the first streamed byte errors
// only close the stream.
func sseSession(authentication *middleware.Authorization, permission string) gin.HandlerFunc {
	return func(c *gin.Context) {
		if _, ok := authentication.Authenticate(c); !ok {
			return
		}
		if !auth.HasPermission(middleware.CurrentUser(c).Role, permission) {
			render.Error(c, http.StatusForbidden, render.Forbidden, "Permission denied")
			return
		}
		c.Next()
	}
}

// commitStreamSession keeps the session row and cookie in step for a GET
// stream. The stream handler never modifies session data, so only the state
// the session middleware left behind is persisted: an idle-timeout touch
// commits its refreshed expiry, a destroy writes the clearing cookie. The
// cookie headers ride the response before the first event byte.
func commitStreamSession(manager *scs.SessionManager, c *gin.Context) {
	ctx := c.Request.Context()
	switch manager.Status(ctx) {
	case scs.Modified:
		// A failed commit leaves the stored row as is; the refreshed expiry
		// simply does not extend and the stream continues.
		if _, _, err := manager.Commit(ctx); err != nil {
			return
		}
	case scs.Destroyed:
		manager.WriteSessionCookie(ctx, c.Writer, "", time.Time{})
	}
}

// sseMediaType is the exact media type the events route answers with.
const sseMediaType = "text/event-stream"
