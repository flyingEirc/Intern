package server

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"errors"
	"log/slog"
	"math"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// TestEventStreamEncodeFailureEndsWithoutPseudoEvent feeds the production
// pump a value encoding/json/v2 actually rejects. PostgreSQL JSON cannot store
// a non-finite number, so the row is built in memory; the failure is Marshal's,
// not a stubbed error. The stream keeps the earlier frame, writes nothing for
// the rejected row, does not invent an end or revoked event, and logs the
// failure without the sibling secret that the partial buffer would contain.
func TestEventStreamEncodeFailureEndsWithoutPseudoEvent(t *testing.T) {
	good := store.RunLog{
		RunID: "run-1", Seq: 1, TS: "2026-01-01T00:00:00Z", Level: "info",
		Event: "queued", Message: "started", Data: map[string]any{"phase": "scan"},
	}
	bad := store.RunLog{
		RunID: "run-1", Seq: 2, TS: "2026-01-01T00:00:01Z", Level: "info",
		Event: "progress", Message: "secret-progress-text",
		Data: map[string]any{"token": "secret-token-value", "ratio": math.NaN()},
	}
	later := store.RunLog{
		RunID: "run-1", Seq: 3, TS: "2026-01-01T00:00:02Z", Level: "info",
		Event: "done", Message: "after-failure",
	}
	want, err := sseMessageFrame(good)
	if err != nil {
		t.Fatal(err)
	}
	assertRealNaNRejection(t, bad)

	var logs bytes.Buffer
	handlers := runHandlers{
		logger:    slog.New(slog.NewJSONHandler(&logs, nil)),
		heartbeat: time.Hour,
	}
	recorder := httptest.NewRecorder()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	previousMode := gin.Mode()
	gin.SetMode(gin.ReleaseMode)
	t.Cleanup(func() { gin.SetMode(previousMode) })
	c, _ := gin.CreateTestContext(recorder)
	c.Request = httptest.NewRequest(http.MethodGet, "/api/v1/runs/run-1/events", nil).WithContext(ctx)

	events := make(chan store.RunLog, 3)
	events <- good
	events <- bad
	events <- later
	dropped := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		handlers.pumpEvents(c, c.Writer, c.Writer, good.RunID, events, dropped)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		cancel()
		<-done
		t.Fatalf("encode failure did not end the stream; body=%q", recorder.Body.String())
	}

	if body := recorder.Body.String(); body != want {
		t.Fatalf("body = %q, want only the encodable frame %q", body, want)
	}
	if len(events) != 1 {
		t.Fatalf("events left = %d, want the event after the failure still queued", len(events))
	}
	assertEncodeFailureLog(t, &logs, bad)
}

// assertRealNaNRejection proves the bad row fails in encoding/json/v2 itself.
func assertRealNaNRejection(t *testing.T, row store.RunLog) {
	t.Helper()
	_, err := sseMessageFrame(row)
	var semantic *json.SemanticError
	if err == nil || !errors.As(err, &semantic) || !strings.Contains(err.Error(), "unsupported value: NaN") {
		t.Fatalf("expected encoding/json/v2 to reject NaN, got %v", err)
	}
}

// assertEncodeFailureLog requires one error log for the rejected sequence and
// no copy of the event message or the sibling secret.
func assertEncodeFailureLog(t *testing.T, logs *bytes.Buffer, row store.RunLog) {
	t.Helper()
	var entry struct {
		Msg   string `json:"msg"`
		RunID string `json:"run_id"`
		Seq   int64  `json:"seq"`
		Error string `json:"error"`
	}
	if err := json.Unmarshal(bytes.TrimSpace(logs.Bytes()), &entry); err != nil {
		t.Fatalf("log %q: %v", logs.Bytes(), err)
	}
	if entry.Msg != "cannot encode sse event" || entry.RunID != row.RunID || entry.Seq != row.Seq ||
		!strings.Contains(entry.Error, "unsupported value: NaN") {
		t.Fatalf("log entry = %+v", entry)
	}
	if strings.Contains(logs.String(), row.Message) || strings.Contains(logs.String(), "secret-token-value") {
		t.Fatalf("encode failure log includes event payload: %s", logs.String())
	}
}
