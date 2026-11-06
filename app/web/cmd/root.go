package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"os"
	"time"

	"github.com/spf13/cobra"

	"github.com/flyingEirc/Intern/app/web/internal/auth"
	"github.com/flyingEirc/Intern/app/web/internal/dispatch"
	"github.com/flyingEirc/Intern/app/web/internal/resources"
	"github.com/flyingEirc/Intern/app/web/internal/server"
	"github.com/flyingEirc/Intern/app/web/internal/store"
	"github.com/flyingEirc/Intern/app/web/internal/webui"
	"github.com/flyingEirc/Intern/internal/fsutil"
)

type application struct {
	config      *configLoader
	out, stderr io.Writer
}

func New(out, stderr io.Writer) *cobra.Command {
	root := &cobra.Command{
		Use: "intern", Short: "Intern Web control plane", SilenceUsage: true, SilenceErrors: true,
		Long: "Intern Web control plane. Configuration precedence: flags > INTERN_* environment > YAML > defaults.",
	}
	root.SetOut(out)
	root.SetErr(stderr)
	app := &application{config: newConfigLoader(root), out: out, stderr: stderr}
	root.AddCommand(app.serveCommand(), app.migrateCommand(), app.userCommand(), app.healthcheckCommand())
	return root
}

func Execute(ctx context.Context) error { return New(os.Stdout, os.Stderr).ExecuteContext(ctx) }

func (a *application) emit(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := a.out.Write(data)
	if n != len(data) {
		return errors.Join(err, io.ErrShortWrite)
	}
	return err
}

func (a *application) serveCommand() *cobra.Command {
	return &cobra.Command{Use: "serve", Short: "Migrate metadata and serve HTTP", Args: cobra.NoArgs, RunE: a.serve}
}

func (a *application) serve(command *cobra.Command, _ []string) (retErr error) {
	// Configuration failure returns before metadata creation or a listen socket.
	c, err := a.config.load()
	if err != nil {
		return err
	}
	level, err := c.logLevel()
	if err != nil {
		return err
	}
	logger := slog.New(slog.NewJSONHandler(a.stderr, &slog.HandlerOptions{Level: level}))
	database, err := store.Open(command.Context(), c.MetadataDir)
	if err != nil {
		return err
	}
	defer func() {
		fsutil.Close(&retErr, database)
		if retErr == nil {
			logger.Info("shutdown complete")
		}
	}()
	authentication, err := auth.New(database, c.Session, auth.DefaultPasswordParams())
	if err != nil {
		return err
	}
	stopCleanup := authentication.StartCleanup(command.Context(), time.Minute, logger)
	defer stopCleanup()
	settings, err := c.resourceSettings()
	if err != nil {
		return err
	}
	// The dispatcher owns its own context so Stop can drain it even when the
	// command context is already canceled: recovery runs before the workers
	// start, and the bounded wait must exceed chain.Complete's 15s commit
	// window so a finishing run commits before the database closes.
	dispatcher, err := startDispatcher(command.Context(), database, settings, logger)
	if err != nil {
		return err
	}
	defer func() {
		if stopErr := dispatcher.Stop(); stopErr != nil {
			// Workers outlived the bounded window: their state is reclaimed by
			// the next start's recovery. This error never describes a
			// completed backup, so it must not be swallowed.
			retErr = errors.Join(retErr, stopErr)
		}
	}()
	engine, err := server.NewEngineWithResources(database, logger, c.TrustedProxies, authentication,
		&resources.Service{Store: database, Settings: settings}, dispatcher, server.WithRestores(dispatcher))
	if err != nil {
		return err
	}
	config := server.Config{
		Listen: c.Listen, CertFile: c.TLS.CertFile, KeyFile: c.TLS.KeyFile,
		ShutdownTimeout: c.ShutdownTimeout, TLSTerminatedByProxy: c.TLSTerminatedByProxy,
	}
	logStart(logger, &c, database)
	// Builds with the webui tag serve the embedded UI on every non-API path of
	// the same origin; other builds serve the API alone, unchanged.
	srv := server.New(config, webui.Mount(server.Handler(engine), webui.Assets()))
	srv.ErrorLog = slog.NewLogLogger(logger.Handler(), slog.LevelError)
	return server.Run(command.Context(), srv, config)
}

// logStart reports the listener shape, including the plaintext-proxy warning
// that must not be silent: a direct client could otherwise bypass TLS.
func logStart(logger *slog.Logger, c *configuration, database *store.Store) {
	plainProxy := c.TLSTerminatedByProxy && c.TLS.CertFile == ""
	logger.Info("starting server", "listen", c.Listen, "instance_id", database.InstanceID(),
		"tls", c.TLS.CertFile != "", "tls_terminated_by_proxy", plainProxy, "webui", webui.Assets() != nil)
	if plainProxy {
		logger.Warn("serving plain HTTP behind a TLS-terminating proxy; firewall this listener so clients cannot connect directly. This mode is not end-to-end TLS.",
			"trusted_proxies", c.TrustedProxies)
	}
}

// startDispatcher launches the run queue and the restore worker with the
// process capabilities. It owns its own context so Stop can drain the workers
// even when the command context is already canceled.
func startDispatcher(ctx context.Context, database *store.Store, settings resources.Settings, logger *slog.Logger) (*dispatch.Dispatcher, error) {
	// A deployment without the restore inputs yields a nil engine: serve still
	// starts, the restore API answers 503 and a queued restore fails visibly as
	// unavailable. A configured deployment whose export root violates the frozen
	// location rules is a startup error, not a per-request failure.
	restores, err := dispatch.NewRestoreEngine(database, settings, logger)
	if err != nil {
		return nil, err
	}
	return dispatch.Start(ctx, dispatch.Config{
		Database: database, Settings: settings, Logger: logger,
		Preflight: &resources.Service{Store: database, Settings: settings},
		Restore:   restores,
	})
}

func (a *application) migrateCommand() *cobra.Command {
	command := &cobra.Command{Use: "migrate", Short: "Manage control-plane migrations"}
	command.AddCommand(
		&cobra.Command{Use: "up", Short: "Apply pending migrations under exclusive ownership", Args: cobra.NoArgs, RunE: a.migrateUp},
		&cobra.Command{Use: "status", Short: "Read migration status without locking or changing the database", Args: cobra.NoArgs, RunE: a.migrateStatus},
	)
	return command
}

func (a *application) migrateUp(command *cobra.Command, _ []string) (retErr error) {
	c, err := a.config.load()
	if err != nil {
		return err
	}
	database, err := store.Open(command.Context(), c.MetadataDir)
	if err != nil {
		return err
	}
	defer fsutil.Close(&retErr, database)
	status, err := store.ReadStatus(command.Context(), c.MetadataDir)
	if err != nil {
		return err
	}
	return a.emit(status)
}

func (a *application) migrateStatus(command *cobra.Command, _ []string) error {
	c, err := a.config.load()
	if err != nil {
		return err
	}
	status, err := store.ReadStatus(command.Context(), c.MetadataDir)
	if err != nil {
		return err
	}
	return a.emit(status)
}
