// Package launch starts a mission's agent and talks to the session it runs in.
//
// The sequence is: provision one worktree per repo, write everything the agent
// needs into the mission directory, then start the agent in a detached tmux
// session. Each of those three steps belongs to someone else — [Workspace],
// [mission.Agent], and tmux — and this package owns only the order and the
// rollback.
//
// Partial failure rolls back. A mission whose worktrees exist for two of three repos
// is worse than one that failed outright, because the agent would start, find the
// third repo missing, and improvise.
package launch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/justinrush/q/internal/git"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/terminal"
)

// agentWindow is the name of the window the agent runs in.
const agentWindow = "agent"

// Workspace provisions and reclaims the git worktrees a mission works in.
//
// It is declared here rather than imported as a concrete type so a launch can be
// tested against a workspace that fails on demand, which is the only way to
// assert that a failed launch leaves nothing behind.
type Workspace interface {
	Prepare(ctx context.Context, operation mission.Operation, ms *mission.Mission) (*git.Provisioned, error)
	PlanReclaim(ctx context.Context, operation mission.Operation, ms mission.Mission) (mission.Plan, error)
	Reclaim(ctx context.Context, operation mission.Operation, ms mission.Mission, force bool) (mission.Report, error)
}

// Launcher starts missions and talks to the sessions they run in.
//
// It owns the sequence — provision the worktrees, write what the agent needs,
// start the session — and delegates each step: worktrees to a [Workspace], the
// invocation to a [mission.Agent], and the session itself to tmux.
type Launcher struct {
	dirs      paths.Dirs
	workspace Workspace
	tmux      *terminal.Tmux
	agents    map[mission.Tool]mission.Agent
	logger    *slog.Logger
	now       func() time.Time
}

// Option configures a Launcher.
type Option func(*Launcher)

// WithAgent registers the agent that runs missions for one tool. A mission whose
// tool has no agent registered is refused rather than launched.
func WithAgent(agent mission.Agent) Option {
	return func(l *Launcher) { l.agents[agent.Tool()] = agent }
}

// WithLogger sets where the launcher reports problems it does not fail on.
func WithLogger(logger *slog.Logger) Option {
	return func(l *Launcher) { l.logger = logger }
}

// WithClock replaces the time source, for tests.
func WithClock(now func() time.Time) Option {
	return func(l *Launcher) { l.now = now }
}

// New returns a Launcher that provisions through workspace and hosts agents in
// tmux sessions.
func New(dirs paths.Dirs, workspace Workspace, tmux *terminal.Tmux, opts ...Option) *Launcher {
	l := &Launcher{
		dirs:      dirs,
		workspace: workspace,
		tmux:      tmux,
		agents:    make(map[mission.Tool]mission.Agent),
		logger:    slog.Default(),
		now:       time.Now,
	}

	for _, opt := range opts {
		opt(l)
	}

	return l
}

// Launch provisions worktrees and starts the mission's agent.
//
// It returns the mission with its runtime fields populated. Persisting the result is
// the caller's responsibility, so that a launch failure and its rollback are a
// single state transition rather than several.
func (l *Launcher) Launch(ctx context.Context, operation mission.Operation, ms mission.Mission) (mission.Mission, error) {
	ms.HookEpoch++

	repos, err := mission.MissionRepos(operation, ms)
	if err != nil {
		return ms, err
	}

	ms.LaunchRepos = repos
	ms.LaunchReposFrozen = true
	operation.Repos = repos

	// claude accepts a session id chosen in advance, so q picks one here and
	// the session is resumable from the instant it starts. codex generates its own
	// internally, so its id has to be learned from its SessionStart hook instead,
	// and stays empty until then.
	if ms.Tool.SupportsPresetSessionID() && ms.AgentSessionID == "" {
		sessionID, err := mission.NewSessionUUID()
		if err != nil {
			return ms, err
		}

		ms.AgentSessionID = sessionID
	}

	// Refuse to adopt an existing session: it belongs to something else, and
	// launching into it would put two agents in one window.
	if session := mission.TmuxSessionName(operation.Slug, ms.Slug, ms.ID); l.tmux.HasSession(ctx, session) {
		return ms, fmt.Errorf("tmux session %s already exists", session)
	}

	provisioned, err := l.workspace.Prepare(ctx, operation, &ms)
	if err != nil {
		provisioned.Rollback(ctx)

		return ms, err
	}

	ms.Work = provisioned.Work

	if err := l.writeArtifacts(operation, ms); err != nil {
		provisioned.Rollback(ctx)

		return ms, err
	}

	if err := l.startSession(ctx, operation, &ms); err != nil {
		provisioned.Rollback(ctx)

		return ms, err
	}

	started := l.now()
	ms.StartedAt = &started
	ms.Status = mission.StatusActive
	ms.AgentState = mission.AgentUnknown
	ms.LaunchError = ""

	return ms, nil
}

// errNoAgentBinary reports that no agent is configured for a mission's tool.
var errNoAgentBinary = errors.New("agent unavailable")

// PlanReclaim reports what reclaiming a mission's worktrees would discard.
func (l *Launcher) PlanReclaim(
	ctx context.Context,
	operation mission.Operation,
	ms mission.Mission,
) (mission.Plan, error) {
	return l.workspace.PlanReclaim(ctx, operation, ms)
}

// Reclaim removes a mission's worktrees, branches, and tmux session.
func (l *Launcher) Reclaim(
	ctx context.Context,
	operation mission.Operation,
	ms mission.Mission,
	force bool,
) (mission.Report, error) {
	return l.workspace.Reclaim(ctx, operation, ms, force)
}
