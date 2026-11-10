package server

import (
	"bufio"
	"encoding/json/v2"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// TestEventStreamDeliversPromptlyThenEnds proves the stream is a real
// unbuffered channel over TLS: an event appended after the subscription starts
// reaches the client promptly, the terminal run ends the stream with exactly one
// end event, and the stream closes itself.
func TestEventStreamDeliversPromptlyThenEnds(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "streamer", "operator")
	_, run := h.seedPolicyWithRun(t, "prompt-stream")

	response, reader, cleanup := h.streamClient(t, cookie, "/api/v1/runs/"+run.ID+"/events", nil)
	defer cleanup()
	assertStreamHeaders(t, response)
	// The replay of the seeded queued event arrives before any new event.
	first := readFrame(t, reader)
	if !strings.Contains(first, "event: message") || !strings.Contains(first, "id: 1") {
		t.Fatalf("first frame = %q", first)
	}

	start := time.Now()
	h.appendLog(t, run.ID, "preflight")
	live := readFrame(t, reader)
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("live event took %s to arrive; the stream is buffered", elapsed)
	}
	if !strings.Contains(live, "id: 2") || !strings.Contains(live, "preflight") {
		t.Fatalf("live frame = %q", live)
	}
	failSeededRun(t, h, run.ID)
	assertFailedThenEnd(t, reader)
	assertStreamClosed(t, reader)
}

func failSeededRun(t *testing.T, h *runHarness, runID string) {
	t.Helper()
	if _, ok, err := h.fixture.database.ClaimRun(t.Context()); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if err := h.fixture.database.FinishRunFailed(t.Context(), runID, store.RunErrorPreflight, "preflight_failed",
		store.SystemAudit("runs.failed", store.AuditDetail{}), &store.RunLogWrite{
			Level: "error", Event: "failed", Message: "run failed",
			Data: map[string]any{"error_class": store.RunErrorPreflight, "code": "preflight_failed"},
		}); err != nil {
		t.Fatal(err)
	}
}

func assertFailedThenEnd(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	terminal := readFrame(t, reader)
	if !strings.Contains(terminal, `"event":"failed"`) || !strings.Contains(terminal, `"error_class"`) {
		t.Fatalf("terminal frame = %q", terminal)
	}
	if end := readFrame(t, reader); end != "event: end\ndata: failed\n\n" {
		t.Fatalf("end frame = %q", end)
	}
}

// assertStreamHeaders checks the frozen response headers of the event stream.
func assertStreamHeaders(t *testing.T, response *http.Response) {
	t.Helper()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if got := response.Header.Get("Content-Type"); got != "text/event-stream" {
		t.Fatalf("content type = %q", got)
	}
	if got := response.Header.Get("Cache-Control"); got != "no-cache" {
		t.Fatalf("cache-control = %q", got)
	}
}

// TestEventStreamReplaysFromTheStoredLog covers replay and both resume shapes:
// a fresh subscription replays every stored event, Last-Event-ID resumes past the
// seen sequence, and the after_seq query does the same.
func TestEventStreamReplaysFromTheStoredLog(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "resumer", "viewer")
	_, run := h.seedPolicyWithRun(t, "replay-stream")
	for _, event := range []string{"claimed", "preflight"} {
		h.appendLog(t, run.ID, event)
	}

	full := readStreamFrames(t, h, cookie, "/api/v1/runs/"+run.ID+"/events", nil, 3)
	for _, want := range []string{"id: 1", "id: 2", "id: 3", "claimed", "preflight"} {
		if !strings.Contains(full, want) {
			t.Fatalf("replay missing %q: %s", want, full)
		}
	}

	lastID := readStreamFrames(t, h, cookie, "/api/v1/runs/"+run.ID+"/events",
		map[string]string{"Last-Event-ID": "1"}, 2)
	if strings.Contains(lastID, "id: 1") {
		t.Fatalf("Last-Event-ID replayed a seen event: %s", lastID)
	}
	for _, want := range []string{"id: 2", "id: 3"} {
		if !strings.Contains(lastID, want) {
			t.Fatalf("Last-Event-ID resume missing %q: %s", want, lastID)
		}
	}

	afterSeq := readStreamFrames(t, h, cookie, "/api/v1/runs/"+run.ID+"/events?after_seq=2", nil, 1)
	if strings.Contains(afterSeq, "id: 2\n") || !strings.Contains(afterSeq, "id: 3") {
		t.Fatalf("after_seq replay = %s", afterSeq)
	}
}

// TestEventStreamHeartbeat proves the periodic comment frame arrives without any
// run activity, which is what keeps a quiet stream alive through proxies. The
// heartbeat ticks independently of the replay, so on a loaded machine a ping can
// legitimately reach the wire before the replayed event: pings ahead of it are
// accepted, anything else is not. Once the replay is delivered the stream is
// idle, because the replayed event is the only one the run has, so everything
// after it must be heartbeat comments and they must keep coming.
func TestEventStreamHeartbeat(t *testing.T) {
	h := newRunHarness(t, WithSSEHeartbeat(40*time.Millisecond))
	cookie := h.login(t, "heartbeat", "operator")
	_, run := h.seedPolicyWithRun(t, "heartbeat-stream")

	response, reader, cleanup := h.streamClient(t, cookie, "/api/v1/runs/"+run.ID+"/events", nil)
	defer cleanup()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if replay := readFirstEvent(t, reader); !strings.Contains(replay, "event: message") {
		t.Fatalf("replay frame = %q", replay)
	}
	for range 2 {
		assertHeartbeat(t, readFrame(t, reader))
	}
}

// heartbeatsBeforeReplay bounds how many heartbeats may reach the wire before
// the replayed event. It only ends a replay that never arrives: at the 40ms
// interval it is the twenty seconds readFrame allows a single frame.
const heartbeatsBeforeReplay = 500

// readFirstEvent returns the first frame that is not a comment. Every comment
// ahead of it must still be a well-formed heartbeat.
func readFirstEvent(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	for range heartbeatsBeforeReplay {
		frame := readFrame(t, reader)
		if !strings.HasPrefix(frame, ":") {
			return frame
		}
		assertHeartbeat(t, frame)
	}
	t.Fatalf("%d heartbeats arrived and the replayed event never did", heartbeatsBeforeReplay)
	return ""
}

// assertHeartbeat requires one frame of an idle stream to be the heartbeat
// comment: nothing else may be written while the run has no activity.
func assertHeartbeat(t *testing.T, frame string) {
	t.Helper()
	if !strings.HasPrefix(frame, ":") {
		t.Fatalf("unexpected frame while idle: %q", frame)
	}
	if !strings.Contains(frame, "ping") {
		t.Fatalf("heartbeat = %q", frame)
	}
}

// TestEventStreamDropsSlowConsumerWithoutBlockingTheRun stalls a subscriber in
// the middle of its first frame and then commits more events than the 256-event
// subscriber buffer holds. Every one of them must commit while the consumer is
// still stuck. The proof is the order of events, not a clock: the consumer is
// released only after the burst, so a producer that waited on it could never
// finish, and the store ends every write after 30 seconds, so such a wait fails
// instead of hanging. The overflow rule itself is pinned at the boundary the
// server owns by TestEventStreamDropsFullSubscriberWithoutBlockingIt.
func TestEventStreamDropsSlowConsumerWithoutBlockingTheRun(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "slow", "operator")
	_, run := h.seedPolicyWithRun(t, "slow-consumer")
	endStream := h.stallEventStream(t, cookie, run.ID)

	const burst = 900
	for i := range burst {
		h.appendLog(t, run.ID, fmt.Sprintf("burst-%03d", i))
	}
	logs, err := h.fixture.database.RunLogsAfter(t.Context(), run.ID, 0, store.MaxRunLogRows+1)
	if err != nil {
		t.Fatal(err)
	}
	if len(logs) != burst+1 {
		t.Fatalf("committed events = %d, want %d", len(logs), burst+1)
	}
	// An event appended after the burst still lands, so the run is not throttled
	// by any subscriber.
	h.appendLog(t, run.ID, "after-burst")
	if last := h.logRowsFor(t, run.ID); last.Event != "after-burst" {
		t.Fatalf("last stored event = %q", last.Event)
	}
	endStream()
}

// TestEventStreamDropsFullSubscriberWithoutBlockingIt exercises the overflow
// rule at the boundary the server owns: the real tail goroutine feeding a
// subscriber buffer that is already full. It must signal the drop and return
// instead of waiting for a consumer that is not reading.
func TestEventStreamDropsFullSubscriberWithoutBlockingIt(t *testing.T) {
	h := newRunHarness(t)
	_, run := h.seedPolicyWithRun(t, "overflow")
	// More stored events than the frozen subscriber capacity.
	for i := range sseBufferCapacity + 20 {
		h.appendLog(t, run.ID, fmt.Sprintf("stored-%03d", i))
	}
	stored, err := h.fixture.database.Run(t.Context(), run.ID)
	if err != nil {
		t.Fatal(err)
	}
	// The subscriber buffer is full from the start and nothing reads it.
	events := make(chan store.RunLog, sseBufferCapacity)
	for range sseBufferCapacity {
		events <- store.RunLog{Event: "filler"}
	}
	dropped := make(chan struct{}, 1)
	done := make(chan struct{})
	go func() {
		defer close(done)
		runHandlers{database: h.fixture.database}.streamRun(t.Context(), stored, 0, events, dropped)
	}()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("the tail goroutine blocked on a full subscriber buffer")
	}
	select {
	case <-dropped:
	default:
		t.Fatal("overflowing the subscriber buffer did not mark the subscriber dropped")
	}
	// The stored log is intact: a dropped subscriber never affects the run.
	if logs, err := h.fixture.database.RunLogsAfter(t.Context(), run.ID, 0, store.MaxRunLogRows+1); err != nil ||
		len(logs) != sseBufferCapacity+21 {
		t.Fatalf("stored log = %d rows, %v", len(logs), err)
	}
}

// logRowsFor returns the newest stored event of one run.
func (h *runHarness) logRowsFor(t *testing.T, runID string) store.RunLog {
	t.Helper()
	rows, err := h.fixture.database.RunLogsAfter(t.Context(), runID, 0, store.MaxRunLogRows+1)
	if err != nil || len(rows) == 0 {
		t.Fatalf("run log empty: %v", err)
	}
	return rows[len(rows)-1]
}

// TestEventStreamRevokedSessionIsToldAndClosed revokes the session while the
// stream is open and proves the server sends `event: revoked` and closes. This
// is the reachable revocation variant: a demoted, disabled or logged-out session
// is rechecked at every heartbeat.
func TestEventStreamRevokedSessionIsToldAndClosed(t *testing.T) {
	h := newRunHarness(t, WithSSEHeartbeat(40*time.Millisecond))
	user := h.fixture.seed(t, "revoked", "operator")
	cookie := h.fixture.login(t, "revoked", testPassword)
	_, run := h.seedPolicyWithRun(t, "revoked-stream")

	response, reader, cleanup := h.streamClient(t, cookie, "/api/v1/runs/"+run.ID+"/events", nil)
	defer cleanup()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if first := readFrame(t, reader); !strings.Contains(first, "event: message") {
		t.Fatalf("first frame = %q", first)
	}
	// The epoch bump is what logout, password change, disable and role change all
	// produce; the running stream must observe it without a new request.
	if err := h.fixture.database.RevokeSessions(t.Context(), user.ID,
		store.SystemAudit("users.revoke_sessions", store.AuditDetail{})); err != nil {
		t.Fatal(err)
	}
	readUntilRevoked(t, reader)
	assertStreamClosed(t, reader)
}

// TestEventStreamLogoutStopsTheStream is the logout case of the heartbeat
// recheck: destroying the session tombstones the token, so the next heartbeat
// sends event: revoked and closes.
func TestEventStreamLogoutStopsTheStream(t *testing.T) {
	h := newRunHarness(t, WithSSEHeartbeat(40*time.Millisecond))
	cookie := h.login(t, "logout-stream", "operator")
	_, run := h.seedPolicyWithRun(t, "logout-stream")

	response, reader, cleanup := h.streamClient(t, cookie, "/api/v1/runs/"+run.ID+"/events", nil)
	defer cleanup()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	if first := readFrame(t, reader); !strings.Contains(first, "event: message") {
		t.Fatalf("first frame = %q", first)
	}
	client := h.jsonClient(t)
	logout := h.do(t, client, http.MethodPost, "/api/v1/auth/logout", "", cookie)
	if logout.StatusCode != http.StatusNoContent {
		t.Fatalf("logout status = %d", logout.StatusCode)
	}
	_ = logout.Body.Close()
	readUntilRevoked(t, reader)
	assertStreamClosed(t, reader)
}

// TestEventStreamCaughtUpSubscriberSeesTerminalBeforeEnd is the same-transaction
// terminal log: a subscriber that has already consumed every live event receives
// the failed row and only then the end event.
func TestEventStreamCaughtUpSubscriberSeesTerminalBeforeEnd(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "terminal-order", "operator")
	_, run := h.seedPolicyWithRun(t, "terminal-order")

	_, reader, cleanup := h.streamClient(t, cookie, "/api/v1/runs/"+run.ID+"/events", nil)
	defer cleanup()
	if first := readFrame(t, reader); !strings.Contains(first, `"event":"queued"`) {
		t.Fatalf("first frame = %q", first)
	}
	if _, ok, err := h.fixture.database.ClaimRun(t.Context()); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if err := h.fixture.database.FinishRunFailed(t.Context(), run.ID, store.RunErrorInterrupted, "interrupted",
		store.SystemAudit("runs.interrupted", store.AuditDetail{}), &store.RunLogWrite{
			Level: "error", Event: "interrupted", Message: "run interrupted", Data: map[string]any{},
		}); err != nil {
		t.Fatal(err)
	}
	terminal := readFrame(t, reader)
	if !strings.Contains(terminal, `"event":"interrupted"`) {
		t.Fatalf("terminal event = %q, want interrupted before end", terminal)
	}
	if end := readFrame(t, reader); end != "event: end\ndata: failed\n\n" {
		t.Fatalf("end frame = %q", end)
	}
}

// TestEventStreamCompletedPayloadIsStructuredAndRedacted is D1: the completed
// SSE frame is a JSON object with seq, ts, level, event, message and data, the
// data object carries backup identity fields, and no secret is present.
func TestEventStreamCompletedPayloadIsStructuredAndRedacted(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "completed-payload", "operator")
	_, run := h.seedPolicyWithRun(t, "completed-payload")

	_, reader, cleanup := h.streamClient(t, cookie, "/api/v1/runs/"+run.ID+"/events", nil)
	defer cleanup()
	if first := readFrame(t, reader); !strings.Contains(first, `"event":"queued"`) {
		t.Fatalf("first frame = %q", first)
	}
	if _, ok, err := h.fixture.database.ClaimRun(t.Context()); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	log := &store.RunLogWrite{
		Level: "info", Event: "completed", Message: "backup completed",
		Data: map[string]any{
			"backup_id": "b-1", "chain_id": "c-1", "seq": 1,
			"payload_size": 42, "payload_sha256": "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		},
	}
	if err := h.fixture.database.FinishRunCompleted(t.Context(), run.ID, "b-1", "c-1", 42,
		store.SystemAudit("runs.completed", store.AuditDetail{}), log); err != nil {
		t.Fatal(err)
	}
	frame := readFrame(t, reader)
	for _, want := range []string{
		`"seq":`, `"ts":`, `"level":`, `"event":"completed"`, `"message":`, `"data":`,
		`"backup_id":"b-1"`, `"chain_id":"c-1"`, `"payload_size":`, `"payload_sha256":`,
	} {
		if !strings.Contains(frame, want) {
			t.Fatalf("completed frame missing %s: %q", want, frame)
		}
	}
	if end := readFrame(t, reader); end != "event: end\ndata: completed\n\n" {
		t.Fatalf("end frame = %q", end)
	}
	for _, secret := range []string{"{{SECRET_sse_source}}", testPassword, h.fixture.keyFile} {
		if strings.Contains(frame, secret) {
			t.Fatalf("completed frame leaked %q", secret)
		}
	}
}

func readUntilRevoked(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		frame := readFrame(t, reader)
		if strings.Contains(frame, "event: revoked") {
			return
		}
		if !strings.HasPrefix(strings.TrimSpace(frame), ":") {
			t.Fatalf("frame before revoked = %q", frame)
		}
	}
	t.Fatal("no revoked event arrived")
}

// TestEventStreamRejectsBeforeHeaders covers the pre-stream JSON failures: an
// unauthenticated request, an unknown run, and an invalid resume sequence all
// answer JSON with no event-stream headers.
func TestEventStreamRejectsBeforeHeaders(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "preflight-json", "operator")
	_, run := h.seedPolicyWithRun(t, "json-errors")

	cases := []struct {
		name    string
		path    string
		cookie  *http.Cookie
		headers map[string]string
		status  int
		code    string
	}{
		{"anonymous", "/api/v1/runs/" + run.ID + "/events", anonymousCookie, nil, http.StatusUnauthorized, "unauthorized"},
		{"missing run", "/api/v1/runs/0191c9c4-0000-7000-8000-000000000000/events", cookie, nil, http.StatusNotFound, "not_found"},
		{"bad after_seq", "/api/v1/runs/" + run.ID + "/events?after_seq=-1", cookie, nil, http.StatusBadRequest, "validation_failed"},
		{"bad last event id", "/api/v1/runs/" + run.ID + "/events", cookie, map[string]string{"Last-Event-ID": "not-a-sequence"}, http.StatusBadRequest, "validation_failed"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			response, _, cleanup := h.streamClient(t, pickCookie(tc.cookie, cookie), tc.path, tc.headers)
			defer cleanup()
			if got := response.Header.Get("Content-Type"); got == "text/event-stream" {
				t.Fatalf("%s answered an event stream", tc.name)
			}
			status, body := readJSONStatus(t, response)
			if status != tc.status {
				t.Fatalf("status = %d, body = %s; want %d", status, body, tc.status)
			}
			if !strings.Contains(body, `"code":"`+tc.code+`"`) {
				t.Fatalf("body = %s", body)
			}
		})
	}
}

// TestEventStreamNeverWritesJSONAfterTheFirstByte proves the after-first-byte
// policy: once the stream opens, an event with no writer left (a terminal run
// whose subscription is still open) closes the stream instead of appending a JSON
// error body.
func TestEventStreamNeverWritesJSONAfterTheFirstByte(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "no-json", "viewer")
	_, run := h.seedPolicyWithRun(t, "no-json-stream")

	response, reader, cleanup := h.streamClient(t, cookie, "/api/v1/runs/"+run.ID+"/events", nil)
	defer cleanup()
	if response.StatusCode != http.StatusOK {
		t.Fatalf("status = %d", response.StatusCode)
	}
	// Delete the run under the open stream: every later read fails. The server
	// must close the stream, not write an error body into the event stream.
	if err := h.fixture.database.DB().Close(); err != nil {
		t.Fatal(err)
	}
	body := drainStream(t, reader)
	if strings.Contains(body, `{"error"`) {
		t.Fatalf("JSON error written after the first byte: %s", body)
	}
}

// TestEventStreamOutlivesServerWriteTimeout is the per-write deadline case. The
// server's WriteTimeout starts counting when a request's header is read, so a
// stream can only outlive it if every frame write first pushes the connection's
// write deadline out through ResponseController. The stream is served to a
// writer that records those calls, and the proof is their order: each frame is
// written right after a deadline far past it. Nothing waits for a timer and no
// real WriteTimeout is set: a tiny one fails the response before its first byte
// on a busy machine, whether or not the handler extends deadlines, and the
// client then reads a TLS bad record MAC instead of a stream.
func TestEventStreamOutlivesServerWriteTimeout(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "deadline", "operator")
	_, run := h.seedPolicyWithRun(t, "deadline-stream")
	stream, end := h.recordEventStream(t, cookie, run.ID)

	stream.awaitFrame(t, "event: message")
	h.appendLog(t, run.ID, "after-write-timeout")
	stream.awaitFrame(t, "after-write-timeout")
	if status := stream.status.Load(); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	assertEveryWriteExtendsTheDeadline(t, end())
}

// assertEveryWriteExtendsTheDeadline requires each recorded frame write to come
// right after a SetWriteDeadline that put the deadline far past the write.
func assertEveryWriteExtendsTheDeadline(t *testing.T, steps []deadlineStep) {
	t.Helper()
	writes := 0
	for i, step := range steps {
		if !step.write {
			continue
		}
		writes++
		if i == 0 || steps[i-1].write {
			t.Fatalf("frame write %d was not preceded by a write deadline extension", writes)
		}
		set := steps[i-1]
		if ahead := set.deadline.Sub(set.at); ahead < sseWriteDeadline/2 {
			t.Fatalf("frame write %d: the deadline was set only %s ahead, want about %s", writes, ahead, sseWriteDeadline)
		}
		if !set.deadline.After(step.at) {
			t.Fatalf("frame write %d happened after its own deadline", writes)
		}
	}
	if writes < 2 {
		t.Fatalf("recorded %d frame writes, want the replay and the live event", writes)
	}
}

// TestEventStreamNeverLeaksSecrets scans every frame and every server log line
// for the generated source password, the sealed ciphertext and the KEK.
func TestEventStreamNeverLeaksSecrets(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "scan", "operator")
	_, run := h.seedPolicyWithRun(t, "secret-stream")
	h.appendLog(t, run.ID, "preflight")
	body := readStreamFrames(t, h, cookie, "/api/v1/runs/"+run.ID+"/events", nil, 2)
	for _, secret := range []string{"{{SECRET_sse_source}}", testPassword} {
		if strings.Contains(body, secret) {
			t.Fatalf("stream leaked %q: %s", secret, body)
		}
	}
	for _, secret := range []string{"{{SECRET_sse_source}}", testPassword, h.fixture.keyFile} {
		if strings.Contains(h.fixture.logs.String(), secret) {
			t.Fatalf("server log leaked %q", secret)
		}
	}
	// The stored ciphertext must not be echoed either.
	var ciphertext string
	if err := h.fixture.database.DB().QueryRowContext(t.Context(),
		`SELECT COALESCE(string_agg(encode(secret_ciphertext, 'hex'), ''), '') FROM sources`).Scan(&ciphertext); err != nil {
		t.Fatal(err)
	}
	if ciphertext != "" && strings.Contains(body, ciphertext) {
		t.Fatal("stream leaked the sealed ciphertext")
	}
}

// TestEventStreamIsNotBufferedBySession proves the stream route bypasses the
// 8 MiB buffered session writer: a stream that stays open longer than a request
// would need must still deliver each byte as it is written, and the response
// must not be turned into a buffered 500 by the flush rejection path.
func TestEventStreamIsNotBufferedBySession(t *testing.T) {
	if !middleware.IsSSEPath("/api/v1/runs/x/events") {
		t.Fatal("the session middleware would buffer the event stream")
	}
	h := newRunHarness(t)
	cookie := h.login(t, "bypass", "viewer")
	_, run := h.seedPolicyWithRun(t, "bypass-stream")

	response, reader, cleanup := h.streamClient(t, cookie, "/api/v1/runs/"+run.ID+"/events", nil)
	defer cleanup()
	// The status and headers arrive before any event is appended, which is only
	// possible with an unbuffered writer.
	if response.StatusCode != http.StatusOK || response.Header.Get("Content-Type") != "text/event-stream" {
		t.Fatalf("status=%d type=%q", response.StatusCode, response.Header.Get("Content-Type"))
	}
	// The seeded run's own queued event is the first frame.
	if first := readFrame(t, reader); !strings.Contains(first, "queued") {
		t.Fatalf("first frame = %q", first)
	}
	for i := range 3 {
		h.appendLog(t, run.ID, fmt.Sprintf("incremental-%d", i))
		frame := readFrame(t, reader)
		if !strings.Contains(frame, fmt.Sprintf("incremental-%d", i)) {
			t.Fatalf("frame %d = %q", i, frame)
		}
	}
	if strings.Contains(h.fixture.logs.String(), "streaming responses are not supported") {
		t.Fatal("the stream hit the buffered flush rejection path")
	}
}

// TestEventStreamEndIsSentExactlyOnce shows a terminal run at connect answers
// replay plus one end frame, and the client sees the connection close.
func TestEventStreamEndIsSentExactlyOnce(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "once", "viewer")
	_, run := h.seedPolicyWithRun(t, "once-stream")
	h.appendLog(t, run.ID, "queued")
	if _, ok, err := h.fixture.database.ClaimRun(t.Context()); err != nil || !ok {
		t.Fatalf("claim: %v ok=%v", err, ok)
	}
	if err := h.fixture.database.FinishRunCanceled(t.Context(), run.ID,
		store.SystemAudit("runs.canceled", store.AuditDetail{}), &store.RunLogWrite{
			Level: "warn", Event: "canceled", Message: "run canceled", Data: map[string]any{},
		}); err != nil {
		t.Fatal(err)
	}
	body := readAllFrames(t, h, cookie, "/api/v1/runs/"+run.ID+"/events", nil)
	if count := strings.Count(body, "event: end"); count != 1 {
		t.Fatalf("end frames = %d in %s", count, body)
	}
	if !strings.HasSuffix(body, "event: end\ndata: canceled\n\n") {
		t.Fatalf("stream did not end with the terminal status: %s", body)
	}
	// The replayed event is the stored one, and the terminal status is only
	// reported by the end event.
	if !strings.Contains(body, `"event":"queued"`) || !strings.Contains(body, `"event":"canceled"`) {
		t.Fatalf("replay missing stored events: %s", body)
	}
}

// pickCookie substitutes the default cookie when a case did not override it.
// A case marked anonymous passes anonymousCookie so it stays unauthenticated.
func pickCookie(override, fallback *http.Cookie) *http.Cookie {
	if override == anonymousCookie {
		return nil
	}
	if override != nil {
		return override
	}
	return fallback
}

// anonymousCookie marks a case that must not send a session cookie at all.
var anonymousCookie = &http.Cookie{Name: "__anonymous__"}

// readAllFrames collects a whole stream until the server closes it.
func readAllFrames(t *testing.T, h *runHarness, cookie *http.Cookie, path string, headers map[string]string) string {
	t.Helper()
	response, reader, cleanup := h.streamClient(t, cookie, path, headers)
	defer cleanup()
	if response.StatusCode != http.StatusOK {
		status, body := readJSONStatus(t, response)
		t.Fatalf("status = %d (%s), want 200", status, body)
	}
	return drainStream(t, reader)
}

// readStreamFrames opens a stream and reads exactly n frames, for runs that are
// still live and therefore will not close on their own.
func readStreamFrames(t *testing.T, h *runHarness, cookie *http.Cookie, path string, headers map[string]string, n int) string {
	t.Helper()
	response, reader, cleanup := h.streamClient(t, cookie, path, headers)
	defer cleanup()
	if response.StatusCode != http.StatusOK {
		status, body := readJSONStatus(t, response)
		t.Fatalf("status = %d (%s), want 200", status, body)
	}
	return readFrames(t, reader, n)
}

// drainStream reads until the server closes the stream and returns every byte.
func drainStream(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	type result struct {
		body string
		err  error
	}
	done := make(chan result, 1)
	go func() {
		var builder strings.Builder
		_, err := io.Copy(&builder, reader)
		done <- result{body: builder.String(), err: err}
	}()
	select {
	case got := <-done:
		return got.body
	case <-time.After(30 * time.Second):
		t.Fatal("stream did not close within 30s")
		return ""
	}
}

// assertStreamClosed requires the server to close the stream eventually, which
// is what replaces a JSON error after the first byte.
func assertStreamClosed(t *testing.T, reader *bufio.Reader) {
	t.Helper()
	drainStream(t, reader)
}

// TestRunEventsRequireRunsReadPermission documents the frozen matrix boundary
// that makes a 403 unreachable on this route: every role allowed to authenticate
// holds runs:read, so only 401 and 404 are reachable before the headers. The
// permission helper itself is proven by the routes that do gate other
// permissions, such as audit:read.
func TestRunEventsRequireRunsReadPermission(t *testing.T) {
	for _, role := range []string{"admin", "operator", "viewer"} {
		if !hasRunsRead(role) {
			t.Fatalf("%s lacks runs:read but is an authenticatable role", role)
		}
	}
}

// hasRunsRead reports whether a role holds runs:read, using the real matrix.
func hasRunsRead(role string) bool {
	for _, permission := range auth.Permissions(role) {
		if permission == "runs:read" {
			return true
		}
	}
	return false
}

// TestRunLogsCapMarkerSurvivesTheAPI proves the capped log is visible through the
// read API with exactly one truncated marker and the truncated flag set.
func TestRunLogsCapMarkerSurvivesTheAPI(t *testing.T) {
	h := newRunHarness(t)
	cookie := h.login(t, "capped", "viewer")
	_, run := h.seedPolicyWithRun(t, "capped-stream")
	// Fill the log exactly, then overflow it: the last row must be the one
	// truncated marker and later events are dropped.
	for i := range store.MaxRunLogRows {
		h.appendLog(t, run.ID, fmt.Sprintf("event-%04d", i))
	}
	for range 3 {
		h.appendLog(t, run.ID, "late")
	}

	client := h.jsonClient(t)
	response := h.do(t, client, http.MethodGet, "/api/v1/runs/"+run.ID+"/logs?limit=1000", "", cookie)
	status, body := readJSONStatus(t, response)
	if status != http.StatusOK {
		t.Fatalf("status=%d body=%s", status, body)
	}
	var page struct {
		Items     []store.RunLog `json:"items"`
		NextAfter *int64         `json:"next_after"`
		Truncated bool           `json:"truncated"`
	}
	if err := json.Unmarshal([]byte(body), &page); err != nil {
		t.Fatal(err)
	}
	if !page.Truncated {
		t.Fatalf("truncated flag = false: %s", body)
	}
	markers := 0
	for _, item := range page.Items {
		if item.Event == store.TruncatedEvent {
			markers++
		}
	}
	if markers != 1 {
		t.Fatalf("truncated markers = %d", markers)
	}
}
