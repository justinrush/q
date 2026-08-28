package main

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/justinrush/q/internal/client"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/repofind"
	"github.com/justinrush/q/internal/tui"
	"github.com/spf13/cobra"
)

func buildTUISubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "tui",
		Short: "Open the board (the default when q is run with no arguments)",
		Args:  cobra.NoArgs,
		RunE:  runTUI,
	}
}

// runTUI starts the board.
func runTUI(cmd *cobra.Command, _ []string) error {
	dirs, err := paths.Resolve(pathOverrides())
	if err != nil {
		return err
	}

	// Log to a file for the duration. The shared logger writes to stderr, and
	// anything printed there while the alternate screen is active lands in the middle
	// of the rendered frame and corrupts it.
	restore, err := redirectLogging(cmd, dirs)
	if err != nil {
		return err
	}
	defer restore()

	c, err := client.Ensure(cmd.Context(), dirs)
	if err != nil {
		return err
	}

	program := tea.NewProgram(
		tui.New(c, tuiOptions()),
		tea.WithAltScreen(),
		tea.WithContext(cmd.Context()),
		// Mouse reporting is deliberately off. Inside tmux, where the user has mouse
		// mode enabled, capturing it would break click-to-select-pane and
		// drag-to-copy for the panes q itself opens.
	)

	if _, err := program.Run(); err != nil {
		return fmt.Errorf("running the board: %w", err)
	}

	return nil
}

// redirectLogging points the context logger at a file and returns a restore function.
func redirectLogging(cmd *cobra.Command, dirs paths.Dirs) (func(), error) {
	if err := dirs.Ensure(); err != nil {
		return nil, err
	}

	path := dirs.LogFile("tui")

	file, err := paths.OpenLog(path)
	if err != nil {
		return nil, err
	}

	cmd.SetContext(withLogger(cmd.Context(), newLogger(file, cfg.LogLevel)))

	return func() { _ = file.Close() }, nil
}

// tuiOptions hands the board the parts of the configuration it needs. The board
// talks to the daemon for everything else.
func tuiOptions() tui.Options {
	return tui.Options{
		Repos: repofind.Options{
			Roots:    cfg.Repos.Roots,
			MaxDepth: cfg.Repos.MaxDepth,
			Skip:     cfg.Repos.Skip,
		},
		DefaultTool: domain.Tool(cfg.Agents.Default),
	}
}
