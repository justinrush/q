package daemon

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/codexapp"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/hookspec"
	"github.com/justinrush/q/internal/state"
)

type fakeCodexStatusReader struct {
	operation codexapp.ThreadSnapshot
	err       error
}

func (f fakeCodexStatusReader) ReadThread(
	_ context.Context,
	_ string,
) (codexapp.ThreadStatus, error) {
	return f.operation.Status, f.err
}

func (f fakeCodexStatusReader) FindThread(
	_ context.Context,
	_ string,
) (codexapp.ThreadSnapshot, bool, error) {
	return f.operation, f.operation.ID != "", f.err
}

func TestApplyCodexStatus(t *testing.T) {
	tests := map[string]struct {
		startStatus  domain.Status
		startWaiting string
		activity     codexapp.Activity
		wantStatus   domain.Status
		wantAgent    domain.AgentState
		wantWaiting  string
		wantAPIError bool
	}{
		"busy clears a stale wait immediately": {
			startStatus:  domain.StatusAwaiting,
			startWaiting: "Bash",
			activity:     codexapp.ActivityBusy,
			wantStatus:   domain.StatusActive,
			wantAgent:    domain.AgentBusy,
		},
		"input waits": {
			startStatus: domain.StatusActive,
			activity:    codexapp.ActivityWaitingInput,
			wantStatus:  domain.StatusAwaiting,
			wantAgent:   domain.AgentWaiting,
			wantWaiting: "Codex needs input",
		},
		"idle turn moves to debrief": {
			startStatus: domain.StatusActive,
			activity:    codexapp.ActivityIdle,
			wantStatus:  domain.StatusDebrief,
			wantAgent:   domain.AgentIdle,
		},
		"idle preserves a closing question": {
			startStatus:  domain.StatusAwaiting,
			startWaiting: "Should I continue?",
			activity:     codexapp.ActivityIdle,
			wantStatus:   domain.StatusAwaiting,
			wantAgent:    domain.AgentWaiting,
			wantWaiting:  "Should I continue?",
		},
		"system error asks for attention": {
			startStatus:  domain.StatusActive,
			activity:     codexapp.ActivityFailed,
			wantStatus:   domain.StatusAwaiting,
			wantAgent:    domain.AgentWaiting,
			wantWaiting:  "Codex system error",
			wantAPIError: true,
		},
		"unknown status changes nothing": {
			startStatus: domain.StatusActive,
			activity:    codexapp.ActivityUnknown,
			wantStatus:  domain.StatusActive,
			wantAgent:   domain.AgentBusy,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			svc := newTestService(t)
			mission := seedCodexMission(t, svc, test.startStatus, test.startWaiting)

			svc.applyCodexStatus(mission.ID, "thr-1", test.activity)

			got, ok := svc.Snapshot().Mission(mission.ID)
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

			if got.HasBadge(domain.BadgeAPIError) != test.wantAPIError {
				t.Errorf("API error badge = %v, want %v", got.HasBadge(domain.BadgeAPIError), test.wantAPIError)
			}
		})
	}
}

func TestTransientCodexApprovalDoesNotMoveMission(t *testing.T) {
	svc := newTestService(t)
	now := time.Date(2026, time.August, 19, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	mission := seedCodexMission(t, svc, domain.StatusActive, "")

	svc.applyReduction(mission.ID, hookspec.Payload{
		Event:     hookspec.EventPermissionRequest,
		SessionID: "thr-1",
		ToolName:  "Bash",
	})

	got, ok := svc.Snapshot().Mission(mission.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != domain.StatusActive {
		t.Errorf("Status = %q before grace, want active", got.Status)
	}

	now = now.Add(time.Second)
	svc.promoteMatureCodexApprovals()
	svc.applyReduction(mission.ID, hookspec.Payload{
		Event:     hookspec.EventPreToolUse,
		SessionID: "thr-1",
		ToolName:  "Bash",
	})

	now = now.Add(2 * codexApprovalGrace)
	svc.promoteMatureCodexApprovals()

	got, ok = svc.Snapshot().Mission(mission.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != domain.StatusActive {
		t.Errorf("Status = %q after approval resolved, want active", got.Status)
	}
}

func TestSustainedCodexHookApprovalMovesAfterGrace(t *testing.T) {
	svc := newTestService(t)
	now := time.Date(2026, time.August, 19, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	mission := seedCodexMission(t, svc, domain.StatusActive, "")

	svc.applyReduction(mission.ID, hookspec.Payload{
		Event:     hookspec.EventPermissionRequest,
		SessionID: "thr-1",
		ToolName:  "Bash",
	})

	now = now.Add(codexApprovalGrace)
	svc.promoteMatureCodexApprovals()

	got, ok := svc.Snapshot().Mission(mission.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != domain.StatusAwaiting {
		t.Errorf("Status = %q after grace, want waiting", got.Status)
	}

	if got.AgentState != domain.AgentWaiting {
		t.Errorf("AgentState = %q, want waiting", got.AgentState)
	}

	if got.WaitingFor != "Bash" {
		t.Errorf("WaitingFor = %q, want Bash", got.WaitingFor)
	}
}

func TestCodexHookApprovalSurvivesAppStatusFailure(t *testing.T) {
	svc := newTestService(t)
	now := time.Date(2026, time.August, 19, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	mission := seedCodexMission(t, svc, domain.StatusActive, "")
	svc.SetCodexStatusReader(fakeCodexStatusReader{err: errors.New("app-server unavailable")})

	svc.applyReduction(mission.ID, hookspec.Payload{
		Event:     hookspec.EventPermissionRequest,
		SessionID: "thr-1",
		ToolName:  "Bash",
	})

	now = now.Add(codexApprovalGrace)
	svc.pollCodex(t.Context())

	got, ok := svc.Snapshot().Mission(mission.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != domain.StatusAwaiting {
		t.Errorf("Status = %q after app-server failure, want waiting", got.Status)
	}

	if got.WaitingFor != "Bash" {
		t.Errorf("WaitingFor = %q, want Bash", got.WaitingFor)
	}
}

func TestCodexAppApprovalUsesGraceAndBusyClearsIt(t *testing.T) {
	svc := newTestService(t)
	now := time.Date(2026, time.August, 19, 10, 0, 0, 0, time.UTC)
	svc.now = func() time.Time { return now }
	mission := seedCodexMission(t, svc, domain.StatusActive, "")

	svc.applyCodexStatus(mission.ID, "thr-1", codexapp.ActivityWaitingApproval)

	got, ok := svc.Snapshot().Mission(mission.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != domain.StatusActive {
		t.Errorf("Status = %q before grace, want active", got.Status)
	}

	now = now.Add(codexApprovalGrace)
	svc.promoteMatureCodexApprovals()

	got, ok = svc.Snapshot().Mission(mission.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != domain.StatusAwaiting {
		t.Errorf("Status = %q after grace, want waiting", got.Status)
	}

	svc.applyCodexStatus(mission.ID, "thr-1", codexapp.ActivityBusy)

	got, ok = svc.Snapshot().Mission(mission.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.Status != domain.StatusActive {
		t.Errorf("Status = %q after busy, want active", got.Status)
	}
}

func TestPollCodexRecoversSessionIDByMissionDirectory(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)
	mission, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "Codex mission",
		Prompt:      "do it",
		Tool:        domain.ToolCodex,
	})
	if err != nil {
		t.Fatalf("CreateMission() error = %v", err)
	}

	err = svc.store.Mutate("test.seed_codex_without_session", func(snap *state.Snapshot) error {
		mission.Status = domain.StatusActive
		mission.AgentState = domain.AgentUnknown
		mission.MissionDir = "/missions/unique"
		snap.PutMission(mission)

		return nil
	})
	if err != nil {
		t.Fatalf("seeding mission: %v", err)
	}

	svc.SetCodexStatusReader(fakeCodexStatusReader{operation: codexapp.ThreadSnapshot{
		ID:     "thr-recovered",
		Status: codexapp.ThreadStatus{Type: "active"},
	}})
	svc.pollCodex(t.Context())

	got, ok := svc.Snapshot().Mission(mission.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if got.AgentSessionID != "thr-recovered" {
		t.Errorf("AgentSessionID = %q", got.AgentSessionID)
	}

	if got.AgentState != domain.AgentBusy {
		t.Errorf("AgentState = %q, want busy", got.AgentState)
	}
}

func seedCodexMission(
	t *testing.T,
	svc *Service,
	status domain.Status,
	waitingFor string,
) domain.Mission {
	t.Helper()

	operation := seedOperation(t, svc)
	mission, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "Codex mission",
		Prompt:      "do it",
		Tool:        domain.ToolCodex,
	})
	if err != nil {
		t.Fatalf("CreateMission() error = %v", err)
	}

	err = svc.store.Mutate("test.seed_codex", func(snap *state.Snapshot) error {
		mission.Status = status
		mission.AgentState = domain.AgentBusy
		mission.AgentSessionID = "thr-1"
		mission.WaitingFor = waitingFor
		if status == domain.StatusAwaiting {
			mission.AgentState = domain.AgentWaiting
		}
		snap.PutMission(mission)

		return nil
	})
	if err != nil {
		t.Fatalf("seeding mission: %v", err)
	}

	return mission
}
