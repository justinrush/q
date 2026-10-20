package main

import (
	"fmt"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/git"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
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
	opts, err := tuiOptions()
	if err != nil {
		return err
	}

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

	c, err := api.Ensure(cmd.Context(), dirs)
	if err != nil {
		return err
	}

	programOpts := []tea.ProgramOption{
		tea.WithAltScreen(),
		tea.WithContext(cmd.Context()),
	}
	if cfg.TUI.Mouse {
		programOpts = append(programOpts, tea.WithMouseCellMotion())
	}

	program := tea.NewProgram(
		tui.New(c, opts),
		programOpts...,
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
func tuiOptions() (tui.Options, error) {
	horizontal, err := horizontalLanes(cfg.TUI.Lanes)
	if err != nil {
		return tui.Options{}, err
	}

	return tui.Options{
		Horizontal: horizontal,
		Repos: git.ScanOptions{
			Roots:    cfg.Repos.Roots,
			MaxDepth: cfg.Repos.MaxDepth,
			Skip:     cfg.Repos.Skip,
		},
		DefaultTool: mission.Tool(cfg.Agents.Default),
		Review: tui.ReviewDefaults{
			Tool:   mission.Tool(cfg.Review.Tool),
			Model:  cfg.Review.Model,
			Effort: cfg.Review.Effort,
		},
		AttachBin: currentTerminalBin(cfg),
	}, nil
}

// horizontalLanes reports whether tui.lanes asks for rows rather than columns.
func horizontalLanes(lanes string) (bool, error) {
	switch lanes {
	case "", lanesVertical:
		return false, nil
	case lanesHorizontal:
		return true, nil
	default:
		return false, fmt.Errorf("unknown tui.lanes %q: want %q or %q", lanes, lanesVertical, lanesHorizontal)
	}
}
