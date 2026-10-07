package daemon

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/terminal"
)

// peerHost is the other installation in tests that pair two.
const peerHost mission.HostID = "h_bbbbbbbbbbbb"

// goneProbe reports that no tmux session exists, which is exactly what this
// host sees for a mission its peer is running.
type goneProbe struct{}

func (goneProbe) HasSession(context.Context, string) bool { return false }

func (goneProbe) ListPanes(context.Context, terminal.Target) ([]terminal.PaneInfo, error) {
	return nil, nil
}

func (goneProbe) CapturePane(context.Context, terminal.Target, int) (string, error) {
	return "", nil
}

// peerRunning stores an active mission whose lease the peer holds, as a sync
// exchange would have left it.
func peerRunning(t *testing.T, svc *Service) mission.Mission {
	t.Helper()

	ms := launchedServiceMission(t, svc)
	ms.Status = mission.StatusActive
	ms.AgentState = mission.AgentBusy
	ms.TmuxSession = "q-mission"
	ms.AgentPaneID = "%1"
	ms.LastEventAt = time.Now()
	ms.Lease = mission.Lease{Holder: peerHost, Epoch: 2}

	if err := svcStore(svc).Apply("test.peer_running", func(snap *mission.Snapshot) error {
		snap.PutMission(ms)

		return nil
	}); err != nil {
		t.Fatalf("storing the peer's mission: %v", err)
	}

	return ms
}

// A mission the peer runs has no session here. Reconciliation must not read
// that as the agent having died, or every mission would be filed for debrief
// fifteen seconds after the other machine started it.
func TestReconcileLeavesMissionsThePeerRunsAlone(t *testing.T) {
	svc := newTestService(t)
	ms := peerRunning(t, svc)

	svc.apply(WithProbe(goneProbe{}))
	svc.Reconcile(t.Context())

	stored, _ := svc.Snapshot().Mission(ms.ID)

	if stored.Status != mission.StatusActive || stored.HasBadge(mission.BadgeTmuxGone) {
		t.Errorf("status %q badges %v, want the peer's mission untouched", stored.Status, stored.Badges)
	}
}

// The same mission held here is reconciled as before, so the guard is the only
// thing that changed.
func TestReconcileStillFilesAMissionThisHostRuns(t *testing.T) {
	svc := newTestService(t)
	ms := peerRunning(t, svc)

	ms.Lease = mission.Lease{Holder: svc.self, Epoch: 3}

	if err := svcStore(svc).Apply("test.take", func(snap *mission.Snapshot) error {
		snap.PutMission(ms)

		return nil
	}); err != nil {
		t.Fatalf("taking the lease: %v", err)
	}

	svc.apply(WithProbe(goneProbe{}))
	svc.Reconcile(t.Context())

	if stored, _ := svc.Snapshot().Mission(ms.ID); stored.Status != mission.StatusDebrief {
		t.Errorf("status = %q, want debrief once the session is gone", stored.Status)
	}
}

// An agent that woke up with a sleeping laptop keeps sending hooks for a
// mission the other machine has since taken. They must not move the card.
func TestHookForAMissionThePeerRunsIsIgnored(t *testing.T) {
	svc := newTestService(t)
	ms := peerRunning(t, svc)

	payload, err := json.Marshal(map[string]string{
		"hook_event_name": mission.EventStop,
		"session_id":      "s1",
	})
	if err != nil {
		t.Fatal(err)
	}

	svc.ApplyHook(api.HookRequest{
		Tool:      mission.ToolClaude,
		Event:     mission.EventStop,
		MissionID: ms.ID,
		Payload:   payload,
	})

	if stored, _ := svc.Snapshot().Mission(ms.ID); stored.Status != mission.StatusActive {
		t.Errorf("status = %q, want the stale hook ignored", stored.Status)
	}
}
