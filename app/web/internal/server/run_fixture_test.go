package server

import (
	"bufio"
	"context"
	"crypto/tls"
	"crypto/x509"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/cookiejar"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

// runHarness is the real serve shape for the run API and SSE battery: the whole
// engine, with the real dispatcher attached, behind a real TLS listener built by
// server.New/Run. Nothing is mocked: SQLite is the control plane, the stream goes
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
	server     *http.Server
	stop       context.CancelFunc
	done       chan error
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
	h.listen(t, 0)
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
	h.listen(t, 0)
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
	h.listen(t, 0)
	return h
}

// listen starts (or restarts) the real server with the given WriteTimeout. The
// timeout case uses a deliberately short one to prove the per-write deadline.
func (h *runHarness) listen(t *testing.T, writeTimeout time.Duration) {
	t.Helper()
	h.certFile, h.keyFile, h.pool = tlsMaterial(t, t.TempDir())
	h.address = freeAddress(t)
	config := Config{Listen: h.address, CertFile: h.certFile, KeyFile: h.keyFile, ShutdownTimeout: 5 * time.Second}
	srv := New(config, Handler(h.engine))
	if writeTimeout > 0 {
		srv.WriteTimeout = writeTimeout
	}
	ctx, cancel := context.WithCancel(context.Background())
	h.server, h.stop = srv, cancel
	h.done = make(chan error, 1)
	go func() { h.done <- Run(ctx, srv, config) }()
	waitListening(t, h.address, true)
	t.Cleanup(func() {
		cancel()
		_ = srv.Close()
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

// restart serves the same engine again with a different WriteTimeout, which is
// how the write-deadline case outlives the server's own limit.
func (h *runHarness) restart(t *testing.T, writeTimeout time.Duration) {
	t.Helper()
	h.stop()
	_ = h.server.Close()
	<-h.done
	h.listen(t, writeTimeout)
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
		MaxChainLength: 3, Parallel: 1, ZstdLevel: 3,
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

// seedPolicyWithRun seeds the rows one run needs and returns the queued run.
func (h *runHarness) seedPolicyWithRun(t *testing.T, key string) (store.Policy, store.Run) {
	t.Helper()
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
	query := `SELECT COUNT(*) FROM audit_logs WHERE actor_type = 'system' AND action = ? AND result = 'success'`
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

// slowStreamClient opens the event stream over a real TLS connection whose
// receive buffer is deliberately tiny. That is a genuinely slow consumer at the
// transport level: the kernel stops accepting bytes, the server's write blocks,
// and the subscriber buffer overflows exactly as it would for a stalled client.
func (h *runHarness) slowStreamClient(t *testing.T, cookie *http.Cookie, path string) (*http.Response, *bufio.Reader, func()) {
	t.Helper()
	transport := &http.Transport{
		TLSClientConfig: &tls.Config{RootCAs: h.pool, MinVersion: tls.VersionTLS12},
		DialTLSContext: func(ctx context.Context, network, _ string) (net.Conn, error) {
			dialer := &net.Dialer{Timeout: 10 * time.Second, Control: smallReceiveBuffer(t)}
			return tls.DialWithDialer(dialer, network, h.address, &tls.Config{RootCAs: h.pool, ServerName: "127.0.0.1"})
		},
	}
	client := &http.Client{Transport: transport}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, "https://"+h.address+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	req.AddCookie(cookie)
	response, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return response, bufio.NewReader(response.Body), func() {
		_ = response.Body.Close()
		client.CloseIdleConnections()
	}
}

// smallReceiveBuffer shrinks SO_RCVBUF on the client socket, which is what makes
// the consumer slow enough to overflow the server's subscriber buffer.
func smallReceiveBuffer(t *testing.T) func(network, address string, conn syscall.RawConn) error {
	return func(_, _ string, raw syscall.RawConn) error {
		var sockErr error
		if err := raw.Control(func(fd uintptr) {
			sockErr = syscall.SetsockoptInt(int(fd), syscall.SOL_SOCKET, syscall.SO_RCVBUF, 2048)
		}); err != nil {
			return err
		}
		if sockErr != nil {
			t.Logf("cannot shrink the receive buffer: %v", sockErr)
		}
		return nil
	}
}
