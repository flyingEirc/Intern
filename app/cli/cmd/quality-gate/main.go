package main

import (
	"context"
	"encoding/json/v2"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"

	"github.com/flyingEirc/IotProject/app/cli/internal/qualitygate"
)

func main() {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := command().ExecuteContext(ctx); err != nil {
		if _, writeErr := fmt.Fprintln(os.Stderr, "quality-gate:", err); writeErr != nil {
			os.Exit(1)
		}
		os.Exit(1)
	}
}
func emit(w io.Writer, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	n, err := w.Write(data)
	if err == nil && n != len(data) {
		return io.ErrShortWrite
	}
	return err
}
func command() *cobra.Command {
	root := &cobra.Command{Use: "quality-gate", SilenceUsage: true, SilenceErrors: true}
	root.AddCommand(matrixCommand(), verifyCommand(), aggregateCommand(), runCommand(), allCommand())
	return root
}
func matrixCommand() *cobra.Command {
	return &cobra.Command{Use: "matrix", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		c, err := qualitygate.Load()
		if err != nil {
			return err
		}
		ids := make([]string, 0, len(c.Profiles))
		for _, p := range c.Profiles {
			ids = append(ids, p.ID)
		}
		return emit(cmd.OutOrStdout(), map[string]any{"profile": ids})
	}}
}
func verifyCommand() *cobra.Command {
	var dir string
	var want qualitygate.Verification
	cmd := &cobra.Command{Use: "verify", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := qualitygate.VerifyDirectory(dir, want)
		if err != nil {
			return err
		}
		return emit(cmd.OutOrStdout(), result)
	}}
	cmd.Flags().StringVar(&dir, "directory", "", "directory containing candidate.json, receipts/ and events.jsonl")
	cmd.Flags().StringVar(&want.SourceSHA, "source", "", "expected source commit SHA")
	cmd.Flags().StringVar(&want.Profile, "profile", "", "locked matrix profile")
	cmd.Flags().StringVar(&want.Platform, "platform", "", "actual container platform")
	cmd.Flags().BoolVar(&want.RequireClean, "ci", false, "reject candidates built from uncommitted source")
	return cmd
}
func aggregateCommand() *cobra.Command {
	var dir, source, platform string
	var jobs qualitygate.Jobs
	cmd := &cobra.Command{Use: "aggregate", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		results, err := qualitygate.Aggregate(dir, source, platform, jobs)
		if err != nil {
			return err
		}
		return emit(cmd.OutOrStdout(), results)
	}}
	cmd.Flags().StringVar(&dir, "directory", "", "downloaded container-<profile> evidence directories")
	cmd.Flags().StringVar(&source, "source", "", "expected source commit SHA")
	cmd.Flags().StringVar(&platform, "platform", "linux/amd64", "expected container platform")
	cmd.Flags().BoolVar(&jobs.Required, "required", true, "whether this change requires business checks")
	cmd.Flags().StringVar(&jobs.Scope, "scope", "", "scope job result")
	cmd.Flags().StringVar(&jobs.Fast, "fast", "", "fast job result")
	cmd.Flags().StringVar(&jobs.Containers, "containers", "", "matrix job result")
	return cmd
}
func runCommand() *cobra.Command {
	var opts qualitygate.RunOptions
	cmd := &cobra.Command{Use: "run", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		result, err := qualitygate.Run(cmd.Context(), opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
		if err != nil {
			return err
		}
		return emit(cmd.OutOrStdout(), result)
	}}
	cmd.Flags().StringVar(&opts.Profile, "profile", "mysql-9.7.1", "locked database/toolchain profile")
	cmd.Flags().StringVar(&opts.Platform, "platform", "", "native container platform; detected if empty")
	cmd.Flags().StringVar(&opts.SourceSHA, "source", "", "expected source commit SHA")
	cmd.Flags().StringVar(&opts.Directory, "directory", ".cache/quality", "parent of a fresh evidence directory")
	cmd.Flags().BoolVar(&opts.CI, "ci", false, "enforce a clean, explicit source SHA")
	return cmd
}

func allCommand() *cobra.Command {
	var opts qualitygate.RunOptions
	cmd := &cobra.Command{Use: "all", Args: cobra.NoArgs, RunE: func(cmd *cobra.Command, _ []string) error {
		contract, err := qualitygate.Load()
		if err != nil {
			return err
		}
		var failures error
		for _, profile := range contract.Profiles {
			opts.Profile = profile.ID
			result, runErr := qualitygate.Run(cmd.Context(), opts, cmd.OutOrStdout(), cmd.ErrOrStderr())
			if runErr == nil {
				runErr = emit(cmd.OutOrStdout(), result)
			}
			failures = errors.Join(failures, runErr)
		}
		return failures
	}}
	cmd.Flags().StringVar(&opts.Platform, "platform", "", "native container platform")
	cmd.Flags().StringVar(&opts.SourceSHA, "source", "", "expected source commit SHA")
	cmd.Flags().StringVar(&opts.Directory, "directory", ".cache/quality", "evidence parent directory")
	cmd.Flags().BoolVar(&opts.CI, "ci", false, "enforce clean candidate source")
	return cmd
}
