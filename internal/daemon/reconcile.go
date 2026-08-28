package daemon

import (
	"context"
	"strings"
	"time"

	"github.com/justinrush/q/internal/launch"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/terminal"
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
	ListPanes(ctx context.Context, target terminal.Target) ([]terminal.PaneInfo, error)
	CapturePane(ctx context.Context, target terminal.Target, lines int) (string, error)
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
	readings := s.heal(ctx, snap.Missions)
	now := s.now()

	for _, ms := range snap.Missions {
		if ms.Status == mission.StatusBriefing || ms.Status.Terminal() || !ms.Launched() {
			continue
		}

		if updated, changed := s.reconcileMission(ctx, ms, readings, now); changed {
			s.persistReconciled(updated)
		}
	}
}

// heal asks each configured healer what the agents' own registries say,
// tolerating an absent or unreadable one.
func (s *Service) heal(ctx context.Context, missions []mission.Mission) map[mission.MissionID]mission.Reading {
	out := map[mission.MissionID]mission.Reading{}

	for _, healer := range s.healers {
		readings, err := healer.Heal(ctx, missions)
		if err != nil {
			s.warn("reading an agent session registry", "error", err)

			continue
		}

		for id, reading := range readings {
			out[id] = reading
		}
	}

	return out
}

// reconcileMission returns the mission with any corrections applied.
func (s *Service) reconcileMission(
	ctx context.Context,
	ms mission.Mission,
	readings map[mission.MissionID]mission.Reading,
	now time.Time,
) (mission.Mission, bool) {
	before := ms

	alive := s.checkSession(ctx, &ms, now)
	if !alive {
		return ms, !sameReconciled(before, ms)
	}

	s.applyReading(&ms, readings, now)
	s.applyTimers(&ms, now)
	s.detectStartupPrompt(ctx, &ms, now)

	return ms, !sameReconciled(before, ms)
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
func (s *Service) detectStartupPrompt(ctx context.Context, ms *mission.Mission, now time.Time) {
	if s.probe == nil || ms.AgentPaneID == "" {
		return
	}

	if ms.AgentState != mission.AgentUnknown || !ms.HasBadge(mission.BadgeHooksSilent) {
		return
	}

	pane, err := s.probe.CapturePane(ctx, terminal.Pane(ms.AgentPaneID), capturedPromptLines)
	if err != nil {
		return
	}

	question, ok := findPrompt(pane)
	if !ok {
		return
	}

	ms.AgentState = mission.AgentWaiting
	ms.WaitingFor = question

	if ms.Status == mission.StatusActive {
		ms.Status = mission.StatusAwaiting
		ms.StatusChangedAt = now
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
func (s *Service) checkSession(ctx context.Context, ms *mission.Mission, now time.Time) bool {
	if s.probe == nil || ms.TmuxSession == "" {
		return true
	}

	// A missing session is definitive regardless of how recently the mission started.
	if !s.probe.HasSession(ctx, ms.TmuxSession) {
		s.markSessionGone(ms)

		return false
	}

	panes, err := s.probe.ListPanes(ctx, terminal.Session(ms.TmuxSession))
	if err != nil {
		return true
	}

	starting := ms.StartedAt != nil && now.Sub(*ms.StartedAt) < launchGrace

	for _, pane := range panes {
		if pane.ID != ms.AgentPaneID {
			continue
		}

		// A dead pane means the agent exited. A pane running something other than an
		// agent means the same thing, except during the moment after launch when the
		// generated script has not yet exec'd. tmux-resurrect can also restore a
		// session whose panes are dead, which looks alive by name alone.
		if pane.Dead || (!agentCommands[pane.Command] && !starting) {
			s.markSessionGone(ms)

			return false
		}

		return true
	}

	// The recorded pane is gone, which the grace period does not excuse: tmux
	// reported its id at creation, so its absence means it really has been closed.
	s.markSessionGone(ms)

	return false
}

// markSessionGone records a mission whose agent has disappeared.
//
// The lane moves to debrief rather than to anything more specific, and the nuance
// lives in a badge: q knows the agent is gone but not whether its work was
// finished, so it asks for a human rather than inventing an answer.
func (s *Service) markSessionGone(ms *mission.Mission) {
	ms.AgentState = mission.AgentDead
	ms.Badges = ms.WithBadge(mission.BadgeTmuxGone, "")

	if ms.Status == mission.StatusActive || ms.Status == mission.StatusAwaiting {
		ms.Status = mission.StatusDebrief
		ms.StatusChangedAt = s.now()
	}
}

// applyReading corrects a mission from what its agent's own registry says.
//
// This is what recovers from a dropped hook. A healer is advisory rather than
// authoritative, so each rule is deliberately conservative about the direction it
// moves a card.
func (s *Service) applyReading(ms *mission.Mission, readings map[mission.MissionID]mission.Reading, now time.Time) {
	reading, ok := readings[ms.ID]
	if !ok {
		return
	}

	// The agent knows the real pane id, which beats anything q inferred.
	if reading.PaneID != "" && reading.PaneID != ms.AgentPaneID {
		ms.AgentPaneID = reading.PaneID
	}

	switch reading.Activity {
	case mission.ActivityWaitingApproval, mission.ActivityWaitingInput:
		// Recovers a dropped PermissionRequest.
		if ms.Status == mission.StatusActive {
			ms.Status = mission.StatusAwaiting
			ms.StatusChangedAt = now
			ms.AgentState = mission.AgentWaiting

			if reading.WaitingFor != "" {
				ms.WaitingFor = reading.WaitingFor
			}
		}
	case mission.ActivityIdle:
		// Recovers a dropped Stop, but only once the session has been idle long
		// enough that a turn cannot still be in flight.
		if ms.Status == mission.StatusActive && !ms.PlanPending &&
			now.Sub(ms.LastEventAt) > idleBeforeDebrief {
			ms.Status = mission.StatusDebrief
			ms.StatusChangedAt = now
			ms.AgentState = mission.AgentIdle
		}
	case mission.ActivityBusy:
		// Recovers a dropped PostToolUse. A pending plan debrief is never overridden:
		// the agent may look busy while the approval dialog waits.
		ms.AgentState = mission.AgentBusy

		if !ms.PlanPending && (ms.Status == mission.StatusAwaiting || ms.Status == mission.StatusDebrief) {
			ms.Status = mission.StatusActive
			ms.StatusChangedAt = now
			ms.WaitingFor = ""
		}
	case mission.ActivityUnknown, mission.ActivityFailed:
	}
}

// applyTimers adds the badges that report q's own uncertainty.
func (s *Service) applyTimers(ms *mission.Mission, now time.Time) {
	// A launched mission that has never reported means the hook bridge is not working.
	if ms.AgentState == mission.AgentUnknown && ms.StartedAt != nil &&
		now.Sub(*ms.StartedAt) > hooksSilentAfter {
		ms.Badges = ms.WithBadge(mission.BadgeHooksSilent, "")
	}

	if ms.AgentState == mission.AgentBusy && !ms.LastEventAt.IsZero() &&
		now.Sub(ms.LastEventAt) > staleAfter {
		ms.Badges = ms.WithBadge(mission.BadgeStale, "quiet")
	}
}

// persistReconciled writes a corrected mission.
func (s *Service) persistReconciled(ms mission.Mission) {
	var updated mission.Mission

	err := s.store.Mutate("mission.reconcile", func(snap *mission.Snapshot) error {
		stored, ok := snap.Mission(ms.ID)
		if !ok {
			return nil
		}

		// A hook may have landed since the snapshot was taken, so only the fields
		// reconciliation owns are carried over.
		stored.Status = ms.Status
		stored.StatusChangedAt = ms.StatusChangedAt
		stored.AgentState = ms.AgentState
		stored.WaitingFor = ms.WaitingFor
		stored.AgentPaneID = ms.AgentPaneID
		stored.Badges = ms.Badges
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
func sameReconciled(a, b mission.Mission) bool {
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
