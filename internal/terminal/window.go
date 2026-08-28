// Window opening. There is no portable way to do this, so q ships one
// implementation per strategy and the cmd package picks between them from the
// user's configuration. Every one of them satisfies [Opener].
package terminal

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/justinrush/q/internal/runner"
)

// ErrNoTerminal is returned by [Manual.Open]. Callers report it to the user
// alongside the command they can run themselves; it is not a failure of
// anything.
var ErrNoTerminal = errors.New("opening a terminal window is disabled")

// Placeholders substituted into a [Command] argv template.
const (
	// PlaceholderDir is replaced by the working directory.
	PlaceholderDir = "{dir}"
	// PlaceholderCmd is replaced by the shell-quoted command, as one argument.
	PlaceholderCmd = "{cmd}"
	// PlaceholderArgv is replaced by the command's arguments, spliced in place.
	PlaceholderArgv = "{argv}"
)

// Spec describes a terminal window to open.
type Spec struct {
	// Dir is the window's working directory.
	Dir string
	// Argv is the command the window runs.
	Argv []string
}

// Opener opens a terminal window running a command.
type Opener interface {
	Open(ctx context.Context, spec Spec) error
	// Raise brings existing windows forward without creating one. An
	// implementation that cannot do so reports no error: the caller asked for a
	// window to be surfaced, and failing to do so is not worth interrupting them.
	Raise(ctx context.Context) error
	// Describe renders the command Open would run, for logs and dry runs.
	Describe(spec Spec) string
}

// Ghostty opens windows through Ghostty 1.3+'s AppleScript interface.
//
// Creating the window this way keeps it inside the existing Ghostty application,
// so macOS includes it in the normal command-backtick window cycle. Values
// travel as osascript arguments rather than being interpolated into the script,
// which preserves paths containing spaces and keeps mission data out of
// executable AppleScript source.
type Ghostty struct {
	scriptBin string
	run       runner.Runner
}

// NewGhostty returns an opener driving Ghostty through the osascript at
// scriptBin.
func NewGhostty(scriptBin string, run runner.Runner) *Ghostty {
	return &Ghostty{scriptBin: scriptBin, run: run}
}

// Open creates a Ghostty window running the spec's command.
func (g *Ghostty) Open(ctx context.Context, spec Spec) error {
	if len(spec.Argv) == 0 {
		return errNoCommand
	}

	if _, err := g.run.Run(ctx, g.spec(spec)); err != nil {
		return fmt.Errorf("opening a terminal window: %w", err)
	}

	return nil
}

// Raise activates Ghostty.
func (g *Ghostty) Raise(ctx context.Context) error {
	spec := runner.Spec{Name: g.scriptBin, Args: []string{"-e", raiseScript}}

	if _, err := g.run.Run(ctx, spec); err != nil {
		return fmt.Errorf("raising Ghostty: %w", err)
	}

	return nil
}

// Describe renders the osascript invocation.
func (g *Ghostty) Describe(spec Spec) string { return g.spec(spec).String() }

func (g *Ghostty) spec(spec Spec) runner.Spec {
	return runner.Spec{
		Name: g.scriptBin,
		Args: []string{"-e", openScript, "--", spec.Dir, strings.Join(spec.Argv, " ")},
	}
}

// Command opens windows by expanding the user's own argv template.
//
// The template is expanded rather than passed through a shell so that a mission
// directory containing a space, a quote, or a dollar sign cannot change what is
// executed.
type Command struct {
	template []string
	run      runner.Runner
}

// NewCommand returns an opener driving the emulator named by template.
//
// The template is validated here rather than at open time, so a configuration
// that could never launch a session is rejected while the user is still looking
// at the config file.
func NewCommand(template []string, run runner.Runner) (*Command, error) {
	if len(template) == 0 {
		return nil, errors.New("terminal.command is empty in the config file")
	}

	// A template that names no placeholder would open a window running something
	// other than the agent, which looks like success and is not.
	if !mentionsCommand(template) {
		return nil, fmt.Errorf(
			"terminal.command must contain %s or %s so the window runs the session",
			PlaceholderArgv, PlaceholderCmd)
	}

	return &Command{template: template, run: run}, nil
}

// Open runs the expanded template.
func (c *Command) Open(ctx context.Context, spec Spec) error {
	if len(spec.Argv) == 0 {
		return errNoCommand
	}

	if _, err := c.run.Run(ctx, c.spec(spec)); err != nil {
		return fmt.Errorf("opening a terminal window: %w", err)
	}

	return nil
}

// Raise is a no-op: an arbitrary emulator offers no way to do it.
func (c *Command) Raise(context.Context) error { return nil }

// Describe renders the expanded template.
func (c *Command) Describe(spec Spec) string { return c.spec(spec).String() }

func (c *Command) spec(spec Spec) runner.Spec {
	var out []string

	joined := strings.Join(spec.Argv, " ")

	for _, part := range c.template {
		switch part {
		case PlaceholderArgv:
			out = append(out, spec.Argv...)
		case PlaceholderDir:
			out = append(out, spec.Dir)
		case PlaceholderCmd:
			out = append(out, joined)
		default:
			part = strings.ReplaceAll(part, PlaceholderDir, spec.Dir)
			out = append(out, strings.ReplaceAll(part, PlaceholderCmd, joined))
		}
	}

	return runner.Spec{Name: out[0], Args: out[1:], Dir: spec.Dir}
}

// Manual opens nothing and reports the command to run by hand, which is what a
// headless or remote setup wants.
type Manual struct{}

// NewManual returns an opener that never opens anything.
func NewManual() Manual { return Manual{} }

// Open always fails with [ErrNoTerminal], naming the command to run.
func (Manual) Open(_ context.Context, spec Spec) error {
	return fmt.Errorf("%w; run this yourself: %s", ErrNoTerminal, strings.Join(spec.Argv, " "))
}

// Raise is a no-op.
func (Manual) Raise(context.Context) error { return nil }

// Describe renders the command the user would run.
func (Manual) Describe(spec Spec) string { return strings.Join(spec.Argv, " ") }

// errNoCommand reports a spec with nothing to run.
var errNoCommand = errors.New("terminal: no command to run")

// mentionsCommand reports whether a template says where the command goes.
func mentionsCommand(template []string) bool {
	for _, part := range template {
		if strings.Contains(part, PlaceholderCmd) || part == PlaceholderArgv {
			return true
		}
	}

	return false
}
