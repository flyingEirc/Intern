package middleware

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

func TestWriteAfterFlushAndStreamingAreRejected(t *testing.T) {
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/", nil)
	assertWriteAfterFlushRejected(t, c.Writer, recorder)
	assertStreamingWriteAndHijackRejected(t, c.Writer)
	assertBufferedOverflowDiscardsBody(t, c.Writer)
}

func assertWriteAfterFlushRejected(t *testing.T, writer gin.ResponseWriter, recorder *httptest.ResponseRecorder) {
	t.Helper()
	buffered := &bufferedResponse{ResponseWriter: writer}
	if _, err := buffered.Write([]byte("ok")); err != nil {
		t.Fatal(err)
	}
	buffered.flush(slog.New(slog.NewTextHandler(io.Discard, nil)), context.Background())
	n, err := buffered.Write([]byte("late"))
	if n != 0 || !errors.Is(err, ErrWriteAfterFlush) {
		t.Fatalf("write after flush = %d, %v", n, err)
	}
	if strings.Contains(recorder.Body.String(), "late") {
		t.Fatalf("late bytes were sent: %s", recorder.Body)
	}
}

func assertStreamingWriteAndHijackRejected(t *testing.T, writer gin.ResponseWriter) {
	t.Helper()
	rejected := &bufferedResponse{ResponseWriter: writer}
	rejected.Flush()
	if !rejected.streaming {
		t.Fatal("Flush did not mark streaming")
	}
	if _, err := rejected.Write([]byte("x")); !errors.Is(err, errStreamingUnsupported) {
		t.Fatalf("write after Flush = %v", err)
	}
	if _, _, err := rejected.Hijack(); !errors.Is(err, errStreamingUnsupported) || !rejected.streaming {
		t.Fatalf("hijack = %v streaming=%t", err, rejected.streaming)
	}
	if rejected.Pusher() != nil {
		t.Fatal("pusher is supported")
	}
}

func assertBufferedOverflowDiscardsBody(t *testing.T, writer gin.ResponseWriter) {
	t.Helper()
	overflow := &bufferedResponse{ResponseWriter: writer}
	payload := bytes.Repeat([]byte("a"), MaxBufferedResponseBytes+1)
	n, err := overflow.Write(payload)
	if n != 0 || !errors.Is(err, errResponseTooLarge) || !overflow.overflow || len(overflow.body) != 0 {
		t.Fatalf("overflow = %d, %v, kept=%d", n, err, len(overflow.body))
	}
}

func TestAuditMiddlewarePanicBecomesOneLoggedError(t *testing.T) {
	database := openAuditPanicDatabase(t)
	var output bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&output, nil))
	engine := gin.New()
	engine.Use(RequestID(), RequestLog(logger), Recovery(logger), Audit(database, logger))
	engine.POST("/api/v1/users", func(c *gin.Context) {
		c.Set(auditContextKey, "bad")
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/users", nil)
	req.Header.Set(render.RequestIDHeader, "audit-panic")
	recorder := httptest.NewRecorder()
	engine.ServeHTTP(recorder, req)
	assertAuditPanicResponse(t, recorder)
	assertAuditPanicLog(t, output.String())
	entries, err := database.AuditLogs(t.Context(), 0, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("audit rows after panic = %+v", entries)
	}
}

func openAuditPanicDatabase(t *testing.T) *store.Store {
	t.Helper()
	dir := t.TempDir()
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}
	database, err := store.Open(t.Context(), dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func assertAuditPanicResponse(t *testing.T, recorder *httptest.ResponseRecorder) {
	t.Helper()
	if recorder.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body)
	}
	if strings.Contains(recorder.Body.String(), "invalid audit context") || strings.Count(recorder.Body.String(), `"code"`) != 1 {
		t.Fatalf("panic response = %s", recorder.Body)
	}
}

func assertAuditPanicLog(t *testing.T, logged string) {
	t.Helper()
	if !strings.Contains(logged, `"msg":"request handler panicked"`) || !strings.Contains(logged, "invalid audit context") || !strings.Contains(logged, "audit-panic") {
		t.Fatal(logged)
	}
	if !strings.Contains(logged, `"msg":"request complete"`) || !strings.Contains(logged, `"status":500`) {
		t.Fatal(logged)
	}
}
