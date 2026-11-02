package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

func TestCrossToolLaunchUsesPlannerAndDestinationSeparately(t *testing.T) {
	launcher, _, _ := newTestLauncher(t)
	ms := launchedMission(t, t.TempDir())
	ms.Tool = mission.ToolCodex
	ms.PlanTool = mission.ToolClaude
	ms.PlanMode = true
	ms.Model = "gpt-6-astra"
	ms.Effort = "low"
	ms.PlanModel = "fable"
	ms.PlanEffort = "medium"
	inv, err := launcher.invocation(ms, false)
	if err != nil {
		t.Fatal(err)
	}
	planner, err := launcher.agentFor(ms.RunningTool())
	if err != nil {
		t.Fatal(err)
	}
	argv := strings.Join(planner.Args(inv), " ")
	if !strings.Contains(argv, "'fable'") || strings.Contains(argv, "gpt-6-astra") || inv.ImplementModel != "" {
		t.Fatalf("planner invocation: %s %+v", argv, inv)
	}
	ms.PlanApproved = true
	ms.ActiveTool = ms.Tool
	ms.AgentSessionID = ""
	ms.ToolHandoff = mission.ToolHandoff{State: mission.ToolHandoffPrepared, Plan: "approved plan", Conversation: "user clarification"}
	inv, err = launcher.invocation(ms, false)
	if err != nil {
		t.Fatal(err)
	}
	if inv.PlanMode || !inv.ControlledTurn || inv.Resume || inv.Model != "gpt-6-astra" {
		t.Fatalf("destination invocation: %+v", inv)
	}
	if err := launcher.writeRelaunchArtifacts(testOperation("/dev/weave"), ms, ms.HandoffPrompt(), false); err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(filepath.Join(ms.MissionDir, mission.ArtifactDir, "planning-conversation.md"))
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "user clarification" {
		t.Fatalf("conversation %s", data)
	}
	if !strings.Contains(readArtifact(t, ms, "prompt.md"), "approved plan") {
		t.Fatal("missing approved plan")
	}
}

func TestCrossToolHandoffKeepsPersistedEpoch(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms := launchedMission(t, t.TempDir())
	ms.Tool = mission.ToolClaude
	ms.PlanTool = mission.ToolCodex
	ms.PlanMode = true
	ms.PlanApproved = true
	ms.ActiveTool = ms.Tool
	ms.AgentSessionID = ""
	ms.HookEpoch = 7
	ms.ToolHandoff.State = mission.ToolHandoffStarting
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "missing")
	got, err := launcher.Relaunch(t.Context(), testOperation("/dev/weave"), ms, "implement")
	if err != nil {
		t.Fatal(err)
	}
	if got.HookEpoch != 7 || got.AgentSessionID == "" {
		t.Fatalf("launch identity: %+v", got)
	}
}
