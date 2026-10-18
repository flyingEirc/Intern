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
			failPanickedRequest(c, logger)
		}()
		c.Next()
	}
}

// failPanickedRequest discards a buffered partial body and writes one JSON 500
// when the response has not reached the client. It does not destroy a session:
// an already-destroyed session stays destroyed, and a modified session is left
// uncommitted because the session middleware does not run its save after a panic
// that unwinds it. Bytes already flushed cannot be replaced.
func failPanickedRequest(c *gin.Context, logger *slog.Logger) {
	if buffered, ok := c.Writer.(*bufferedResponse); ok {
		if buffered.flushed {
			c.Abort()
			logger.ErrorContext(c.Request.Context(), "panic after the response was flushed",
				"request_id", c.Writer.Header().Get(render.RequestIDHeader))
			return
		}
		buffered.prepareFailure()
		if err := render.HTTPError(buffered, http.StatusInternalServerError, render.Internal, "An internal error occurred"); err != nil {
			logger.ErrorContext(c.Request.Context(), "cannot write error response", "error", err)
		}
		buffered.flush(logger, c.Request.Context())
		c.Abort()
		return
	}
	if c.Writer.Written() {
		c.Abort()
		logger.ErrorContext(c.Request.Context(), "panic after the response was written",
			"request_id", c.Writer.Header().Get(render.RequestIDHeader))
		return
	}
	render.Error(c, http.StatusInternalServerError, render.Internal, "An internal error occurred")
}

func panicValue(value any) string {
	text := fmt.Sprint(value)
	if len(text) <= PanicValueLimit {
		return text
	}
	return text[:PanicValueLimit]
}
