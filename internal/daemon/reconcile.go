package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/justinrush/q/internal/claudereg"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/launch"
	"github.com/justinrush/q/internal/state"
	"github.com/justinrush/q/internal/tmuxc"
)

// Reconciliation tuning.
const (
	// ReconcileInterval is how often the daemon checks reality against its records.
	ReconcileInterval = 15 * time.Second
	// hooksSilentAfter is how long a launched mission may go without a SessionStart
	// before q says so.
	//
	// This badge matters more than it looks. If hook wiring breaks, every card
	// silently stops updating and the board becomes confidently wrong, which is
	// worse than being visibly unsure.
	hooksSilentAfter = 30 * time.Second
	// idleBeforeDebrief is how long a claude session must read as idle before a
	// missed Stop is assumed.
	idleBeforeDebrief = 30 * time.Second
	// staleAfter is how long a busy mission may go quiet before being marked stale.
	//
	// This remains a fallback for either agent when its live status source is
	// unavailable.
	staleAfter = 5 * time.Minute
	// launchGrace is how long after launch the agent pane is allowed to be running
	// something other than the agent itself.
	//
	// tmux starts the generated launch script, which then execs the agent. For that
	// brief moment the pane's command is a shell, and treating it as a dead agent
	// would mark a perfectly healthy mission as gone seconds after starting it.
	launchGrace = 15 * time.Second
)

// SessionProbe reports on tmux sessions and panes.
//
// It is an interface so reconciliation can be tested without a tmux server.
type SessionProbe interface {
	HasSession(ctx context.Context, session string) bool
	ListPanes(ctx context.Context, target tmuxc.Target) ([]tmuxc.PaneInfo, error)
	CapturePane(ctx context.Context, target tmuxc.Target, lines int) (string, error)
}

// promptMarkers are phrases an agent uses when it has stopped to ask something at
// startup.
//
// Reading the pane is a last resort, used only for a mission that has never reported. It
// exists because an agent that stops to ask a question before its hooks are configured
// blocks forever in a detached session, showing nothing but a badge. codex does exactly
// this for directory trust and for hook review. The markers are the agents' own
// wording, so they are left exactly as those tools print them.
var promptMarkers = []string{
	"do you trust",
	"need review",
	"press enter",
	"1. yes",
	"[y/n]",
	"(y/n)",
	"continue?",
}

// capturedPromptLines is how much of the pane to read when looking for a question.
const capturedPromptLines = 40

// SetProbe attaches the tmux probe reconciliation uses.
func (s *Service) SetProbe(p SessionProbe) { s.probe = p }

// agentCommands are the process names a live agent pane runs.
//
// Anything else in an agent pane means the agent has exited, which matters for more
// than reporting: sending a message to a pane that fell back to a shell would execute
// it. The list is shared with the launcher so both judgements agree.
var agentCommands = launch.AgentCommands

// Reconcile brings mission records back in line with observable reality.
//
// It exists because hooks can be missed: the daemon may have been down, a hook may
// have timed out, or the wiring may be broken outright. Anything it corrects, it
// corrects toward a lane that asks for human attention rather than one that claims
// completion, and where it cannot tell it adds a badge instead of guessing.
func (s *Service) Reconcile(ctx context.Context) {
	snap := s.store.Snapshot()
	registry := s.scanRegistry()
	now := s.now()

	for _, mission := range snap.Missions {
		if mission.Status == domain.StatusBriefing || mission.Status.Terminal() || !mission.Launched() {
			continue
		}

		if updated, changed := s.reconcileMission(ctx, mission, registry, now); changed {
			s.persistReconciled(updated)
		}
	}
}

// scanRegistry reads claude's live session registry, tolerating its absence.
func (s *Service) scanRegistry() map[string]claudereg.Session {
	dir, err := claudereg.DefaultDir()
	if err != nil {
		return nil
	}

	sessions, err := claudereg.Scan(dir)
	if err != nil {
		s.warn("reading the claude session registry", "error", err)

		return nil
	}

	return claudereg.BySessionID(sessions)
}

// reconcileMission returns the mission with any corrections applied.
func (s *Service) reconcileMission(
	ctx context.Context,
	mission domain.Mission,
	registry map[string]claudereg.Session,
	now time.Time,
) (domain.Mission, bool) {
	before := mission

	alive := s.checkSession(ctx, &mission, now)
	if !alive {
		return mission, !sameReconciled(before, mission)
	}

	s.applyRegistry(&mission, registry, now)
	s.applyTimers(&mission, now)
	s.detectStartupPrompt(ctx, &mission, now)

	return mission, !sameReconciled(before, mission)
}

// detectStartupPrompt looks for an agent stopped at a question before it ever reported.
//
// Without this, an agent blocked at a startup prompt sits in the in-progress lane
// indefinitely with only a hooks-silent badge to show for it, and the only way to find
// out why is to attach to the session by hand. Reading the pane turns that into a card
// that says what it is waiting for.
//
// It runs only for a mission that has never reported, so a working agent's pane is never
// scraped and its screen contents never influence its lane.
func (s *Service) detectStartupPrompt(ctx context.Context, mission *domain.Mission, now time.Time) {
	if s.probe == nil || mission.AgentPaneID == "" {
		return
	}

	if mission.AgentState != domain.AgentUnknown || !mission.HasBadge(domain.BadgeHooksSilent) {
		return
	}

	pane, err := s.probe.CapturePane(ctx, tmuxc.Pane(mission.AgentPaneID), capturedPromptLines)
	if err != nil {
		return
	}

	question, ok := findPrompt(pane)
	if !ok {
		return
	}

	mission.AgentState = domain.AgentWaiting
	mission.WaitingFor = question

	if mission.Status == domain.StatusActive {
		mission.Status = domain.StatusAwaiting
		mission.StatusChangedAt = now
	}
}

// findPrompt reports the question an agent is waiting on, if its pane shows one.
func findPrompt(pane string) (string, bool) {
	var (
		lines    []string
		isPrompt bool
	)

	for line := range strings.SplitSeq(pane, "\n") {
		trimmed := strings.TrimSpace(strings.TrimLeft(line, "›>· "))
		if trimmed == "" {
			continue
		}

		lines = append(lines, trimmed)

		lower := strings.ToLower(trimmed)

		for _, marker := range promptMarkers {
			if strings.Contains(lower, marker) {
				isPrompt = true
			}
		}
	}

	if !isPrompt || len(lines) == 0 {
		return "", false
	}

	// The first line is the heading an agent puts above its question, which reads
	// better on a card than the answer options below it.
	question := lines[0]
	if len(question) > maxPromptLength {
		question = strings.TrimSpace(question[:maxPromptLength]) + "…"
	}

	return "waiting at prompt: " + question, true
}

// maxPromptLength bounds the captured question, which is screen text of any length.
const maxPromptLength = 70

// checkSession verifies the mission's tmux session and agent pane, reporting whether the
// agent still appears to be running.
func (s *Service) checkSession(ctx context.Context, mission *domain.Mission, now time.Time) bool {
	if s.probe == nil || mission.TmuxSession == "" {
		return true
	}

	// A missing session is definitive regardless of how recently the mission started.
	if !s.probe.HasSession(ctx, mission.TmuxSession) {
		s.markSessionGone(mission)

		return false
	}

	panes, err := s.probe.ListPanes(ctx, tmuxc.Session(mission.TmuxSession))
	if err != nil {
		return true
	}

	starting := mission.StartedAt != nil && now.Sub(*mission.StartedAt) < launchGrace

	for _, pane := range panes {
		if pane.ID != mission.AgentPaneID {
			continue
		}

		// A dead pane means the agent exited. A pane running something other than an
		// agent means the same thing, except during the moment after launch when the
		// generated script has not yet exec'd. tmux-resurrect can also restore a
		// session whose panes are dead, which looks alive by name alone.
		if pane.Dead || (!agentCommands[pane.Command] && !starting) {
			s.markSessionGone(mission)

			return false
		}

		return true
	}

	// The recorded pane is gone, which the grace period does not excuse: tmux
	// reported its id at creation, so its absence means it really has been closed.
	s.markSessionGone(mission)

	return false
}

// markSessionGone records a mission whose agent has disappeared.
//
// The lane moves to debrief rather than to anything more specific, and the nuance
// lives in a badge: q knows the agent is gone but not whether its work was
// finished, so it asks for a human rather than inventing an answer.
func (s *Service) markSessionGone(mission *domain.Mission) {
	mission.AgentState = domain.AgentDead
	mission.Badges = mission.WithBadge(domain.BadgeTmuxGone, "")

	if mission.Status == domain.StatusActive || mission.Status == domain.StatusAwaiting {
		mission.Status = domain.StatusDebrief
		mission.StatusChangedAt = s.now()
	}
}

// applyRegistry corrects a claude mission from claude's own live status.
//
// This is what recovers from a dropped hook. Each rule is deliberately conservative
// about the direction it moves a card.
func (s *Service) applyRegistry(mission *domain.Mission, registry map[string]claudereg.Session, now time.Time) {
	if mission.Tool != domain.ToolClaude || mission.AgentSessionID == "" {
		return
	}

	session, ok := registry[mission.AgentSessionID]
	if !ok {
		return
	}

	// The registry knows the real pane id, which beats anything q inferred.
	if pane := session.PaneID(); pane != "" && pane != mission.AgentPaneID {
		mission.AgentPaneID = pane
	}

	switch session.Status {
	case claudereg.StatusAwaiting:
		// Recovers a dropped PermissionRequest.
		if mission.Status == domain.StatusActive {
			mission.Status = domain.StatusAwaiting
			mission.StatusChangedAt = now
			mission.AgentState = domain.AgentWaiting

			if session.WaitingFor != "" {
				mission.WaitingFor = session.WaitingFor
			}
		}
	case claudereg.StatusIdle:
		// Recovers a dropped Stop, but only once the session has been idle long
		// enough that a turn cannot still be in flight.
		if mission.Status == domain.StatusActive && !mission.PlanPending &&
			now.Sub(mission.LastEventAt) > idleBeforeDebrief {
			mission.Status = domain.StatusDebrief
			mission.StatusChangedAt = now
			mission.AgentState = domain.AgentIdle
		}
	case claudereg.StatusBusy:
		// Recovers a dropped PostToolUse. A pending plan debrief is never overridden:
		// the agent may look busy while the approval dialog waits.
		mission.AgentState = domain.AgentBusy

		if !mission.PlanPending && (mission.Status == domain.StatusAwaiting || mission.Status == domain.StatusDebrief) {
			mission.Status = domain.StatusActive
			mission.StatusChangedAt = now
			mission.WaitingFor = ""
		}
	}
}

// applyTimers adds the badges that report q's own uncertainty.
func (s *Service) applyTimers(mission *domain.Mission, now time.Time) {
	// A launched mission that has never reported means the hook bridge is not working.
	if mission.AgentState == domain.AgentUnknown && mission.StartedAt != nil &&
		now.Sub(*mission.StartedAt) > hooksSilentAfter {
		mission.Badges = mission.WithBadge(domain.BadgeHooksSilent, "")
	}

	if mission.AgentState == domain.AgentBusy && !mission.LastEventAt.IsZero() &&
		now.Sub(mission.LastEventAt) > staleAfter {
		mission.Badges = mission.WithBadge(domain.BadgeStale, "quiet")
	}
}

// persistReconciled writes a corrected mission.
func (s *Service) persistReconciled(mission domain.Mission) {
	var updated domain.Mission

	err := s.store.Mutate("mission.reconcile", func(snap *state.Snapshot) error {
		stored, ok := snap.Mission(mission.ID)
		if !ok {
			return nil
		}

		// A hook may have landed since the snapshot was taken, so only the fields
		// reconciliation owns are carried over.
		stored.Status = mission.Status
		stored.StatusChangedAt = mission.StatusChangedAt
		stored.AgentState = mission.AgentState
		stored.WaitingFor = mission.WaitingFor
		stored.AgentPaneID = mission.AgentPaneID
		stored.Badges = mission.Badges
		stored.UpdatedAt = s.now()

		updated = stored
		snap.PutMission(stored)

		return nil
	})
	if err != nil {
		s.warn("recording reconciliation", "error", err)

		return
	}

	if updated.ID != "" {
		s.publishMission(updated)
	}
}

// sameReconciled reports whether two missions agree on every field reconciliation may
// change.
func sameReconciled(a, b domain.Mission) bool {
	if a.Status != b.Status || a.AgentState != b.AgentState ||
		a.WaitingFor != b.WaitingFor || a.AgentPaneID != b.AgentPaneID ||
		len(a.Badges) != len(b.Badges) {
		return false
	}

	for i := range a.Badges {
		if a.Badges[i] != b.Badges[i] {
			return false
		}
	}

	return true
}

// RunReconciler reconciles on a fixed interval until ctx is canceled.
func (s *Service) RunReconciler(ctx context.Context) {
	ticker := time.NewTicker(ReconcileInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Reconcile(ctx)
		}
	}
}
