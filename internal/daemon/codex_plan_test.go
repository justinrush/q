package daemon

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"

	"github.com/justinrush/q/internal/mission"
)

type controlledRuntime struct {
	mu         sync.Mutex
	stage      string
	starts     int
	interrupts int
	settings   mission.TurnSettings
}

func (*controlledRuntime) Close() error { return nil }
func (r *controlledRuntime) Read(_ context.Context, ms mission.Mission) (mission.Reading, bool, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	return mission.Reading{SessionID: ms.AgentSessionID, HookEpoch: ms.HookEpoch, Activity: mission.ActivityBusy, Settings: r.settings}, true, nil
}
func (r *controlledRuntime) Interrupt(context.Context, string) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.interrupts++
	if r.stage == "interrupt" {
		return errors.New("interrupt failed")
	}
	return nil
}
func (r *controlledRuntime) ConfigureImplementation(context.Context, mission.Mission) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	if r.stage == "configure" {
		return errors.New("configure failed")
	}
	return nil
}
func (r *controlledRuntime) StartImplementation(_ context.Context, ms mission.Mission, _ string) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.starts++
	if r.stage == "start" {
		return "", errors.New("start failed")
	}
	r.settings = mission.TurnSettings{SessionID: ms.AgentSessionID, TurnID: "implementation", Mode: "default", Model: ms.Model, Effort: ms.Effort, At: time.Now()}
	if r.stage == "verify" {
		r.settings.Model = "wrong-model"
	}
	return "implementation", nil
}

func setupControlledPlan(t *testing.T) (*Service, mission.Mission, *controlledRuntime, *planMessenger) {
	t.Helper()
	svc := newTestService(t)
	runtime := &controlledRuntime{}
	messenger := newPlanMessenger()
	svc.apply(WithMessenger(messenger), WithRuntime(mission.ToolCodex, runtime))
	ms := seedPlanning(t, svc, func(m *mission.Mission) {
		m.Tool = mission.ToolCodex
		m.Model = "gpt-6-sol"
		m.Effort = "low"
		m.PlanModel = "gpt-6-astra"
		m.PlanEffort = "low"
	})
	return svc, ms, runtime, messenger
}

func TestCodexApprovalVerifiesImplementation(t *testing.T) {
	svc, ms, runtime, _ := setupControlledPlan(t)
	got, err := svc.ApprovePlan(t.Context(), ms.ID)
	if err != nil {
		t.Fatal(err)
	}
	if !got.PlanApproved || got.PlanTransition.State != mission.PlanTransitionVerified || got.ObservedSettings.Model != "gpt-6-sol" {
		t.Fatalf("handoff: %+v", got)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 1 || runtime.interrupts != 1 {
		t.Fatalf("starts=%d interrupts=%d", runtime.starts, runtime.interrupts)
	}
}

func TestCodexHandoffFailuresRemainRetryable(t *testing.T) {
	for _, stage := range []string{"interrupt", "configure", "relaunch", "start", "verify"} {
		t.Run(stage, func(t *testing.T) {
			svc, ms, runtime, messenger := setupControlledPlan(t)
			runtime.stage = stage
			if stage == "relaunch" {
				messenger.err = errors.New("relaunch failed")
			}
			if _, err := svc.ApprovePlan(t.Context(), ms.ID); err == nil {
				t.Fatal("expected failure")
			}
			failed, _ := svc.Snapshot().Mission(ms.ID)
			if !failed.PlanApproved || !failed.PlanFailed() || failed.Status != mission.StatusAwaiting || failed.PlanTransition.Error == "" {
				t.Fatalf("failure not retained: %+v", failed)
			}
			// Stale and ordinary busy reports cannot erase the paused failure.
			svc.applyRuntimeReading(ms.ID, mission.Reading{SessionID: failed.AgentSessionID, HookEpoch: failed.HookEpoch, Activity: mission.ActivityBusy})
			failed, _ = svc.Snapshot().Mission(ms.ID)
			if failed.Status != mission.StatusAwaiting {
				t.Fatal("busy reading unpaused failure")
			}
			runtime.mu.Lock()
			runtime.stage = ""
			runtime.mu.Unlock()
			messenger.err = nil
			got, err := svc.Resume(t.Context(), ms.ID, "")
			if err != nil {
				t.Fatal(err)
			}
			if !got.PlanApproved || got.PlanTransition.State != mission.PlanTransitionVerified {
				t.Fatalf("retry=%+v", got)
			}
		})
	}
}

func TestCodexPendingHandoffSurvivesServiceRestart(t *testing.T) {
	svc, ms, runtime, messenger := setupControlledPlan(t)
	_, err := svc.changePlan(ms.ID, func(m *mission.Mission) { m.PlanApproved = true; pendingImplementation(m, svc.now()) })
	if err != nil {
		t.Fatal(err)
	}
	restarted := NewService(svc.store, svc.hub, svc.dirs, WithRuntime(mission.ToolCodex, runtime), WithMessenger(messenger))
	restarted.reconcileCodexPlan(ms.ID)
	waitVerified(t, restarted, ms.ID)
	// Repeated polls must not launch another continuation.
	restarted.reconcileCodexPlan(ms.ID)
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 1 {
		t.Fatalf("started %d times", runtime.starts)
	}
}

func waitVerified(t *testing.T, svc *Service, id mission.MissionID) {
	t.Helper()
	deadline := time.After(3 * time.Second)
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		got, _ := svc.Snapshot().Mission(id)
		if got.PlanTransition.State == mission.PlanTransitionVerified {
			return
		}
		if got.PlanFailed() {
			t.Fatal(got.PlanTransition.Error)
		}
		select {
		case <-deadline:
			t.Fatal("handoff did not finish")
		case <-ticker.C:
		}
	}
}

func TestCodexFreshEvidenceRejectsOldLaunch(t *testing.T) {
	now := time.Now()
	ms := mission.Mission{AgentSessionID: "s", HookEpoch: 2, PlanTransition: mission.PlanTransition{HookEpoch: 2, Since: now}, ObservedSettings: mission.TurnSettings{SessionID: "s", At: now.Add(-time.Second)}}
	if freshPlanEvidence(ms) {
		t.Fatal("old record verified launch")
	}
	ms.ObservedSettings.At = now
	if !freshPlanEvidence(ms) {
		t.Fatal("fresh record rejected")
	}
	ms.ObservedSettings.SessionID = "other"
	if freshPlanEvidence(ms) {
		t.Fatal("other thread verified launch")
	}
}

func TestCodexVerificationRecoveryDoesNotResubmit(t *testing.T) {
	svc, ms, runtime, _ := setupControlledPlan(t)
	_, err := svc.changePlan(ms.ID, func(m *mission.Mission) {
		m.PlanApproved = true
		m.BeginPlanVerification(svc.now())
		m.ObservedSettings = mission.TurnSettings{SessionID: m.AgentSessionID, Mode: "default", Model: m.Model, Effort: m.Effort, At: svc.now()}
	})
	if err != nil {
		t.Fatal(err)
	}
	svc.reconcileCodexPlan(ms.ID)
	got, _ := svc.Snapshot().Mission(ms.ID)
	if got.PlanTransition.State != mission.PlanTransitionVerified {
		t.Fatalf("state=%+v", got.PlanTransition)
	}
	runtime.mu.Lock()
	defer runtime.mu.Unlock()
	if runtime.starts != 0 {
		t.Fatal("resubmitted already running turn")
	}
}

func TestCodexPlanningMismatchAndTimeoutPause(t *testing.T) {
	for _, missing := range []bool{false, true} {
		t.Run(map[bool]string{false: "wrong effort", true: "missing evidence"}[missing], func(t *testing.T) {
			svc, ms, _, _ := setupControlledPlan(t)
			_, err := svc.changePlan(ms.ID, func(m *mission.Mission) {
				m.BeginPlanVerification(svc.now().Add(-time.Minute))
				if !missing {
					m.ObservedSettings = mission.TurnSettings{SessionID: m.AgentSessionID, Mode: "plan", Model: m.PlanModel, Effort: "medium", At: svc.now()}
				}
			})
			if err != nil {
				t.Fatal(err)
			}
			svc.reconcileCodexPlan(ms.ID)
			deadline := time.After(time.Second)
			for {
				got, _ := svc.Snapshot().Mission(ms.ID)
				if got.PlanFailed() {
					if got.PlanApproved {
						t.Fatal("verification failure approved the plan")
					}
					// Wait until the failure worker releases its reservation before cleanup.
					if svc.planFlights.claim(ms.ID) {
						svc.planFlights.release(ms.ID)
						break
					}
				}
				select {
				case <-deadline:
					t.Fatal("verification did not pause")
				case <-time.After(time.Millisecond):
				}
			}
		})
	}
}

func TestCodexDuplicateApprovalIsSerialized(t *testing.T) {
	svc, ms, _, _ := setupControlledPlan(t)
	if !svc.planFlights.claim(ms.ID) {
		t.Fatal("claim")
	}
	if _, err := svc.ApprovePlan(t.Context(), ms.ID); !errors.Is(err, ErrConflict) {
		t.Fatalf("err=%v", err)
	}
	got, _ := svc.Snapshot().Mission(ms.ID)
	if got.PlanApproved {
		t.Fatal("duplicate approval changed state")
	}
	svc.planFlights.release(ms.ID)
	if _, err := svc.ApprovePlan(t.Context(), ms.ID); err != nil {
		t.Fatal(err)
	}
}
