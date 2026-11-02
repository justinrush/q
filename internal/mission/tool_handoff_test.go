package mission

import (
	"encoding/json"
	"testing"
)

func TestCrossToolPhaseSettings(t *testing.T) {
	ms := Mission{Tool: ToolCodex, PlanTool: ToolClaude, PlanMode: true, Model: "gpt-6-astra", Effort: "low"}
	if ms.RunningTool() != ToolClaude || !ms.SplitsPlan() {
		t.Fatal("planner not selected")
	}
	model, effort := ms.PhaseModel()
	if model != "" || effort != "" {
		t.Fatal("inherited another tool's settings")
	}
	ms.PlanApproved = true
	if ms.RunningTool() != ToolClaude {
		t.Fatal("approval changed active session tool")
	}
	ms.ActiveTool = ToolCodex
	if ms.RunningTool() != ToolCodex {
		t.Fatal("destination not active")
	}
	model, effort = ms.PhaseModel()
	if model != "gpt-6-astra" || effort != "low" {
		t.Fatal("implementation settings lost")
	}
}

func TestHandoffSyncKeepsContextAndClearsLocalIdentity(t *testing.T) {
	ms := Mission{Tool: ToolCodex, PlanTool: ToolClaude, PlanMode: true, ActiveTool: ToolCodex,
		PlanningSessionID: "private-session", PlanningTranscriptPath: "/private/log", AgentSessionID: "destination",
		ToolHandoff: ToolHandoff{State: "verified", Plan: "approved", Conversation: "clarification"}}
	shared := ms.Shared()
	if shared.ToolHandoff != ms.ToolHandoff || shared.ActiveTool != ToolCodex {
		t.Fatal("handoff context lost")
	}
	if shared.PlanningSessionID != "" || shared.PlanningTranscriptPath != "" || shared.AgentSessionID != "" {
		t.Fatal("session identity leaked")
	}
	var old Mission
	if err := json.Unmarshal([]byte(`{"tool":"claude","planMode":true,"model":"fable"}`), &old); err != nil {
		t.Fatal(err)
	}
	if old.RunningTool() != ToolClaude || old.CrossToolPlan() {
		t.Fatal("legacy behavior changed")
	}
}

func TestPeerToolSwitchDiscardsLocalPlannerIdentity(t *testing.T) {
	local := Mission{ID: "mission", Tool: ToolCodex, PlanTool: ToolClaude, PlanMode: true, AgentSessionID: "claude-session", TranscriptPath: "planner.jsonl", Lease: Lease{Holder: "peer", Epoch: 1}, UsageByHost: map[HostID]Usage{"self": {USD: 2}}}
	snap := Snapshot{Self: HostInfo{ID: "self"}, Missions: []Mission{local}}
	incoming := local.Shared()
	incoming.ActiveTool = ToolCodex
	incoming.PlanApproved = true
	incoming.ToolHandoff.State = ToolHandoffVerified
	var changes Merge
	snap.mergeMission(incoming, false, &changes)
	got, _ := snap.Mission(local.ID)
	if got.AgentSessionID != "" || got.PlanningSessionID != "claude-session" || got.PriorToolUsage.USD != 2 || got.RunningTool() != ToolCodex {
		t.Fatalf("merged session: %+v", got)
	}
}
