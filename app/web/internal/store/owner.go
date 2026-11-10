package store

import (
	"context"
	"database/sql"
	"database/sql/driver"
	"errors"
	"fmt"
	"io"
	"net"
	"strings"
	"sync"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/stdlib"
)

var (
	// ErrOwnerInUse reports that another process already owns this database.
	ErrOwnerInUse = errors.New("PostgreSQL control plane is already owned by another process")
	// ErrOwnerLost reports that the owned connection ended. The store never
	// reconnects: a process that cannot prove it still owns the database must
	// stop scheduling and let a restarted process take over.
	ErrOwnerLost  = errors.New("PostgreSQL control-plane owner connection was lost")
	errStoreClose = errors.New("PostgreSQL control-plane store is closing")
)

// ownerLockSQL is a session-level advisory lock scoped by database name. The
// two-int4 form keeps one database's owner independent from another database
// in the same PostgreSQL cluster.
const ownerLockSQL = `SELECT pg_try_advisory_lock(hashtext(current_database()), 17477)`

type ownerGuard struct {
	mu        sync.Mutex
	attempted bool
	acquired  bool
	err       error
	lost      chan struct{}
}

func newOwnerGuard() *ownerGuard {
	return &ownerGuard{lost: make(chan struct{})}
}

func (o *ownerGuard) beforeConnect(context.Context, *pgx.ConnConfig) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.attempted || o.err != nil {
		return o.failLocked(ErrOwnerLost)
	}
	o.attempted = true
	return nil
}

func (o *ownerGuard) afterConnect(ctx context.Context, conn *pgx.Conn) error {
	var held bool
	if err := conn.QueryRow(ctx, ownerLockSQL).Scan(&held); err != nil {
		return o.fail(errors.Join(err, closeOwnerConnection(conn)))
	}
	if !held {
		return o.fail(errors.Join(ErrOwnerInUse, closeOwnerConnection(conn)))
	}
	o.mu.Lock()
	o.acquired = true
	o.mu.Unlock()
	return nil
}

func (o *ownerGuard) resetSession(_ context.Context, conn *pgx.Conn) error {
	o.mu.Lock()
	lost, acquired := o.err != nil, o.acquired
	o.mu.Unlock()
	if lost || !acquired {
		return driver.ErrBadConn
	}
	if conn.IsClosed() {
		o.lose()
		return driver.ErrBadConn
	}
	return nil
}

// lose permanently records the owned session as lost.
func (o *ownerGuard) lose() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err == nil {
		o.err = ErrOwnerLost
		close(o.lost)
	}
}

func (o *ownerGuard) stop() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.err == nil {
		o.err = errStoreClose
		close(o.lost)
	}
}

func (o *ownerGuard) fail(reason error) error {
	if reason == nil {
		reason = ErrOwnerLost
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.failLocked(reason)
}

func (o *ownerGuard) failLocked(reason error) error {
	if o.err == nil {
		o.err = reason
		close(o.lost)
	}
	return o.err
}

func (o *ownerGuard) errValue() error {
	o.mu.Lock()
	defer o.mu.Unlock()
	return o.err
}

func (o *ownerGuard) connector(config pgx.ConnConfig) driver.Connector {
	// Driver operations use independent, bounded contexts below. Request
	// cancellation must never become ErrBadConn or release the owner lock.
	// PostgreSQL's statement_timeout precedes the independent network deadline.
	// CancelRequest is unused for interactive statements: its delivery can race
	// the next statement, including a transaction's COMMIT. Offline import arms
	// it and waits out that race before rolling back.
	return ownerConnector{Connector: stdlib.GetConnector(config,
		stdlib.OptionBeforeConnect(o.beforeConnect),
		stdlib.OptionAfterConnect(o.afterConnect),
		stdlib.OptionResetSession(o.resetSession),
	)}
}

type ownerConnector struct{ driver.Connector }

func (c ownerConnector) Connect(ctx context.Context) (driver.Conn, error) {
	conn, err := c.Connector.Connect(ctx)
	if err != nil {
		return nil, err
	}
	pgxConn, ok := conn.(*stdlib.Conn)
	if !ok {
		return nil, errors.Join(fmt.Errorf("unexpected PostgreSQL driver connection %T", conn), conn.Close())
	}
	return &ownerConnection{Conn: pgxConn}, nil
}

type ownerConnection struct{ *stdlib.Conn }

func (c *ownerConnection) ExecContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Result, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	operation, stop := c.statementContext(ctx)
	defer stop()
	result, err := c.Conn.ExecContext(operation, query, args)
	return finishCallerCancel(ctx, result, err)
}

func (c *ownerConnection) QueryContext(ctx context.Context, query string, args []driver.NamedValue) (driver.Rows, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	operation, stop := c.statementContext(ctx)
	rows, err := c.Conn.QueryContext(operation, query, args)
	return adoptDriverRows(ctx, rows, err, stop)
}

func (c *ownerConnection) PrepareContext(ctx context.Context, query string) (driver.Stmt, error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	operation, stop := c.statementContext(ctx)
	stmt, err := c.Conn.PrepareContext(operation, query)
	stop()
	if err != nil {
		return nil, err
	}
	return ownerStmt{Stmt: stmt, conn: c}, nil
}

type ownerStmt struct {
	driver.Stmt
	conn *ownerConnection
}

func (s ownerStmt) ExecContext(ctx context.Context, args []driver.NamedValue) (driver.Result, error) {
	execer, ok := s.Stmt.(driver.StmtExecContext)
	if !ok {
		return nil, errors.New("PostgreSQL statement does not support context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	operation, stop := s.conn.statementContext(ctx)
	defer stop()
	result, err := execer.ExecContext(operation, args)
	return finishCallerCancel(ctx, result, err)
}

func (s ownerStmt) QueryContext(ctx context.Context, args []driver.NamedValue) (driver.Rows, error) {
	queryer, ok := s.Stmt.(driver.StmtQueryContext)
	if !ok {
		return nil, errors.New("PostgreSQL statement does not support context")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	operation, stop := s.conn.statementContext(ctx)
	rows, err := queryer.QueryContext(operation, args)
	return adoptDriverRows(ctx, rows, err, stop)
}

func finishCallerCancel(ctx context.Context, result driver.Result, err error) (driver.Result, error) {
	if ctx.Err() == nil {
		return result, err
	}
	if err == nil {
		return nil, ctx.Err()
	}
	if isImport(ctx) {
		return nil, errors.Join(ctx.Err(), err)
	}
	return result, err
}

func adoptDriverRows(ctx context.Context, rows driver.Rows, err error, stop func()) (driver.Rows, error) {
	if err != nil {
		stop()
		return nil, err
	}
	if ctx.Err() != nil {
		closeErr := rows.Close()
		stop()
		return nil, errors.Join(ctx.Err(), closeErr)
	}
	driverRows, ok := rows.(*stdlib.Rows)
	if !ok {
		closeErr := rows.Close()
		stop()
		return nil, errors.Join(errors.New("unexpected PostgreSQL driver rows"), closeErr)
	}
	return &ownerRows{Rows: driverRows, cancel: stop}, nil
}

type ownerRows struct {
	*stdlib.Rows
	cancel func()
}

func (r *ownerRows) Close() error {
	err := r.Rows.Close()
	r.cancel()
	return err
}

func (c *ownerConnection) Ping(ctx context.Context) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	probe, cancel := context.WithTimeout(context.WithoutCancel(ctx), StatementTimeout+time.Second)
	defer cancel()
	err := c.pingSession(probe)
	if err == nil || !ownerSessionLost(err) {
		return err
	}
	return errors.Join(err, driver.ErrBadConn)
}

func (c *ownerConnection) pingSession(ctx context.Context) error {
	if c.Conn.Conn().IsClosed() {
		return driver.ErrBadConn
	}
	healthProbeMu.Lock()
	probe := healthProbe
	healthProbeMu.Unlock()
	if probe != nil {
		return probe(ctx, c)
	}
	return c.Conn.Conn().Ping(ctx)
}

func (c *ownerConnection) ResetSession(ctx context.Context) error {
	// database/sql can call ResetSession with a context canceled just after
	// checkout. stdlib treats a canceled liveness ping as ErrBadConn, even
	// when the physical session is healthy. Only an independent probe may
	// decide that the owner session has been lost.
	probe, cancel := context.WithTimeout(context.WithoutCancel(ctx), StatementTimeout+time.Second)
	defer cancel()
	return c.Conn.ResetSession(probe)
}

func closeOwnerConnection(conn *pgx.Conn) error {
	ctx, cancel := context.WithTimeout(context.WithoutCancel(context.Background()), 5*time.Second)
	defer cancel()
	return conn.Close(ctx)
}

type importContextKey struct{}

func withImport(ctx context.Context) context.Context {
	return context.WithValue(ctx, importContextKey{}, struct{}{})
}

func isImport(ctx context.Context) bool {
	_, ok := ctx.Value(importContextKey{}).(struct{})
	return ok
}

var (
	healthProbeMu sync.Mutex
	// healthProbe replaces the protocol ping in tests. The replacement runs on
	// the same Watch path so a server-canceled probe is observable.
	healthProbe func(context.Context, *ownerConnection) error
)

func (c *ownerConnection) statementContext(ctx context.Context) (context.Context, func()) {
	if !isImport(ctx) {
		operation, cancel := context.WithTimeout(context.WithoutCancel(ctx), StatementTimeout+time.Second)
		return operation, cancel
	}
	return c.watchImport(ctx)
}

// importCancelSettle is the pause after CancelRequest before the next command.
// The server can apply the cancel to the following statement, including ROLLBACK.
const importCancelSettle = 100 * time.Millisecond

// watchImport aborts the in-flight import statement when ctx is canceled.
// The context passed to pgx is not canceled: the default watcher would set a
// socket deadline, drop the owner session, and release the advisory lock
// before rollback.
func (c *ownerConnection) watchImport(ctx context.Context) (context.Context, func()) {
	if ctx.Done() == nil {
		return context.WithoutCancel(ctx), func() {}
	}
	stop := make(chan struct{})
	finished := make(chan struct{})
	var cancelOnce sync.Once
	sendCancel := func() { cancelOnce.Do(c.cancelImportQuery) }
	go func() {
		defer close(finished)
		select {
		case <-ctx.Done():
			sendCancel()
		case <-stop:
		}
	}()
	var stopOnce sync.Once
	return context.WithoutCancel(ctx), func() {
		stopOnce.Do(func() {
			if ctx.Err() != nil {
				sendCancel()
			}
			close(stop)
			<-finished
			if ctx.Err() != nil {
				time.Sleep(importCancelSettle)
			}
		})
	}
}

func (c *ownerConnection) cancelImportQuery() {
	cancelCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pgConn := c.Conn.Conn().PgConn()
	if pgConn.IsClosed() {
		return
	}
	if err := pgConn.CancelRequest(cancelCtx); err != nil {
		return
	}
}

// ownerSessionLost reports that the owned session itself is gone. A canceled
// query, including SQLSTATE 57014, leaves the session usable.
func ownerSessionLost(err error) bool {
	if err == nil || errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return fatalPostgres(pgErr)
	}
	return connectionFailure(err)
}

func fatalPostgres(pgErr *pgconn.PgError) bool {
	severity := pgErr.SeverityUnlocalized
	if severity == "" {
		severity = pgErr.Severity
	}
	return strings.EqualFold(severity, "FATAL") || strings.EqualFold(severity, "PANIC")
}

func connectionFailure(err error) bool {
	if errors.Is(err, driver.ErrBadConn) || errors.Is(err, pgconn.ErrConnClosed) || errors.Is(err, sql.ErrConnDone) {
		return true
	}
	if errors.Is(err, io.EOF) || errors.Is(err, io.ErrClosedPipe) || errors.Is(err, net.ErrClosed) {
		return true
	}
	var netErr net.Error
	return errors.As(err, &netErr)
}
