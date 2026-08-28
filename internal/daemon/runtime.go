package daemon

import (
	"context"
	"time"

	"github.com/justinrush/q/internal/mission"
)

const (
	runtimePollInterval = time.Second
	runtimeReadTimeout  = 3 * time.Second
	// approvalGrace is how long a "waiting on approval" reading must persist
	// before it moves a card.
	//
	// A runtime reports the approval the instant it is raised, which is often a
	// few hundred milliseconds before the agent resolves it on its own. Holding
	// the reading briefly keeps the board from flashing a request the human never
	// needed to see.
	approvalGrace = 2 * time.Second
)

// approvalCandidate is a waiting reading that has not yet outlived the grace.
type approvalCandidate struct {
	missionID  mission.MissionID
	reading    mission.Reading
	observedAt time.Time
	promoted   bool
}

// RunRuntimeWatchers keeps cards aligned with what each agent's own runtime says
// its sessions are doing. Hooks still provide tool names and closing-message
// text.
func (s *Service) RunRuntimeWatchers(ctx context.Context) {
	if len(s.runtimes) == 0 {
		return
	}

	s.pollRuntimes(ctx)

	ticker := time.NewTicker(runtimePollInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.pollRuntimes(ctx)
		}
	}
}

// pollRuntimes reads every live mission through its agent's runtime.
func (s *Service) pollRuntimes(ctx context.Context) {
	for _, ms := range s.store.Snapshot().Missions {
		runtime, ok := s.runtimes[ms.Tool]
		if !ok || ms.Status == mission.StatusBriefing || ms.Status.Terminal() {
			continue
		}

		readCtx, cancel := context.WithTimeout(ctx, runtimeReadTimeout)
		reading, found, err := runtime.Read(readCtx, ms)
		cancel()

		if err != nil || !found {
			continue
		}

		s.applyRuntimeReading(ms.ID, reading)
	}

	s.promoteMatureApprovals()
}

// applyRuntimeReading records or applies one authoritative reading.
func (s *Service) applyRuntimeReading(id mission.MissionID, reading mission.Reading) {
	if reading.Activity == mission.ActivityWaitingApproval {
		s.noteApproval(id, reading, s.now())
		s.promoteMatureApprovals()

		return
	}

	if reading.Activity != mission.ActivityUnknown {
		s.clearApproval(id)
	}

	s.applyRuntimeReadingNow(id, reading)
}

// applyRuntimeReadingNow writes a reading through to the card.
func (s *Service) applyRuntimeReadingNow(id mission.MissionID, reading mission.Reading) {
	var updated mission.Mission

	err := s.store.Mutate("mission.runtime_status", func(snap *mission.Snapshot) error {
		ms, ok := snap.Mission(id)
		if !ok || ms.Status.Terminal() {
			return nil
		}

		if ms.AgentSessionID != "" && ms.AgentSessionID != reading.SessionID {
			return nil
		}

		before := ms
		now := s.now()
		ms.AgentSessionID = reading.SessionID
		applyActivity(&ms, reading, now)

		if sameRuntimeState(before, ms) {
			return nil
		}

		if ms.Status != before.Status {
			ms.Order = snap.NextOrder(ms.Status)
			ms.StatusChangedAt = now
		}

		ms.LastEventAt = now
		ms.UpdatedAt = now
		updated = ms
		snap.PutMission(ms)

		return nil
	})
	if err != nil {
		s.warn("applying agent runtime status", "error", err)

		return
	}

	if updated.ID != "" {
		s.publishMission(updated)
	}
}

// noteApproval records a waiting reading, starting its grace period.
func (s *Service) noteApproval(id mission.MissionID, reading mission.Reading, now time.Time) {
	s.approvalMu.Lock()
	defer s.approvalMu.Unlock()

	candidate, ok := s.approvals[id]
	if !ok || (candidate.reading.SessionID != "" && reading.SessionID != "" &&
		candidate.reading.SessionID != reading.SessionID) {
		s.approvals[id] = approvalCandidate{missionID: id, reading: reading, observedAt: now}

		return
	}

	if candidate.reading.SessionID == "" {
		candidate.reading.SessionID = reading.SessionID
	}

	if candidate.reading.WaitingFor == "" {
		candidate.reading.WaitingFor = reading.WaitingFor
	}

	s.approvals[id] = candidate
}

// clearApproval forgets a mission's pending approval.
func (s *Service) clearApproval(id mission.MissionID) {
	s.approvalMu.Lock()
	delete(s.approvals, id)
	s.approvalMu.Unlock()
}

// promoteMatureApprovals applies the waiting readings that outlived the grace.
func (s *Service) promoteMatureApprovals() {
	now := s.now()

	var mature []approvalCandidate

	s.approvalMu.Lock()
	for id, candidate := range s.approvals {
		if candidate.promoted || now.Sub(candidate.observedAt) < approvalGrace {
			continue
		}

		candidate.promoted = true
		s.approvals[id] = candidate
		mature = append(mature, candidate)
	}
	s.approvalMu.Unlock()

	for _, candidate := range mature {
		s.applyRuntimeReadingNow(candidate.missionID, candidate.reading)
	}
}

// applyActivity moves a card to match what the agent's runtime reports.
func applyActivity(ms *mission.Mission, reading mission.Reading, now time.Time) {
	switch reading.Activity {
	case mission.ActivityBusy:
		ms.AgentState = mission.AgentBusy
		ms.Status = mission.StatusActive
		ms.WaitingFor = ""
		ms.FinishedAt = nil
		ms.Badges = ms.WithoutBadge(mission.BadgeStale)
		ms.Badges = ms.WithoutBadge(mission.BadgeAPIError)
	case mission.ActivityWaitingApproval, mission.ActivityWaitingInput:
		ms.AgentState = mission.AgentWaiting
		ms.Status = mission.StatusAwaiting
		ms.WaitingFor = reading.WaitingFor
		ms.FinishedAt = nil
		ms.Badges = ms.WithoutBadge(mission.BadgeStale)
	case mission.ActivityIdle:
		// A Stop hook may have classified the closing message as a question.
		// Preserve that semantic wait while using the runtime for process truth.
		if ms.Status == mission.StatusAwaiting {
			return
		}

		ms.AgentState = mission.AgentIdle
		ms.Badges = ms.WithoutBadge(mission.BadgeStale)

		if ms.Status == mission.StatusActive {
			ms.Status = mission.StatusDebrief
			finished := now
			ms.FinishedAt = &finished
		}
	case mission.ActivityFailed:
		ms.AgentState = mission.AgentWaiting
		ms.Status = mission.StatusAwaiting
		ms.WaitingFor = reading.WaitingFor
		ms.Badges = ms.WithBadge(mission.BadgeAPIError, "runtime")
	case mission.ActivityUnknown:
		return
	}
}

// sameRuntimeState reports whether a reading changed anything worth persisting.
func sameRuntimeState(a, b mission.Mission) bool {
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
