package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// planMessenger records relaunches as they happen, including the ones q makes
// on its own from a hook.
type planMessenger struct {
	relaunched chan mission.Mission
	err        error
}

func newPlanMessenger() *planMessenger {
	return &planMessenger{relaunched: make(chan mission.Mission, 4)}
}

func (*planMessenger) SendMessage(context.Context, mission.Mission, string) error { return nil }

func (m *planMessenger) Relaunch(
	_ context.Context,
	_ mission.Operation,
	ms mission.Mission,
	_ string,
) (mission.Mission, error) {
	if m.err != nil {
		return ms, m.err
	}

	ms.HookEpoch++
	m.relaunched <- ms

	return ms, nil
}

func (*planMessenger) Stop(context.Context, mission.Mission) error { return nil }

func (*planMessenger) View(_ context.Context, _ mission.Operation, ms mission.Mission) (mission.Mission, error) {
	return ms, nil
}

// awaitRelaunch reports the mission a relaunch was asked for, or false if none
// came.
func (m *planMessenger) awaitRelaunch(wait time.Duration) (mission.Mission, bool) {
	timeout := time.NewTimer(wait)
	defer timeout.Stop()

	// Looked at first on its own, so a relaunch already recorded is never lost
	// to a timer that has also fired.
	select {
	case ms := <-m.relaunched:
		return ms, true
	default:
	}

	select {
	case ms := <-m.relaunched:
		return ms, true
	case <-timeout.C:
		return mission.Mission{}, false
	}
}

// seedPlanning stores a launched plan-mode mission.
func seedPlanning(t *testing.T, svc *Service, mutate func(*mission.Mission)) mission.Mission {
	t.Helper()

	operation := seedOperation(t, svc)

	ms, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "mission",
		Prompt:      "x",
		Tool:        mission.ToolClaude,
		Model:       "opus",
		Effort:      "low",
		PlanMode:    true,
		PlanModel:   "fable",
		PlanEffort:  "high",
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	started := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)

	if err := svcStore(svc).Mutate("test.planning", func(snap *mission.Snapshot) error {
		stored, _ := snap.Mission(ms.ID)
		stored.StartedAt = &started
		stored.Status = mission.StatusDebrief
		stored.AgentSessionID = "sess-1"
		stored.HookEpoch = 1
		stored.PlanPending = true

		if mutate != nil {
			mutate(&stored)
		}

		ms = stored
		snap.PutMission(stored)

		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	return ms
}

func TestCreateMissionPlanSettings(t *testing.T) {
	cases := []struct {
		name    string
		req     api.CreateMissionRequest
		wantErr error
	}{
		{name: "a planning model and effort", req: api.CreateMissionRequest{PlanMode: true, PlanModel: "fable", PlanEffort: "high"}},
		{name: "a planning model alone", req: api.CreateMissionRequest{PlanMode: true, PlanModel: "fable"}},
		{
			name:    "an effort with no model to belong to",
			req:     api.CreateMissionRequest{PlanMode: true, PlanEffort: "high"},
			wantErr: ErrInvalid,
		},
		{
			name:    "a model that could not go on a command line",
			req:     api.CreateMissionRequest{PlanMode: true, PlanModel: "--dangerous"},
			wantErr: ErrInvalid,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)

			tc.req.OperationID = seedOperation(t, svc).ID
			tc.req.Name = "mission"
			tc.req.Prompt = "x"

			ms, err := svc.CreateMission(tc.req)
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}

			if err == nil && (ms.PlanModel != tc.req.PlanModel || ms.PlanEffort != tc.req.PlanEffort) {
				t.Errorf("stored %q/%q, want %q/%q", ms.PlanModel, ms.PlanEffort, tc.req.PlanModel, tc.req.PlanEffort)
			}
		})
	}
}

func TestUpdateMissionPlanSettings(t *testing.T) {
	fable, high, none := "fable", "high", ""

	cases := []struct {
		name     string
		launched bool
		req      api.UpdateMissionRequest
		wantErr  error
	}{
		{name: "changed before launch", req: api.UpdateMissionRequest{PlanModel: &fable, PlanEffort: &high}},
		{name: "cleared before launch", req: api.UpdateMissionRequest{PlanModel: &none, PlanEffort: &none}},
		{
			name:    "the model cleared from under its effort",
			req:     api.UpdateMissionRequest{PlanModel: &none},
			wantErr: ErrInvalid,
		},
		{
			name:     "fixed once launched",
			launched: true,
			req:      api.UpdateMissionRequest{PlanModel: &fable},
			wantErr:  ErrConflict,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)

			ms := seedPlanning(t, svc, func(ms *mission.Mission) {
				ms.Status = mission.StatusBriefing

				if !tc.launched {
					ms.StartedAt = nil
				}
			})

			if _, err := svc.UpdateMission(ms.ID, tc.req); !errors.Is(err, tc.wantErr) {
				t.Errorf("err = %v, want %v", err, tc.wantErr)
			}
		})
	}
}

// Approving from q restarts the session, which is what moves it off its
// planning settings; the restart reads the mission, so approval has to be on it
// first.
func TestApprovePlan(t *testing.T) {
	relaunchFailed := errors.New("tmux is gone")

	cases := []struct {
		name         string
		mutate       func(*mission.Mission)
		relaunchErr  error
		wantErr      error
		wantRelaunch bool
		wantApproved bool
		wantPending  bool
	}{
		{
			name:         "a pending plan",
			wantRelaunch: true,
			wantApproved: true,
		},
		{
			name:         "a plan q never saw announced",
			mutate:       func(ms *mission.Mission) { ms.PlanPending = false },
			wantRelaunch: true,
			wantApproved: true,
		},
		{
			name:         "a mission that plans and implements on the same settings",
			mutate:       func(ms *mission.Mission) { ms.PlanModel, ms.PlanEffort = "", "" },
			wantRelaunch: true,
			wantApproved: true,
		},
		{
			name:    "an agent that changes model itself is approved in its own dialog",
			mutate:  func(ms *mission.Mission) { ms.Tool = mission.ToolOpencode },
			wantErr: ErrConflict,
			// Untouched, so still as seeded.
			wantPending: true,
		},
		{
			name:         "a plan already approved",
			mutate:       func(ms *mission.Mission) { ms.PlanApproved = true; ms.PlanPending = false },
			wantErr:      ErrConflict,
			wantApproved: true,
		},
		{
			name:    "a mission that never planned",
			mutate:  func(ms *mission.Mission) { ms.PlanMode = false; ms.PlanPending = false },
			wantErr: ErrConflict,
		},
		{
			name:    "a mission not yet launched",
			mutate:  func(ms *mission.Mission) { ms.StartedAt = nil },
			wantErr: ErrConflict,
			// Untouched, so still as seeded.
			wantPending: true,
		},
		{
			name:        "a restart that fails leaves the plan to be approved again",
			relaunchErr: relaunchFailed,
			wantErr:     relaunchFailed,
			wantPending: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			messenger := newPlanMessenger()
			messenger.err = tc.relaunchErr
			svc.apply(WithMessenger(messenger))

			ms := seedPlanning(t, svc, tc.mutate)

			_, err := svc.Dispatch(t.Context(), ms.ID,
				api.SetStatusRequest{To: mission.StatusActive, ApprovePlan: true})
			if !errors.Is(err, tc.wantErr) {
				t.Fatalf("err = %v, want %v", err, tc.wantErr)
			}

			relaunched, ok := messenger.awaitRelaunch(0)
			if ok != tc.wantRelaunch {
				t.Fatalf("relaunched = %v, want %v", ok, tc.wantRelaunch)
			}

			if ok && ms.Tool == mission.ToolCodex {
				waitVerified(t, svc, ms.ID)
			}
			if ok && relaunched.Planning() {
				t.Error("the session was restarted while the mission still read as planning")
			}

			stored, _ := svc.Snapshot().Mission(ms.ID)

			if stored.PlanApproved != tc.wantApproved {
				t.Errorf("PlanApproved = %v, want %v", stored.PlanApproved, tc.wantApproved)
			}

			if stored.PlanPending != tc.wantPending {
				t.Errorf("PlanPending = %v, want %v", stored.PlanPending, tc.wantPending)
			}

			if tc.wantRelaunch && stored.Status != mission.StatusActive {
				t.Errorf("Status = %s, want active", stored.Status)
			}
		})
	}
}

// A plan accepted in the agent's own UI leaves the agent implementing on its
// planning model, so q restarts it, but only when the two differ: otherwise the
// agent is already where the mission wants it and is left alone.
func TestPlanApprovedInTheAgentRestartsOnlyASplit(t *testing.T) {
	hook := func(tool mission.Tool, event string, payload map[string]any) api.HookRequest {
		payload["session_id"] = "sess-1"

		data, err := json.Marshal(payload)
		if err != nil {
			panic(err)
		}

		return api.HookRequest{Tool: tool, Event: event, Payload: data, HookEpoch: 1}
	}

	cases := []struct {
		name         string
		mutate       func(*mission.Mission)
		hook         api.HookRequest
		wantApproved bool
		wantRelaunch bool
	}{
		{
			name: "claude leaving plan mode on a split",
			hook: hook(mission.ToolClaude, mission.EventPostToolUse,
				map[string]any{"tool_name": mission.ExitPlanModeTool}),
			wantApproved: true,
			wantRelaunch: true,
		},
		{
			name:   "claude leaving plan mode with nothing to switch to",
			mutate: func(ms *mission.Mission) { ms.PlanModel, ms.PlanEffort = "", "" },
			hook: hook(mission.ToolClaude, mission.EventPostToolUse,
				map[string]any{"tool_name": mission.ExitPlanModeTool}),
			wantApproved: true,
		},
		{
			name:   "opencode leaving plan mode changes model itself",
			mutate: func(ms *mission.Mission) { ms.Tool = mission.ToolOpencode },
			hook: hook(mission.ToolOpencode, mission.EventPostToolUse,
				map[string]any{"tool_name": "plan_exit"}),
			wantApproved: true,
		},
		{
			name:   "codex told to implement on a split",
			mutate: func(ms *mission.Mission) { ms.Tool = mission.ToolCodex },
			hook: hook(mission.ToolCodex, mission.EventUserPromptSubmit,
				map[string]any{"prompt": "Implement the plan."}),
			wantApproved: true,
			wantRelaunch: true,
		},
		{
			name:   "codex given more to plan",
			mutate: func(ms *mission.Mission) { ms.Tool = mission.ToolCodex },
			hook: hook(mission.ToolCodex, mission.EventUserPromptSubmit,
				map[string]any{"prompt": "Also cover the migration."}),
		},
		{
			name: "an ordinary tool finishing",
			hook: hook(mission.ToolClaude, mission.EventPostToolUse, map[string]any{"tool_name": "Bash"}),
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			messenger := newPlanMessenger()
			svc.apply(WithMessenger(messenger))

			ms := seedPlanning(t, svc, tc.mutate)
			if ms.Tool == mission.ToolCodex {
				svc.apply(WithRuntime(mission.ToolCodex, &controlledRuntime{}))
			}

			tc.hook.MissionID = ms.ID
			svc.ApplyHook(tc.hook)

			wait := 50 * time.Millisecond
			if tc.wantRelaunch {
				wait = 2 * time.Second
			}

			relaunched, ok := messenger.awaitRelaunch(wait)
			if ok != tc.wantRelaunch {
				t.Fatalf("relaunched = %v, want %v", ok, tc.wantRelaunch)
			}

			if ok && ms.Tool == mission.ToolCodex {
				waitVerified(t, svc, ms.ID)
			}
			if ok && relaunched.Planning() {
				t.Error("the session was restarted while the mission still read as planning")
			}

			stored, _ := svc.Snapshot().Mission(ms.ID)
			if stored.PlanApproved != tc.wantApproved {
				t.Errorf("PlanApproved = %v, want %v", stored.PlanApproved, tc.wantApproved)
			}
		})
	}
}
