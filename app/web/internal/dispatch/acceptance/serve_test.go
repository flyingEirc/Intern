//go:build integration

package acceptance

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"syscall"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/server"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/internal/credential"
)

// TestW4ServeChild is the re-execed serve process: the same store.Open,
// dispatch.Start, Handler and TLS listener path as intern-web serve, driven by
// the parent over HTTPS with real SIGTERM/SIGKILL.
func TestW4ServeChild(t *testing.T) {
	if os.Getenv("INTERN_W4_SERVE") != "1" {
		t.Fatal("serve child requires INTERN_W4_SERVE")
	}
	requireNative(t)
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	err := runServe(ctx,
		os.Getenv("INTERN_W4_METADATA_DIR"),
		os.Getenv("INTERN_W4_LISTEN"),
		os.Getenv("INTERN_W4_TLS_CERT"),
		os.Getenv("INTERN_W4_TLS_KEY"),
		os.Getenv("INTERN_W4_KEK"),
		os.Getenv("INTERN_W4_WORK_ROOT"),
		os.Getenv("INTERN_W4_XTRABACKUP"),
	)
	if ctx.Err() != nil {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
}

func runServe(ctx context.Context, metadata, listen, cert, key, kek, work, xtrabackup string) (retErr error) {
	if metadata == "" || listen == "" || cert == "" || key == "" || kek == "" || work == "" || xtrabackup == "" {
		return errors.New("serve child is missing required INTERN_W4_* paths")
	}
	database, err := store.Open(ctx, metadata)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, database.Close()) }()
	engine, shutdown, err := serveEngine(ctx, database, kek, metadata, work, xtrabackup)
	if err != nil {
		return err
	}
	defer shutdown()
	config := server.Config{Listen: listen, CertFile: cert, KeyFile: key, ShutdownTimeout: 30 * time.Second}
	return server.Run(ctx, server.New(config, server.Handler(engine)), config)
}

// serveEngine is the production serve wiring: dispatcher, session service and
// the resource service, exactly as cmd.serve assembles them.
func serveEngine(ctx context.Context, database *store.Store, kek, metadata, work, xtrabackup string) (*gin.Engine, func(), error) {
	sealer, err := credential.Open(kek, metadata, work)
	if err != nil {
		return nil, nil, err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	settings := resources.Settings{
		Sealer: sealer, KeyFile: kek, MetadataDir: metadata, WorkRoot: work,
		XtrabackupPath: xtrabackup, MinWorkBytes: 1, MinBackupBytes: 1,
	}
	svc := &resources.Service{Store: database, Settings: settings}
	authn, err := auth.New(database, auth.DefaultSessionConfig(), auth.DefaultPasswordParams())
	if err != nil {
		return nil, nil, err
	}
	stopCleanup := authn.StartCleanup(ctx, time.Minute, logger)
	dispatcher, err := dispatch.Start(ctx, dispatch.Config{
		Database: database, Settings: settings, Logger: logger, Preflight: svc,
	})
	if err != nil {
		stopCleanup()
		return nil, nil, err
	}
	engine, err := server.NewEngineWithResources(database, logger, nil, authn, svc, dispatcher)
	if err != nil {
		_ = dispatcher.Stop()
		stopCleanup()
		return nil, nil, err
	}
	shutdown := func() {
		_ = dispatcher.Stop()
		stopCleanup()
	}
	return engine, shutdown, nil
}
