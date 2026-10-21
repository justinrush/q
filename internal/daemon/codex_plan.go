package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/justinrush/q/internal/mission"
)

const planVerificationTimeout = 45 * time.Second

func codexPlan(ms mission.Mission) bool { return ms.Tool == mission.ToolCodex && ms.PlanMode }

func pendingImplementation(ms *mission.Mission, now time.Time) {
	ms.PlanTransition = mission.PlanTransition{State: mission.PlanTransitionPending,
		Phase: mission.PhaseImplement, SessionID: ms.AgentSessionID, HookEpoch: ms.HookEpoch, Since: now}
}

// keepPlanFailure prevents late hooks and status polls from hiding a paused
// transition. Approval remains recorded, so retry does not ask for it again.
func keepPlanFailure(ms *mission.Mission) {
	if !ms.PlanFailed() {
		return
	}
	ms.Status = mission.StatusAwaiting
	ms.AgentState = mission.AgentWaiting
	ms.WaitingFor = "Codex handoff failed; retry with p or resume"
	ms.FinishedAt = nil
	ms.TurnEnded = true
}

func (s *Service) changePlan(id mission.MissionID, change func(*mission.Mission), expected ...mission.Mission) (mission.Mission, error) {
	var updated mission.Mission
	err := s.store.Mutate("mission.plan_transition", func(snap *mission.Snapshot) error {
		ms, ok := snap.Mission(id)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}
		if ms.Status.Terminal() || !s.holds(ms) {
			return fmt.Errorf("%w: mission is no longer active on this host", ErrConflict)
		}
		if len(expected) > 0 && (ms.HookEpoch != expected[0].HookEpoch || ms.AgentSessionID != expected[0].AgentSessionID) {
			return fmt.Errorf("%w: Codex session changed during handoff", ErrConflict)
		}
		before := ms.Status
		change(&ms)
		keepPlanFailure(&ms)
		if before != ms.Status {
			ms.Order = snap.NextOrder(ms.Status)
			ms.StatusChangedAt = s.now()
		}
		ms.UpdatedAt = s.now()
		updated = ms
		snap.PutMission(ms)
		return nil
	})
	if err == nil {
		s.publishMission(updated)
	}
	return updated, err
}

func (s *Service) failCodexPlan(ms mission.Mission, cause error) error {
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// Record failure first: late Stop/UserPromptSubmit events must not unpause it.
	current, exists := s.store.Snapshot().Mission(ms.ID)
	if !exists || current.HookEpoch != ms.HookEpoch || current.AgentSessionID != ms.AgentSessionID {
		return fmt.Errorf("%w: Codex session changed during handoff", ErrConflict)
	}
	_, err := s.changePlan(ms.ID, func(current *mission.Mission) {
		current.PlanTransition.State = mission.PlanTransitionFailed
		current.PlanTransition.Error = cause.Error()
	}, ms)
	if err != nil {
		return err
	}
	controller, ok := s.runtimes[mission.ToolCodex].(mission.PlanController)
	if ok && ms.AgentSessionID != "" {
		if stopErr := controller.Interrupt(ctx, ms.AgentSessionID); stopErr != nil {
			cause = fmt.Errorf("%w; could not interrupt Codex: %v", cause, stopErr)
		}
	}
	// Stop the terminal as well, so it cannot start another turn while failed.
	if s.messenger != nil {
		if stopErr := s.messenger.Stop(ctx, ms); stopErr != nil {
			cause = fmt.Errorf("%w; could not stop terminal: %v", cause, stopErr)
		}
	}
	_, _ = s.changePlan(ms.ID, func(current *mission.Mission) { current.PlanTransition.Error = cause.Error() }, ms)
	return cause
}

func expectedSettings(ms mission.Mission) (mode, model, effort string) {
	model, effort = ms.PhaseModel()
	mode = "plan"
	if ms.PlanApproved {
		mode = "default"
	}
	return
}

func settingsMismatch(ms mission.Mission) error {
	mode, model, effort := expectedSettings(ms)
	got := ms.ObservedSettings
	return fmt.Errorf("requested %s %s/%s; observed %s %s/%s", mode, model, effort, got.Mode, got.Model, got.Effort)
}

func settingsMatch(ms mission.Mission) bool {
	mode, model, effort := expectedSettings(ms)
	got := ms.ObservedSettings
	return got.Mode == mode && (model == "" || got.Model == model) && (effort == "" || got.Effort == effort)
}

func freshPlanEvidence(ms mission.Mission) bool {
	got, t := ms.ObservedSettings, ms.PlanTransition
	return got.SessionID != "" && got.SessionID == ms.AgentSessionID &&
		(t.HookEpoch == 0 || t.HookEpoch == ms.HookEpoch) && !got.At.IsZero() &&
		(t.Since.IsZero() || !got.At.Before(t.Since)) &&
		(t.TurnID == "" || got.TurnID == t.TurnID || t.State == mission.PlanTransitionVerified)
}

// reconcileCodexPlan runs after every observation, including unavailable ones.
// It repairs missed approval hooks and survives a restart between approval and
// the first verified implementation turn.
func (s *Service) reconcileCodexPlan(id mission.MissionID) {
	ms, ok := s.store.Snapshot().Mission(id)
	if !ok || !codexPlan(ms) || !ms.Running() || !s.holds(ms) || ms.PlanFailed() {
		return
	}
	t := ms.PlanTransition
	if t.State == mission.PlanTransitionPending && ms.AgentSessionID != "" {
		go s.implementPlan(id)
		return
	}
	if freshPlanEvidence(ms) {
		if settingsMatch(ms) {
			if t.State != mission.PlanTransitionVerified {
				_, _ = s.changePlan(id, func(m *mission.Mission) { m.PlanTransition.State = mission.PlanTransitionVerified })
			}
			return
		}
		// A saved approval from an older q has no durable handoff. Recover it once.
		if ms.PlanApproved && t.State == "" {
			_, _ = s.changePlan(id, func(m *mission.Mission) { pendingImplementation(m, s.now()) })
			go s.implementPlan(id)
			return
		}
		s.pauseCodexPlan(ms, settingsMismatch(ms))
		return
	}
	since := t.Since
	if since.IsZero() && ms.StartedAt != nil {
		since = *ms.StartedAt
	}
	if !since.IsZero() && s.now().Sub(since) > planVerificationTimeout {
		s.pauseCodexPlan(ms, fmt.Errorf("timed out verifying Codex mode/model/effort; managed-server controls and a readable current rollout are required"))
	}
}

func (s *Service) pauseCodexPlan(ms mission.Mission, cause error) {
	if !s.planFlights.claim(ms.ID) {
		return
	}
	go func() {
		defer s.planFlights.release(ms.ID)
		_ = s.failCodexPlan(ms, cause)
	}()
}

// approveCodexPlan also retries an already-approved, failed handoff.
func (s *Service) approveCodexPlan(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	if !ms.Planning() && !ms.PlanFailed() && ms.PlanTransition.State != mission.PlanTransitionPending {
		return mission.Mission{}, fmt.Errorf("%w: %s has no plan awaiting approval", ErrConflict, ms.Name)
	}
	if !s.planFlights.claim(ms.ID) {
		return mission.Mission{}, fmt.Errorf("%w: Codex handoff already in progress", ErrConflict)
	}
	defer s.planFlights.release(ms.ID)
	ms, err := s.changePlan(ms.ID, func(m *mission.Mission) {
		m.PlanApproved = true
		m.PlanPending = false
		pendingImplementation(m, s.now())
	})
	if err != nil {
		return ms, err
	}
	return s.runCodexHandoff(ctx, ms)
}

func (s *Service) runCodexHandoff(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	ctx, cancel := context.WithTimeout(ctx, planSwitchTimeout)
	defer cancel()
	controller, ok := s.runtimes[mission.ToolCodex].(mission.PlanController)
	fail := func(err error) (mission.Mission, error) { return mission.Mission{}, s.failCodexPlan(ms, err) }
	if !ok {
		return fail(fmt.Errorf("codex managed-server controls are unavailable; update Codex and retry"))
	}
	if err := controller.Interrupt(ctx, ms.AgentSessionID); err != nil {
		return fail(err)
	}
	relaunched, launchErr := s.relaunch(ctx, ms.ID, "") // ControlledTurn suppresses a positional prompt.
	if launchErr != nil {
		current, _ := s.store.Snapshot().Mission(ms.ID)
		if current.ID != "" {
			ms = current
		}
		return fail(launchErr)
	}
	ms = relaunched
	if ready, ok := s.messenger.(interface {
		ReadyForTurn(context.Context, mission.Mission) error
	}); ok {
		if err := ready.ReadyForTurn(ctx, ms); err != nil {
			return fail(err)
		}
	}
	if err := controller.ConfigureImplementation(ctx, ms); err != nil {
		return fail(err)
	}
	// Persist the boundary before sending turn/start. A lost response is recovered
	// from the rollout; it is never blindly submitted a second time.
	updated, err := s.changePlan(ms.ID, func(m *mission.Mission) {
		m.BeginPlanVerification(s.now())
	})
	if err != nil {
		return fail(err)
	}
	ms = updated
	message := ms.PlanTransition.Message
	if message == "" {
		message = planSwitchedMessage
	}
	turn, err := controller.StartImplementation(ctx, ms, message)
	if err != nil {
		return fail(err)
	}
	updated, err = s.changePlan(ms.ID, func(m *mission.Mission) { m.PlanTransition.TurnID = turn })
	if err != nil {
		return fail(err)
	}
	return s.verifyCodexHandoff(ctx, updated)
}

func (s *Service) verifyCodexHandoff(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	fail := func(err error) (mission.Mission, error) { return mission.Mission{}, s.failCodexPlan(ms, err) }
	deadline := time.NewTimer(planVerificationTimeout)
	defer deadline.Stop()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		reading, found, readErr := s.runtimes[mission.ToolCodex].Read(ctx, ms)
		if readErr == nil && found {
			s.applyRuntimeReading(ms.ID, reading)
			ms, _ = s.store.Snapshot().Mission(ms.ID)
			if freshPlanEvidence(ms) {
				if !settingsMatch(ms) {
					return fail(settingsMismatch(ms))
				}
				return s.changePlan(ms.ID, func(m *mission.Mission) { m.PlanTransition.State = mission.PlanTransitionVerified })
			}
		}
		select {
		case <-ctx.Done():
			return fail(ctx.Err())
		case <-deadline.C:
			return fail(fmt.Errorf("timed out verifying Codex implementation settings"))
		case <-ticker.C:
		}
	}
}

func (s *Service) retryCodexPlan(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	if ms.PlanApproved {
		return s.approveCodexPlan(ctx, ms)
	}
	if !s.planFlights.claim(ms.ID) {
		return mission.Mission{}, fmt.Errorf("%w: Codex retry already in progress", ErrConflict)
	}
	defer s.planFlights.release(ms.ID)
	return s.relaunch(ctx, ms.ID, "") // Launch re-enters native plan mode and resets verification.
}
