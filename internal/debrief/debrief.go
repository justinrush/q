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
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/gadgets"
	"github.com/justinrush/q/internal/gitx"
	"github.com/justinrush/q/internal/settings"
	"github.com/justinrush/q/internal/termopen"
	"github.com/justinrush/q/internal/tmuxc"
)

// agentWindow is the window the agent runs in, and the one debrief panes are added to.
const agentWindow = "agent"

// mainVertical keeps the agent as the wide left pane with editors stacked beside it.
const mainVertical = "main-vertical"

// Mode selects how the debrief session is presented.
type Mode string

// The presentation modes.
const (
	// ModeAttach opens a new terminal attached to the session.
	ModeAttach Mode = "attach"
	// ModeSteal detaches any other client first, which is wanted when a stale client
	// on another machine or terminal is holding the session.
	ModeSteal Mode = "steal"
	// ModeRaise brings an already-attached window forward instead of opening another.
	ModeRaise Mode = "raise"
	// ModePrepare arranges the panes but does not attach, which is what the board
	// wants when it is only refreshing the layout.
	ModePrepare Mode = "prepare"
)

// Result describes what opening a debrief did.
type Result struct {
	Session string `json:"session"`
	// Touched lists the repos with changes worth a look.
	Touched []Touched `json:"touched"`
	// PanesAdded counts editor panes created by this call.
	PanesAdded int `json:"panesAdded"`
	// AlreadyAttached lists the ttys of clients already viewing the session.
	AlreadyAttached []string `json:"alreadyAttached,omitempty"`
	// Attached reports that this call brought a client to the session.
	Attached bool `json:"attached"`
	// AttachCommand is the command to run by hand when q was configured not to
	// open windows itself. It is empty whenever q did open one.
	AttachCommand string `json:"attachCommand,omitempty"`
	// NeedsRelaunch reports that the session is gone and the agent must be
	// restarted before there is anything to attach to.
	NeedsRelaunch bool `json:"needsRelaunch"`
}

// Touched is one repo with debriefable changes.
type Touched struct {
	Repo         string `json:"repo"`
	WorktreePath string `json:"worktreePath"`
	Branch       string `json:"branch"`
	Dirty        bool   `json:"dirty"`
	Ahead        int    `json:"ahead"`
	ShortStat    string `json:"shortStat,omitempty"`
}

// Config configures an Opener.
type Config struct {
	Git    *gitx.Git
	Tmux   *tmuxc.Tmux
	Term   *termopen.Opener
	Bins   *gadgets.Resolver
	Logger *slog.Logger
	// Editor is the argv opened on each changed worktree. Empty means the
	// built-in default, which follows $VISUAL and $EDITOR.
	Editor []string
}

// Opener arranges and attaches debrief sessions.
type Opener struct {
	git    *gitx.Git
	tmux   *tmuxc.Tmux
	term   *termopen.Opener
	bins   *gadgets.Resolver
	logger *slog.Logger
	editor []string
}

// New returns an Opener.
func New(cfg Config) *Opener {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Opener{
		git:    cfg.Git,
		tmux:   cfg.Tmux,
		term:   cfg.Term,
		bins:   cfg.Bins,
		logger: logger,
		editor: cfg.Editor,
	}
}

// Open arranges the debrief session and attaches to it according to mode.
//
// The mission is returned alongside the result because arranging panes records their ids,
// which the caller persists so a second open does not duplicate them.
func (o *Opener) Open(ctx context.Context, mission domain.Mission, mode Mode) (Result, domain.Mission, error) {
	if mission.TmuxSession == "" {
		return Result{}, mission, fmt.Errorf("mission %s has never been launched", mission.ID)
	}

	result := Result{Session: mission.TmuxSession}

	touched, err := o.touchedRepos(ctx, mission)
	if err != nil {
		return result, mission, err
	}

	result.Touched = touched

	if !o.tmux.HasSession(ctx, mission.TmuxSession) {
		// Nothing to attach to. The caller decides whether to relaunch, because
		// reviving an agent is a bigger step than opening a window.
		result.NeedsRelaunch = true

		return result, mission, nil
	}

	added, mission, err := o.arrangePanes(ctx, mission, touched)
	result.PanesAdded = added
	if err != nil {
		return result, mission, err
	}

	clients, err := o.tmux.HasClients(ctx, mission.TmuxSession)
	if err != nil {
		return result, mission, err
	}

	result.AlreadyAttached = clients

	if err := o.attach(ctx, mission, mode, &result); err != nil {
		return result, mission, err
	}

	return result, mission, nil
}

// Touched reports what a mission changed, without touching tmux.
//
// This is the read-only view the board uses to label cards; arranging panes is a
// separate, deliberate act.
func (o *Opener) Touched(ctx context.Context, mission domain.Mission) ([]Touched, error) {
	return o.touchedRepos(ctx, mission)
}

// touchedRepos reports which of a mission's worktrees have changes worth a look.
//
// The comparison is against each worktree's pinned branch point rather than against
// origin, so it needs no network and cannot drift when someone else's fetch moves the
// default branch.
func (o *Opener) touchedRepos(ctx context.Context, mission domain.Mission) ([]Touched, error) {
	var touched []Touched

	for _, work := range mission.Worktrees() {
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

		touched = append(touched, Touched{
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
	mission domain.Mission,
	touched []Touched,
) (int, domain.Mission, error) {
	if len(touched) == 0 {
		return 0, mission, nil
	}

	panes, err := o.tmux.ListPanes(ctx, tmuxc.Session(mission.TmuxSession))
	if err != nil {
		return 0, mission, err
	}

	live := make(map[string]bool, len(panes))
	for _, pane := range panes {
		live[pane.ID] = true
	}

	editor, err := o.editorArgv()
	if err != nil {
		return 0, mission, err
	}

	var added int

	for _, item := range touched {
		work := mission.Work[item.Repo]

		if work.DebriefPaneID != "" && live[work.DebriefPaneID] {
			continue
		}

		paneID, err := o.tmux.SplitWindow(ctx, tmuxc.SplitOptions{
			Target:  tmuxc.Window(mission.TmuxSession, agentWindow),
			Dir:     item.WorktreePath,
			Command: editor,
		})
		if err != nil {
			return added, mission, err
		}

		work.DebriefPaneID = paneID
		mission.Work[item.Repo] = work
		added++

		// Reflow before the next split. Without this, tmux repeatedly halves the
		// active pane until it is too small to split, even when the window has
		// ample room for the finished main-vertical layout.
		o.finishLayout(ctx, mission)
	}

	return added, mission, nil
}

// finishLayout tidies the window and leaves the agent pane focused.
//
// Failures here are logged rather than returned: the panes exist and are usable, and
// losing the debrief over a layout call would be the wrong trade.
func (o *Opener) finishLayout(ctx context.Context, mission domain.Mission) {
	window := tmuxc.Window(mission.TmuxSession, agentWindow)

	if err := o.tmux.SelectLayout(ctx, window, mainVertical); err != nil {
		o.logger.Warn("applying the debrief layout", "error", err)
	}

	// The user is here to talk to the agent, so land on it rather than in an editor.
	if mission.AgentPaneID != "" {
		if err := o.tmux.SelectPane(ctx, tmuxc.Pane(mission.AgentPaneID)); err != nil {
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
		command = settings.DefaultEditorCommand()
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
func (o *Opener) attach(ctx context.Context, mission domain.Mission, mode Mode, result *Result) error {
	switch mode {
	case ModePrepare:
		return nil
	case ModeRaise:
		result.Attached = true

		return o.term.Raise(ctx)
	}

	argv := o.tmux.AttachArgs(mission.TmuxSession, mode == ModeSteal)

	err := o.term.Open(ctx, termopen.Spec{Dir: mission.MissionDir, Argv: argv})

	// Opening no window is a configured choice, not a failure. The panes are
	// arranged either way, so the useful thing to return is how to reach them.
	if errors.Is(err, termopen.ErrNoTerminal) {
		result.AttachCommand = strings.Join(argv, " ")

		return nil
	}

	if err != nil {
		return err
	}

	result.Attached = true

	return nil
}
