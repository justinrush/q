package mission

import (
	"fmt"
	"strings"
)

// Tool identifies which coding agent runs a mission.
type Tool string

// The supported agents.
const (
	ToolClaude Tool = "claude"
	ToolCodex  Tool = "codex"
	ToolGemini Tool = "gemini"
)

// DefaultTool is the agent a mission gets when none is named.
const DefaultTool = ToolClaude

// capabilities is what an agent can be asked to do.
//
// This is a property of the agent's identity rather than of the implementation
// in internal/claude, internal/codex, or internal/gemini, because clients need
// it without holding one: the board disables the plan toggle before any daemon
// is consulted, and the CLI validates --plan before a request is sent.
type capabilities struct {
	// glyph is the one-rune marker used on board cards.
	glyph string
	// planMode reports whether the agent can be launched into a plan-then-approve
	// flow.
	//
	// claude and gemini can: claude takes --permission-mode plan and raises an
	// ExitPlanMode permission request, gemini takes --approval-mode plan and
	// raises an exit_plan_mode confirmation, and q routes either to the debrief
	// lane. codex 0.147.0 exposes no equivalent — it has no --permission-mode
	// flag, and permission_mode does not appear in its binary at all — so the
	// board disables the plan toggle for codex missions rather than accepting a
	// flag it would silently ignore.
	planMode bool
	// presetSessionID reports whether q can choose the agent's session identifier
	// before launching it.
	//
	// claude accepts --session-id <uuid>, so its session is addressable for
	// --resume from the moment it starts. codex and gemini both generate an id
	// internally with no way to override it, so q must learn it from the
	// SessionStart hook instead; until that hook arrives, such a mission has no
	// resumable handle.
	presetSessionID bool
}

// known is every agent q recognizes. Adding one here and writing the matching
// [Agent] implementation is the whole of teaching q a new agent; nothing else
// branches on which tool a mission uses.
var known = map[Tool]capabilities{
	ToolClaude: {glyph: "◆", planMode: true, presetSessionID: true},
	ToolCodex:  {glyph: "◇"},
	ToolGemini: {glyph: "◈", planMode: true},
}

// Tools lists every supported agent, in the order the board cycles them.
var Tools = []Tool{ToolClaude, ToolCodex, ToolGemini}

// Valid reports whether t is a supported agent.
func (t Tool) Valid() bool { _, ok := known[t]; return ok }

// String implements fmt.Stringer.
func (t Tool) String() string { return string(t) }

// Glyph is a one-rune marker used on board cards.
func (t Tool) Glyph() string { return known[t].glyph }

// SupportsPlanMode reports whether the agent can be launched into a
// plan-then-approve flow.
func (t Tool) SupportsPlanMode() bool { return known[t].planMode }

// SupportsPresetSessionID reports whether q can choose the agent's session
// identifier before launching it.
func (t Tool) SupportsPresetSessionID() bool { return known[t].presetSessionID }

// Next returns the agent after t, wrapping around. The board's tool toggle uses
// it so a new agent joins the rotation without touching the TUI.
func (t Tool) Next() Tool {
	for i, tool := range Tools {
		if tool == t {
			return Tools[(i+1)%len(Tools)]
		}
	}

	return DefaultTool
}

// ParseTool converts a user-supplied string to a Tool.
func ParseTool(v string) (Tool, error) {
	if t := Tool(v); t.Valid() {
		return t, nil
	}

	return "", fmt.Errorf("unknown tool %q (want %s)", v, ToolList())
}

// ToolList renders the supported agents for a message, e.g. "claude, codex, or
// gemini". Deriving it means a new agent updates every error and help string
// that names the set.
func ToolList() string {
	names := make([]string, 0, len(Tools))
	for _, tool := range Tools {
		names = append(names, string(tool))
	}

	switch len(names) {
	case 0:
		return ""
	case 1:
		return names[0]
	default:
		return strings.Join(names[:len(names)-1], ", ") + " or " + names[len(names)-1]
	}
}
