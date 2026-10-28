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
	// The child mirrors intern-web serve: a canceled command context is a
	// normal shutdown, and every other error — including a dispatcher drain
	// that outlived its bounded window — fails the process so the parent can
	// read a non-zero exit instead of guessing.
	if err := runServe(ctx,
		os.Getenv("INTERN_W4_METADATA_DIR"),
		os.Getenv("INTERN_W4_LISTEN"),
		os.Getenv("INTERN_W4_TLS_CERT"),
		os.Getenv("INTERN_W4_TLS_KEY"),
		os.Getenv("INTERN_W4_KEK"),
		os.Getenv("INTERN_W4_WORK_ROOT"),
		os.Getenv("INTERN_W4_XTRABACKUP"),
		os.Getenv("INTERN_W5_EXPORT_ROOT"),
		os.Getenv("INTERN_W5_XBSTREAM"),
		os.Getenv("INTERN_W5_MYSQLD"),
	); err != nil {
		t.Fatal(err)
	}
}

// runServe is intern-web serve: the same store.Open, dispatcher, engine and
// server wiring, including cmd/root.go's shutdown order and error joining. The
// dispatcher's Stop error is never dropped: a drain that outlived its bounded
// window fails the process exactly as it fails the real command.
func runServe(ctx context.Context, metadata, listen, cert, key, kek, work, xtrabackup,
	export, xbstream, mysqld string,
) (retErr error) {
	if err := requireServePaths(metadata, listen, cert, key, kek, work, xtrabackup); err != nil {
		return err
	}
	database, err := store.Open(ctx, metadata)
	if err != nil {
		return err
	}
	defer func() { retErr = errors.Join(retErr, database.Close()) }()
	engine, shutdown, err := serveEngine(ctx, database, kek, metadata, work, xtrabackup, export, xbstream, mysqld)
	if err != nil {
		return err
	}
	defer func() {
		// cmd/root.go: cancel and drain the workers, and surface a drain that
		// did not finish inside its bounded window as a real failure.
		if stopErr := shutdown(); stopErr != nil {
			retErr = errors.Join(retErr, stopErr)
		}
	}()
	config := server.Config{Listen: listen, CertFile: cert, KeyFile: key, ShutdownTimeout: 30 * time.Second}
	return server.Run(ctx, server.New(config, server.Handler(engine)), config)
}

// requireServePaths rejects an incompletely configured child before it opens
// anything, so a missing INTERN_W4_* value fails loudly instead of serving with
// a default.
func requireServePaths(paths ...string) error {
	for _, path := range paths {
		if path == "" {
			return errors.New("serve child is missing required INTERN_W4_* paths")
		}
	}
	return nil
}

// serveEngine is the production serve wiring: dispatcher, session service and
// the resource service, exactly as cmd.serve assembles them, plus the restore
// engine when the deployment carries every restore input. The returned
// shutdown closes both and joins their errors.
func serveEngine(ctx context.Context, database *store.Store, kek, metadata, work, xtrabackup,
	export, xbstream, mysqld string,
) (*gin.Engine, func() error, error) {
	sealer, err := credential.Open(kek, metadata, work)
	if err != nil {
		return nil, nil, err
	}
	logger := slog.New(slog.NewJSONHandler(os.Stderr, nil))
	settings := resources.Settings{
		Sealer: sealer, KeyFile: kek, MetadataDir: metadata, WorkRoot: work,
		XtrabackupPath: xtrabackup, MinWorkBytes: 1, MinBackupBytes: 1,
		ExportRoot: export, XbstreamPath: xbstream, MysqldPath: mysqld, RestoreExpansionFactor: 8,
	}
	svc := &resources.Service{Store: database, Settings: settings}
	authn, err := auth.New(database, auth.DefaultSessionConfig(), auth.DefaultPasswordParams())
	if err != nil {
		return nil, nil, err
	}
	stopCleanup := authn.StartCleanup(ctx, time.Minute, logger)
	restoreEngine, err := dispatch.NewRestoreEngine(database, settings, logger)
	if err != nil {
		stopCleanup()
		return nil, nil, err
	}
	dispatcher, err := dispatch.Start(ctx, dispatch.Config{
		Database: database, Settings: settings, Logger: logger, Preflight: svc, Restore: restoreEngine,
	})
	if err != nil {
		stopCleanup()
		return nil, nil, err
	}
	engine, err := server.NewEngineWithResources(database, logger, nil, authn, svc, dispatcher, server.WithRestores(dispatcher))
	if err != nil {
		stopErr := dispatcher.Stop()
		stopCleanup()
		return nil, nil, errors.Join(err, stopErr)
	}
	shutdown := func() error {
		stopErr := dispatcher.Stop()
		stopCleanup()
		return stopErr
	}
	return engine, shutdown, nil
}
