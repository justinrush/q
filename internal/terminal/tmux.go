// Package terminal is the terminal q drives: the tmux server that hosts every
// mission's session, and the window openers that put one in front of the human.
//
// # tmux
//
// Two of the user's tmux settings make the naive approach wrong, so this package
// makes the correct approach the only one available:
//
// Targets are always written with a leading "=". Without it tmux matches session
// names by prefix and fnmatch, so `has-session -t q-foo` also matches
// `q-foo-bar`. q generates session names from operation and mission slugs, which
// share prefixes constantly, so this is a live hazard rather than a theoretical
// one. [Target] renders the "=" itself and no call site can omit it.
//
// Panes are addressed by the ids tmux reports at creation, never by index. The
// user's config lets tmux-sensible flip pane-base-index to 1, so a computed
// "session:0.1" may not be the pane intended. Every creation call therefore asks
// for the pane id back.
package terminal

import (
	"context"
	"fmt"
	"slices"
	"strings"

	"github.com/justinrush/q/internal/runner"
)

// Default geometry for detached sessions.
//
// A detached `new-session` defaults to 80x24. An agent TUI started that way would
// render at 80 columns for as long as it ran unattended, so q asks for a
// realistic size up front.
const (
	DefaultWidth  = 240
	DefaultHeight = 60
)

// paneIDFormat asks tmux to print the created pane's id.
const paneIDFormat = "#{pane_id}"

// Tmux runs tmux commands.
type Tmux struct {
	bin string
	run runner.Runner
}

// New returns a Tmux that invokes the binary at bin through run.
func NewTmux(bin string, run runner.Runner) *Tmux {
	return &Tmux{bin: bin, run: run}
}

// Target is a tmux target specifier.
//
// Construct one with [Session], [Window], or [Pane]; the zero value is not
// usable. Its String method emits the exact-match form.
type Target struct {
	value string
	exact bool
}

// Session targets a session by name, matched exactly.
func Session(name string) Target { return Target{value: name, exact: true} }

// Window targets a named window within a session, matched exactly.
func Window(session, window string) Target {
	return Target{value: session + ":" + window, exact: true}
}

// Pane targets a pane by the id tmux assigned it, e.g. "%13". Pane ids are
// already unambiguous and must not be prefixed.
func Pane(id string) Target { return Target{value: id} }

// String renders the target for a -t flag.
func (t Target) String() string {
	if t.exact {
		return "=" + t.value
	}

	return t.value
}

// Empty reports whether the target names nothing.
func (t Target) Empty() bool { return t.value == "" }

// exec runs tmux with the given arguments.
func (t *Tmux) exec(ctx context.Context, args ...string) (runner.Result, error) {
	return t.run.Run(ctx, runner.Spec{Name: t.bin, Args: args})
}

// HasSession reports whether a session exists.
func (t *Tmux) HasSession(ctx context.Context, session string) bool {
	_, err := t.exec(ctx, "has-session", "-t", Session(session).String())

	return err == nil
}

// NewSessionOptions describes a detached session to create.
type NewSessionOptions struct {
	// Name is the session name.
	Name string
	// Window is the name of the initial window.
	Window string
	// Dir is the working directory for the first pane.
	Dir string
	// Env is passed with -e, becoming the session environment. It is how a mission
	// tells its agent's hooks which mission they belong to. The daemon token is
	// deliberately never placed here, because `tmux show-environment` prints this
	// in plaintext.
	Env map[string]string
	// Command is the argv to run in the first pane. A single element is passed as
	// one word, so a generated script path needs no quoting.
	Command []string
	// Width and Height override the default geometry.
	Width, Height int
}

// NewSession creates a detached session and returns the first pane's id.
func (t *Tmux) NewSession(ctx context.Context, opts NewSessionOptions) (string, error) {
	width, height := opts.Width, opts.Height
	if width == 0 {
		width = DefaultWidth
	}

	if height == 0 {
		height = DefaultHeight
	}

	args := []string{
		"new-session", "-d",
		"-s", opts.Name,
		"-x", fmt.Sprint(width),
		"-y", fmt.Sprint(height),
	}

	if opts.Window != "" {
		args = append(args, "-n", opts.Window)
	}

	if opts.Dir != "" {
		args = append(args, "-c", opts.Dir)
	}

	args = append(args, envArgs(opts.Env)...)
	args = append(args, "-P", "-F", paneIDFormat)
	args = append(args, opts.Command...)

	res, err := t.exec(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("creating tmux session %s: %w", opts.Name, err)
	}

	return res.Out(), nil
}

// envArgs renders an environment map as repeated -e flags, in sorted order so the
// argv is deterministic and testable.
func envArgs(env map[string]string) []string {
	if len(env) == 0 {
		return nil
	}

	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}

	slices.Sort(keys)

	args := make([]string, 0, len(keys)*2)
	for _, k := range keys {
		args = append(args, "-e", k+"="+env[k])
	}

	return args
}

// SplitOptions describes a pane to create by splitting an existing one.
type SplitOptions struct {
	// Target is the session, window, or pane to split.
	Target Target
	// Dir is the working directory for the new pane.
	Dir string
	// Horizontal splits left/right instead of top/bottom.
	Horizontal bool
	// SizePercent, when non-zero, sets the new pane's share.
	SizePercent int
	// Command is the argv to run in the new pane.
	Command []string
}

// SplitWindow creates a pane and returns its id.
func (t *Tmux) SplitWindow(ctx context.Context, opts SplitOptions) (string, error) {
	args := []string{"split-window", "-d"}

	if opts.Horizontal {
		args = append(args, "-h")
	}

	if opts.SizePercent > 0 {
		// -l with a percentage; the older -p form is deprecated in tmux 3.x.
		args = append(args, "-l", fmt.Sprintf("%d%%", opts.SizePercent))
	}

	args = append(args, "-t", opts.Target.String())

	if opts.Dir != "" {
		args = append(args, "-c", opts.Dir)
	}

	args = append(args, "-P", "-F", paneIDFormat)
	args = append(args, opts.Command...)

	res, err := t.exec(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("splitting %s: %w", opts.Target, err)
	}

	return res.Out(), nil
}

// PaneInfo describes one pane.
type PaneInfo struct {
	ID string
	// Dead reports a pane whose command exited while remain-on-exit kept it
	// visible.
	Dead bool
	// Command is the pane's foreground command, e.g. "claude".
	Command string
	// Dir is the pane's current working directory.
	Dir string
}

// ListPanes reports the panes of a session or window.
func (t *Tmux) ListPanes(ctx context.Context, target Target) ([]PaneInfo, error) {
	const format = "#{pane_id}\t#{pane_dead}\t#{pane_current_command}\t#{pane_current_path}"

	res, err := t.exec(ctx, "list-panes", "-t", target.String(), "-F", format)
	if err != nil {
		return nil, fmt.Errorf("listing panes of %s: %w", target, err)
	}

	var panes []PaneInfo

	for _, line := range res.Lines() {
		fields := strings.Split(line, "\t")
		if len(fields) < 4 {
			continue
		}

		panes = append(panes, PaneInfo{
			ID:      fields[0],
			Dead:    fields[1] == "1",
			Command: fields[2],
			Dir:     fields[3],
		})
	}

	return panes, nil
}

// ListWindowNames reports the window names of a session.
func (t *Tmux) ListWindowNames(ctx context.Context, session string) ([]string, error) {
	res, err := t.exec(ctx, "list-windows", "-t", Session(session).String(), "-F", "#{window_name}")
	if err != nil {
		return nil, fmt.Errorf("listing windows of %s: %w", session, err)
	}

	return res.Lines(), nil
}

// ListSessionNames reports every session on the server.
func (t *Tmux) ListSessionNames(ctx context.Context) ([]string, error) {
	res, err := t.exec(ctx, "list-sessions", "-F", "#{session_name}")
	if err != nil {
		// No server running is not an error; there are simply no sessions.
		if runner.IsExit(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("listing tmux sessions: %w", err)
	}

	return res.Lines(), nil
}

// HasClients reports whether any client is attached to a session, and the ttys of
// those that are.
func (t *Tmux) HasClients(ctx context.Context, session string) ([]string, error) {
	res, err := t.exec(ctx, "list-clients", "-t", Session(session).String(), "-F", "#{client_tty}")
	if err != nil {
		if runner.IsExit(err) {
			return nil, nil
		}

		return nil, fmt.Errorf("listing clients of %s: %w", session, err)
	}

	return res.Lines(), nil
}

// SetWindowOption sets a window option.
func (t *Tmux) SetWindowOption(ctx context.Context, target Target, name, value string) error {
	if _, err := t.exec(ctx, "set-option", "-w", "-t", target.String(), name, value); err != nil {
		return fmt.Errorf("setting window option %s on %s: %w", name, target, err)
	}

	return nil
}

// SetPaneOption sets a pane option.
func (t *Tmux) SetPaneOption(ctx context.Context, target Target, name, value string) error {
	if _, err := t.exec(ctx, "set-option", "-p", "-t", target.String(), name, value); err != nil {
		return fmt.Errorf("setting pane option %s on %s: %w", name, target, err)
	}

	return nil
}

// SelectLayout applies a named layout to a window.
func (t *Tmux) SelectLayout(ctx context.Context, target Target, layout string) error {
	if _, err := t.exec(ctx, "select-layout", "-t", target.String(), layout); err != nil {
		return fmt.Errorf("applying layout %s to %s: %w", layout, target, err)
	}

	return nil
}

// SelectPane focuses a pane.
func (t *Tmux) SelectPane(ctx context.Context, target Target) error {
	if _, err := t.exec(ctx, "select-pane", "-t", target.String()); err != nil {
		return fmt.Errorf("selecting pane %s: %w", target, err)
	}

	return nil
}

// SelectWindow focuses a window.
func (t *Tmux) SelectWindow(ctx context.Context, target Target) error {
	if _, err := t.exec(ctx, "select-window", "-t", target.String()); err != nil {
		return fmt.Errorf("selecting window %s: %w", target, err)
	}

	return nil
}

// KillSession terminates a session, treating an absent one as success.
func (t *Tmux) KillSession(ctx context.Context, session string) error {
	_, err := t.exec(ctx, "kill-session", "-t", Session(session).String())
	if err != nil && !runner.IsExit(err) {
		return fmt.Errorf("killing session %s: %w", session, err)
	}

	return nil
}

// AttachArgs returns the argv for attaching to a session, for handing to a
// terminal emulator. detachOthers corresponds to `attach -d`.
func (t *Tmux) AttachArgs(session string, detachOthers bool) []string {
	args := []string{t.bin, "attach-session"}
	if detachOthers {
		args = append(args, "-d")
	}

	return append(args, "-t", Session(session).String())
}

// LoadBuffer loads a file into a named tmux buffer.
func (t *Tmux) LoadBuffer(ctx context.Context, name, path string) error {
	if _, err := t.exec(ctx, "load-buffer", "-b", name, path); err != nil {
		return fmt.Errorf("loading tmux buffer from %s: %w", path, err)
	}

	return nil
}

// PasteBuffer pastes a named buffer into a pane and deletes the buffer.
//
// Bracketed paste is used so a multi-line message arrives as one paste rather
// than as a series of submissions, one per newline.
func (t *Tmux) PasteBuffer(ctx context.Context, target Target, name string) error {
	if _, err := t.exec(ctx, "paste-buffer", "-d", "-p", "-b", name, "-t", target.String()); err != nil {
		return fmt.Errorf("pasting tmux buffer into %s: %w", target, err)
	}

	return nil
}

// SendKeys sends key names to a pane, e.g. "Enter".
func (t *Tmux) SendKeys(ctx context.Context, target Target, keys ...string) error {
	args := append([]string{"send-keys", "-t", target.String()}, keys...)

	if _, err := t.exec(ctx, args...); err != nil {
		return fmt.Errorf("sending keys to %s: %w", target, err)
	}

	return nil
}

// CapturePane returns the visible contents of a pane.
func (t *Tmux) CapturePane(ctx context.Context, target Target, lines int) (string, error) {
	args := []string{"capture-pane", "-p", "-t", target.String()}
	if lines > 0 {
		args = append(args, "-S", fmt.Sprintf("-%d", lines))
	}

	res, err := t.exec(ctx, args...)
	if err != nil {
		return "", fmt.Errorf("capturing pane %s: %w", target, err)
	}

	return string(res.Stdout), nil
}
