// Package debrief opens a mission for human debrief.
//
// A debrief session is the mission's own tmux session, not a new one: the agent is still
// sitting in it, so opening a debrief means attaching to the live conversation and
// adding an editor pane for each repo the mission actually changed.
package debrief

import (
	"context"
	"errors"
	"fmt"
	"github.com/justinrush/q/internal/api"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/justinrush/q/internal/git"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/terminal"
)

// agentWindow is the window the agent runs in, and the one debrief panes are added to.
const agentWindow = "agent"

// mainVertical keeps the agent as the wide left pane with editors stacked beside it.
const mainVertical = "main-vertical"

// Opener arranges and attaches debrief sessions.
type Opener struct {
	git    *git.Client
	tmux   *terminal.Tmux
	term   terminal.Opener
	logger *slog.Logger
	editor []string
}

// Option configures an Opener.
type Option func(*Opener)

// WithEditor sets the argv opened on each changed worktree, e.g.
// ["nvim", "+Neotree"]. The worktree is the process's working directory, so no
// path argument is needed.
func WithEditor(argv []string) Option {
	return func(o *Opener) { o.editor = argv }
}

// WithLogger sets where the opener reports problems it does not fail on.
func WithLogger(logger *slog.Logger) Option {
	return func(o *Opener) { o.logger = logger }
}

// New returns an Opener that inspects worktrees with gitc, arranges panes in
// tmux, and opens windows through window.
func New(gitc *git.Client, tmux *terminal.Tmux, window terminal.Opener, opts ...Option) *Opener {
	o := &Opener{git: gitc, tmux: tmux, term: window, logger: slog.Default()}

	for _, opt := range opts {
		opt(o)
	}

	return o
}

// Open arranges the debrief session and attaches to it according to mode.
//
// The mission is returned alongside the result because arranging panes records their ids,
// which the caller persists so a second open does not duplicate them.
func (o *Opener) Open(ctx context.Context, ms mission.Mission, mode api.Mode) (api.Result, mission.Mission, error) {
	if ms.TmuxSession == "" {
		return api.Result{}, ms, fmt.Errorf("mission %s has never been launched", ms.ID)
	}

	result := api.Result{Session: ms.TmuxSession}

	touched, err := o.touchedRepos(ctx, ms)
	if err != nil {
		return result, ms, err
	}

	result.Touched = touched

	if !o.tmux.HasSession(ctx, ms.TmuxSession) {
		// Nothing to attach to. The caller decides whether to relaunch, because
		// reviving an agent is a bigger step than opening a window.
		result.NeedsRelaunch = true

		return result, ms, nil
	}

	added, ms, err := o.arrangePanes(ctx, ms, touched)
	result.PanesAdded = added
	if err != nil {
		return result, ms, err
	}

	clients, err := o.tmux.HasClients(ctx, ms.TmuxSession)
	if err != nil {
		return result, ms, err
	}

	result.AlreadyAttached = clients

	if err := o.attach(ctx, ms, mode, &result); err != nil {
		return result, ms, err
	}

	return result, ms, nil
}

// Touched reports what a mission changed, without touching tmux.
//
// This is the read-only view the board uses to label cards; arranging panes is a
// separate, deliberate act.
func (o *Opener) Touched(ctx context.Context, ms mission.Mission) ([]api.Touched, error) {
	return o.touchedRepos(ctx, ms)
}

// touchedRepos reports which of a mission's worktrees have changes worth a look.
//
// The comparison is against each worktree's pinned branch point rather than against
// origin, so it needs no network and cannot drift when someone else's fetch moves the
// default branch.
func (o *Opener) touchedRepos(ctx context.Context, ms mission.Mission) ([]api.Touched, error) {
	var touched []api.Touched

	for _, work := range ms.Worktrees() {
		if !work.Created {
			continue
		}

		if _, err := os.Stat(work.WorktreePath); err != nil {
			o.logger.Warn("worktree is missing", "worktree", work.WorktreePath, "error", err)

			continue
		}

		summary, err := o.git.TouchedSummary(ctx, work.WorktreePath, work.BaseSHA)
		if err != nil {
			return nil, err
		}

		if !summary.Any() {
			continue
		}

		touched = append(touched, api.Touched{
			Repo:         work.RepoName,
			WorktreePath: work.WorktreePath,
			Branch:       work.Branch,
			Dirty:        summary.Dirty,
			Ahead:        summary.Ahead,
			ShortStat:    summary.ShortStat,
		})
	}

	return touched, nil
}

// arrangePanes adds an editor pane for each touched repo that does not already have
// one, returning how many were created.
//
// Idempotence is by recorded pane id cross-checked against the panes tmux actually
// reports, so a pane the user closed is recreated while an existing one is left alone.
func (o *Opener) arrangePanes(
	ctx context.Context,
	ms mission.Mission,
	touched []api.Touched,
) (int, mission.Mission, error) {
	if len(touched) == 0 {
		return 0, ms, nil
	}

	panes, err := o.tmux.ListPanes(ctx, terminal.Session(ms.TmuxSession))
	if err != nil {
		return 0, ms, err
	}

	live := make(map[string]bool, len(panes))
	for _, pane := range panes {
		live[pane.ID] = true
	}

	editor, err := o.editorArgv()
	if err != nil {
		return 0, ms, err
	}

	var added int

	for _, item := range touched {
		work := ms.Work[item.Repo]

		if work.DebriefPaneID != "" && live[work.DebriefPaneID] {
			continue
		}

		paneID, err := o.tmux.SplitWindow(ctx, terminal.SplitOptions{
			Target:  terminal.Window(ms.TmuxSession, agentWindow),
			Dir:     item.WorktreePath,
			Command: editor,
		})
		if err != nil {
			return added, ms, err
		}

		work.DebriefPaneID = paneID
		ms.Work[item.Repo] = work
		added++

		// Reflow before the next split. Without this, tmux repeatedly halves the
		// active pane until it is too small to split, even when the window has
		// ample room for the finished main-vertical layout.
		o.finishLayout(ctx, ms)
	}

	return added, ms, nil
}

// finishLayout tidies the window and leaves the agent pane focused.
//
// Failures here are logged rather than returned: the panes exist and are usable, and
// losing the debrief over a layout call would be the wrong trade.
func (o *Opener) finishLayout(ctx context.Context, ms mission.Mission) {
	window := terminal.Window(ms.TmuxSession, agentWindow)

	if err := o.tmux.SelectLayout(ctx, window, mainVertical); err != nil {
		o.logger.Warn("applying the debrief layout", "error", err)
	}

	// The user is here to talk to the agent, so land on it rather than in an editor.
	if ms.AgentPaneID != "" {
		if err := o.tmux.SelectPane(ctx, terminal.Pane(ms.AgentPaneID)); err != nil {
			o.logger.Warn("focusing the agent pane", "error", err)
		}
	}
}

// editorArgv returns the command to run in each debrief pane.
//
// The editor is resolved to an absolute path rather than left to the pane's shell.
// A configured command is often a shell alias in the user's own setup, and an alias
// is not an executable; going through an interactive shell to reach one would also
// print that shell's startup banner into the pane.
func (o *Opener) editorArgv() ([]string, error) {
	command := o.editor
	if len(command) == 0 {
		return nil, errors.New("no editor is configured")
	}

	bin, err := exec.LookPath(command[0])
	if err != nil {
		return nil, fmt.Errorf("resolving the editor %q: %w", command[0], err)
	}

	abs, err := filepath.Abs(bin)
	if err != nil {
		return nil, fmt.Errorf("resolving the editor %q: %w", command[0], err)
	}

	return append([]string{abs}, command[1:]...), nil
}

// attach brings a client to the session according to mode.
func (o *Opener) attach(ctx context.Context, ms mission.Mission, mode api.Mode, result *api.Result) error {
	switch mode {
	case api.ModePrepare:
		return nil
	case api.ModeRaise:
		result.Attached = true

		return o.term.Raise(ctx)
	}

	argv := o.tmux.AttachArgs(ms.TmuxSession, mode == api.ModeSteal)

	err := o.term.Open(ctx, terminal.Spec{Dir: ms.MissionDir, Argv: argv})

	// Opening no window is a configured choice, not a failure. The panes are
	// arranged either way, so the useful thing to return is how to reach them.
	if errors.Is(err, terminal.ErrNoTerminal) {
		result.AttachCommand = strings.Join(argv, " ")

		return nil
	}

	if err != nil {
		return err
	}

	result.Attached = true

	return nil
}
