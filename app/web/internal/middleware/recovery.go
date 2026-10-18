package middleware

import (
	"fmt"
	"log/slog"
	"net/http"
	"runtime/debug"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/render"
)

// PanicValueLimit is the maximum panic text included in one error log.
const PanicValueLimit = 2048

func Recovery(logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		defer func() {
			value := recover()
			if value == nil {
				return
			}
			logger.ErrorContext(c.Request.Context(), "request handler panicked",
				"request_id", c.Writer.Header().Get(render.RequestIDHeader),
				"panic_value", panicValue(value),
				"stack", string(debug.Stack()))
			if c.Writer.Written() {
				c.Abort()
				return
			}
			render.Error(c, http.StatusInternalServerError, render.Internal, "An internal error occurred")
		}()
		c.Next()
	}
}

func panicValue(value any) string {
	text := fmt.Sprint(value)
	if len(text) <= PanicValueLimit {
		return text
	}
	return text[:PanicValueLimit]
}
