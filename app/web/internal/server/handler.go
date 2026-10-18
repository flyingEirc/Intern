package server

import (
	"net/http"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/middleware"
)

// Handler fixes the security boundary: cross-origin protection rejects browser
// requests before Gin. Session loading lives inside Gin so a rejected request
// is still request-logged, and /healthz and /readyz never touch the session store.
func Handler(engine *gin.Engine) http.Handler {
	protection := http.NewCrossOriginProtection()
	protection.SetDenyHandler(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Dispatch only the marked denial to the engine's first middleware. This
		// obtains the real Gin route/IP and aborts before all business handlers,
		// without ever loading a session.
		engine.ServeHTTP(w, middleware.CrossOriginDeniedRequest(r))
	}))
	return protection.Handler(engine)
}
