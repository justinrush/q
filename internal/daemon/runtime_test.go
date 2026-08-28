package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// fakeRuntime stands in for an agent runtime, so the rules below are exercised
// without a live agent process.
type fakeRuntime struct {
	reading mission.Reading
	found   bool
	err     error
}

func (f fakeRuntime) Read(context.Context, mission.Mission) (mission.Reading, bool, error) {
	return f.reading, f.found, f.err
}

func (fakeRuntime) Close() error { return nil }

// waitingReading is the reading an agent runtime returns for a given activity.
func waitingReading(activity mission.Activity) mission.Reading {
	reading := mission.Reading{SessionID: "thr-1", Activity: activity}

	switch activity {
	case mission.ActivityWaitingApproval:
		reading.WaitingFor = "Codex approval"
	case mission.ActivityWaitingInput:
		reading.WaitingFor = "Codex needs input"
	case mission.ActivityFailed:
		reading.WaitingFor = "Codex system error"
	}

	return reading
}

func TestApplyRuntimeReading(t *testing.T) {
	tests := map[string]struct {
		startStatus  mission.Status
		startWaiting string
		activity     mission.Activity
		wantStatus   mission.Status
		wantAgent    mission.AgentState
		wantWaiting  string
		wantAPIError bool
	}{
		"busy clears a stale wait immediately": {
			startStatus:  mission.StatusAwaiting,
			startWaiting: "Bash",
			activity:     mission.ActivityBusy,
			wantStatus:   mission.StatusActive,
			wantAgent:    mission.AgentBusy,
		},
		"input waits": {
			startStatus: mission.StatusActive,
			activity:    mission.ActivityWaitingInput,
			wantStatus:  mission.StatusAwaiting,
			wantAgent:   mission.AgentWaiting,
			wantWaiting: "Codex needs input",
		},
		"idle turn moves to debrief": {
			startStatus: mission.StatusActive,
			activity:    mission.ActivityIdle,
			wantStatus:  mission.StatusDebrief,
			wantAgent:   mission.AgentIdle,
		},
		"idle preserves a closing question": {
			startStatus:  mission.StatusAwaiting,
			startWaiting: "Should I continue?",
			activity:     mission.ActivityIdle,
			wantStatus:   mission.StatusAwaiting,
			wantAgent:    mission.AgentWaiting,
			wantWaiting:  "Should I continue?",
		},
		"system error asks for attention": {
			startStatus:  mission.StatusActive,
			activity:     mission.ActivityFailed,
			wantStatus:   mission.StatusAwaiting,
			wantAgent:    mission.AgentWaiting,
			wantWaiting:  "Codex system error",
			wantAPIError: true,
		},
		"unknown status changes nothing": {
			startStatus: mission.StatusActive,
			activity:    mission.ActivityUnknown,
			wantStatus:  mission.StatusActive,
			wantAgent:   mission.AgentBusy,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			svc := newTestService(t)
			ms := seedCodexMission(t, svc, test.startStatus, test.startWaiting)

			svc.applyRuntimeReading(ms.ID, waitingReading(test.activity))

			got, ok := svc.Snapshot().Mission(ms.ID)
			if !ok {
				t.Fatal("mission disappeared")
			}

			if got.Status != test.wantStatus {
				t.Errorf("Status = %q, want %q", got.Status, test.wantStatus)
			}

			if got.AgentState != test.wantAgent {
				t.Errorf("AgentState = %q, want %q", got.AgentState, test.wantAgent)
			}

			if got.WaitingFor != test.wantWaiting {
				t.Errorf("WaitingFor = %q, want %q", got.WaitingFor, test.wantWaiting)
			}

			if got.HasBadge(mission.BadgeAPIError) != test.wantAPIError {
				t.Errorf("API error badge = %v, want %v", got.HasBadge(mission.BadgeAPIError), test.wantAPIError)
			}
		})
	}
}

func TestTransientApprovalDoesNotMoveMission(t *testing.T) {
	svc := newTestService(t)
	// The hook damper only applies where a runtime can later resolve the approval.
	svc.apply(WithRuntime(mission.ToolCodex, fakeRuntime{}))
	now := time.Date(2026, time.August, 19, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	ms := seedCodexMission(t, svc, mission.StatusActive, "")

	svc.applyReduction(ms.ID, mission.HookEvent{
		Event:     mission.EventPermissionRequest,
		SessionID: "thr-1",
		ToolName:  "Bash",
	})

	got, ok := svc.Snapshot().Mission(ms.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != mission.StatusActive {
		t.Errorf("Status = %q before grace, want active", got.Status)
	}

	now = now.Add(time.Second)
	svc.promoteMatureApprovals()
	svc.applyReduction(ms.ID, mission.HookEvent{
		Event:     mission.EventPreToolUse,
		SessionID: "thr-1",
		ToolName:  "Bash",
	})

	now = now.Add(2 * approvalGrace)
	svc.promoteMatureApprovals()

	got, ok = svc.Snapshot().Mission(ms.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != mission.StatusActive {
		t.Errorf("Status = %q after approval resolved, want active", got.Status)
	}
}

func TestSustainedHookApprovalMovesAfterGrace(t *testing.T) {
	svc := newTestService(t)
	// The hook damper only applies where a runtime can later resolve the approval.
	svc.apply(WithRuntime(mission.ToolCodex, fakeRuntime{}))
	now := time.Date(2026, time.August, 19, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	ms := seedCodexMission(t, svc, mission.StatusActive, "")

	svc.applyReduction(ms.ID, mission.HookEvent{
		Event:     mission.EventPermissionRequest,
		SessionID: "thr-1",
		ToolName:  "Bash",
	})

	now = now.Add(approvalGrace)
	svc.promoteMatureApprovals()

	got, ok := svc.Snapshot().Mission(ms.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != mission.StatusAwaiting {
		t.Errorf("Status = %q after grace, want waiting", got.Status)
	}

	if got.AgentState != mission.AgentWaiting {
		t.Errorf("AgentState = %q, want waiting", got.AgentState)
	}

	if got.WaitingFor != "Bash" {
		t.Errorf("WaitingFor = %q, want Bash", got.WaitingFor)
	}
}

func TestHookApprovalSurvivesRuntimeFailure(t *testing.T) {
	svc := newTestService(t)
	now := time.Date(2026, time.August, 19, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	ms := seedCodexMission(t, svc, mission.StatusActive, "")
	svc.apply(WithRuntime(mission.ToolCodex, fakeRuntime{err: errors.New("app-server unavailable")}))

	svc.applyReduction(ms.ID, mission.HookEvent{
		Event:     mission.EventPermissionRequest,
		SessionID: "thr-1",
		ToolName:  "Bash",
	})

	now = now.Add(approvalGrace)
	svc.pollRuntimes(t.Context())

	got, ok := svc.Snapshot().Mission(ms.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != mission.StatusAwaiting {
		t.Errorf("Status = %q after app-server failure, want waiting", got.Status)
	}

	if got.WaitingFor != "Bash" {
		t.Errorf("WaitingFor = %q, want Bash", got.WaitingFor)
	}
}

func TestRuntimeApprovalUsesGraceAndBusyClearsIt(t *testing.T) {
	svc := newTestService(t)
	now := time.Date(2026, time.August, 19, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	ms := seedCodexMission(t, svc, mission.StatusActive, "")

	svc.applyRuntimeReading(ms.ID, waitingReading(mission.ActivityWaitingApproval))

	got, ok := svc.Snapshot().Mission(ms.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != mission.StatusActive {
		t.Errorf("Status = %q before grace, want active", got.Status)
	}

	now = now.Add(approvalGrace)
	svc.promoteMatureApprovals()

	got, ok = svc.Snapshot().Mission(ms.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != mission.StatusAwaiting {
		t.Errorf("Status = %q after grace, want waiting", got.Status)
	}

	svc.applyRuntimeReading(ms.ID, waitingReading(mission.ActivityBusy))

	got, ok = svc.Snapshot().Mission(ms.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != mission.StatusActive {
		t.Errorf("Status = %q after busy, want active", got.Status)
	}
}

func TestPollRecoversSessionIDByMissionDirectory(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)
	ms, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "Codex mission",
		Prompt:      "do it",
		Tool:        mission.ToolCodex,
	})
	if err != nil {
		t.Fatalf("CreateMission() error = %v", err)
	}

	err = svc.store.Mutate("test.seed_codex_without_session", func(snap *mission.Snapshot) error {
		ms.Status = mission.StatusActive
		ms.AgentState = mission.AgentUnknown
		ms.MissionDir = "/missions/unique"
		snap.PutMission(ms)

		return nil
	})
	if err != nil {
		t.Fatalf("seeding mission: %v", err)
	}

	svc.apply(WithRuntime(mission.ToolCodex, fakeRuntime{
		reading: mission.Reading{SessionID: "thr-recovered", Activity: mission.ActivityBusy},
		found:   true,
	}))
	svc.pollRuntimes(t.Context())

	got, ok := svc.Snapshot().Mission(ms.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.AgentSessionID != "thr-recovered" {
		t.Errorf("AgentSessionID = %q", got.AgentSessionID)
	}

	if got.AgentState != mission.AgentBusy {
		t.Errorf("AgentState = %q, want busy", got.AgentState)
	}
}

func seedCodexMission(
	t *testing.T,
	svc *Service,
	status mission.Status,
	waitingFor string,
) mission.Mission {
	t.Helper()

	operation := seedOperation(t, svc)
	ms, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "Codex mission",
		Prompt:      "do it",
		Tool:        mission.ToolCodex,
	})
	if err != nil {
		t.Fatalf("CreateMission() error = %v", err)
	}

	err = svc.store.Mutate("test.seed_codex", func(snap *mission.Snapshot) error {
		ms.Status = status
		ms.AgentState = mission.AgentBusy
		ms.AgentSessionID = "thr-1"
		ms.WaitingFor = waitingFor
		if status == mission.StatusAwaiting {
			ms.AgentState = mission.AgentWaiting
		}
		snap.PutMission(ms)

		return nil
	})
	if err != nil {
		t.Fatalf("seeding mission: %v", err)
	}

	return ms
}
