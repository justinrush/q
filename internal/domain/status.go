package domain

import "fmt"

// Status is the board lane a mission occupies.
//
// Status is what the human means, and it is deliberately separate from
// [AgentState], which is what the agent process is observably doing. Conflating
// the two produces a board that lies: an agent can be mid-thought (busy) while
// the card correctly sits in "debrief" because the human has not looked yet.
type Status string

// The board lanes, in display order.
const (
	// StatusBriefing is a mission that has been written but not sent into the
	// field.
	StatusBriefing Status = "briefing"
	// StatusActive is a mission whose agent is running.
	StatusActive Status = "active"
	// StatusAwaiting is a mission whose agent is blocked on the human.
	StatusAwaiting Status = "awaiting"
	// StatusDebrief is a mission whose agent has finished a turn, or is waiting
	// for plan approval, and needs the human to look.
	StatusDebrief Status = "debrief"
	// StatusClosed is a mission the human has filed. It is terminal.
	StatusClosed Status = "closed"
)

// Lanes lists every status in board order.
var Lanes = []Status{StatusBriefing, StatusActive, StatusAwaiting, StatusDebrief, StatusClosed}

// statusLabels are the human-facing lane names.
var statusLabels = map[Status]string{
	StatusBriefing: "briefing",
	StatusActive:   "active",
	StatusAwaiting: "awaiting orders",
	StatusDebrief:  "debrief",
	StatusClosed:   "closed",
}

// statusPrecedence resolves competing transitions inside the reducer's
// coalescing window.
//
// Hooks for a single logical moment arrive as separate OS processes whose
// scheduling order is arbitrary: a Stop can land microseconds after the
// PermissionRequest that should outrank it. Rather than trusting arrival order
// or hook-process clocks, the reducer keeps the highest-precedence lane seen in
// the window. "Waiting on me" wins because a card that wrongly says the agent
// is done is worse than one that wrongly asks for attention.
var statusPrecedence = map[Status]int{
	StatusBriefing: 0,
	StatusActive:   1,
	StatusDebrief:  2,
	StatusAwaiting: 3,
	StatusClosed:   4,
}

// Label returns the human-facing lane name, e.g. "awaiting orders".
func (s Status) Label() string {
	if l, ok := statusLabels[s]; ok {
		return l
	}

	return string(s)
}

// Valid reports whether s is a known lane.
func (s Status) Valid() bool {
	_, ok := statusLabels[s]

	return ok
}

// Terminal reports whether s is a lane no agent event may move a mission out of.
// Only the human files a mission as done, and only the human takes it back.
func (s Status) Terminal() bool { return s == StatusClosed }

// Precedence returns the coalescing weight described on [statusPrecedence].
func (s Status) Precedence() int { return statusPrecedence[s] }

// String implements fmt.Stringer using the wire value.
func (s Status) String() string { return string(s) }

// ParseStatus accepts either the wire value ("awaiting") or the human label
// ("awaiting orders"), so CLI flags can take whichever the user types.
func ParseStatus(v string) (Status, error) {
	if s := Status(v); s.Valid() {
		return s, nil
	}

	for status, label := range statusLabels {
		if label == v {
			return status, nil
		}
	}

	return "", fmt.Errorf("unknown status %q (want one of briefing, active, awaiting, debrief, closed)", v)
}

// AgentState is the observed state of an agent process, as distinct from the
// lane its card sits in.
//
// It is never rendered as a lane. It drives badges and lets the reconciler tell
// "this card is stale because the agent died" apart from "this card is waiting
// because the agent asked a question".
type AgentState string

// The observable agent states.
const (
	// AgentUnknown means q has not yet heard from the agent. A freshly
	// launched mission sits here until its SessionStart hook arrives.
	AgentUnknown AgentState = "unknown"
	// AgentBusy means the agent is working.
	AgentBusy AgentState = "busy"
	// AgentWaiting means the agent is blocked on input.
	AgentWaiting AgentState = "waiting"
	// AgentIdle means the agent finished a turn and is awaiting a prompt.
	AgentIdle AgentState = "idle"
	// AgentDead means the process or its tmux pane is gone.
	AgentDead AgentState = "dead"
)

// Valid reports whether a is a known agent state.
func (a AgentState) Valid() bool {
	switch a {
	case AgentUnknown, AgentBusy, AgentWaiting, AgentIdle, AgentDead:
		return true
	default:
		return false
	}
}

// String implements fmt.Stringer.
func (a AgentState) String() string { return string(a) }
