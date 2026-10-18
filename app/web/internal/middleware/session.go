package middleware

import (
	"bufio"
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/render"
)

// MaxBufferedResponseBytes is the largest handler body held until the session
// commit decision. Larger bodies and streaming writes become one 500 and do not
// commit session changes. The limit is not a truncation of a 200 response.
const MaxBufferedResponseBytes = 8 << 20

// ErrWriteAfterFlush is returned when a handler writes after the buffer has
// been sent to the client.
var ErrWriteAfterFlush = errors.New("write after flush")

var (
	errResponseTooLarge     = errors.New("response exceeds buffer limit")
	errStreamingUnsupported = errors.New("streaming responses are not supported")
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
	if buffered.overflow || buffered.streaming {
		rejectBuffered(c, manager, logger, buffered)
		return
	}
	// status 0 means the handler wrote nothing. Status() would report that as 200.
	// Only a real 5xx suppresses a new or modified session. A handler that already
	// destroyed its session still needs the clear-cookie; this does not call Destroy.
	if buffered.status >= 500 {
		if manager.Status(ctx) == scs.Destroyed {
			manager.WriteSessionCookie(ctx, buffered, "", time.Time{})
		}
		buffered.flush(logger, ctx)
		return
	}
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

func rejectBuffered(c *gin.Context, manager *scs.SessionManager, logger *slog.Logger, buffered *bufferedResponse) {
	ctx := c.Request.Context()
	requestID := c.Writer.Header().Get(render.RequestIDHeader)
	if buffered.streaming {
		logger.ErrorContext(ctx, "streaming responses are not supported", "request_id", requestID)
	} else {
		logger.ErrorContext(ctx, "response exceeded buffer limit", "request_id", requestID, "limit", MaxBufferedResponseBytes)
	}
	if manager.Status(ctx) == scs.Destroyed {
		manager.WriteSessionCookie(ctx, buffered, "", time.Time{})
	}
	buffered.prepareFailure()
	if err := render.HTTPError(buffered, http.StatusInternalServerError, render.Internal, "An internal error occurred"); err != nil {
		logger.ErrorContext(ctx, "cannot write error response", "error", err)
	}
	buffered.flush(logger, ctx)
}

func logSessionFailure(c *gin.Context, logger *slog.Logger, err error) {
	logger.ErrorContext(c.Request.Context(), "session persistence failed",
		"request_id", c.Writer.Header().Get(render.RequestIDHeader), "error", err)
}

// bufferedResponse holds the handler output until the session commit finishes.
// Streaming is unsupported. Flush cannot return an error, so it only marks the
// response; the final flush then rejects it instead of sending a partial 200.
type bufferedResponse struct {
	gin.ResponseWriter
	status    int
	body      []byte
	flushed   bool
	overflow  bool
	streaming bool
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
		return 0, ErrWriteAfterFlush
	}
	if b.streaming {
		return 0, errStreamingUnsupported
	}
	if b.overflow {
		return 0, errResponseTooLarge
	}
	if len(data) > MaxBufferedResponseBytes || len(b.body) > MaxBufferedResponseBytes-len(data) {
		b.overflow = true
		b.body = nil
		b.Header().Del("Content-Length")
		return 0, errResponseTooLarge
	}
	if b.status == 0 {
		b.status = http.StatusOK
	}
	b.body = append(b.body, data...)
	return len(data), nil
}

func (b *bufferedResponse) WriteString(s string) (int, error) { return b.Write([]byte(s)) }

func (b *bufferedResponse) Flush() { b.streaming = true }

func (b *bufferedResponse) Hijack() (net.Conn, *bufio.ReadWriter, error) {
	b.streaming = true
	return nil, nil, errStreamingUnsupported
}

func (b *bufferedResponse) Pusher() http.Pusher { return nil }

func (b *bufferedResponse) prepareFailure() {
	b.body = nil
	b.status = 0
	b.overflow = false
	b.streaming = false
	b.Header().Del("Content-Length")
	b.Header().Del("Content-Type")
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
