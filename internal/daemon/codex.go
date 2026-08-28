package daemon

import (
	"context"
	"time"

	"github.com/justinrush/q/internal/codexapp"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/state"
)

const (
	codexPollInterval  = time.Second
	codexReadTimeout   = 3 * time.Second
	codexApprovalGrace = 2 * time.Second
)

type codexApprovalCandidate struct {
	missionID  domain.MissionID
	sessionID  string
	waitingFor string
	observedAt time.Time
	promoted   bool
}

// CodexStatusReader reads the runtime status of a loaded Codex operation.
type CodexStatusReader interface {
	ReadThread(ctx context.Context, operationID string) (codexapp.ThreadStatus, error)
	FindThread(ctx context.Context, cwd string) (codexapp.ThreadSnapshot, bool, error)
}

// SetCodexStatusReader attaches app-server runtime status to the service.
func (s *Service) SetCodexStatusReader(reader CodexStatusReader) {
	s.codex = reader
}

// RunCodexWatcher keeps Codex cards aligned with app-server's structured
// runtime status. Hooks still provide tool names and closing-message text.
func (s *Service) RunCodexWatcher(ctx context.Context) {
	if s.codex == nil {
		return
	}

	s.pollCodex(ctx)

	ticker := time.NewTicker(codexPollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollCodex(ctx)
		}
	}
}

func (s *Service) pollCodex(ctx context.Context) {
	for _, mission := range s.store.Snapshot().Missions {
		if mission.Tool != domain.ToolCodex || mission.Status == domain.StatusBriefing || mission.Status.Terminal() {
			continue
		}

		readCtx, cancel := context.WithTimeout(ctx, codexReadTimeout)
		status, sessionID, err := s.readCodexStatus(readCtx, mission)
		cancel()
		if err != nil {
			continue
		}

		s.applyCodexStatus(mission.ID, sessionID, status.Classify())
	}

	s.promoteMatureCodexApprovals()
}

func (s *Service) readCodexStatus(
	ctx context.Context,
	mission domain.Mission,
) (codexapp.ThreadStatus, string, error) {
	if mission.AgentSessionID != "" {
		status, err := s.codex.ReadThread(ctx, mission.AgentSessionID)

		return status, mission.AgentSessionID, err
	}

	operation, found, err := s.codex.FindThread(ctx, mission.MissionDir)
	if err != nil {
		return codexapp.ThreadStatus{}, "", err
	}

	if !found {
		return codexapp.ThreadStatus{}, "", nil
	}

	return operation.Status, operation.ID, nil
}

func (s *Service) applyCodexStatus(id domain.MissionID, sessionID string, activity codexapp.Activity) {
	if activity == codexapp.ActivityWaitingApproval {
		s.noteCodexApproval(id, sessionID, "Codex approval", s.now())
		s.promoteMatureCodexApprovals()

		return
	}

	if activity != codexapp.ActivityUnknown {
		s.clearCodexApproval(id)
	}

	s.applyCodexStatusNow(id, sessionID, activity, "")
}

func (s *Service) applyCodexStatusNow(
	id domain.MissionID,
	sessionID string,
	activity codexapp.Activity,
	waitingFor string,
) {
	var updated domain.Mission

	err := s.store.Mutate("mission.codex_status", func(snap *state.Snapshot) error {
		mission, ok := snap.Mission(id)
		if !ok || mission.Tool != domain.ToolCodex || mission.Status.Terminal() {
			return nil
		}

		if mission.AgentSessionID != "" && mission.AgentSessionID != sessionID {
			return nil
		}

		before := mission
		now := s.now()
		mission.AgentSessionID = sessionID
		applyCodexActivity(&mission, activity, now)
		if activity == codexapp.ActivityWaitingApproval && waitingFor != "" {
			mission.WaitingFor = waitingFor
		}

		if sameCodexState(before, mission) {
			return nil
		}

		if mission.Status != before.Status {
			mission.Order = snap.NextOrder(mission.Status)
			mission.StatusChangedAt = now
		}

		mission.LastEventAt = now
		mission.UpdatedAt = now
		updated = mission
		snap.PutMission(mission)

		return nil
	})
	if err != nil {
		s.warn("applying Codex runtime status", "error", err)

		return
	}

	if updated.ID != "" {
		s.publishMission(updated)
	}
}

func (s *Service) noteCodexApproval(
	id domain.MissionID,
	sessionID string,
	waitingFor string,
	now time.Time,
) {
	s.codexApprovalMu.Lock()
	defer s.codexApprovalMu.Unlock()

	candidate, ok := s.codexApprovals[id]
	if !ok || (candidate.sessionID != "" && sessionID != "" && candidate.sessionID != sessionID) {
		s.codexApprovals[id] = codexApprovalCandidate{
			missionID:  id,
			sessionID:  sessionID,
			waitingFor: waitingFor,
			observedAt: now,
		}

		return
	}

	if candidate.sessionID == "" {
		candidate.sessionID = sessionID
	}

	if candidate.waitingFor == "" || candidate.waitingFor == "Codex approval" {
		candidate.waitingFor = waitingFor
	}

	s.codexApprovals[id] = candidate
}

func (s *Service) clearCodexApproval(id domain.MissionID) {
	s.codexApprovalMu.Lock()
	delete(s.codexApprovals, id)
	s.codexApprovalMu.Unlock()
}

func (s *Service) promoteMatureCodexApprovals() {
	now := s.now()
	var mature []codexApprovalCandidate

	s.codexApprovalMu.Lock()
	for id, candidate := range s.codexApprovals {
		if candidate.promoted || now.Sub(candidate.observedAt) < codexApprovalGrace {
			continue
		}

		candidate.promoted = true
		s.codexApprovals[id] = candidate
		mature = append(mature, candidate)
	}
	s.codexApprovalMu.Unlock()

	for _, candidate := range mature {
		s.applyCodexStatusNow(
			candidate.missionID,
			candidate.sessionID,
			codexapp.ActivityWaitingApproval,
			candidate.waitingFor,
		)
	}
}

func applyCodexActivity(mission *domain.Mission, activity codexapp.Activity, now time.Time) {
	switch activity {
	case codexapp.ActivityBusy:
		mission.AgentState = domain.AgentBusy
		mission.Status = domain.StatusActive
		mission.WaitingFor = ""
		mission.FinishedAt = nil
		mission.Badges = mission.WithoutBadge(domain.BadgeStale)
		mission.Badges = mission.WithoutBadge(domain.BadgeAPIError)
	case codexapp.ActivityWaitingApproval:
		mission.AgentState = domain.AgentWaiting
		mission.Status = domain.StatusAwaiting
		mission.WaitingFor = "Codex approval"
		mission.FinishedAt = nil
		mission.Badges = mission.WithoutBadge(domain.BadgeStale)
	case codexapp.ActivityWaitingInput:
		mission.AgentState = domain.AgentWaiting
		mission.Status = domain.StatusAwaiting
		mission.WaitingFor = "Codex needs input"
		mission.FinishedAt = nil
		mission.Badges = mission.WithoutBadge(domain.BadgeStale)
	case codexapp.ActivityIdle:
		// A Stop hook may have classified the closing message as a question.
		// Preserve that semantic wait while using app-server for process truth.
		if mission.Status == domain.StatusAwaiting {
			return
		}

		mission.AgentState = domain.AgentIdle
		mission.Badges = mission.WithoutBadge(domain.BadgeStale)
		if mission.Status == domain.StatusActive {
			mission.Status = domain.StatusDebrief
			finished := now
			mission.FinishedAt = &finished
		}
	case codexapp.ActivityFailed:
		mission.AgentState = domain.AgentWaiting
		mission.Status = domain.StatusAwaiting
		mission.WaitingFor = "Codex system error"
		mission.Badges = mission.WithBadge(domain.BadgeAPIError, "app-server")
	case codexapp.ActivityUnknown:
		return
	}
}

func sameCodexState(a, b domain.Mission) bool {
	if a.Status != b.Status || a.AgentState != b.AgentState ||
		a.WaitingFor != b.WaitingFor || a.AgentSessionID != b.AgentSessionID ||
		len(a.Badges) != len(b.Badges) {
		return false
	}

	if (a.FinishedAt == nil) != (b.FinishedAt == nil) {
		return false
	}

	for i := range a.Badges {
		if a.Badges[i] != b.Badges[i] {
			return false
		}
	}

	return true
}
