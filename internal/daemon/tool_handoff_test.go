package daemon

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

type toolMessenger struct {
	*planMessenger
	svc       *Service
	calls     int
	preflight error
}

func (m *toolMessenger) PreflightTool(mission.Tool) error { return m.preflight }
func (m *toolMessenger) Relaunch(_ context.Context, _ mission.Operation, ms mission.Mission, _ string) (mission.Mission, error) {
	m.calls++
	if m.err != nil {
		return ms, m.err
	}
	if ms.AgentSessionID != "" {
		return ms, errors.New("source session passed to destination")
	}
	ms.AgentSessionID = "destination"
	ms.BeginPlanVerification(time.Now())
	// Exercise the SessionStart race before the launcher result is committed.
	m.svc.ApplyHook(api.HookRequest{MissionID: ms.ID, Tool: ms.Tool, HookEpoch: ms.HookEpoch, Event: mission.EventSessionStart, Payload: []byte(`{"session_id":"destination"}`)})
	return ms, nil
}

func setupToolPlan(t *testing.T, target mission.Tool) (*Service, mission.Mission, *toolMessenger, *controlledRuntime) {
	t.Helper()
	svc := newTestService(t)
	messenger := &toolMessenger{planMessenger: newPlanMessenger(), svc: svc}
	runtime := &controlledRuntime{}
	svc.apply(WithMessenger(messenger), WithRuntime(mission.ToolCodex, runtime))
	planner := mission.ToolClaude
	transcript := `{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"ExitPlanMode","input":{"plan":"Implement approved change"}}]}}` + "\n"
	if target == mission.ToolClaude {
		planner = mission.ToolCodex
		transcript = `{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<proposed_plan>Implement approved change</proposed_plan>"}]}}` + "\n"
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "transcript.jsonl")
	if err := os.WriteFile(path, []byte(transcript), 0600); err != nil {
		t.Fatal(err)
	}
	ms := seedPlanning(t, svc, func(m *mission.Mission) {
		m.Tool = target
		m.PlanTool = planner
		m.TranscriptPath = path
		m.MissionDir = dir
	})
	return svc, ms, messenger, runtime
}

func TestCrossToolApprovalBothDirections(t *testing.T) {
	for _, tool := range []mission.Tool{mission.ToolClaude, mission.ToolCodex} {
		t.Run(string(tool), func(t *testing.T) {
			svc, ms, messenger, runtime := setupToolPlan(t, tool)
			got, err := svc.ApprovePlan(t.Context(), ms.ID)
			if err != nil {
				t.Fatal(err)
			}
			if got.ToolHandoff.State != "verified" || got.RunningTool() != tool || got.AgentSessionID != "destination" || got.PlanningSessionID != "sess-1" {
				t.Fatalf("handoff: %+v", got)
			}
			if got.ToolHandoff.Plan != "Implement approved change" || got.ToolHandoff.Conversation == "" {
				t.Fatal("context lost")
			}
			if _, err := svc.ApprovePlan(t.Context(), ms.ID); err != nil {
				t.Fatal(err)
			}
			if messenger.calls != 1 {
				t.Fatalf("launched %d times", messenger.calls)
			}
			runtime.mu.Lock()
			defer runtime.mu.Unlock()
			if tool == mission.ToolCodex && runtime.starts != 1 {
				t.Fatalf("started %d turns", runtime.starts)
			}
			if tool == mission.ToolClaude && runtime.interrupts != 1 {
				t.Fatal("source Codex not interrupted")
			}
		})
	}
}

func TestCrossToolPreflightAndMissingPlanStayPaused(t *testing.T) {
	for _, stage := range []string{"preflight", "context"} {
		t.Run(stage, func(t *testing.T) {
			svc, ms, messenger, _ := setupToolPlan(t, mission.ToolCodex)
			if stage == "preflight" {
				messenger.preflight = errors.New("missing executable")
			} else {
				if err := os.Remove(ms.TranscriptPath); err != nil {
					t.Fatal(err)
				}
			}
			_, err := svc.ApprovePlan(t.Context(), ms.ID)
			if err == nil {
				t.Fatal("expected failure")
			}
			got, _ := svc.store.Snapshot().Mission(ms.ID)
			if got.ToolHandoff.State != "failed" || got.Status != mission.StatusAwaiting || messenger.calls != 0 {
				t.Fatalf("not paused: %+v", got)
			}
		})
	}
}

func TestCrossToolConfigurationValidation(t *testing.T) {
	svc := newTestService(t)
	op := seedOperation(t, svc)
	for _, tc := range []struct {
		planner, target mission.Tool
		plan, wantErr   bool
	}{
		{mission.ToolClaude, mission.ToolCodex, true, false}, {mission.ToolCodex, mission.ToolClaude, true, false},
		{mission.ToolOpencode, mission.ToolClaude, true, true}, {mission.ToolClaude, mission.ToolAgy, true, true},
		{mission.ToolClaude, mission.ToolCodex, false, true}, {"unknown", mission.ToolCodex, true, true},
	} {
		_, err := svc.CreateMission(api.CreateMissionRequest{OperationID: op.ID, Name: "test", Prompt: "test", Tool: tc.target, PlanTool: tc.planner, PlanMode: tc.plan})
		if (err != nil) != tc.wantErr {
			t.Fatalf("%+v: %v", tc, err)
		}
	}
}

func TestCrossToolNativeApprovalAndStaleSourceHooks(t *testing.T) {
	for _, target := range []mission.Tool{mission.ToolClaude, mission.ToolCodex} {
		t.Run(string(target), func(t *testing.T) {
			svc, ms, _, _ := setupToolPlan(t, target)
			event := mission.EventPostToolUse
			payload := []byte(`{"session_id":"sess-1","tool_name":"ExitPlanMode"}`)
			if ms.PlanningTool() == mission.ToolCodex {
				event = mission.EventUserPromptSubmit
				payload = []byte(`{"session_id":"sess-1","prompt":"Implement the plan."}`)
			}
			svc.ApplyHook(api.HookRequest{MissionID: ms.ID, Tool: ms.PlanningTool(), HookEpoch: ms.HookEpoch, Event: event, Payload: payload})
			deadline := time.Now().Add(3 * time.Second)
			for {
				got, _ := svc.store.Snapshot().Mission(ms.ID)
				if got.ToolHandoff.State == mission.ToolHandoffVerified {
					break
				}
				if got.ToolHandoff.State == mission.ToolHandoffFailed || time.Now().After(deadline) {
					t.Fatalf("handoff: %+v", got.ToolHandoff)
				}
				time.Sleep(10 * time.Millisecond)
			}
			svc.ApplyHook(api.HookRequest{MissionID: ms.ID, Tool: ms.PlanningTool(), HookEpoch: ms.HookEpoch, Event: mission.EventSessionStart, Payload: []byte(`{"session_id":"old-planner"}`)})
			got, _ := svc.store.Snapshot().Mission(ms.ID)
			if got.AgentSessionID != "destination" || got.RunningTool() != target {
				t.Fatal("stale source replaced destination")
			}
		})
	}
}

func TestCrossToolUsageSurvivesRepeatedMetering(t *testing.T) {
	svc, ms, _, _ := setupToolPlan(t, mission.ToolCodex)
	planner := &fakeMeter{measured: true, metering: mission.Metering{Usage: usageOf(2)}}
	implementer := &fakeMeter{measured: true, metering: mission.Metering{Usage: usageOf(3)}}
	svc.meters[mission.ToolClaude] = planner
	svc.meters[mission.ToolCodex] = implementer
	if _, err := svc.ApprovePlan(t.Context(), ms.ID); err != nil {
		t.Fatal(err)
	}
	svc.meterMission(ms.ID)
	svc.meterMission(ms.ID)
	got, _ := svc.store.Snapshot().Mission(ms.ID)
	if got.Usage.USD != 5 {
		t.Fatalf("usage = %v, want 5", got.Usage.USD)
	}
	seen := map[mission.Tool]float64{}
	for _, part := range got.Usage.Slices() {
		seen[part.Tool] += part.USD
	}
	if seen[mission.ToolClaude] != 2 || seen[mission.ToolCodex] != 3 {
		t.Fatalf("tool attribution: %v", seen)
	}
}

func TestCrossToolProtocolAndBoardProjection(t *testing.T) {
	ms := mission.Mission{Tool: mission.ToolCodex, PlanTool: mission.ToolClaude, PlanMode: true, ToolHandoff: mission.ToolHandoff{Plan: "plan", Conversation: "conversation"}}
	if exchangeProtocol(mission.Snapshot{Missions: []mission.Mission{ms}}) != api.SyncProtocolCrossTools {
		t.Fatal("sent cross-tool mission to legacy protocol")
	}
	if exchangeProtocol(mission.Snapshot{}) != api.SyncProtocol {
		t.Fatal("changed ordinary protocol")
	}
	board := boardPayload(ms).(mission.Mission)
	if board.ToolHandoff.Plan != "" || board.ToolHandoff.Conversation != "" || ms.ToolHandoff.Conversation == "" {
		t.Fatal("board projection modified durable context")
	}
}

func TestCrossToolPreparedHandoffSurvivesRestart(t *testing.T) {
	svc, ms, messenger, runtime := setupToolPlan(t, mission.ToolCodex)
	var err error
	ms, err = svc.changePlan(ms.ID, func(m *mission.Mission) { m.PlanApproved = true; m.PlanPending = false })
	if err != nil {
		t.Fatal(err)
	}
	ms, err = svc.prepareToolHandoff(t.Context(), ms)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(ms.PlanningTranscriptPath); err != nil {
		t.Fatal(err)
	}
	restarted := NewService(svc.store, svc.hub, svc.dirs, WithRuntime(mission.ToolCodex, runtime), WithMessenger(messenger))
	messenger.svc = restarted
	got, err := restarted.ApprovePlan(t.Context(), ms.ID)
	if err != nil {
		t.Fatal(err)
	}
	if got.ToolHandoff.State != mission.ToolHandoffVerified || messenger.calls != 1 {
		t.Fatalf("recovery: %+v", got.ToolHandoff)
	}
}

func TestCrossToolPreflightFailureCanRetryWithoutApprovalLoss(t *testing.T) {
	svc, ms, messenger, _ := setupToolPlan(t, mission.ToolCodex)
	messenger.preflight = errors.New("missing executable")
	if _, err := svc.ApprovePlan(t.Context(), ms.ID); err == nil {
		t.Fatal("expected failure")
	}
	got, _ := svc.store.Snapshot().Mission(ms.ID)
	if !got.PlanApproved {
		t.Fatal("approval was lost")
	}
	messenger.preflight = nil
	got, err := svc.Resume(t.Context(), ms.ID, "")
	if err != nil {
		t.Fatal(err)
	}
	if got.ToolHandoff.State != mission.ToolHandoffVerified || messenger.calls != 1 {
		t.Fatalf("retry: %+v", got.ToolHandoff)
	}
}

func TestCrossToolDisablePlanningClearsDependentSettings(t *testing.T) {
	svc := newTestService(t)
	op := seedOperation(t, svc)
	ms, err := svc.CreateMission(api.CreateMissionRequest{OperationID: op.ID, Name: "draft", Prompt: "test", Tool: mission.ToolCodex, PlanMode: true, PlanTool: mission.ToolClaude, PlanModel: "fable", PlanEffort: "medium"})
	if err != nil {
		t.Fatal(err)
	}
	off := false
	got, err := svc.UpdateMission(ms.ID, api.UpdateMissionRequest{PlanMode: &off})
	if err != nil {
		t.Fatal(err)
	}
	if got.PlanTool != "" || got.PlanModel != "" || got.PlanEffort != "" {
		t.Fatalf("planning settings retained: %+v", got)
	}
}

func TestCompletedToolHandoffUsesOrdinaryCodexRevival(t *testing.T) {
	svc, ms, runtime, _ := setupControlledPlan(t)
	_, err := svc.changePlan(ms.ID, func(m *mission.Mission) {
		m.PlanTool = mission.ToolClaude
		m.ActiveTool = mission.ToolCodex
		m.PlanApproved = true
		m.ToolHandoff.State = mission.ToolHandoffVerified
		pendingImplementation(m, svc.now())
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.implementPlan(ms.ID)
	got, _ := svc.store.Snapshot().Mission(ms.ID)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 1 || got.PlanTransition.State != mission.PlanTransitionVerified {
		t.Fatalf("revival stalled: starts=%d transition=%+v", runtime.starts, got.PlanTransition)
	}
}
