// The pure state machine mapping agent hook events onto mission state.
//
// [Reduce] runs no processes and touches no clock of its own, so every transition
// in the table below is unit-testable directly. That matters more here than
// anywhere else in q: this is the code that decides whether the board is telling
// the truth.

package mission

import (
	"strconv"
	"strings"
	"time"
)

// maxLastMessage bounds the stored closing message of a turn, which becomes a card
// subtitle and is otherwise unbounded model output.
const maxLastMessage = 240

// maxWaitingFor bounds the description of what an agent is blocked on.
const maxWaitingFor = 60

// Reduction is the outcome of reducing one event.
type Reduction struct {
	// Mission carries every change except the lane.
	Mission Mission
	// ProposedStatus is the lane this event argues for, or empty if it argues for
	// none.
	//
	// The lane is proposed rather than applied because hooks for a single logical
	// moment arrive as separate processes whose scheduling order is arbitrary, so a
	// Stop can land microseconds after the PermissionRequest that should outrank it.
	// The caller resolves that by precedence rather than by arrival order.
	ProposedStatus Status
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
	EventSessionStart:      true,
	EventSessionEnd:        true,
	EventUserPromptSubmit:  true,
	EventPostToolUse:       true,
	EventPermissionRequest: true,
	EventPermissionDenied:  true,
	EventStopFailure:       true,
}

// Reduce applies one hook event to a mission.
//
// Three guards run before the table. A done mission is terminal: only a human files a
// card, and only a human takes it back, so no agent event may resurrect one. A
// draft mission ignores everything but SessionStart, since anything else is a stray
// event from a previous incarnation. And an event whose session id contradicts the
// one q recorded is ignored, so an abandoned session cannot move a live card.
func Reduce(ms Mission, ev HookEvent, now time.Time) Reduction {
	if ms.Status.Terminal() {
		return Reduction{Mission: ms}
	}

	if ms.Status == StatusBriefing && ev.Event != EventSessionStart {
		return Reduction{Mission: ms}
	}

	if !sessionMatches(ms, ev) {
		return Reduction{Mission: ms}
	}

	before := ms
	ms.LastEventAt = now
	ms.UpdatedAt = now

	proposed := apply(&ms, ev, now)

	return Reduction{
		Mission:        ms,
		ProposedStatus: proposed,
		Definite:       definiteEvents[ev.Event],
		Changed:        proposed != "" || !equivalent(before, ms),
	}
}

// sessionMatches reports whether an event belongs to the session q recorded.
//
// An empty recorded id means q has not learned it yet, which is the normal
// state for a codex mission before its SessionStart arrives, so anything is accepted
// then.
func sessionMatches(ms Mission, ev HookEvent) bool {
	if ms.AgentSessionID == "" || ev.SessionID == "" {
		return true
	}

	return ms.AgentSessionID == ev.SessionID
}

// apply mutates the mission for one event and returns the lane it argues for.
func apply(ms *Mission, ev HookEvent, now time.Time) Status {
	switch ev.Event {
	case EventSessionStart:
		return applySessionStart(ms, ev)
	case EventSessionEnd:
		return applySessionEnd(ms, ev, now)
	case EventUserPromptSubmit:
		return applyUserPrompt(ms)
	case EventPreToolUse:
		return applyPreToolUse(ms)
	case EventPostToolUse:
		return applyPostToolUse(ms, ev)
	case EventPermissionRequest:
		return applyPermissionRequest(ms, ev)
	case EventPermissionDenied:
		return applyPermissionDenied(ms, ev)
	case EventNotification:
		return applyNotification(ms, ev)
	case EventStop:
		return applyStop(ms, ev, now)
	case EventStopFailure:
		return applyStopFailure(ms, ev)
	case EventPreCompact:
		ms.AgentState = AgentBusy
		ms.Badges = ms.WithBadge(BadgeCompacting, "")

		return ""
	case EventPostCompact:
		ms.AgentState = AgentBusy
		ms.Badges = ms.WithoutBadge(BadgeCompacting)

		return ""
	case EventSubagentStart, EventSubagentStop:
		ms.AgentState = AgentBusy

		return ""
	default:
		return ""
	}
}

// applySessionStart records the session and confirms the agent came up.
//
// For codex this is the only place its session id is ever learned, which is what
// makes the mission resumable at all.
func applySessionStart(ms *Mission, ev HookEvent) Status {
	if ev.SessionID != "" {
		ms.AgentSessionID = ev.SessionID
	}

	ms.AgentState = AgentBusy
	ms.LaunchError = ""
	ms.Badges = ms.WithoutBadge(BadgeHooksSilent)
	ms.Badges = ms.WithoutBadge(BadgeEnded)

	// A clear, compact, or fork is a continuation of the same work rather than a
	// new start, so it must not drag a card back out of debrief.
	switch ev.Source {
	case SourceClear, SourceCompact, SourceFork:
		return ""
	default:
		return StatusActive
	}
}

// applySessionEnd records that the agent process is gone.
func applySessionEnd(ms *Mission, ev HookEvent, now time.Time) Status {
	ms.AgentState = AgentDead
	ms.Badges = ms.WithBadge(BadgeEnded, ev.Reason)

	if ms.Status == StatusActive {
		finished := now
		ms.FinishedAt = &finished

		return StatusDebrief
	}

	return ""
}

// applyUserPrompt clears the blocked state, because the human evidently is not
// waiting on anything.
func applyUserPrompt(ms *Mission) Status {
	ms.AgentState = AgentBusy
	ms.WaitingFor = ""
	ms.Badges = ms.WithoutBadge(BadgeStale)

	return StatusActive
}

// applyPreToolUse marks progress without arguing for a lane.
func applyPreToolUse(ms *Mission) Status {
	ms.AgentState = AgentBusy
	ms.Badges = ms.WithoutBadge(BadgeStale)

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
func applyPostToolUse(ms *Mission, ev HookEvent) Status {
	ms.AgentState = AgentBusy
	ms.Badges = ms.WithoutBadge(BadgeStale)

	if ev.IsPlanApproval() {
		ms.PlanPending = false
		ms.WaitingFor = ""

		return StatusActive
	}

	if ms.Status == StatusAwaiting {
		ms.WaitingFor = ""

		return StatusActive
	}

	return ""
}

// applyPermissionRequest is the immediate blocked-on-the-human signal.
//
// A request to leave plan mode is special: it means a plan is ready to read, so the
// card goes to debrief rather than to waiting. Opening that debrief attaches the
// human to the live dialog, and approving it there switches claude into accept-edits
// natively, with nothing killed or resumed.
func applyPermissionRequest(ms *Mission, ev HookEvent) Status {
	ms.AgentState = AgentWaiting

	if ev.IsPlanApproval() {
		ms.PlanPending = true
		ms.WaitingFor = "plan debrief"

		return StatusDebrief
	}

	ms.WaitingFor = describeTool(ev)

	return StatusAwaiting
}

// applyPermissionDenied handles a refused permission.
//
// Only a rejected plan needs a human. Ordinary denials come from the user's own
// PreToolUse guard hooks, which deny programmatically and which the agent handles
// itself; treating those as blocked would park cards in "awaiting orders" for events
// that need no attention at all.
func applyPermissionDenied(ms *Mission, ev HookEvent) Status {
	ms.AgentState = AgentBusy

	if ev.IsPlanApproval() {
		ms.PlanPending = false
		ms.WaitingFor = "plan rejected, needs direction"

		return StatusAwaiting
	}

	return ""
}

// applyNotification handles claude's multiplexed notification event.
func applyNotification(ms *Mission, ev HookEvent) Status {
	switch ev.NotificationType {
	case NotificationPermissionPrompt,
		NotificationAgentNeedsInput,
		NotificationWorkerPermissionPrompt:
		// A six-second-late confirmation of something PermissionRequest normally
		// reported already, so it only promotes a card that still looks busy.
		if ms.Status != StatusActive {
			return ""
		}

		ms.AgentState = AgentWaiting

		if ms.WaitingFor == "" {
			ms.WaitingFor = firstLine(ev.Message)
		}

		return StatusAwaiting
	case NotificationIdlePrompt:
		// Sixty seconds of quiet is not the same as being blocked or finished, so
		// this is a badge and nothing more.
		ms.Badges = ms.WithBadge(BadgeStale, "idle")

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
func applyStop(ms *Mission, ev HookEvent, now time.Time) Status {
	ms.Badges = ms.WithoutBadge(BadgeStale)

	question := closingQuestion(ev.LastAssistantMessage)

	if ev.LastAssistantMessage != "" {
		// The ask makes the better subtitle when there is one: the message's first
		// line is a sentence from the middle of the agent's reasoning, and showing it
		// hides the thing being asked.
		summary := firstLine(ev.LastAssistantMessage)
		if question != "" {
			summary = question
		}

		ms.LastMessage = truncate(summary, maxLastMessage)
	}

	if ev.BackgroundTasks > 0 {
		ms.AgentState = AgentBusy
		ms.Badges = ms.WithBadge(BadgeBackground, strconv.Itoa(ev.BackgroundTasks))

		return StatusActive
	}

	ms.Badges = ms.WithoutBadge(BadgeBackground)

	if ms.PlanPending {
		ms.AgentState = AgentWaiting

		return ""
	}

	// FinishedAt stays unset: the agent is blocked at its prompt, and stamping a
	// finish time would have the card report an age for work that has not ended.
	//
	// An existing description is left in place, because a Stop racing a
	// PermissionRequest must not replace the live prompt's tool name with prose from
	// the turn that led up to it.
	if question != "" {
		ms.AgentState = AgentWaiting

		if ms.WaitingFor == "" {
			ms.WaitingFor = truncate(question, maxWaitingFor)
		}

		return StatusAwaiting
	}

	ms.AgentState = AgentIdle
	ms.WaitingFor = ""

	finished := now
	ms.FinishedAt = &finished

	return StatusDebrief
}

// applyStopFailure records a turn that ended in an API error.
//
// claude ignores this hook's output entirely, so it is purely a report. The card
// needs a human either way, since the work did not finish.
func applyStopFailure(ms *Mission, ev HookEvent) Status {
	ms.AgentState = AgentWaiting
	ms.Badges = ms.WithBadge(BadgeAPIError, ev.Reason)
	ms.WaitingFor = "API error"

	if ev.Reason != "" {
		ms.WaitingFor = "API error: " + ev.Reason
	}

	return StatusAwaiting
}

// describeTool renders what an agent is blocked on, e.g. "Bash".
func describeTool(ev HookEvent) string {
	if ev.ToolName != "" {
		return truncate(ev.ToolName, maxWaitingFor)
	}

	if ev.Message != "" {
		return truncate(firstLine(ev.Message), maxWaitingFor)
	}

	return "permission"
}

// equivalent reports whether two missions differ in any field the reducer touches.
func equivalent(a, b Mission) bool {
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
