// Package termopen opens a terminal window running a command.
//
// There is no portable way to do this, so q supports three modes. On macOS,
// Ghostty 1.3+ has a native AppleScript interface; creating the window through
// it keeps the window inside the existing Ghostty application, so macOS includes
// it in the normal command-backtick window cycle. Anywhere else, the user names
// an argv template for their own emulator. The third mode opens nothing and
// reports the command to run by hand, which is what a headless or remote setup
// wants.
//
// In the AppleScript mode, values travel as osascript arguments rather than being
// interpolated into the script. This preserves paths containing spaces and keeps
// mission data outside executable AppleScript source.
package termopen

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/justinrush/q/internal/runner"
	"github.com/justinrush/q/internal/settings"
)

// ErrNoTerminal is returned by Open when the terminal mode is "none". Callers
// report it to the user alongside the command they can run themselves; it is not
// a failure of anything.
var ErrNoTerminal = errors.New("opening a terminal window is disabled")

// Placeholders substituted into a command-mode argv template.
const (
	// PlaceholderDir is replaced by the working directory.
	PlaceholderDir = "{dir}"
	// PlaceholderCmd is replaced by the shell-quoted command, as one argument.
	PlaceholderCmd = "{cmd}"
	// PlaceholderArgv is replaced by the command's arguments, spliced in place.
	PlaceholderArgv = "{argv}"
)

// Opener launches terminal windows.
type Opener struct {
	// Mode is one of the settings.Terminal* modes.
	Mode string
	// Command is the argv template used in command mode.
	Command []string
	// ScriptBin is the absolute path to the macOS osascript utility, used in
	// Ghostty mode.
	ScriptBin string
	// Run executes the launch command.
	Run runner.Runner
}

// Config configures an Opener.
type Config struct {
	Mode      string
	Command   []string
	ScriptBin string
	Run       runner.Runner
}

// New returns an Opener. An unset mode falls back to the platform default.
func New(cfg Config) *Opener {
	mode := cfg.Mode
	if mode == "" {
		mode = settings.DefaultTerminalMode()
	}

	return &Opener{Mode: mode, Command: cfg.Command, ScriptBin: cfg.ScriptBin, Run: cfg.Run}
}

// Spec describes a terminal window to open.
type Spec struct {
	// Dir is the window's working directory.
	Dir string
	// Argv is the command the window runs.
	Argv []string
}

// Open creates a terminal window running the spec's command.
func (o *Opener) Open(ctx context.Context, spec Spec) error {
	if len(spec.Argv) == 0 {
		return fmt.Errorf("termopen: no command to run")
	}

	run, err := o.openSpec(spec)
	if err != nil {
		return err
	}

	if _, err := o.Run.Run(ctx, run); err != nil {
		return fmt.Errorf("opening a terminal window: %w", err)
	}

	return nil
}

// Raise brings the terminal's existing windows forward without creating one.
//
// Only the Ghostty mode can do this. Elsewhere it is a no-op rather than an
// error: the caller asked for an existing window to be surfaced, and failing to
// do so is not worth interrupting them over.
func (o *Opener) Raise(ctx context.Context) error {
	if o.Mode != settings.TerminalGhostty {
		return nil
	}

	spec := runner.Spec{Name: o.ScriptBin, Args: []string{"-e", raiseScript}}

	if _, err := o.Run.Run(ctx, spec); err != nil {
		return fmt.Errorf("raising Ghostty: %w", err)
	}

	return nil
}

// Describe renders the command Open would run, for logs and dry runs.
func (o *Opener) Describe(spec Spec) string {
	run, err := o.openSpec(spec)
	if err != nil {
		return err.Error()
	}

	return run.String()
}

// openSpec builds the process q runs to get a window.
func (o *Opener) openSpec(spec Spec) (runner.Spec, error) {
	switch o.Mode {
	case settings.TerminalNone:
		return runner.Spec{}, fmt.Errorf("%w; run this yourself: %s",
			ErrNoTerminal, strings.Join(spec.Argv, " "))
	case settings.TerminalCommand:
		return o.commandSpec(spec)
	default:
		return o.ghosttySpec(spec), nil
	}
}

// ghosttySpec drives Ghostty's AppleScript interface.
func (o *Opener) ghosttySpec(spec Spec) runner.Spec {
	return runner.Spec{
		Name: o.ScriptBin,
		Args: []string{
			"-e",
			openScript,
			"--",
			spec.Dir,
			strings.Join(spec.Argv, " "),
		},
	}
}

// commandSpec expands the user's argv template.
//
// The template is expanded rather than passed through a shell so that a mission
// directory containing a space, a quote, or a dollar sign cannot change what is
// executed.
func (o *Opener) commandSpec(spec Spec) (runner.Spec, error) {
	if len(o.Command) == 0 {
		return runner.Spec{}, fmt.Errorf(
			"terminal.mode is %q but terminal.command is empty in the config file",
			settings.TerminalCommand)
	}

	var out []string

	for _, part := range o.Command {
		switch part {
		case PlaceholderArgv:
			out = append(out, spec.Argv...)
		case PlaceholderDir:
			out = append(out, spec.Dir)
		case PlaceholderCmd:
			out = append(out, strings.Join(spec.Argv, " "))
		default:
			part = strings.ReplaceAll(part, PlaceholderDir, spec.Dir)
			out = append(out, strings.ReplaceAll(part, PlaceholderCmd, strings.Join(spec.Argv, " ")))
		}
	}

	// A template that names no placeholder would open a window running something
	// other than the agent, which looks like success and is not.
	if len(out) == len(o.Command) && !mentionsCommand(o.Command) {
		return runner.Spec{}, fmt.Errorf(
			"terminal.command must contain %s or %s so the window runs the session",
			PlaceholderArgv, PlaceholderCmd)
	}

	return runner.Spec{Name: out[0], Args: out[1:], Dir: spec.Dir}, nil
}

// mentionsCommand reports whether a template says where the command goes.
func mentionsCommand(template []string) bool {
	for _, part := range template {
		if strings.Contains(part, PlaceholderCmd) || part == PlaceholderArgv {
			return true
		}
	}

	return false
}
