package daemon

import (
	"context"
	"fmt"
	"time"

	"github.com/justinrush/q/internal/conversation"
	"github.com/justinrush/q/internal/mission"
)

// approveToolPlan serializes native and q approvals. The checkpoints survive
// restart independently of approval and of the destination's observed settings.
func (s *Service) approveToolPlan(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	if !s.planFlights.claim(ms.ID) {
		return ms, fmt.Errorf("%w: tool handoff is already running", ErrConflict)
	}
	defer s.planFlights.release(ms.ID)
	if !s.inflight.claim(ms.ID) {
		return ms, fmt.Errorf("%w: mission is already starting", ErrConflict)
	}
	defer s.inflight.release(ms.ID)
	ctx, cancel := context.WithTimeout(ctx, planSwitchTimeout)
	defer cancel()
	ms, _ = s.store.Snapshot().Mission(ms.ID)
	if !s.holds(ms) || ms.Status.Terminal() {
		return ms, fmt.Errorf("%w: mission is not owned by this host", ErrConflict)
	}
	if ms.ToolHandoff.State == mission.ToolHandoffVerified {
		return ms, nil
	}
	ms, err := s.changePlan(ms.ID, func(m *mission.Mission) {
		m.PlanApproved = true
		m.PlanPending = false
		m.ToolHandoff.Error = ""
		if m.RunningTool() != m.Tool {
			m.ToolHandoff.State = mission.ToolHandoffPending
		} else if m.ToolHandoff.State == mission.ToolHandoffFailed {
			m.ToolHandoff.State = m.ToolHandoff.ResumeState
		}
	})
	if err != nil {
		return ms, err
	}
	preflight, ok := s.messenger.(interface{ PreflightTool(mission.Tool) error })
	if !ok {
		return s.failToolHandoff(ms, fmt.Errorf("launcher does not support tool handoffs"))
	}
	if err := preflight.PreflightTool(ms.Tool); err != nil {
		return s.failToolHandoff(ms, err)
	}
	for _, step := range []func(context.Context, mission.Mission) (mission.Mission, error){
		s.resolveToolDefaults, s.prepareToolHandoff, s.recoverToolDestination, s.launchToolDestination, s.observeToolDestination, s.startToolImplementation,
	} {
		next, stepErr := step(ctx, ms)
		if stepErr != nil {
			return s.failToolHandoff(ms, stepErr)
		}
		ms = next
	}
	return s.changePlan(ms.ID, func(m *mission.Mission) { m.ToolHandoff.State = mission.ToolHandoffVerified; m.ToolHandoff.Error = "" })
}

func (s *Service) failToolHandoff(ms mission.Mission, cause error) (mission.Mission, error) {
	updated, err := s.changePlan(ms.ID, func(m *mission.Mission) {
		if m.ToolHandoff.State != mission.ToolHandoffFailed {
			m.ToolHandoff.ResumeState = m.ToolHandoff.State
		}
		m.ToolHandoff.State = mission.ToolHandoffFailed
		m.ToolHandoff.Error = cause.Error()
		keepToolHandoffFailure(m)
	})
	if err != nil {
		return updated, err
	}
	// Record failure before stopping so late hooks cannot unpause the card.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if updated.RunningTool() == mission.ToolCodex && updated.AgentSessionID != "" {
		if controller, ok := s.runtimes[mission.ToolCodex].(mission.PlanController); ok {
			if err := controller.Interrupt(ctx, updated.AgentSessionID); err != nil {
				s.warn("interrupting failed tool handoff", "error", err)
			}
		}
	}
	if err := s.messenger.Stop(ctx, updated); err != nil {
		s.warn("stopping failed tool handoff", "error", err)
	}
	return updated, cause
}

func (s *Service) prepareToolHandoff(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	if ms.RunningTool() == ms.Tool {
		return ms, nil
	}
	if ms.RunningTool() == mission.ToolCodex {
		controller, ok := s.runtimes[mission.ToolCodex].(mission.PlanController)
		if !ok {
			return ms, fmt.Errorf("codex managed-server controls are unavailable")
		}
		if err := controller.Interrupt(ctx, ms.AgentSessionID); err != nil {
			return ms, err
		}
	}
	if err := s.messenger.Stop(ctx, ms); err != nil {
		return ms, err
	}
	exported, err := conversation.FromMission(ms)
	if err != nil {
		return ms, err
	}
	s.meterMission(ms.ID)
	return s.changePlan(ms.ID, func(m *mission.Mission) {
		m.ToolHandoff.Plan = exported.Plan
		m.ToolHandoff.Conversation = exported.Conversation
		m.ToolHandoff.State = mission.ToolHandoffPrepared
		m.PriorToolUsage = m.UsageByHost[s.self].Clone()
		m.PlanningSessionID = m.AgentSessionID
		m.PlanningTranscriptPath = m.TranscriptPath
		m.ActiveTool = m.Tool
		m.AgentSessionID = ""
		m.TranscriptPath = ""
		m.CodexUsage = nil
		m.ObservedSettings = mission.TurnSettings{}
		m.LastMessage = ""
		m.HookEpoch++
		m.PlanTransition = mission.PlanTransition{}
	})
}

func (s *Service) launchToolDestination(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	if s.sessionAlive(ctx, ms) {
		return ms, nil
	}
	if ms.ToolHandoff.TurnSubmitted && ms.AgentSessionID == "" {
		return ms, fmt.Errorf("destination session is unavailable; cannot safely recover the submitted implementation turn")
	}
	op, _ := s.store.Snapshot().Operation(ms.OperationID)
	ms, err := s.changePlan(ms.ID, func(m *mission.Mission) {
		m.ToolHandoff.State = mission.ToolHandoffStarting
		m.ToolHandoffStartedAt = s.now()
		m.HookEpoch++
		if m.Tool == mission.ToolCodex && !m.ToolHandoff.TurnSubmitted {
			pendingImplementation(m, s.now())
			m.PlanTransition.Message = m.HandoffPrompt()
		}
		if m.ToolHandoff.TurnSubmitted {
			m.PlanTransition.HookEpoch = m.HookEpoch
		}
	})
	if err != nil {
		return ms, err
	}
	relaunched, err := s.messenger.Relaunch(ctx, op, ms, ms.HandoffPrompt())
	if err != nil {
		return ms, err
	}
	return s.commitRelaunch(relaunched)
}

func (s *Service) observeToolDestination(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	if ms.Tool == mission.ToolCodex {
		if ready, ok := s.messenger.(interface {
			ReadyForTurn(context.Context, mission.Mission) error
		}); ok {
			if err := ready.ReadyForTurn(ctx, ms); err != nil {
				return ms, err
			}
		}
	}
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	deadline := time.NewTimer(planVerificationTimeout)
	defer deadline.Stop()
	for {
		if runtime, ok := s.runtimes[ms.Tool]; ok {
			reading, found, err := runtime.Read(ctx, ms)
			if err == nil && found {
				s.applyRuntimeReading(ms.ID, reading)
			}
		}
		ms, _ = s.store.Snapshot().Mission(ms.ID)
		if ms.AgentSessionID != "" && (ms.Tool == mission.ToolCodex || ms.LastEventAt.After(ms.ToolHandoffStartedAt)) {
			return ms, nil
		}
		select {
		case <-ctx.Done():
			return ms, ctx.Err()
		case <-deadline.C:
			return ms, fmt.Errorf("timed out discovering destination session")
		case <-ticker.C:
		}
	}
}

func (s *Service) startToolImplementation(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	if ms.Tool != mission.ToolCodex {
		return ms, nil
	}
	controller, ok := s.runtimes[mission.ToolCodex].(mission.PlanController)
	if !ok {
		return ms, fmt.Errorf("codex managed-server controls are unavailable")
	}
	// A lost turn/start response is recovered from observed settings, never by
	// submitting the implementation prompt a second time.
	if !freshPlanEvidence(ms) || !settingsMatch(ms) {
		if ms.ToolHandoff.TurnSubmitted {
			return s.verifyCodexHandoff(ctx, ms)
		}
		if err := controller.ConfigureImplementation(ctx, ms); err != nil {
			return ms, err
		}
		var err error
		ms, err = s.changePlan(ms.ID, func(m *mission.Mission) {
			m.ToolHandoff.State = mission.ToolHandoffVerifying
			m.ToolHandoff.TurnSubmitted = true
			m.BeginPlanVerification(s.now())
		})
		if err != nil {
			return ms, err
		}
		turn, err := controller.StartImplementation(ctx, ms, ms.HandoffPrompt())
		if err != nil {
			return ms, err
		}
		ms, err = s.changePlan(ms.ID, func(m *mission.Mission) { m.PlanTransition.TurnID = turn })
		if err != nil {
			return ms, err
		}
	}
	return s.verifyCodexHandoff(ctx, ms)
}

func keepToolHandoffFailure(ms *mission.Mission) {
	if ms.ToolHandoff.State != mission.ToolHandoffFailed {
		return
	}
	ms.Status = mission.StatusAwaiting
	ms.AgentState = mission.AgentWaiting
	ms.WaitingFor = "Tool handoff failed: " + ms.ToolHandoff.Error + "; retry with p or resume"
	ms.TurnEnded = true
	ms.FinishedAt = nil
}

func (s *Service) reconcileToolHandoff(ms mission.Mission) {
	if ms.ToolHandoffPending() && ms.ToolHandoff.State != mission.ToolHandoffFailed && s.holds(ms) && !ms.Status.Terminal() {
		go s.implementPlan(ms.ID)
	}
}

func (s *Service) recoverToolDestination(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	if recovery, ok := s.messenger.(interface {
		RecoverToolSession(context.Context, mission.Mission) (mission.Mission, error)
	}); ok {
		recovered, err := recovery.RecoverToolSession(ctx, ms)
		if err != nil {
			return ms, err
		}
		if recovered.AgentSessionID != ms.AgentSessionID || recovered.AgentPaneID != ms.AgentPaneID {
			ms, err = s.changePlan(ms.ID, func(m *mission.Mission) {
				m.AgentSessionID = recovered.AgentSessionID
				m.AgentPaneID = recovered.AgentPaneID
			})
			if err != nil {
				return ms, err
			}
		}
	}
	if ms.Tool == mission.ToolCodex && ms.AgentSessionID == "" {
		if runtime, ok := s.runtimes[ms.Tool]; ok {
			reading, found, err := runtime.Read(ctx, ms)
			if err != nil {
				return ms, err
			}
			if found {
				s.applyRuntimeReading(ms.ID, reading)
				ms, _ = s.store.Snapshot().Mission(ms.ID)
			}
		}
	}
	return ms, nil
}

// A fresh Codex thread has no observed turn settings yet. Resolve its default
// from the same native configuration/catalog used by the mission form.
func (s *Service) resolveToolDefaults(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	if ms.Tool != mission.ToolCodex || ms.Model != "" || ms.ToolHandoff.ImplementationModel != "" {
		return ms, nil
	}
	model := s.ModelsFor(ms.Tool).Default
	if model == "" {
		model = s.RefreshModels(ctx)[ms.Tool].Default
	}
	if model == "" {
		return ms, fmt.Errorf("cannot resolve the Codex default model; refresh q models before retrying")
	}
	return s.changePlan(ms.ID, func(m *mission.Mission) { m.ToolHandoff.ImplementationModel = model })
}
