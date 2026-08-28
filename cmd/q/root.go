package main

import (
	"fmt"
	"os"

	"github.com/spf13/cobra"
)

// BuildRootCommand assembles the q command tree.
//
// Running q with no arguments opens the TUI, which is the primary interface;
// the subcommands exist for scripting and for the agent-facing hook bridge.
func BuildRootCommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "q",
		Short: "Run coding agents across several git repos at once",
		Long: "q is the quartermaster for your coding agents.\n\n" +
			"An operation is an area of investigation and the repos it spans; a mission is " +
			"one unit of agent work inside it, running in its own git worktree per repo.\n\n" +
			"Running q with no arguments opens the board.",
		Args:              cobra.NoArgs,
		SilenceErrors:     true,
		SilenceUsage:      true,
		Version:           versionString(),
		PersistentPreRunE: applyConfig,
		RunE:              runTUI,
	}

	cmd.PersistentFlags().StringVar(&configFlag, "config", "",
		"path to the config file (default ~/.q-config.json)")

	cmd.AddCommand(
		buildConfigSubcommand(),
		buildDaemonSubcommand(),
		buildDoctorSubcommand(),
		buildHookSubcommand(),
		buildOpenSubcommand(),
		buildMissionSubcommand(),
		buildOperationSubcommand(),
		buildTUISubcommand(),
	)

	return cmd
}

// applyConfig loads the configuration before any subcommand runs and retunes the
// logger to the configured level.
//
// It runs here rather than in main so that --config is parsed first, and so a
// broken config file stops the command with a clear error instead of being
// discovered later by whichever package first needed a value from it.
func applyConfig(cmd *cobra.Command, _ []string) error {
	loaded, err := loadConfig(configPath())
	if err != nil {
		return err
	}

	cfg = loaded

	cmd.SetContext(withLogger(cmd.Context(), newLogger(os.Stderr, cfg.LogLevel)))

	return nil
}

// buildConfigSubcommand exposes where the configuration comes from and what it
// resolved to, which is the first question to ask when q is looking in the wrong
// place for something.
func buildConfigSubcommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "config",
		Short: "Show and create q's configuration",
		Args:  cobra.NoArgs,
		RunE:  func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	cmd.AddCommand(
		&cobra.Command{
			Use:   "path",
			Short: "Print the config file q reads",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				_, err := fmt.Fprintln(cmd.OutOrStdout(), configPath())

				return err
			},
		},
		&cobra.Command{
			Use:   "show",
			Short: "Print the effective configuration, after file and environment overrides",
			Args:  cobra.NoArgs,
			RunE: func(cmd *cobra.Command, _ []string) error {
				return writeSampleConfig(cmd.OutOrStdout(), cfg)
			},
		},
		buildConfigInitSubcommand(),
	)

	return cmd
}

// buildConfigInitSubcommand writes the effective configuration to disk as a
// starting point.
func buildConfigInitSubcommand() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:   "init",
		Short: "Write a config file populated with the current effective values",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path, err := createConfigFile(configPath(), force)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "wrote %s\n", path)

			return err
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "overwrite an existing config file")

	return cmd
}
