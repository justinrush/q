package main

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/justinrush/q/internal/buildinfo"
	"github.com/justinrush/q/internal/client"
	"github.com/justinrush/q/internal/daemon"
	"github.com/justinrush/q/internal/paths"
	"github.com/spf13/cobra"
)

func buildDaemonSubcommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "daemon",
		Short: "Manage the background process that owns q state",
		Long: "The daemon owns q's state and supervises running agent sessions.\n\n" +
			"It runs in the background so that agents outlive the board: a mission keeps " +
			"running after you close the TUI, its hooks still have somewhere to report, " +
			"and the reconciler that keeps cards honest keeps running.\n\n" +
			"You do not normally need these commands; q starts a daemon on demand.",
		Args: cobra.NoArgs,
	}

	cmd.AddCommand(
		buildDaemonRunSubcommand(),
		buildDaemonStatusSubcommand(),
		buildDaemonStopSubcommand(),
		buildDaemonRestartSubcommand(),
	)

	return cmd
}

func buildDaemonRestartSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "restart",
		Short: "Stop the running daemon and start a fresh one",
		Long: "Stop the running daemon and start a replacement.\n\n" +
			"Worth knowing about: a running daemon keeps serving with the configuration " +
			"and environment it started with, and `q daemon run` defers to it rather " +
			"than replacing it. After rebuilding q or changing an environment override, " +
			"restart rather than starting another one.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dirs, err := paths.Resolve(pathOverrides())
			if err != nil {
				return err
			}

			if err := client.Stop(dirs); err != nil {
				return err
			}

			if err := waitForStop(cmd.Context(), dirs); err != nil {
				return err
			}

			c, err := client.Ensure(cmd.Context(), dirs)
			if err != nil {
				return err
			}

			health, err := c.Health(cmd.Context())
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "restarted (pid %d, %s)\n", health.PID, c.Handle().Addr)

			return err
		},
	}
}

// waitForStop blocks until no daemon answers, so the replacement does not lose the
// instance lock to the one being replaced.
func waitForStop(ctx context.Context, dirs paths.Dirs) error {
	deadline := time.Now().Add(5 * time.Second)

	for time.Now().Before(deadline) {
		if _, err := client.Connect(ctx, dirs); err != nil {
			return nil
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}

	return errors.New("the running daemon did not stop within 5s")
}

func buildDaemonRunSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "run",
		Short: "Run the daemon in the foreground",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dirs, err := paths.Resolve(pathOverrides())
			if err != nil {
				return err
			}

			logger := loggerFrom(cmd.Context())

			err = daemon.Run(cmd.Context(), daemon.RunConfig{
				Dirs:     dirs,
				Version:  buildinfo.Semantic(),
				Logger:   logger,
				Settings: cfg,
			})

			// Losing the race to another daemon is success: the caller wanted a
			// daemon running, not necessarily this process to be it.
			if errors.Is(err, daemon.ErrAlreadyRunning) {
				logger.Info("a q daemon is already running")

				return nil
			}

			return err
		},
	}
}

func buildDaemonStatusSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "status",
		Short: "Report whether the daemon is running",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dirs, err := paths.Resolve(pathOverrides())
			if err != nil {
				return err
			}

			c, err := client.Connect(cmd.Context(), dirs)
			if errors.Is(err, daemon.ErrNoDaemon) {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), "not running")

				return err
			}

			if err != nil {
				return err
			}

			health, err := c.Health(cmd.Context())
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"running\n  pid          %d\n  address      %s\n  version      %s\n"+
					"  binary       %s\n  uptime       %s\n  operations      %d\n"+
					"  missions        %d\n  open boards  %d\n",
				health.PID, c.Handle().Addr, health.Version, health.Binary, health.Uptime,
				health.Operations, health.Missions, health.Subscribers)

			return err
		},
	}
}

func buildDaemonStopSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "stop",
		Short: "Stop the running daemon",
		Long: "Stop the daemon. Agent sessions are unaffected: they live in detached tmux " +
			"sessions and keep running. Their status updates are spooled to disk and " +
			"applied when the daemon next starts.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			dirs, err := paths.Resolve(pathOverrides())
			if err != nil {
				return err
			}

			if err := client.Stop(dirs); err != nil {
				return err
			}

			_, err = fmt.Fprintln(cmd.OutOrStdout(), "stopped")

			return err
		},
	}
}
