package cmd

import (
	"context"
	"encoding/json/v2"
	"errors"
	"io"
	"log/slog"
	"os"

	"github.com/spf13/cobra"

	"github.com/flyingEirc/Intern/app/web/internal/server"
	"github.com/flyingEirc/Intern/app/web/internal/store"
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
	root.AddCommand(app.serveCommand(), app.migrateCommand())
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
	engine, err := server.NewEngine(database, logger, c.TrustedProxies)
	if err != nil {
		return err
	}
	config := server.Config{
		Listen: c.Listen, CertFile: c.TLS.CertFile, KeyFile: c.TLS.KeyFile,
		ShutdownTimeout: c.ShutdownTimeout,
	}
	logger.Info("starting server", "listen", c.Listen, "instance_id", database.InstanceID())
	srv := server.New(config, engine)
	srv.ErrorLog = slog.NewLogLogger(logger.Handler(), slog.LevelError)
	return server.Run(command.Context(), srv, config)
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
