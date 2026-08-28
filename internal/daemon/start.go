package daemon

import (
	"context"
	"fmt"
	"slices"
	"sync"

	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/state"
)

// Launcher provisions a mission's worktrees and starts its agent.
//
// It is an interface so the service can be tested without git or tmux, and so the
// orchestration package does not have to know about HTTP or the store.
type Launcher interface {
	Launch(ctx context.Context, operation domain.Operation, mission domain.Mission) (domain.Mission, error)
}

// inflight tracks missions currently being launched, so a double keypress cannot
// start two agents against the same worktrees.
type inflight struct {
	mu  sync.Mutex
	set map[domain.MissionID]struct{}
}

// claim reserves a mission, reporting false if it is already being launched.
func (i *inflight) claim(id domain.MissionID) bool {
	i.mu.Lock()
	defer i.mu.Unlock()

	if i.set == nil {
		i.set = map[domain.MissionID]struct{}{}
	}

	if _, busy := i.set[id]; busy {
		return false
	}

	i.set[id] = struct{}{}

	return true
}

// release frees a claimed mission.
func (i *inflight) release(id domain.MissionID) {
	i.mu.Lock()
	defer i.mu.Unlock()

	delete(i.set, id)
}

// Start launches a draft mission's agent.
//
// Provisioning worktrees takes seconds and can fail, which the five lanes have no
// way to express, so the card moves to in-progress with a launching badge and rolls
// back to briefing with an explanation if anything goes wrong. Leaving it active
// with no session would make the reconciler badge it as a dead session instead,
// which would be a misleading way to report "the fetch failed".
func (s *Service) Start(ctx context.Context, id domain.MissionID) (domain.Mission, error) {
	if s.launcher == nil {
		return domain.Mission{}, fmt.Errorf("%w: this daemon cannot launch agents", ErrConflict)
	}

	snap := s.store.Snapshot()

	mission, ok := snap.Mission(id)
	if !ok {
		return domain.Mission{}, fmt.Errorf("%w: mission %s", ErrNotFound, id)
	}

	if mission.Launched() {
		return domain.Mission{}, fmt.Errorf("%w: mission %s has already been launched", ErrConflict, id)
	}

	operation, ok := snap.Operation(mission.OperationID)
	if !ok {
		return domain.Mission{}, fmt.Errorf("%w: operation %s", ErrNotFound, mission.OperationID)
	}

	if !s.inflight.claim(id) {
		return domain.Mission{}, fmt.Errorf("%w: mission %s is already starting", ErrConflict, id)
	}
	defer s.inflight.release(id)

	if err := s.markLaunching(id); err != nil {
		return domain.Mission{}, err
	}

	launched, err := s.launcher.Launch(ctx, operation, mission)
	if err != nil {
		return domain.Mission{}, s.markLaunchFailed(id, err)
	}

	return s.commitLaunch(launched)
}

// markLaunching records that provisioning has begun.
func (s *Service) markLaunching(id domain.MissionID) error {
	var updated domain.Mission

	err := s.store.Mutate("mission.launching", func(snap *state.Snapshot) error {
		mission, ok := snap.Mission(id)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}

		mission.Status = domain.StatusActive
		mission.Order = snap.NextOrder(domain.StatusActive)
		mission.AgentState = domain.AgentUnknown
		mission.Badges = mission.WithBadge(domain.BadgeLaunching, "")
		mission.LaunchError = ""
		mission.UpdatedAt = s.now()
		updated = mission
		snap.PutMission(mission)

		return nil
	})
	if err != nil {
		return err
	}

	s.publishMission(updated)

	return nil
}

// markLaunchFailed returns the card to draft with an explanation, and reports the
// original failure.
func (s *Service) markLaunchFailed(id domain.MissionID, cause error) error {
	var updated domain.Mission

	if err := s.store.Mutate("mission.launch_failed", func(snap *state.Snapshot) error {
		mission, ok := snap.Mission(id)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}

		mission.Status = domain.StatusBriefing
		mission.Order = snap.NextOrder(domain.StatusBriefing)
		mission.AgentState = domain.AgentUnknown
		mission.Badges = mission.WithoutBadge(domain.BadgeLaunching)
		mission.LaunchError = cause.Error()
		mission.StartedAt = nil
		mission.TmuxSession = ""
		mission.AgentPaneID = ""
		mission.Work = nil
		mission.UpdatedAt = s.now()
		updated = mission
		snap.PutMission(mission)

		return nil
	}); err != nil {
		return fmt.Errorf("launch failed (%w), and recording that failed too: %w", cause, err)
	}

	s.publishMission(updated)

	return cause
}

// commitLaunch persists a successful launch.
func (s *Service) commitLaunch(launched domain.Mission) (domain.Mission, error) {
	var updated domain.Mission

	err := s.store.Mutate("mission.launched", func(snap *state.Snapshot) error {
		stored, ok := snap.Mission(launched.ID)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, launched.ID)
		}

		// Carry over only the launch results, so a rename made while worktrees
		// were being provisioned is not clobbered.
		stored.Status = domain.StatusActive
		stored.AgentState = launched.AgentState
		stored.MissionDir = launched.MissionDir
		stored.TmuxSession = launched.TmuxSession
		stored.AgentPaneID = launched.AgentPaneID
		stored.AgentSessionID = launched.AgentSessionID
		stored.HookEpoch = launched.HookEpoch
		stored.Work = launched.Work
		stored.LaunchRepos = slices.Clone(launched.LaunchRepos)
		stored.LaunchReposFrozen = launched.LaunchReposFrozen
		stored.StartedAt = launched.StartedAt
		stored.LaunchError = ""
		stored.Badges = stored.WithoutBadge(domain.BadgeLaunching)
		stored.UpdatedAt = s.now()

		updated = stored
		snap.PutMission(stored)

		return nil
	})
	if err != nil {
		return domain.Mission{}, err
	}

	s.publishMission(updated)

	return updated, nil
}
