// Package statem is the pure state machine mapping agent hook events onto mission
// state.
//
// It has no dependencies beyond the domain types, runs no processes, and touches no
// clock of its own, so every transition in the table below is unit-testable
// directly. That matters more here than anywhere else in q: this is the code
// that decides whether the board is telling the truth.
package statem

import (
	"strconv"
	"strings"
	"time"

	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/hookspec"
)

// maxLastMessage bounds the stored closing message of a turn, which becomes a card
// subtitle and is otherwise unbounded model output.
const maxLastMessage = 240

// maxWaitingFor bounds the description of what an agent is blocked on.
const maxWaitingFor = 60

// Result is the outcome of reducing one event.
type Result struct {
	// Mission carries every change except the lane.
	Mission domain.Mission
	// ProposedStatus is the lane this event argues for, or empty if it argues for
	// none.
	//
	// The lane is proposed rather than applied because hooks for a single logical
	// moment arrive as separate processes whose scheduling order is arbitrary, so a
	// Stop can land microseconds after the PermissionRequest that should outrank it.
	// The caller resolves that by precedence rather than by arrival order.
	ProposedStatus domain.Status
	// Definite reports that the proposal reflects an observed resolution rather
	// than a possibly-stale reading, and so may lower the lane immediately.
	//
	// PostToolUse completing and the human submitting a prompt are proof the block
	// is over. A Stop is not: it may be the tail of a turn whose permission request
	// has not been answered yet.
	Definite bool
	// Changed reports whether anything was updated.
	Changed bool
}

// definiteEvents are the events whose lane proposal reflects something that
// demonstrably already happened.
//
// Stop and Notification are deliberately absent. Stop is the event that races with
// PermissionRequest, and Notification is by construction a delayed restatement of
// something already reported.
var definiteEvents = map[string]bool{
	hookspec.EventSessionStart:      true,
	hookspec.EventSessionEnd:        true,
	hookspec.EventUserPromptSubmit:  true,
	hookspec.EventPostToolUse:       true,
	hookspec.EventPermissionRequest: true,
	hookspec.EventPermissionDenied:  true,
	hookspec.EventStopFailure:       true,
}

// Reduce applies one hook event to a mission.
//
// Three guards run before the table. A done mission is terminal: only a human files a
// card, and only a human takes it back, so no agent event may resurrect one. A
// draft mission ignores everything but SessionStart, since anything else is a stray
// event from a previous incarnation. And an event whose session id contradicts the
// one q recorded is ignored, so an abandoned session cannot move a live card.
func Reduce(mission domain.Mission, ev hookspec.Payload, now time.Time) Result {
	if mission.Status.Terminal() {
		return Result{Mission: mission}
	}

	if mission.Status == domain.StatusBriefing && ev.Event != hookspec.EventSessionStart {
		return Result{Mission: mission}
	}

	if !sessionMatches(mission, ev) {
		return Result{Mission: mission}
	}

	before := mission
	mission.LastEventAt = now
	mission.UpdatedAt = now

	proposed := apply(&mission, ev, now)

	return Result{
		Mission:        mission,
		ProposedStatus: proposed,
		Definite:       definiteEvents[ev.Event],
		Changed:        proposed != "" || !equivalent(before, mission),
	}
}

// sessionMatches reports whether an event belongs to the session q recorded.
//
// An empty recorded id means q has not learned it yet, which is the normal
// state for a codex mission before its SessionStart arrives, so anything is accepted
// then.
func sessionMatches(mission domain.Mission, ev hookspec.Payload) bool {
	if mission.AgentSessionID == "" || ev.SessionID == "" {
		return true
	}

	return mission.AgentSessionID == ev.SessionID
}

// apply mutates the mission for one event and returns the lane it argues for.
func apply(mission *domain.Mission, ev hookspec.Payload, now time.Time) domain.Status {
	switch ev.Event {
	case hookspec.EventSessionStart:
		return applySessionStart(mission, ev)
	case hookspec.EventSessionEnd:
		return applySessionEnd(mission, ev, now)
	case hookspec.EventUserPromptSubmit:
		return applyUserPrompt(mission)
	case hookspec.EventPreToolUse:
		return applyPreToolUse(mission)
	case hookspec.EventPostToolUse:
		return applyPostToolUse(mission, ev)
	case hookspec.EventPermissionRequest:
		return applyPermissionRequest(mission, ev)
	case hookspec.EventPermissionDenied:
		return applyPermissionDenied(mission, ev)
	case hookspec.EventNotification:
		return applyNotification(mission, ev)
	case hookspec.EventStop:
		return applyStop(mission, ev, now)
	case hookspec.EventStopFailure:
		return applyStopFailure(mission, ev)
	case hookspec.EventPreCompact:
		mission.AgentState = domain.AgentBusy
		mission.Badges = mission.WithBadge(domain.BadgeCompacting, "")

		return ""
	case hookspec.EventPostCompact:
		mission.AgentState = domain.AgentBusy
		mission.Badges = mission.WithoutBadge(domain.BadgeCompacting)

		return ""
	case hookspec.EventSubagentStart, hookspec.EventSubagentStop:
		mission.AgentState = domain.AgentBusy

		return ""
	default:
		return ""
	}
}

// applySessionStart records the session and confirms the agent came up.
//
// For codex this is the only place its session id is ever learned, which is what
// makes the mission resumable at all.
func applySessionStart(mission *domain.Mission, ev hookspec.Payload) domain.Status {
	if ev.SessionID != "" {
		mission.AgentSessionID = ev.SessionID
	}

	mission.AgentState = domain.AgentBusy
	mission.LaunchError = ""
	mission.Badges = mission.WithoutBadge(domain.BadgeHooksSilent)
	mission.Badges = mission.WithoutBadge(domain.BadgeEnded)

	// A clear, compact, or fork is a continuation of the same work rather than a
	// new start, so it must not drag a card back out of debrief.
	switch ev.Source {
	case hookspec.SourceClear, hookspec.SourceCompact, hookspec.SourceFork:
		return ""
	default:
		return domain.StatusActive
	}
}

// applySessionEnd records that the agent process is gone.
func applySessionEnd(mission *domain.Mission, ev hookspec.Payload, now time.Time) domain.Status {
	mission.AgentState = domain.AgentDead
	mission.Badges = mission.WithBadge(domain.BadgeEnded, ev.Reason)

	if mission.Status == domain.StatusActive {
		finished := now
		mission.FinishedAt = &finished

		return domain.StatusDebrief
	}

	return ""
}

// applyUserPrompt clears the blocked state, because the human evidently is not
// waiting on anything.
func applyUserPrompt(mission *domain.Mission) domain.Status {
	mission.AgentState = domain.AgentBusy
	mission.WaitingFor = ""
	mission.Badges = mission.WithoutBadge(domain.BadgeStale)

	return domain.StatusActive
}

// applyPreToolUse marks progress without arguing for a lane.
func applyPreToolUse(mission *domain.Mission) domain.Status {
	mission.AgentState = domain.AgentBusy
	mission.Badges = mission.WithoutBadge(domain.BadgeStale)

	return ""
}

// applyPostToolUse is the self-healing transition.
//
// The most common real flow is that the human attaches, answers a prompt in the
// pane, and never touches q. Without this, the board starts lying within
// minutes of first use, so a completed tool call clears a waiting card.
//
// ExitPlanMode completing means the plan was approved and the agent is working
// again, which is how a plan card returns to in-progress without being restarted.
func applyPostToolUse(mission *domain.Mission, ev hookspec.Payload) domain.Status {
	mission.AgentState = domain.AgentBusy
	mission.Badges = mission.WithoutBadge(domain.BadgeStale)

	if ev.IsPlanApproval() {
		mission.PlanPending = false
		mission.WaitingFor = ""

		return domain.StatusActive
	}

	if mission.Status == domain.StatusAwaiting {
		mission.WaitingFor = ""

		return domain.StatusActive
	}

	return ""
}

// applyPermissionRequest is the immediate blocked-on-the-human signal.
//
// A request to leave plan mode is special: it means a plan is ready to read, so the
// card goes to debrief rather than to waiting. Opening that debrief attaches the
// human to the live dialog, and approving it there switches claude into accept-edits
// natively, with nothing killed or resumed.
func applyPermissionRequest(mission *domain.Mission, ev hookspec.Payload) domain.Status {
	mission.AgentState = domain.AgentWaiting

	if ev.IsPlanApproval() {
		mission.PlanPending = true
		mission.WaitingFor = "plan debrief"

		return domain.StatusDebrief
	}

	mission.WaitingFor = describeTool(ev)

	return domain.StatusAwaiting
}

// applyPermissionDenied handles a refused permission.
//
// Only a rejected plan needs a human. Ordinary denials come from the user's own
// PreToolUse guard hooks, which deny programmatically and which the agent handles
// itself; treating those as blocked would park cards in "awaiting orders" for events
// that need no attention at all.
func applyPermissionDenied(mission *domain.Mission, ev hookspec.Payload) domain.Status {
	mission.AgentState = domain.AgentBusy

	if ev.IsPlanApproval() {
		mission.PlanPending = false
		mission.WaitingFor = "plan rejected, needs direction"

		return domain.StatusAwaiting
	}

	return ""
}

// applyNotification handles claude's multiplexed notification event.
func applyNotification(mission *domain.Mission, ev hookspec.Payload) domain.Status {
	switch ev.NotificationType {
	case hookspec.NotificationPermissionPrompt,
		hookspec.NotificationAgentNeedsInput,
		hookspec.NotificationWorkerPermissionPrompt:
		// A six-second-late confirmation of something PermissionRequest normally
		// reported already, so it only promotes a card that still looks busy.
		if mission.Status != domain.StatusActive {
			return ""
		}

		mission.AgentState = domain.AgentWaiting

		if mission.WaitingFor == "" {
			mission.WaitingFor = firstLine(ev.Message)
		}

		return domain.StatusAwaiting
	case hookspec.NotificationIdlePrompt:
		// Sixty seconds of quiet is not the same as being blocked or finished, so
		// this is a badge and nothing more.
		mission.Badges = mission.WithBadge(domain.BadgeStale, "idle")

		return ""
	default:
		return ""
	}
}

// applyStop ends a turn.
//
// In-flight background work means the session is paused rather than finished, so the
// card stays active with a badge. A pending plan debrief outranks a Stop
// entirely: downgrading it would silently strip the card of its "needs your
// approval" meaning. And a turn that ends by asking the human something is not
// finished work at all, so it goes to waiting rather than to debrief; see
// [closingQuestion].
func applyStop(mission *domain.Mission, ev hookspec.Payload, now time.Time) domain.Status {
	mission.Badges = mission.WithoutBadge(domain.BadgeStale)

	question := closingQuestion(ev.LastAssistantMessage)

	if ev.LastAssistantMessage != "" {
		// The ask makes the better subtitle when there is one: the message's first
		// line is a sentence from the middle of the agent's reasoning, and showing it
		// hides the thing being asked.
		summary := firstLine(ev.LastAssistantMessage)
		if question != "" {
			summary = question
		}

		mission.LastMessage = truncate(summary, maxLastMessage)
	}

	if ev.BackgroundTasks > 0 {
		mission.AgentState = domain.AgentBusy
		mission.Badges = mission.WithBadge(domain.BadgeBackground, strconv.Itoa(ev.BackgroundTasks))

		return domain.StatusActive
	}

	mission.Badges = mission.WithoutBadge(domain.BadgeBackground)

	if mission.PlanPending {
		mission.AgentState = domain.AgentWaiting

		return ""
	}

	// FinishedAt stays unset: the agent is blocked at its prompt, and stamping a
	// finish time would have the card report an age for work that has not ended.
	//
	// An existing description is left in place, because a Stop racing a
	// PermissionRequest must not replace the live prompt's tool name with prose from
	// the turn that led up to it.
	if question != "" {
		mission.AgentState = domain.AgentWaiting

		if mission.WaitingFor == "" {
			mission.WaitingFor = truncate(question, maxWaitingFor)
		}

		return domain.StatusAwaiting
	}

	mission.AgentState = domain.AgentIdle
	mission.WaitingFor = ""

	finished := now
	mission.FinishedAt = &finished

	return domain.StatusDebrief
}

// applyStopFailure records a turn that ended in an API error.
//
// claude ignores this hook's output entirely, so it is purely a report. The card
// needs a human either way, since the work did not finish.
func applyStopFailure(mission *domain.Mission, ev hookspec.Payload) domain.Status {
	mission.AgentState = domain.AgentWaiting
	mission.Badges = mission.WithBadge(domain.BadgeAPIError, ev.Reason)
	mission.WaitingFor = "API error"

	if ev.Reason != "" {
		mission.WaitingFor = "API error: " + ev.Reason
	}

	return domain.StatusAwaiting
}

// describeTool renders what an agent is blocked on, e.g. "Bash".
func describeTool(ev hookspec.Payload) string {
	if ev.ToolName != "" {
		return truncate(ev.ToolName, maxWaitingFor)
	}

	if ev.Message != "" {
		return truncate(firstLine(ev.Message), maxWaitingFor)
	}

	return "permission"
}

// equivalent reports whether two missions differ in any field the reducer touches.
func equivalent(a, b domain.Mission) bool {
	if a.AgentState != b.AgentState ||
		a.WaitingFor != b.WaitingFor ||
		a.PlanPending != b.PlanPending ||
		a.AgentSessionID != b.AgentSessionID ||
		a.LastMessage != b.LastMessage ||
		a.LaunchError != b.LaunchError ||
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

// firstLine returns the first line of s.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if head, _, found := strings.Cut(s, "\n"); found {
		return strings.TrimSpace(head)
	}

	return s
}

// truncate shortens s to at most n characters, marking where it was cut.
func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}

	return strings.TrimSpace(s[:n]) + "…"
}
