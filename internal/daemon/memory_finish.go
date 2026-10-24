package daemon

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/memory"
	"github.com/justinrush/q/internal/mission"
)

const captureTimeout = 5 * time.Minute

func (s *Service) validCapture(ms mission.Mission, token string) bool {
	f := ms.Finish
	return token != "" && f.Token == token && s.holds(ms) && f.Lease == ms.Lease && f.HookEpoch == ms.HookEpoch &&
		ms.Launched() && !ms.Status.Terminal() && (f.Phase == mission.FinishSending || f.Phase == mission.FinishCapturing || f.Phase == mission.FinishAwaitingIdle || f.Phase == mission.FinishReady)
}

func (s *Service) changeFinish(id mission.MissionID, fn func(*mission.Mission)) error {
	var updated mission.Mission
	err := s.store.Mutate("mission.finish-capture", func(snap *mission.Snapshot) error {
		ms, ok := snap.Mission(id)
		if !ok {
			return ErrNotFound
		}
		before := ms
		fn(&ms)
		if before.Finish == ms.Finish && before.AgentState == ms.AgentState && before.Status == ms.Status {
			return mission.ErrUnchanged
		}
		ms.UpdatedAt = s.now()
		updated = ms
		snap.PutMission(ms)
		return nil
	})
	if err == nil && updated.ID != "" {
		s.publishMission(updated)
	}
	return err
}

func (s *Service) cancelFinish(id mission.MissionID) error {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	return s.changeFinish(id, func(ms *mission.Mission) { ms.Finish = mission.FinishCapture{} })
}

func (s *Service) requestFinish(ctx context.Context, id mission.MissionID, req api.SetStatusRequest) (mission.Mission, error) {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	ms, _, err := s.missionWithOperation(id)
	if err != nil {
		return ms, err
	}
	switch {
	case req.CancelFinish:
		err = s.changeFinish(id, func(m *mission.Mission) { m.Finish = mission.FinishCapture{} })
	case req.SkipMemory || !ms.Launched() || ms.Status.Terminal() || s.memory == nil || !s.captureOnFinish:
		if err = s.changeFinish(id, func(m *mission.Mission) { m.Finish = mission.FinishCapture{} }); err != nil {
			return ms, err
		}
		ms, _, err = s.FinishMission(ctx, id, req.Force)
		return ms, err
	case ms.Finish.Phase == mission.FinishReady && !req.RetryCapture:
		if !s.validCapture(ms, ms.Finish.Token) || ms.AgentState != mission.AgentIdle || ms.Finish.IdleAt.IsZero() {
			return ms, fmt.Errorf("%w: capture context changed; retry capture or skip memory", ErrConflict)
		}
		// This is a fresh, explicit cleanup request after capture, including a fresh
		// dirty-work confirmation. Never carry an earlier Force across an agent turn.
		ms, _, err = s.FinishMission(ctx, id, req.Force)
		return ms, err
	case ms.Finish.Token == "" || req.RetryCapture:
		err = s.changeFinish(id, func(m *mission.Mission) {
			token := memory.NewID()
			if m.Finish.Token != "" && m.Finish.Lease == m.Lease && m.Finish.HookEpoch == m.HookEpoch {
				token = m.Finish.Token
			}
			m.Finish = mission.FinishCapture{Token: token, Phase: mission.FinishQueued, StartedAt: s.now(), HookEpoch: m.HookEpoch, Lease: m.Lease}
		})
	}
	if err != nil {
		return ms, err
	}
	ms, _, err = s.missionWithOperation(id)
	return ms, err
}

func capturePrompt(ms mission.Mission) string {
	return fmt.Sprintf(`q is preparing to finish this mission. Save shared memory before its session and worktrees are removed.
Capture token: %s
Do not edit repositories, commit, push, or continue implementation. Do not change your native memory.
Save exactly one concise summary of the work, outcomes, decisions, tests actually run, and unresolved issues. Add only worthwhile reusable lessons (zero is fine). Include evidence and limitations; exclude credentials and raw transcripts. Keep the whole batch below 128 KiB, each record below 16 KiB.
Pipe a JSON object to:
q memory save --mission %s --capture %s --file -
Format: {"records":[{"kind":"summary","title":"Short title","body":"Markdown account","tags":["keyword"]},{"kind":"lesson","title":"Reusable discovery","body":"Evidence and when it applies","tags":[]}]}
The save command must succeed. Then end your turn immediately with a short statement. If saving is blocked, say why and stop; do not claim it was saved.`, ms.Finish.Token, ms.ID, ms.Finish.Token)
}

// RunFinishCaptures advances persisted jobs without holding an HTTP request open.
func (s *Service) RunFinishCaptures(ctx context.Context) {
	if s.memory == nil {
		return
	}
	// Sending is intentionally persisted before delivery. A crash here makes
	// delivery uncertain; ask for a retry instead of submitting the prompt twice.
	s.finishMu.Lock()
	for _, ms := range s.store.Snapshot().Missions {
		if s.holds(ms) && ms.Finish.Phase == mission.FinishSending && !s.memory.Captured(ms.Finish.Token) {
			_ = s.failCapture(ms.ID, "prompt delivery was interrupted; retry capture or finish without memory")
		}
	}
	s.finishMu.Unlock()
	ticker := time.NewTicker(time.Second)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		for _, ms := range s.store.Snapshot().Missions {
			if ms.Finish.Token != "" {
				s.advanceCapture(ctx, ms.ID)
			}
		}
	}
}

func (s *Service) failCapture(id mission.MissionID, reason string) error {
	return s.changeFinish(id, func(m *mission.Mission) { m.Finish.Phase = mission.FinishFailed; m.Finish.Error = reason })
}

func (s *Service) advanceCapture(ctx context.Context, id mission.MissionID) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	ms, _, err := s.missionWithOperation(id)
	if err != nil || ms.Finish.Token == "" {
		return
	}
	f := ms.Finish
	if !s.holds(ms) {
		return
	}
	if f.Lease != ms.Lease || f.HookEpoch != ms.HookEpoch || ms.Status.Terminal() {
		_ = s.changeFinish(id, func(m *mission.Mission) { m.Finish = mission.FinishCapture{} })
		return
	}
	if f.Phase == mission.FinishFailed || f.Phase == mission.FinishReady {
		return
	}
	// Recover a durable save whose acknowledgment was interrupted. Still require
	// another idle observation after recovery, rather than trusting old activity.
	if f.AcknowledgedAt.IsZero() && s.memory.Captured(f.Token) {
		_ = s.changeFinish(id, func(m *mission.Mission) {
			m.Finish.AcknowledgedAt = s.now()
			m.Finish.Phase = mission.FinishAwaitingIdle
		})
		return
	}
	if s.now().Sub(f.StartedAt) >= captureTimeout {
		_ = s.failCapture(id, "memory capture timed out; retry capture or finish without memory")
		return
	}
	if !s.sessionAlive(ctx, ms) {
		_ = s.failCapture(id, "agent session is unavailable; retry capture or finish without memory")
		return
	}
	if f.Phase == mission.FinishQueued {
		s.sendCapture(ctx, ms)
		return
	}

	if f.Phase != mission.FinishAwaitingIdle || f.IdleAt.IsZero() || ms.AgentState != mission.AgentIdle {
		return
	}
	// Dirty work always gets a new confirmation after capture; a Force from
	// before the capture turn never authorizes discarding its new work.
	plan, err := s.PlanDelete(ctx, id)
	if err != nil {
		_ = s.failCapture(id, "checking cleanup safety: "+err.Error())
		return
	}
	if plan.NeedsForce || len(plan.KeptBranches) > 0 {
		_ = s.changeFinish(id, func(m *mission.Mission) {
			m.Finish.Phase = mission.FinishReady
			m.Finish.Error = "memory saved; finish again to confirm cleanup"
		})
		return
	}
	if _, _, err = s.FinishMission(ctx, id, false); err != nil {
		_ = s.changeFinish(id, func(m *mission.Mission) {
			m.Finish.Phase = mission.FinishReady
			m.Finish.Error = "memory saved; cleanup needs attention: " + err.Error()
		})
	}
}

func observeFinishIdle(ms *mission.Mission, now time.Time) {
	if ms.AgentState != mission.AgentIdle {
		ms.Finish.IdleAt = time.Time{}
		return
	}
	if ms.Finish.Phase == mission.FinishAwaitingIdle && ms.Finish.IdleAt.IsZero() && ms.AgentState == mission.AgentIdle && now.After(ms.Finish.AcknowledgedAt) {
		ms.Finish.IdleAt = now
	}
}

func observeFinishHook(ms *mission.Mission, event mission.HookEvent, now time.Time) {
	if ms.Finish.Token == "" {
		return
	}
	if event.Event == mission.EventUserPromptSubmit {
		// OpenCode reports user submissions without their text. Accept its first
		// submission after delivery as the capture prompt; another submission is
		// ordinary work and cancels cleanup. Providers with text must match the token.
		capture := strings.Contains(event.Prompt, "Capture token: "+ms.Finish.Token)
		emptySubmission := event.Prompt == "" && !ms.Finish.PromptSeen &&
			(ms.Finish.Phase == mission.FinishSending || ms.Finish.Phase == mission.FinishCapturing)
		if !capture && !emptySubmission {
			ms.Finish = mission.FinishCapture{}
			return
		}
		ms.Finish.PromptSeen = true
	}
	if ms.AgentState != mission.AgentIdle {
		ms.Finish.IdleAt = time.Time{}
	}
	if event.Event == mission.EventStop {
		observeFinishIdle(ms, now)
	}
}

func (s *Service) sendCapture(ctx context.Context, ms mission.Mission) {
	id := ms.ID

	if ms.PlanMode && !ms.PlanApproved {
		_ = s.failCapture(id, "agent is still in planning mode; finish without memory or leave planning mode and retry")
		return
	}
	if ms.AgentState != mission.AgentIdle {
		return
	}
	if s.messenger == nil {
		_ = s.failCapture(id, "agent messaging is unavailable")
		return
	}
	if err := s.changeFinish(id, func(m *mission.Mission) { m.Finish.Phase = mission.FinishSending }); err != nil {
		return
	}
	// SendMessage may cause an immediate save API call. Do not hold finishMu
	// across delivery, so the agent can acknowledge while the terminal submits.
	s.finishMu.Unlock()
	sendCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	err := s.messenger.SendMessage(sendCtx, ms, capturePrompt(ms))
	cancel()
	s.finishMu.Lock()
	current, _, readErr := s.missionWithOperation(id)
	if readErr != nil || current.Finish.Token != ms.Finish.Token {
		return
	}
	// A successful save proves delivery even when the terminal's response was
	// lost. Likewise a newer retry or cancellation must keep its own state.
	if current.Finish.Phase != mission.FinishSending {
		return
	}
	if err != nil {
		_ = s.failCapture(id, "prompt delivery failed or is uncertain: "+err.Error())
		return
	}
	_ = s.changeFinish(id, func(m *mission.Mission) { m.Finish.Phase = mission.FinishCapturing })
}
