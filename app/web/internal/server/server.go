package server

import (
	"context"
	"crypto/tls"
	"errors"
	"net"
	"net/http"
	"sync"
	"time"

	"github.com/flyingEirc/Intern/app/web/internal/middleware"
	"github.com/flyingEirc/Intern/app/web/internal/render"
	"github.com/flyingEirc/Intern/app/web/internal/store"
)

type Config struct {
	Listen               string
	CertFile             string
	KeyFile              string
	ShutdownTimeout      time.Duration
	TLSTerminatedByProxy bool
}

func New(config Config, handler http.Handler) *http.Server {
	return &http.Server{
		Addr: config.Listen, Handler: handler,
		ReadHeaderTimeout: 10 * time.Second, ReadTimeout: 30 * time.Second,
		WriteTimeout: 60 * time.Second, IdleTimeout: 120 * time.Second,
		MaxHeaderBytes: 1 << 20,
		TLSConfig:      &tls.Config{MinVersion: tls.VersionTLS12},
	}
}

// postCancelDrainMargin keeps the handler wait from ending in the same instant
// as a SQLite busy deadline. The busy handler can sleep slightly past
// store.BusyTimeout, and a second statement can do the same.
const postCancelDrainMargin = 500 * time.Millisecond

// errHandlersStillRunning is returned when handlers are still inside ServeHTTP
// after cancellation and postCancelDrainWait. A normal SQLite lock wait finishes
// inside that window: the statement already in the driver, which does not
// observe cancellation, plus the detached audit that can follow it. Handlers
// still running then are stuck in I/O or ignoring cancellation. Run cannot both
// return in finite time and keep the database open until they finish.
var errHandlersStillRunning = errors.New("handlers still running after cancellation and the SQLite busy and detached-audit bounds; normal lock waits finish within that time, and a finite shutdown cannot keep the database open for handlers that ignore cancellation or block beyond it")

// postCancelDrainWait is how long Run waits after it cancels request contexts.
// The in-flight statement can ignore cancellation for up to store.BusyTimeout.
// The detached audit that follows has an AuditTimeout context budget, but once
// it enters the driver its busy callback can ignore cancellation for up to
// store.BusyTimeout as well. Those two constants are equal, so the sum covers
// both waits. The margin keeps the wait from ending on the same instant as the
// driver's deadline. Stuck I/O past this window is not a normal lock wait.
func postCancelDrainWait() time.Duration {
	return middleware.AuditTimeout + store.BusyTimeout + postCancelDrainMargin
}

// Run serves until ctx is canceled. The caller closes the database after Run
// returns, so Run waits for tracked handlers first. The one exception is
// errHandlersStillRunning: those handlers are past a normal SQLite busy wait,
// and waiting for them without a bound would not be finite.
//
// Cancellation stops new connections, then waits ShutdownTimeout while request
// contexts stay active. If handlers remain, or Shutdown reports success while a
// tracked handler is still inside ServeHTTP, Run stops admitting new work,
// bounds detached audits, cancels request contexts, closes connections, and
// waits postCancelDrainWait. Own handlers are not all interrupted immediately.
// A statement already in the SQLite busy handler can run until store.BusyTimeout.
func Run(ctx context.Context, srv *http.Server, config Config) error {
	if err := config.validate(); err != nil {
		return err
	}
	drain := newHandlerDrain(srv.Handler)
	srv.Handler = drain
	audit := middleware.NewDetachedAudit()
	root, cancelRoot := context.WithCancel(context.Background())
	defer cancelRoot()
	installBaseContext(srv, audit.WithContext(root))

	done := make(chan error, 1)
	go func() {
		var err error
		if config.CertFile != "" {
			err = srv.ListenAndServeTLS(config.CertFile, config.KeyFile)
		} else {
			err = srv.ListenAndServe()
		}
		done <- err
	}()
	select {
	case err := <-done:
		return serverError(err)
	case <-ctx.Done():
		return shutdown(srv, config, drain, audit, cancelRoot, done)
	}
}

func (config Config) validate() error {
	if config.ShutdownTimeout <= 0 {
		return errors.New("shutdown_timeout must be positive")
	}
	return RejectUnprotectedListen(config.Listen, config.CertFile != "", config.TLSTerminatedByProxy)
}

func shutdown(srv *http.Server, config Config, drain *handlerDrain, audit *middleware.DetachedAudit, cancelRoot context.CancelFunc, done <-chan error) error {
	shutdownCtx, cancel := context.WithTimeout(context.Background(), config.ShutdownTimeout)
	shutdownErr := srv.Shutdown(shutdownCtx)
	cancel()
	// Shutdown can report success while a handler is still tracked, or a request
	// can pass Accept as the listener closes. Seal before deciding the server is
	// idle so that decision cannot race with another n++.
	if shutdownErr == nil && drain.drainedAfterSeal() {
		return serverError(<-done)
	}
	return stopRemainingHandlers(srv, drain, audit, cancelRoot, shutdownErr, done)
}

// stopRemainingHandlers is the forced drain. It runs when the graceful deadline
// fired and when Shutdown reported success but a tracked handler is still inside
// ServeHTTP. Both paths bound detached audits, cancel request contexts, close
// connections, and wait out a normal SQLite busy before the caller may close
// the database. Work still running after that is not a normal lock wait.
func stopRemainingHandlers(srv *http.Server, drain *handlerDrain, audit *middleware.DetachedAudit, cancelRoot context.CancelFunc, shutdownErr error, done <-chan error) error {
	drain.seal()
	audit.Bound(middleware.AuditTimeout)
	cancelRoot()
	closeErr := srv.Close()
	drained := drain.Wait(postCancelDrainWait())
	listenErr := serverError(<-done)
	if !drained {
		return errors.Join(shutdownErr, closeErr, errHandlersStillRunning, listenErr)
	}
	return errors.Join(shutdownErr, closeErr, listenErr)
}

func installBaseContext(srv *http.Server, base context.Context) {
	previous := srv.BaseContext
	srv.BaseContext = func(ln net.Listener) context.Context {
		if previous == nil {
			return base
		}
		parent := previous(ln)
		if parent == nil {
			return base
		}
		ctx, cancel := context.WithCancel(parent)
		go func() {
			select {
			case <-base.Done():
				cancel()
			case <-ctx.Done():
			}
		}()
		if audit, ok := middleware.DetachedAuditFrom(base); ok {
			return audit.WithContext(ctx)
		}
		return ctx
	}
}

func serverError(err error) error {
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// handlerDrain counts ServeHTTP calls that entered the application handler.
// http.Server.Shutdown can return while a handler is still running. A WaitGroup
// Add/Wait pair races with a call that arrives as Wait begins, so the counter
// and the closed gate share one mutex. After seal, a new call cannot increment
// n, and Wait cannot observe zero while a call that passed the gate is still inside.
type handlerDrain struct {
	next   http.Handler
	mu     sync.Mutex
	cond   *sync.Cond
	n      int
	closed bool
}

func newHandlerDrain(next http.Handler) *handlerDrain {
	drain := &handlerDrain{next: next}
	drain.cond = sync.NewCond(&drain.mu)
	return drain
}

func (d *handlerDrain) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	d.mu.Lock()
	if d.closed {
		d.mu.Unlock()
		rejectShuttingDown(w)
		return
	}
	d.n++
	d.mu.Unlock()
	defer func() {
		d.mu.Lock()
		d.n--
		if d.n == 0 {
			d.cond.Broadcast()
		}
		d.mu.Unlock()
	}()
	next := d.next
	if next == nil {
		next = http.DefaultServeMux
	}
	next.ServeHTTP(w, r)
}

// seal refuses new application work. Concurrent ServeHTTP either already
// counted n under this lock or observes the gate and returns without entering next.
func (d *handlerDrain) seal() {
	d.mu.Lock()
	d.closed = true
	d.mu.Unlock()
}

// drainedAfterSeal closes admission and reports whether every call that passed
// the gate has already returned. seal publishes closed under the same mutex as
// n++, so a call cannot enter between seal and Wait.
func (d *handlerDrain) drainedAfterSeal() bool {
	d.seal()
	return d.Wait(0)
}

func rejectShuttingDown(w http.ResponseWriter) {
	w.Header().Set("Connection", "close")
	if err := render.HTTPError(w, http.StatusServiceUnavailable, render.Unavailable, "Server is shutting down"); err != nil {
		return
	}
}

func (d *handlerDrain) Wait(timeout time.Duration) bool {
	d.mu.Lock()
	defer d.mu.Unlock()
	if d.n == 0 || timeout <= 0 {
		return d.n == 0
	}
	timedOut := false
	timer := time.AfterFunc(timeout, func() {
		d.mu.Lock()
		timedOut = true
		d.cond.Broadcast()
		d.mu.Unlock()
	})
	defer timer.Stop()
	// The flag is read under the same lock the timer sets, including when the
	// callback runs before Wait. A bare Broadcast would be lost in that window.
	for d.n > 0 && !timedOut {
		d.cond.Wait()
	}
	return d.n == 0
}
