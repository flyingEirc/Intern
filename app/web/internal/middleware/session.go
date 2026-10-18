package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"strconv"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/render"
)

// Session loads and saves the scs session inside Gin, after cross-origin
// rejection and before the handler. /healthz and /readyz skip the store entirely.
// The handler's bytes stay buffered until Commit returns, so a failed commit
// writes one 500 and the request log observes that status.
func Session(manager *scs.SessionManager, logger *slog.Logger) gin.HandlerFunc {
	return func(c *gin.Context) {
		path := c.Request.URL.Path
		if path == "/healthz" || path == "/readyz" {
			c.Next()
			return
		}
		c.Writer.Header().Add("Vary", "Cookie")
		token := ""
		if cookie, err := c.Request.Cookie(manager.Cookie.Name); err == nil {
			token = cookie.Value
		}
		ctx, err := manager.Load(c.Request.Context(), token)
		if err != nil {
			logSessionFailure(c, logger, err)
			render.Error(c, http.StatusInternalServerError, render.Internal, "An internal error occurred")
			return
		}
		c.Request = c.Request.WithContext(ctx)
		buffered := &bufferedResponse{ResponseWriter: c.Writer}
		c.Writer = buffered
		c.Next()
		saveSession(c, manager, logger, buffered)
	}
}

func saveSession(c *gin.Context, manager *scs.SessionManager, logger *slog.Logger, buffered *bufferedResponse) {
	if buffered.flushed {
		return
	}
	ctx := c.Request.Context()
	switch manager.Status(ctx) {
	case scs.Modified:
		token, expiry, err := manager.Commit(ctx)
		if err != nil {
			logSessionFailure(c, logger, err)
			buffered.prepareFailure()
			if writeErr := render.HTTPError(buffered, http.StatusInternalServerError, render.Internal, "An internal error occurred"); writeErr != nil {
				logger.ErrorContext(ctx, "cannot write error response", "error", writeErr)
			}
			buffered.flush(logger, ctx)
			return
		}
		manager.WriteSessionCookie(ctx, buffered, token, expiry)
	case scs.Destroyed:
		manager.WriteSessionCookie(ctx, buffered, "", time.Time{})
	}
	buffered.flush(logger, ctx)
}

func logSessionFailure(c *gin.Context, logger *slog.Logger, err error) {
	logger.ErrorContext(c.Request.Context(), "session persistence failed",
		"request_id", c.Writer.Header().Get(render.RequestIDHeader), "error", err)
}

// bufferedResponse holds the handler output until the session commit finishes.
type bufferedResponse struct {
	gin.ResponseWriter
	status  int
	body    []byte
	flushed bool
}

func (b *bufferedResponse) Status() int {
	if b.status == 0 {
		return http.StatusOK
	}
	return b.status
}

func (b *bufferedResponse) Size() int {
	if b.status == 0 && len(b.body) == 0 {
		return -1
	}
	return len(b.body)
}

func (b *bufferedResponse) Written() bool { return b.status != 0 || len(b.body) > 0 }

func (b *bufferedResponse) WriteHeader(code int) {
	if b.flushed || code <= 0 {
		return
	}
	b.status = code
}

func (b *bufferedResponse) WriteHeaderNow() {}

func (b *bufferedResponse) Write(data []byte) (int, error) {
	if b.flushed {
		return len(data), nil
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	b.body = append(b.body, data...)
	return len(data), nil
}

func (b *bufferedResponse) WriteString(s string) (int, error) { return b.Write([]byte(s)) }

func (b *bufferedResponse) Flush() {}

func (b *bufferedResponse) prepareFailure() {
	b.body = nil
	b.status = 0
	b.Header().Del("Content-Length")
}

func (b *bufferedResponse) flush(logger *slog.Logger, ctx context.Context) {
	if b.flushed {
		return
	}
	b.flushed = true
	status := b.Status()
	if len(b.body) > 0 && b.Header().Get("Content-Length") == "" {
		b.Header().Set("Content-Length", strconv.Itoa(len(b.body)))
	}
	b.ResponseWriter.WriteHeader(status)
	b.ResponseWriter.WriteHeaderNow()
	if len(b.body) == 0 {
		return
	}
	if _, err := b.ResponseWriter.Write(b.body); err != nil {
		logger.ErrorContext(ctx, "cannot write response", "error", err)
	}
}
