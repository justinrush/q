package mission

// ToolHandoff is the portable, immutable context for a cross-tool continuation.
// Session identifiers and transcript paths belong to the host, not this bundle.
type ToolHandoff struct {
	ImplementationModel string `json:"implementationModel,omitempty"`
	ResumeState         string `json:"resumeState,omitempty"`
	TurnSubmitted       bool   `json:"turnSubmitted,omitempty"`
	State               string `json:"state,omitempty"`
	Plan                string `json:"plan,omitempty"`
	Conversation        string `json:"conversation,omitempty"`
	Error               string `json:"error,omitempty"`
}

func (m Mission) PlanningTool() Tool {
	if m.PlanTool != "" {
		return m.PlanTool
	}
	return m.Tool
}

func (m Mission) CrossToolPlan() bool {
	return m.PlanMode && m.PlanningTool() != m.Tool
}

// RunningTool remains the planner after approval until the handoff switches it.
func (m Mission) RunningTool() Tool {
	if m.ActiveTool != "" {
		return m.ActiveTool
	}
	if m.CrossToolPlan() {
		return m.PlanningTool()
	}
	return m.Tool
}

func (m Mission) ToolHandoffPending() bool {
	return m.CrossToolPlan() && m.PlanApproved && m.ToolHandoff.State != ToolHandoffVerified
}

func (m Mission) HandoffPrompt() string {
	if m.ToolHandoff.Plan == "" {
		return ""
	}
	return "\n\nThe user approved the following plan in " + string(m.PlanningTool()) +
		". Read .q/prompt.md for the original mission brief and instructions. Implement it using the existing worktrees. Inspect existing changes before continuing; the planning agent may have begun work. " +
		"Read .q/planning-conversation.md for the planning discussion and clarifications.\n\n" + m.ToolHandoff.Plan
}

const (
	ToolHandoffPending   = "pending"
	ToolHandoffPrepared  = "prepared"
	ToolHandoffStarting  = "starting"
	ToolHandoffVerifying = "verifying"
	ToolHandoffVerified  = "verified"
	ToolHandoffFailed    = "failed"
)

// A peer may complete the handoff while this host retains the old planner's
// resumable identity. It must never resume that identity in the destination tool.
func (m *Mission) archivePlanningSession(before Mission, host HostID) {
	m.PlanningSessionID = before.AgentSessionID
	m.PlanningTranscriptPath = before.TranscriptPath
	m.PriorToolUsage = before.UsageByHost[host].Clone()
	m.AgentSessionID = ""
	m.TranscriptPath = ""
	m.CodexUsage = nil
	m.ObservedSettings = TurnSettings{}
	m.PlanTransition = PlanTransition{}
	m.HookEpoch++
}
