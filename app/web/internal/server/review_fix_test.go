package server

import (
	"context"
	"database/sql"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/alexedwards/scs/v2"
	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

func TestRejectUnprotectedListen(t *testing.T) {
	cases := []struct {
		address string
		tls     bool
		proxy   bool
		ok      bool
	}{
		{"127.0.0.1:8080", false, false, true},
		{"localhost:8080", false, false, true},
		{"LOCALHOST:8080", false, false, true},
		{"[::1]:8080", false, false, true},
		{"0.0.0.0:8080", false, false, false},
		{":8080", false, false, false},
		{"192.0.2.10:8080", false, false, false},
		{"unresolvable.invalid:8080", false, false, false},
		{"192.0.2.10:8443", true, false, true},
		{"192.0.2.10:8080", false, true, true},
		{"example.com:8080", false, true, true},
	}
	for _, tc := range cases {
		err := RejectUnprotectedListen(tc.address, tc.tls, tc.proxy)
		if (err == nil) != tc.ok {
			t.Fatalf("listen %q tls=%t proxy=%t: %v", tc.address, tc.tls, tc.proxy, err)
		}
		if err != nil && !strings.Contains(err.Error(), "non-loopback") {
			t.Fatalf("listen %q error = %v", tc.address, err)
		}
	}
}

func TestRejectUnprotectedListenDoesNotResolveNames(t *testing.T) {
	resolver := net.DefaultResolver
	net.DefaultResolver = &net.Resolver{PreferGo: true, Dial: func(context.Context, string, string) (net.Conn, error) {
		t.Error("listen policy resolved DNS")
		return nil, errors.New("resolved")
	}}
	t.Cleanup(func() { net.DefaultResolver = resolver })
	start := time.Now()
	err := RejectUnprotectedListen("unresolvable.invalid:8080", false, false)
	if err == nil || !strings.Contains(err.Error(), "non-loopback") || time.Since(start) > time.Second {
		t.Fatalf("hostname listen: %v after %s", err, time.Since(start))
	}
}

func TestRunRejectsPlainNonLoopbackBeforeListen(t *testing.T) {
	config := Config{Listen: "192.0.2.10:8080", ShutdownTimeout: time.Second}
	ctx, cancel := context.WithTimeout(t.Context(), time.Second)
	defer cancel()
	start := time.Now()
	err := Run(ctx, New(config, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		t.Error("handler ran")
	})), config)
	if err == nil || !strings.Contains(err.Error(), "non-loopback") || time.Since(start) > time.Second {
		t.Fatalf("run error = %v after %s", err, time.Since(start))
	}
	config.ShutdownTimeout = 0
	config.Listen = "127.0.0.1:0"
	if err = Run(t.Context(), New(config, http.NotFoundHandler()), config); err == nil || !strings.Contains(err.Error(), "shutdown_timeout") {
		t.Fatalf("timeout error = %v", err)
	}
}

func TestShutdownWaitsForDetachedDatabaseWork(t *testing.T) {
	database := openReviewDatabase(t)
	entered := make(chan struct{})
	ready := make(chan error, 1)
	config := Config{Listen: freeAddress(t), ShutdownTimeout: 50 * time.Millisecond}
	srv := New(config, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		close(entered)
		<-r.Context().Done()
		ready <- database.Ready(context.WithoutCancel(r.Context()))
		w.WriteHeader(http.StatusNoContent)
	}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv, config) }()
	finish := shutdownFinisher(cancel, done)
	t.Cleanup(func() { _ = finish() })
	waitListening(t, config.Listen, true)
	go requestUntilShutdown(config.Listen)
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not enter the handler")
	}
	start := time.Now()
	err := finish()
	if elapsed := time.Since(start); elapsed > 3*time.Second {
		t.Fatalf("cooperative shutdown took %s", elapsed)
	}
	if !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "handlers still running") {
		t.Fatalf("shutdown error = %v", err)
	}
	assertDatabaseWorkFinished(t, ready)
}

func shutdownFinisher(cancel context.CancelFunc, done <-chan error) func() error {
	var once sync.Once
	var runErr error
	return func() error {
		once.Do(func() {
			runErr = waitShutdownResult(cancel, done)
		})
		return runErr
	}
}

func waitShutdownResult(cancel context.CancelFunc, done <-chan error) error {
	cancel()
	select {
	case err := <-done:
		return err
	case <-time.After(8 * time.Second):
		return errors.New("shutdown timed out")
	}
}

func requestUntilShutdown(listen string) {
	client := &http.Client{Timeout: 8 * time.Second}
	resp, err := client.Get("http://" + listen + "/slow")
	if resp != nil {
		resp.Body.Close()
	}
	_ = err
}

func assertDatabaseWorkFinished(t *testing.T, ready <-chan error) {
	t.Helper()
	select {
	case readyErr := <-ready:
		if readyErr != nil {
			t.Fatal(readyErr)
		}
	default:
		t.Fatal("database work did not finish before Run returned")
	}
}

func TestShutdownReportsUncooperativeHandlers(t *testing.T) {
	started := make(chan struct{})
	block := make(chan struct{})
	config := Config{Listen: freeAddress(t), ShutdownTimeout: 20 * time.Millisecond}
	srv := New(config, http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
		close(started)
		<-block
	}))
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, srv, config) }()
	var finishOnce sync.Once
	var runErr error
	finish := func() error {
		finishOnce.Do(func() {
			cancel()
			select {
			case runErr = <-done:
			case <-time.After(postCancelDrainWait() + 3*time.Second):
				runErr = errors.New("shutdown timed out")
			}
		})
		return runErr
	}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(block) }) }
	t.Cleanup(func() {
		_ = finish()
		release()
	})
	waitListening(t, config.Listen, true)
	go func() {
		client := &http.Client{Timeout: 8 * time.Second}
		resp, err := client.Get("http://" + config.Listen + "/stuck")
		if resp != nil {
			resp.Body.Close()
		}
		_ = err
	}()
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("request did not enter the handler")
	}
	start := time.Now()
	err := finish()
	elapsed := time.Since(start)
	release()
	if elapsed+200*time.Millisecond < postCancelDrainWait() {
		t.Fatalf("shutdown returned after %s, before post-cancel drain %s", elapsed, postCancelDrainWait())
	}
	if err == nil || !strings.Contains(err.Error(), "handlers still running after cancellation") || !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("shutdown error = %v", err)
	}
}

// TestShutdownWaitsOutSQLiteBusyBeforeReturning holds the database write lock
// with a second connection's BEGIN IMMEDIATE, then logs out through the real
// handler. Cancel is armed only after that logout stays inside a SQLite call.
// Run must still be blocked past the old graceful-plus-audit window while the
// logout is inside SQLite, and it may return only after that driver call
// finishes. Closing the client does not end the lock wait. The wait is the
// driver's busy sleep, not a channel stand-in.
func TestShutdownWaitsOutSQLiteBusyBeforeReturning(t *testing.T) {
	fixture := newAuthFixture(t)
	fixture.seed(t, "admin", "admin")
	config := Config{Listen: freeAddress(t), ShutdownTimeout: time.Second}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan error, 1)
	go func() { done <- Run(ctx, New(config, Handler(fixture.engine)), config) }()
	finish := waitRun(cancel, done, postCancelDrainWait()+config.ShutdownTimeout+3*time.Second)
	t.Cleanup(func() { _ = finish() })
	waitListening(t, config.Listen, true)
	cookie := liveLoginCookie(t, config.Listen)
	release := holdImmediateWriteLock(t, filepath.Join(fixture.dir, "intern.db"))
	defer release()
	logoutDone := make(chan logoutResult, 1)
	go func() {
		status, err := postStatus("http://"+config.Listen+"/api/v1/auth/logout", "", cookie, 20*time.Second)
		logoutDone <- logoutResult{status: status, err: err}
	}()
	waitForDriverBusy(t, fixture.database)
	cancel()
	// Past the old graceful timeout plus AuditTimeout, the logout must still be
	// inside SQLite and Run must not have returned. Close drops the client
	// connection; the handler's lock wait does not end with it.
	assertStillInSQLite(t, fixture.database, done, config.ShutdownTimeout+middleware.AuditTimeout+400*time.Millisecond)
	release()
	err := finish()
	assertBusyShutdownFinished(t, fixture, err)
	select {
	case result := <-logoutDone:
		t.Logf("logout client status=%d err=%v", result.status, result.err)
	case <-time.After(5 * time.Second):
		t.Fatal("logout client did not return")
	}
}

type logoutResult struct {
	status int
	err    error
}

func waitRun(cancel context.CancelFunc, done <-chan error, limit time.Duration) func() error {
	var once sync.Once
	var runErr error
	return func() error {
		once.Do(func() {
			cancel()
			select {
			case runErr = <-done:
			case <-time.After(limit):
				runErr = errors.New("shutdown timed out")
			}
		})
		return runErr
	}
}

func liveLoginCookie(t *testing.T, address string) *http.Cookie {
	t.Helper()
	body := `{"username":"admin","password":"` + testPassword + `"}`
	status, responseBody, cookies, err := postRaw("http://"+address+"/api/v1/auth/login", body, nil, 5*time.Second)
	if err != nil || status != http.StatusOK || len(cookies) != 1 {
		t.Fatalf("login status=%d err=%v cookies=%d body=%s", status, err, len(cookies), responseBody)
	}
	return cookies[0]
}

func postStatus(rawURL, body string, cookie *http.Cookie, timeout time.Duration) (int, error) {
	status, _, _, err := postRaw(rawURL, body, cookie, timeout)
	return status, err
}

func postRaw(rawURL, body string, cookie *http.Cookie, timeout time.Duration) (int, string, []*http.Cookie, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, rawURL, strings.NewReader(body))
	if err != nil {
		return 0, "", nil, err
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != nil {
		req.AddCookie(cookie)
	}
	resp, err := (&http.Client{}).Do(req)
	if err != nil {
		return 0, "", nil, err
	}
	defer resp.Body.Close()
	payload, err := io.ReadAll(io.LimitReader(resp.Body, 4096))
	return resp.StatusCode, string(payload), resp.Cookies(), err
}

func holdImmediateWriteLock(t *testing.T, path string) func() {
	t.Helper()
	u := url.URL{Scheme: "file", Path: path}
	query := u.Query()
	query.Add("_pragma", "busy_timeout(5000)")
	u.RawQuery = query.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	conn, err := db.Conn(t.Context())
	if err != nil {
		t.Fatal(err)
	}
	lockCtx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	if _, err = conn.ExecContext(lockCtx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	var once sync.Once
	return func() {
		once.Do(func() {
			_, _ = conn.ExecContext(context.Background(), "ROLLBACK")
			_ = conn.Close()
			_ = db.Close()
		})
	}
}

func waitForDriverBusy(t *testing.T, database *store.Store) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	var since time.Time
	for time.Now().Before(deadline) {
		if database.ConnectionsInUse() > 0 {
			if since.IsZero() {
				since = time.Now()
			}
			if time.Since(since) >= 200*time.Millisecond {
				return
			}
		} else {
			since = time.Time{}
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatal("logout did not stay inside a SQLite call")
}

func assertStillInSQLite(t *testing.T, database *store.Store, done <-chan error, wait time.Duration) {
	t.Helper()
	select {
	case err := <-done:
		t.Fatalf("Run returned while the write lock was still held: %v", err)
	case <-time.After(wait):
	}
	if database.ConnectionsInUse() == 0 {
		t.Fatal("SQLite call finished before the old shutdown window elapsed")
	}
}

func assertBusyShutdownFinished(t *testing.T, fixture *authFixture, err error) {
	t.Helper()
	if err == nil || !errors.Is(err, context.DeadlineExceeded) || strings.Contains(err.Error(), "handlers still running") {
		t.Fatalf("shutdown error = %v", err)
	}
	if fixture.database.ConnectionsInUse() != 0 {
		t.Fatal("SQLite call still active after Run returned")
	}
	if strings.Contains(fixture.logs.String(), "database is closed") {
		t.Fatalf("database closed under the logout: %s", fixture.logs.String())
	}
}

func TestHandlerDrainRejectsAfterSeal(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var nextCalls atomic.Int32
	drain := newHandlerDrain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if nextCalls.Add(1) == 1 {
			close(entered)
			<-release
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	go drain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("handler did not enter")
	}
	drain.seal()
	late := httptest.NewRecorder()
	drain.ServeHTTP(late, httptest.NewRequest(http.MethodGet, "/late", nil))
	assertLateRejected(t, late, nextCalls.Load())
	if drain.Wait(0) {
		t.Fatal("sealed drain reported idle while the first handler was running")
	}
	close(release)
	if !drain.Wait(time.Second) || nextCalls.Load() != 1 {
		t.Fatalf("drained=%v calls=%d", drain.Wait(0), nextCalls.Load())
	}
}

func assertLateRejected(t *testing.T, late *httptest.ResponseRecorder, calls int32) {
	t.Helper()
	if late.Code != http.StatusServiceUnavailable || calls != 1 || !strings.Contains(late.Body.String(), render.Unavailable) {
		t.Fatalf("late status=%d calls=%d body=%s", late.Code, calls, late.Body)
	}
	if late.Header().Get("Connection") != "close" {
		t.Fatalf("connection = %q", late.Header().Get("Connection"))
	}
}

func TestHandlerDrainSealDoesNotAdmitConcurrentCalls(t *testing.T) {
	var nextCalls atomic.Int32
	drain := newHandlerDrain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		nextCalls.Add(1)
		w.WriteHeader(http.StatusNoContent)
	}))
	drain.seal()
	var group sync.WaitGroup
	for range 32 {
		group.Add(1)
		go func() {
			defer group.Done()
			drain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/", nil))
		}()
	}
	group.Wait()
	if nextCalls.Load() != 0 || !drain.Wait(time.Second) {
		t.Fatalf("calls=%d", nextCalls.Load())
	}
}

func TestShutdownSuccessWaitsForTrackedHandler(t *testing.T) {
	entered := make(chan struct{})
	release := make(chan struct{})
	var releaseOnce sync.Once
	unlock := func() { releaseOnce.Do(func() { close(release) }) }
	var nextCalls atomic.Int32
	drain := newHandlerDrain(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		if nextCalls.Add(1) == 1 {
			close(entered)
			<-release
		}
		w.WriteHeader(http.StatusNoContent)
	}))
	srv := &http.Server{Handler: drain, ReadHeaderTimeout: time.Second}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	serveDone := make(chan error, 1)
	go func() { serveDone <- srv.Serve(listener) }()
	go drain.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "http://example/", nil))
	select {
	case <-entered:
	case <-time.After(time.Second):
		unlock()
		t.Fatal("handler did not enter")
	}
	_, cancelRoot := context.WithCancel(context.Background())
	finished := make(chan error, 1)
	go func() {
		finished <- shutdown(srv, Config{ShutdownTimeout: time.Second}, drain, middleware.NewDetachedAudit(), cancelRoot, serveDone)
	}()
	var once sync.Once
	var shutdownErr error
	take := func() error {
		once.Do(func() {
			select {
			case shutdownErr = <-finished:
			case <-time.After(2 * time.Second):
				shutdownErr = errors.New("shutdown timed out")
			}
		})
		return shutdownErr
	}
	t.Cleanup(func() {
		unlock()
		cancelRoot()
		_ = take()
	})
	waitForShutdownRejection(t, drain, finished)
	calls := nextCalls.Load()
	unlock()
	if err = take(); err != nil {
		t.Fatal(err)
	}
	if nextCalls.Load() != calls {
		t.Fatalf("handler calls = %d, want %d", nextCalls.Load(), calls)
	}
}

func waitForShutdownRejection(t *testing.T, drain *handlerDrain, finished <-chan error) {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		select {
		case err := <-finished:
			t.Fatalf("shutdown returned before the tracked handler finished: %v", err)
		default:
		}
		late := httptest.NewRecorder()
		drain.ServeHTTP(late, httptest.NewRequest(http.MethodGet, "http://example/late", nil))
		if late.Code == http.StatusServiceUnavailable {
			if !strings.Contains(late.Body.String(), render.Unavailable) {
				t.Fatalf("rejection body = %s", late.Body)
			}
			return
		}
		time.Sleep(time.Millisecond)
	}
	t.Fatal("late request was not rejected")
}

func openReviewDatabase(t *testing.T) *store.Store {
	t.Helper()
	database, err := store.Open(t.Context(), filepath.Join(t.TempDir(), "metadata"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { database.Close() })
	return database
}

func TestPanicDiscardsPartialBodyAndSkipsSessionCommit(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	f.engine.POST("/api/v1/panic-buffer", func(c *gin.Context) {
		f.service.Sessions.Put(c.Request.Context(), "panic_marker", "1")
		_, _ = c.Writer.Write([]byte(`{"partial":true,"secret":"panic-body-secret"}`))
		panic("handler-panic-marker")
	})
	f.engine.GET("/api/v1/panic-marker", func(c *gin.Context) {
		c.String(http.StatusOK, f.service.Sessions.GetString(c.Request.Context(), "panic_marker"))
	})
	response := serveEngine(t, f, "POST", "/api/v1/panic-buffer", "", "panic-buffer", cookie)
	if response.Code != http.StatusInternalServerError {
		t.Fatalf("status=%d body=%s", response.Code, response.Body)
	}
	if strings.Contains(response.Body.String(), "partial") || strings.Contains(response.Body.String(), "panic-body-secret") || strings.Contains(response.Body.String(), "handler-panic-marker") {
		t.Fatalf("panic response leaked: %s", response.Body)
	}
	assertSingleError(t, response, render.Internal)
	if len(response.Result().Cookies()) != 0 {
		t.Fatalf("panic set a cookie: %v", response.Result().Cookies())
	}
	assertLoggedRequest(t, f.logs.String(), "panic-buffer", 500)
	assertPanicLog(t, f.logs.Bytes(), "panic-buffer", "handler-panic-marker")
	f.request(t, "GET", "/api/v1/auth/session", "", cookie, 200)
	marker := serveEngine(t, f, "GET", "/api/v1/panic-marker", "", "panic-marker", cookie)
	if marker.Code != http.StatusOK || marker.Body.String() != "" {
		t.Fatalf("panic marker committed: %d %q", marker.Code, marker.Body)
	}
}

func TestBusinessFailureDoesNotCommitSession(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	f.engine.POST("/api/v1/fail-session", func(c *gin.Context) {
		f.service.Sessions.Put(c.Request.Context(), "fail_marker", "1")
		render.Error(c, http.StatusInternalServerError, render.Internal, "An internal error occurred")
	})
	f.engine.GET("/api/v1/fail-marker", func(c *gin.Context) {
		c.String(http.StatusOK, f.service.Sessions.GetString(c.Request.Context(), "fail_marker"))
	})
	response := serveEngine(t, f, "POST", "/api/v1/fail-session", "", "fail-session", cookie)
	assertSingleError(t, response, render.Internal)
	if len(response.Result().Cookies()) != 0 {
		t.Fatalf("5xx set a cookie: %v", response.Result().Cookies())
	}
	assertLoggedRequest(t, f.logs.String(), "fail-session", 500)
	f.request(t, "GET", "/api/v1/auth/session", "", cookie, 200)
	marker := serveEngine(t, f, "GET", "/api/v1/fail-marker", "", "fail-marker", cookie)
	if marker.Code != http.StatusOK || marker.Body.String() != "" {
		t.Fatalf("5xx marker committed: %d %q", marker.Code, marker.Body)
	}
}

type panicSessionStore struct {
	scs.CtxStore
}

func (panicSessionStore) Find(string) ([]byte, bool, error) { panic("session-find-panic") }

func (panicSessionStore) FindCtx(context.Context, string) ([]byte, bool, error) {
	panic("session-find-panic")
}

func TestSessionLoadPanicDoesNotDestroySession(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	original := f.service.Sessions.Store
	store, ok := original.(scs.CtxStore)
	if !ok {
		t.Fatal("session store is not contextual")
	}
	f.service.Sessions.Store = panicSessionStore{CtxStore: store}
	t.Cleanup(func() { f.service.Sessions.Store = original })
	req := apiRequest("POST", "/api/v1/auth/logout", "", cookie)
	req.Header.Set(render.RequestIDHeader, "session-panic")
	response := requestAPI(t, f.client, req, http.StatusInternalServerError)
	assertSingleError(t, response, render.Internal)
	if strings.Contains(response.Body.String(), "session-find-panic") {
		t.Fatalf("panic leaked: %s", response.Body)
	}
	if len(response.Result().Cookies()) != 0 {
		t.Fatalf("session panic cleared the cookie: %v", response.Result().Cookies())
	}
	assertPanicLog(t, f.logs.Bytes(), "session-panic", "session-find-panic")
	assertLoggedRequest(t, f.logs.String(), "session-panic", 500)
	f.service.Sessions.Store = original
	f.request(t, "GET", "/api/v1/auth/session", "", cookie, 200)
	if _, found, err := f.database.Sessions().Find(cookie.Value); err != nil || !found {
		t.Fatalf("session destroyed by load panic: found=%t err=%v", found, err)
	}
	if n := countAudit(t, f.dir, "auth.logout", "success"); n != 0 {
		t.Fatalf("session panic recorded auth.logout success=%d", n)
	}
}

func TestResponseBufferLimitAndStreaming(t *testing.T) {
	if middleware.MaxBufferedResponseBytes != 8<<20 {
		t.Fatalf("buffer cap = %d", middleware.MaxBufferedResponseBytes)
	}
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	registerBufferedBodyRoutes(f)
	assertExactBufferAccepted(t, serveEngine(t, f, "POST", "/api/v1/exact-body", "", "exact-body", cookie))
	assertOversizeBufferRejected(t, f, serveEngine(t, f, "POST", "/api/v1/wide-body", "", "wide-body", cookie))
	assertStreamingBufferRejected(t, f, serveEngine(t, f, "POST", "/api/v1/stream-body", "", "stream-body", cookie))
	marker := serveEngine(t, f, "GET", "/api/v1/buffer-marker", "", "buffer-marker", cookie)
	if marker.Body.String() != "" {
		t.Fatalf("buffer rejection committed session data %q", marker.Body)
	}
}

func registerBufferedBodyRoutes(f *authFixture) {
	f.engine.POST("/api/v1/exact-body", func(c *gin.Context) {
		payload := strings.Repeat("b", middleware.MaxBufferedResponseBytes)
		if _, err := c.Writer.Write([]byte(payload)); err != nil {
			panic(err)
		}
	})
	f.engine.POST("/api/v1/wide-body", func(c *gin.Context) {
		f.service.Sessions.Put(c.Request.Context(), "wide_marker", "1")
		_, _ = c.Writer.Write([]byte(strings.Repeat("a", middleware.MaxBufferedResponseBytes+1)))
	})
	f.engine.POST("/api/v1/stream-body", func(c *gin.Context) {
		f.service.Sessions.Put(c.Request.Context(), "stream_marker", "1")
		_, _ = c.Writer.Write([]byte("stream-body-secret"))
		c.Writer.Flush()
	})
	f.engine.GET("/api/v1/buffer-marker", func(c *gin.Context) {
		c.String(http.StatusOK, f.service.Sessions.GetString(c.Request.Context(), "wide_marker")+f.service.Sessions.GetString(c.Request.Context(), "stream_marker"))
	})
}

func assertExactBufferAccepted(t *testing.T, exact *httptest.ResponseRecorder) {
	t.Helper()
	if exact.Code != http.StatusOK || exact.Body.Len() != middleware.MaxBufferedResponseBytes {
		t.Fatalf("exact buffer status=%d len=%d", exact.Code, exact.Body.Len())
	}
}

func assertOversizeBufferRejected(t *testing.T, f *authFixture, wide *httptest.ResponseRecorder) {
	t.Helper()
	if wide.Code != http.StatusInternalServerError || strings.Contains(wide.Body.String(), "aaa") {
		t.Fatalf("oversize status=%d len=%d", wide.Code, wide.Body.Len())
	}
	assertSingleError(t, wide, render.Internal)
	if len(wide.Result().Cookies()) != 0 {
		t.Fatal("oversize response committed a cookie")
	}
	assertLoggedRequest(t, f.logs.String(), "wide-body", 500)
	if !strings.Contains(f.logs.String(), "response exceeded buffer limit") {
		t.Fatal(f.logs.String())
	}
}

func assertStreamingBufferRejected(t *testing.T, f *authFixture, stream *httptest.ResponseRecorder) {
	t.Helper()
	if stream.Code != http.StatusInternalServerError || strings.Contains(stream.Body.String(), "stream-body-secret") {
		t.Fatalf("stream status=%d body=%s", stream.Code, stream.Body)
	}
	assertSingleError(t, stream, render.Internal)
	assertLoggedRequest(t, f.logs.String(), "stream-body", 500)
	if !strings.Contains(f.logs.String(), "streaming responses are not supported") {
		t.Fatal(f.logs.String())
	}
}

type blockingAnonymousHasher struct {
	auth.PasswordHasher
	started chan struct{}
	hold    chan struct{}
	once    sync.Once
}

func (b *blockingAnonymousHasher) block() {
	b.once.Do(func() { close(b.started) })
	<-b.hold
}

func (b *blockingAnonymousHasher) Verify(password, encoded string) (bool, error) {
	b.block()
	return b.PasswordHasher.Verify(password, encoded)
}

func (b *blockingAnonymousHasher) VerifyDummy(password string) error {
	b.block()
	return b.PasswordHasher.VerifyDummy(password)
}

func TestAnonymousVerificationDoesNotBlockAuthenticatedMutations(t *testing.T) {
	f := newAuthFixture(t)
	f.seed(t, "admin", "admin")
	cookie := f.login(t, "admin", testPassword)
	blocker := &blockingAnonymousHasher{
		PasswordHasher: f.service.Passwords,
		started:        make(chan struct{}),
		hold:           make(chan struct{}),
	}
	f.service.Passwords = blocker
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(blocker.hold) }) }
	var requests sync.WaitGroup
	t.Cleanup(func() {
		release()
		requests.Wait()
	})
	loginErr := startBlockedAnonymousLogin(f, &requests)
	select {
	case <-blocker.started:
	case <-time.After(5 * time.Second):
		t.Fatal("anonymous verification did not start")
	}
	passwordCookie := changePasswordWhileAnonymousHeld(t, f, cookie, &requests)
	createUserWhileAnonymousHeld(t, f, passwordCookie, &requests)
	release()
	select {
	case err := <-loginErr:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("blocked login did not finish")
	}
}

func startBlockedAnonymousLogin(f *authFixture, requests *sync.WaitGroup) <-chan error {
	loginErr := make(chan error, 1)
	requests.Add(1)
	go func() {
		defer requests.Done()
		req := apiRequest("POST", "/api/v1/auth/login", `{"username":"missing","password":"`+testPassword+`"}`, nil)
		req.Header.Set(render.RequestIDHeader, "blocked-login")
		recorder := httptest.NewRecorder()
		f.client.handler.ServeHTTP(recorder, req)
		if recorder.Code != http.StatusUnauthorized {
			loginErr <- errors.New(recorder.Body.String())
			return
		}
		loginErr <- nil
	}()
	return loginErr
}

func changePasswordWhileAnonymousHeld(t *testing.T, f *authFixture, cookie *http.Cookie, requests *sync.WaitGroup) *http.Cookie {
	t.Helper()
	body := `{"current_password":"` + testPassword + `","new_password":"` + changedPassword + `"}`
	changed := make(chan *httptest.ResponseRecorder, 1)
	changeErr := make(chan error, 1)
	requests.Add(1)
	go func() {
		defer requests.Done()
		response, err := f.client.roundTrip(apiRequest("PUT", "/api/v1/auth/password", body, cookie))
		if err != nil {
			changeErr <- err
			return
		}
		changed <- response
	}()
	var passwordCookie *http.Cookie
	select {
	case err := <-changeErr:
		t.Fatal(err)
	case response := <-changed:
		if response.Code != http.StatusNoContent {
			t.Fatalf("password change status=%d body=%s", response.Code, response.Body)
		}
		cookies := response.Result().Cookies()
		if len(cookies) != 1 {
			t.Fatalf("password cookies=%d", len(cookies))
		}
		passwordCookie = cookies[0]
	case <-time.After(5 * time.Second):
		t.Fatal("password change blocked while anonymous verification was held")
	}
	return passwordCookie
}

func createUserWhileAnonymousHeld(t *testing.T, f *authFixture, passwordCookie *http.Cookie, requests *sync.WaitGroup) {
	t.Helper()
	created := make(chan *httptest.ResponseRecorder, 1)
	createErr := make(chan error, 1)
	requests.Add(1)
	go func() {
		defer requests.Done()
		response, err := f.client.roundTrip(apiRequest("POST", "/api/v1/users", `{"username":"pressure","password":"`+changedPassword+`","role":"viewer"}`, passwordCookie))
		if err != nil {
			createErr <- err
			return
		}
		created <- response
	}()
	select {
	case err := <-createErr:
		t.Fatal(err)
	case response := <-created:
		if response.Code != http.StatusCreated {
			t.Fatalf("create user status=%d body=%s", response.Code, response.Body)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("user create blocked while anonymous verification was held")
	}
}

func serveEngine(t *testing.T, f *authFixture, method, path, body, requestID string, cookie *http.Cookie) *httptest.ResponseRecorder {
	t.Helper()
	req := apiRequest(method, path, body, cookie)
	if requestID != "" {
		req.Header.Set(render.RequestIDHeader, requestID)
	}
	recorder := httptest.NewRecorder()
	f.client.handler.ServeHTTP(recorder, req)
	return recorder
}
