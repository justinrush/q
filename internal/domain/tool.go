package domain

import "fmt"

// Tool identifies which coding agent runs a mission.
type Tool string

// The supported agents.
const (
	ToolClaude Tool = "claude"
	ToolCodex  Tool = "codex"
)

// Tools lists every supported agent.
var Tools = []Tool{ToolClaude, ToolCodex}

// Valid reports whether t is a supported agent.
func (t Tool) Valid() bool { return t == ToolClaude || t == ToolCodex }

// String implements fmt.Stringer.
func (t Tool) String() string { return string(t) }

// Glyph is a one-rune marker used on board cards.
func (t Tool) Glyph() string {
	if t == ToolCodex {
		return "◇"
	}

	return "◆"
}

// SupportsPlanMode reports whether the agent can be launched into a
// plan-then-approve flow.
//
// Only claude can: it takes --permission-mode plan and raises an ExitPlanMode
// permission request that q routes to the debrief lane. codex 0.147.0
// exposes no equivalent — it has no --permission-mode flag, and permission_mode
// does not appear in its binary at all — so the board disables the plan toggle
// for codex missions rather than accepting a flag it would silently ignore.
func (t Tool) SupportsPlanMode() bool { return t == ToolClaude }

// SupportsPresetSessionID reports whether q can choose the agent's session
// identifier before launching it.
//
// claude accepts --session-id <uuid>, so its session is addressable for --resume
// from the moment it starts. codex generates a UUIDv7 internally with no way to
// override it, so q must learn the id from the SessionStart hook instead;
// until that hook arrives, a codex mission has no resumable handle.
func (t Tool) SupportsPresetSessionID() bool { return t == ToolClaude }

// ParseTool converts a user-supplied string to a Tool.
func ParseTool(v string) (Tool, error) {
	if t := Tool(v); t.Valid() {
		return t, nil
	}

	return "", fmt.Errorf("unknown tool %q (want claude or codex)", v)
}
