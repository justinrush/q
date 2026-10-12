package mission

import "fmt"

// Tool identifies which coding agent runs a mission.
type Tool string

// The supported agents.
const (
	ToolClaude   Tool = "claude"
	ToolCodex    Tool = "codex"
	ToolAgy      Tool = "agy"
	ToolOpencode Tool = "opencode"
)

// DefaultTool is the agent a mission gets when none is named.
const DefaultTool = ToolClaude

// capabilities is what an agent can be asked to do.
//
// This is a property of the agent's identity rather than of the implementation
// in internal/claude or internal/codex, because clients need it without holding
// one: the board disables the plan toggle before any daemon is consulted, and
// the CLI validates --plan before a request is sent.
type capabilities struct {
	// glyph is the one-rune marker used on board cards.
	glyph string
	// planMode reports whether the agent can be launched into a plan-then-approve
	// flow that q can follow: one where it can tell that a plan is ready, and
	// that it was accepted.
	//
	// agy is absent although its CLI has a plan mode, because q has no verified
	// event for either moment.
	planMode bool
	// planEndsTurn reports that the agent presents a plan by ending its turn,
	// and takes approval as an ordinary prompt, rather than raising a permission
	// request for the human to answer.
	//
	// codex works this way. Its plan mode is entered with a slash command typed
	// into its UI, a finished plan is a turn that ends without a closing
	// message, and accepting it submits a prompt codex writes itself.
	planEndsTurn bool
	// planSwitchesItself reports that the agent moves to its implementing model
	// by itself when its plan is accepted, so q neither needs to restart it nor
	// can usefully do so.
	//
	// opencode works this way: its plan and build agents each carry a model of
	// their own, and accepting a plan hands the session from one to the other.
	// A restart cannot do the same, because a resumed opencode session takes
	// its model from its last message whatever it is told on the command line.
	planSwitchesItself bool
	// presetSessionID reports whether q can choose the agent's session identifier
	// before launching it.
	//
	// claude accepts --session-id <uuid>, so its session is addressable for
	// --resume from the moment it starts. codex generates a UUIDv7 internally with
	// no way to override it, so q must learn the id from the SessionStart hook
	// instead; until that hook arrives, a codex mission has no resumable handle.
	presetSessionID bool
}

// known is every agent q recognizes. Adding one here and writing the matching
// [Agent] implementation is the whole of teaching q a new agent; nothing else
// branches on which tool a mission uses.
var known = map[Tool]capabilities{
	ToolClaude:   {glyph: "◆", planMode: true, presetSessionID: true},
	ToolCodex:    {glyph: "◇", planMode: true, planEndsTurn: true},
	ToolAgy:      {glyph: "△"},
	ToolOpencode: {glyph: "⬡", planMode: true, planSwitchesItself: true},
}

// Tools lists every supported agent, in the order the board cycles them.
var Tools = []Tool{ToolClaude, ToolCodex, ToolAgy, ToolOpencode}

// Valid reports whether t is a supported agent.
func (t Tool) Valid() bool { _, ok := known[t]; return ok }

// String implements fmt.Stringer.
func (t Tool) String() string { return string(t) }

// Glyph is a one-rune marker used on board cards.
func (t Tool) Glyph() string { return known[t].glyph }

// SupportsPlanMode reports whether the agent can be launched into a
// plan-then-approve flow.
func (t Tool) SupportsPlanMode() bool { return known[t].planMode }

// PlanEndsTurn reports whether the agent presents a plan by ending its turn
// rather than by asking permission to leave plan mode.
func (t Tool) PlanEndsTurn() bool { return known[t].planEndsTurn }

// PlanSwitchesItself reports whether the agent moves to its implementing model
// by itself when its plan is accepted.
func (t Tool) PlanSwitchesItself() bool { return known[t].planSwitchesItself }

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

	return "", fmt.Errorf("unknown tool %q (want claude, codex, agy, or opencode)", v)
}
