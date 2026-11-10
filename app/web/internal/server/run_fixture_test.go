package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// runHarness is the real serve shape for the run API and SSE battery: the whole
// engine, with the real dispatcher attached, behind a real TLS listener built by
// server.New/Run. Nothing is mocked: PostgreSQL is the control plane, the stream goes
// over the wire and the sessions live in the same database.
type runHarness struct {
	t          *testing.T
	fixture    *authFixture
	dispatcher *dispatch.Dispatcher
	engine     *gin.Engine
	address    string
	pool       *x509.CertPool
	certFile   string
	keyFile    string
}

// newRunHarness starts a real TLS listener serving the run routes.
func newRunHarness(t *testing.T, opts ...EngineOption) *runHarness {
	t.Helper()
	// The configured fixture (real KEK) is what a seeded source needs; the
	// runtime settings then make the dispatcher's preflight reachable.
	f := newAuthFixture(t)
	settings := runSettings(t, f)
	dispatcher, err := dispatch.Start(t.Context(), dispatch.Config{
		Database: f.database, Settings: settings, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = dispatcher.Stop() })
	engine, err := NewEngineWithResources(f.database, slog.New(slog.NewJSONHandler(&f.logs, nil)), nil, f.service,
		&resources.Service{Store: f.database, Settings: settings}, dispatcher, append(opts, WithRestores(dispatcher))...)
	if err != nil {
		t.Fatal(err)
	}
	f.engine = engine
	h := &runHarness{t: t, fixture: f, dispatcher: dispatcher, engine: engine}
	h.listen(t)
	return h
}

// newUnconfiguredRunHarness is decision 9's deployment: rows exist from a
// configured era, but the dispatcher and the resources service start with no
// KEK, work root or XtraBackup path. Serve still answers.
func newUnconfiguredRunHarness(t *testing.T) *runHarness {
	t.Helper()
	f := newAuthFixture(t)
	dispatcher, err := dispatch.Start(t.Context(), dispatch.Config{
		Database: f.database, Settings: resources.Settings{},
		Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("an unconfigured runtime must still start the dispatcher: %v", err)
	}
	t.Cleanup(func() { _ = dispatcher.Stop() })
	engine, err := NewEngineWithResources(f.database, slog.New(slog.NewJSONHandler(&f.logs, nil)), nil, f.service,
		&resources.Service{Store: f.database, Settings: resources.Settings{}}, dispatcher, WithRestores(dispatcher))
	if err != nil {
		t.Fatal(err)
	}
	f.engine = engine
	h := &runHarness{t: t, fixture: f, dispatcher: dispatcher, engine: engine}
	h.listen(t)
	return h
}

// newControlHarness serves the run routes over TLS with an injected dispatcher
// implementation. Tests that need a successful replica retry without FTP use it.
func newControlHarness(t *testing.T, control DispatcherControl) *runHarness {
	t.Helper()
	f := newAuthFixture(t)
	settings := runSettings(t, f)
	engine, err := NewEngineWithResources(f.database, slog.New(slog.NewJSONHandler(&f.logs, nil)), nil, f.service,
		&resources.Service{Store: f.database, Settings: settings}, control)
	if err != nil {
		t.Fatal(err)
	}
	f.engine = engine
	h := &runHarness{t: t, fixture: f, engine: engine}
	h.listen(t)
	return h
}

// listen starts the real TLS server of the harness on a free loopback address.
func (h *runHarness) listen(t *testing.T) {
	t.Helper()
	h.certFile, h.keyFile, h.pool = tlsMaterial(t, t.TempDir())
	h.address = freeAddress(t)
	config := Config{Listen: h.address, CertFile: h.certFile, KeyFile: h.keyFile, ShutdownTimeout: 5 * time.Second}
	srv := New(config, Handler(h.engine))
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv, config) }()
	waitListening(t, h.address, true)
	t.Cleanup(func() {
		finish := waitRun(cancel, done, config.ShutdownTimeout+postCancelDrainWait()+3*time.Second)
		if err := finish(); err != nil && !errors.Is(err, context.DeadlineExceeded) {
			t.Errorf("stop run harness: %v", err)
		}
	})
}

// stopRuns shuts the worker loops down while the HTTP surface keeps serving, so
// a queued run stays queued and the stream tests own every event in its log.
func (h *runHarness) stopRuns() {
	if h.dispatcher == nil {
		return
	}
	if err := h.dispatcher.Stop(); err != nil {
		h.t.Fatalf("stop dispatcher: %v", err)
	}
	h.dispatcher = nil
}

// streamClient opens the event stream with its own TLS client and returns a
// reader over the response body. The server's WriteTimeout must never cancel
// the read, so the client has no overall timeout; the test's own deadline does.
func (h *runHarness) streamClient(t *testing.T, cookie *http.Cookie, path string, headers map[string]string) (*http.Response, *bufio.Reader, func()) {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		Jar: jar,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: h.pool, MinVersion: tls.VersionTLS12},
		},
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+h.address+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	for key, value := range headers {
		req.Header.Set(key, value)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	cleanup := func() {
		_ = response.Body.Close()
		client.CloseIdleConnections()
	}
	return response, bufio.NewReader(response.Body), cleanup
}

// readFrame reads one SSE frame: its comment/id/event/data lines up to the blank
// separator. The deadline keeps a stalled stream from hanging the package.
func readFrame(t *testing.T, reader *bufio.Reader) string {
	t.Helper()
	type result struct {
		frame string
		err   error
	}
	done := make(chan result, 1)
	go func() {
		var builder strings.Builder
		for {
			line, err := reader.ReadString('\n')
			if err != nil {
				done <- result{frame: builder.String(), err: err}
				return
			}
			if line == "\n" {
				done <- result{frame: builder.String() + "\n"}
				return
			}
			builder.WriteString(line)
		}
	}()
	select {
	case got := <-done:
		if got.err != nil && got.frame == "" {
			t.Fatalf("read frame: %v", got.err)
		}
		return got.frame
	case <-time.After(20 * time.Second):
		t.Fatal("no SSE frame arrived within 20s")
		return ""
	}
}

// readFrames reads exactly n frames from a stream that stays open and returns
// them joined. A live stream has no reason to close, so tests must never drain it.
func readFrames(t *testing.T, reader *bufio.Reader, n int) string {
	t.Helper()
	var builder strings.Builder
	for range n {
		builder.WriteString(readFrame(t, reader))
	}
	return builder.String()
}

// readJSONStatus reads the pre-stream response and returns its status and body.
func readJSONStatus(t *testing.T, response *http.Response) (int, string) {
	t.Helper()
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		t.Fatal(err)
	}
	return response.StatusCode, string(body)
}

// seedRunRepository inserts one local repository through the store.
func (h *runHarness) seedRepository(t *testing.T, name string) store.Repository {
	t.Helper()
	root := filepath.Join(t.TempDir(), name)
	if err := os.Mkdir(root, 0o700); err != nil {
		t.Fatal(err)
	}
	created, err := h.fixture.database.CreateRepository(t.Context(), store.RepositoryInput{
		Name: name, Kind: store.RepositoryLocal, TLSMode: store.TLSModeNone, Root: root,
	}, nil, store.SystemAudit("repositories.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// seedRunSource inserts one MySQL source whose socket nothing listens on.
func (h *runHarness) seedSource(t *testing.T, name string) store.Source {
	t.Helper()
	created, err := h.fixture.database.CreateSource(t.Context(), store.SourceInput{
		Name: name, Socket: filepath.Join(t.TempDir(), "absent.sock"), Datadir: "/var/lib/mysql",
		Username: "backup", Secret: []byte("{{SECRET_sse_source}}"),
	}, h.fixture.sealer, store.SystemAudit("sources.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	return created
}

// seedRunPolicy inserts one enabled policy on the seeded source and repository.
func (h *runHarness) seedPolicy(t *testing.T, name string, sourceID, repositoryID string) store.Policy {
	t.Helper()
	created, err := h.fixture.database.CreatePolicy(t.Context(), store.PolicyInput{
		Name: name, SourceID: sourceID, LocalRepositoryID: repositoryID,
		FullCron: "0 * * * *", IncrementalCron: "30 * * * *", Timezone: "UTC",
	}, store.SystemAudit("policies.create", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	enabled, err := h.fixture.database.EnablePolicy(t.Context(), created.ID, true,
		&store.PreflightRecord{OK: true, Checks: []string{"identity"}, Warnings: []store.PreflightWarning{}},
		store.SystemAudit("policies.enable", store.AuditDetail{}))
	if err != nil {
		t.Fatal(err)
	}
	return enabled
}

// seedPolicyOnly seeds the rows a trigger needs without creating a run, so a
// test that triggers through the API owns the only row.
func (h *runHarness) seedPolicyOnly(t *testing.T, key string) store.Policy {
	t.Helper()
	repository := h.seedRepository(t, "sse-local-"+key)
	source := h.seedSource(t, "sse-source-"+key)
	return h.seedPolicy(t, "sse-policy-"+key, source.ID, repository.ID)
}

// seedPolicyWithRun stops the worker loops, then seeds the rows one run needs
// and returns the queued run. The stop comes first: a live dispatcher could
// claim the run between the insert and a later stop and fail it on the
// unconfigured runtime, writing events the test does not own.
func (h *runHarness) seedPolicyWithRun(t *testing.T, key string) (store.Policy, store.Run) {
	t.Helper()
	h.stopRuns()
	repository := h.seedRepository(t, "sse-local-"+key)
	source := h.seedSource(t, "sse-source-"+key)
	policy := h.seedPolicy(t, "sse-policy-"+key, source.ID, repository.ID)
	run, err := h.fixture.database.CreateRun(t.Context(), store.RunInput{
		PolicyID: policy.ID, SourceID: source.ID, Kind: store.RunKindFull, Trigger: store.RunTriggerManual,
		IdempotencyKey: key, RequestHash: dispatch.MarshalTriggerBody(store.RunKindFull), ScheduledAt: time.Now().UTC(),
	})
	if err != nil {
		t.Fatal(err)
	}
	// The stopped worker never writes the queued event, so the seed writes the
	// same fixed-format row it would have written.
	if err := h.fixture.database.AppendRunLog(t.Context(), run.ID, "info", "queued", "queued", map[string]any{}); err != nil {
		t.Fatal(err)
	}
	return policy, run
}

// login returns a cookie for a freshly seeded user with the given role.
func (h *runHarness) login(t *testing.T, username, role string) *http.Cookie {
	t.Helper()
	h.fixture.seed(t, username, role)
	return h.fixture.login(t, username, testPassword)
}

// run reads one stored run.
func (h *runHarness) run(t *testing.T, id string) store.Run {
	t.Helper()
	run, err := h.fixture.database.Run(t.Context(), id)
	if err != nil {
		t.Fatal(err)
	}
	return run
}

// awaitTerminal waits for the run to reach a terminal status.
func (h *runHarness) awaitTerminal(t *testing.T, id string) store.Run {
	t.Helper()
	var last store.Run
	deadline := time.Now().Add(30 * time.Second)
	for {
		last = h.run(t, id)
		switch last.Status {
		case store.RunStatusCompleted, store.RunStatusFailed, store.RunStatusCanceled:
			return last
		}
		if time.Now().After(deadline) {
			t.Fatalf("run %s stayed %s", id, last.Status)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

// appendLog writes one event into the run's bounded log.
func (h *runHarness) appendLog(t *testing.T, runID, event string) {
	t.Helper()
	if err := h.fixture.database.AppendRunLog(t.Context(), runID, "info", event, event, map[string]any{}); err != nil {
		t.Fatal(err)
	}
}

// jsonClient is a TLS client with a cookie jar for the pre-stream JSON cases.
func (h *runHarness) jsonClient(t *testing.T) *http.Client {
	t.Helper()
	jar, err := cookiejar.New(nil)
	if err != nil {
		t.Fatal(err)
	}
	return &http.Client{
		Timeout: 15 * time.Second,
		Jar:     jar,
		Transport: &http.Transport{
			TLSClientConfig: &tls.Config{RootCAs: h.pool, MinVersion: tls.VersionTLS12},
		},
	}
}

// do issues one request over TLS and returns the response.
func (h *runHarness) do(t *testing.T, client *http.Client, method, path, body string, cookie *http.Cookie) *http.Response {
	t.Helper()
	var reader io.Reader
	if body != "" {
		reader = strings.NewReader(body)
	}
	req, err := http.NewRequestWithContext(t.Context(), method, "https://"+h.address+path, reader)
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

// postTrigger posts one manual trigger request.
func (h *runHarness) postTrigger(t *testing.T, client *http.Client, cookie *http.Cookie, policyID, kind, key string) *http.Response {
	t.Helper()
	req, err := http.NewRequestWithContext(t.Context(), http.MethodPost,
		"https://"+h.address+"/api/v1/policies/"+policyID+"/runs", strings.NewReader(`{"kind":"`+kind+`"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Idempotency-Key", key)
	req.AddCookie(cookie)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = response.Body.Close() })
	return response
}

// triggerOverHTTP posts one trigger and returns the accepted run id.
func (h *runHarness) triggerOverHTTP(t *testing.T, client *http.Client, cookie *http.Cookie, policyID, kind, key string) string {
	t.Helper()
	response := h.postTrigger(t, client, cookie, policyID, kind, key)
	status, body := readJSONStatus(t, response)
	if status != http.StatusAccepted {
		t.Fatalf("trigger status=%d body=%s", status, body)
	}
	created := decodeBody[dispatch.CreatedRun](t, body)
	if created.RunID == "" {
		t.Fatalf("trigger body = %s", body)
	}
	return created.RunID
}

// runsOf lists one policy's runs.
func (h *runHarness) runsOf(t *testing.T, policyID string) []store.Run {
	t.Helper()
	runs, err := h.fixture.database.Runs(t.Context(), store.RunsFilter{PolicyID: policyID, Limit: 200})
	if err != nil {
		t.Fatal(err)
	}
	return runs
}

// systemAudit counts the dispatcher's system audit rows for one action.
func (h *runHarness) systemAudit(t *testing.T, action string) int {
	t.Helper()
	var count int
	query := `SELECT COUNT(*) FROM audit_logs WHERE actor_type = 'system' AND action = $1 AND result = 'success'`
	if err := h.fixture.database.DB().QueryRowContext(t.Context(), query, action).Scan(&count); err != nil {
		t.Fatal(err)
	}
	return count
}

// materializeDispatcher starts a fresh dispatcher against the same store, which
// is what a serve restart does, and registers it on the harness.
func (h *runHarness) materializeDispatcher(t *testing.T) {
	t.Helper()
	settings := runSettings(t, h.fixture)
	dispatcher, err := dispatch.Start(t.Context(), dispatch.Config{
		Database: h.fixture.database, Settings: settings, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	h.dispatcher = dispatcher
	t.Cleanup(func() { _ = dispatcher.Stop() })
}

// stalledResponse is the response writer of a client that has stopped reading.
// Its first body write blocks until unblock, as a socket write does once the
// peer never drains its receive window, but without depending on kernel buffer
// sizes or a clock: stalled closes as soon as a write is blocked, so a test knows
// the consumer is stuck before it produces anything.
type stalledResponse struct {
	header      http.Header
	status      atomic.Int32
	stalled     chan struct{}
	release     chan struct{}
	stalledOnce sync.Once
	releaseOnce sync.Once
}

func newStalledResponse() *stalledResponse {
	return &stalledResponse{header: http.Header{}, stalled: make(chan struct{}), release: make(chan struct{})}
}

func (r *stalledResponse) Header() http.Header { return r.header }

func (r *stalledResponse) WriteHeader(status int) { r.status.Store(int32(status)) }

func (r *stalledResponse) Flush() {}

func (r *stalledResponse) Write(body []byte) (int, error) {
	r.stalledOnce.Do(func() { close(r.stalled) })
	<-r.release
	return len(body), nil
}

// unblock lets every blocked and later write return. It is safe to call more
// than once.
func (r *stalledResponse) unblock() {
	r.releaseOnce.Do(func() { close(r.release) })
}

// stallEventStream serves one run's event stream to a consumer that stops
// reading at the first frame and returns once that write is blocked, so
// everything the test produces afterwards is produced against a stuck consumer.
// The returned function cancels the request, releases the consumer and waits
// for the handler to return. The minutes below only end a stream that never
// starts or never ends; they are not a speed bound.
func (h *runHarness) stallEventStream(t *testing.T, cookie *http.Cookie, runID string) (end func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	consumer := newStalledResponse()
	t.Cleanup(func() {
		cancel()
		consumer.unblock()
	})
	served := make(chan struct{})
	go func() {
		defer close(served)
		req := apiRequest(http.MethodGet, "/api/v1/runs/"+runID+"/events", "", cookie).WithContext(ctx)
		Handler(h.engine).ServeHTTP(consumer, req)
	}()
	select {
	case <-consumer.stalled:
	case <-served:
		t.Fatalf("the stream ended before its first frame, status = %d", consumer.status.Load())
	case <-time.After(time.Minute):
		t.Fatal("the stream never wrote its first frame")
	}
	if status := consumer.status.Load(); status != http.StatusOK {
		t.Fatalf("status = %d", status)
	}
	return func() {
		t.Helper()
		cancel()
		consumer.unblock()
		select {
		case <-served:
		case <-time.After(time.Minute):
			t.Fatal("the stream did not end after its request was canceled")
		}
	}
}

// deadlineResponse is the response writer of a connection that supports write
// deadlines. It records, in order, every deadline the handler sets and every
// body write, so a test can prove that each write was preceded by a fresh
// deadline without waiting for any deadline to expire.
type deadlineResponse struct {
	header http.Header
	status atomic.Int32
	// steps is appended by the handler goroutine only and read after served
	// closes, which orders every append before the read.
	steps  []deadlineStep
	frames chan string
	served chan struct{}
}

// deadlineStep is one recorded call: a SetWriteDeadline, or a body write.
type deadlineStep struct {
	at       time.Time
	deadline time.Time
	write    bool
}

func newDeadlineResponse() *deadlineResponse {
	return &deadlineResponse{header: http.Header{}, frames: make(chan string, 64), served: make(chan struct{})}
}

func (r *deadlineResponse) Header() http.Header { return r.header }

func (r *deadlineResponse) WriteHeader(status int) { r.status.Store(int32(status)) }

func (r *deadlineResponse) Flush() {}

func (r *deadlineResponse) SetWriteDeadline(deadline time.Time) error {
	r.steps = append(r.steps, deadlineStep{at: time.Now(), deadline: deadline})
	return nil
}

func (r *deadlineResponse) Write(body []byte) (int, error) {
	r.steps = append(r.steps, deadlineStep{at: time.Now(), write: true})
	r.frames <- string(body)
	return len(body), nil
}

// awaitFrame returns once a written frame contains want. The minute only ends a
// stream that never writes it; it is not a speed bound.
func (r *deadlineResponse) awaitFrame(t *testing.T, want string) {
	t.Helper()
	giveUp := time.After(time.Minute)
	for {
		select {
		case frame := <-r.frames:
			if strings.Contains(frame, want) {
				return
			}
		case <-r.served:
			t.Fatalf("the stream ended before a frame containing %q, status = %d", want, r.status.Load())
		case <-giveUp:
			t.Fatalf("no frame containing %q was written", want)
		}
	}
}

// recordEventStream serves one run's event stream to a deadlineResponse. The
// returned function cancels the request, waits for the handler to return and
// hands back everything the writer recorded.
func (h *runHarness) recordEventStream(t *testing.T, cookie *http.Cookie, runID string) (*deadlineResponse, func() []deadlineStep) {
	t.Helper()
	ctx, cancel := context.WithCancel(t.Context())
	t.Cleanup(cancel)
	recorder := newDeadlineResponse()
	go func() {
		defer close(recorder.served)
		req := apiRequest(http.MethodGet, "/api/v1/runs/"+runID+"/events", "", cookie).WithContext(ctx)
		Handler(h.engine).ServeHTTP(recorder, req)
	}()
	return recorder, func() []deadlineStep {
		t.Helper()
		cancel()
		select {
		case <-recorder.served:
		case <-time.After(time.Minute):
			t.Fatal("the stream did not end after its request was canceled")
		}
		return recorder.steps
	}
}
