package daemon

import (
	"context"
	"errors"
	"fmt"
	"github.com/justinrush/q/internal/api"
	"strings"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/terminal"
)

// Debriefer arranges and attaches a mission's debrief session, and reports what changed.
type Debriefer interface {
	Open(ctx context.Context, ms mission.Mission, mode api.Mode) (api.Result, mission.Mission, error)
	Touched(ctx context.Context, ms mission.Mission) ([]api.Touched, error)
}

// Messenger delivers text to a live agent session and revives dead ones.
type Messenger interface {
	SendMessage(ctx context.Context, ms mission.Mission, text string) error
	Relaunch(ctx context.Context, operation mission.Operation, ms mission.Mission, message string) (mission.Mission, error)
}

// WithMessenger attaches the component that talks to a live agent session.
func WithMessenger(m Messenger) Option {
	return func(s *Service) { s.messenger = m }
}

// WithReclaimer attaches the component that reclaims a deleted mission's resources.
func WithReclaimer(r Reclaimer) Option {
	return func(s *Service) { s.reclaimer = r }
}

// Reclaimer removes a mission's worktrees, branches, and tmux session.
type Reclaimer interface {
	PlanReclaim(ctx context.Context, operation mission.Operation, ms mission.Mission) (mission.Plan, error)
	Reclaim(ctx context.Context, operation mission.Operation, ms mission.Mission, force bool) (mission.Report, error)
}

// PlanDelete reports what deleting a mission would discard.
//
// The board asks for this before confirming, so the dialog can name what is about to be
// lost rather than asking the human to be sure about nothing.
func (s *Service) PlanDelete(ctx context.Context, id mission.MissionID) (mission.Plan, error) {
	ms, operation, err := s.missionWithOperation(id)
	if err != nil {
		return mission.Plan{}, err
	}

	if s.reclaimer == nil || !ms.Launched() {
		// Nothing was ever provisioned, so there is nothing to plan.
		return mission.Plan{}, nil
	}

	return s.reclaimer.PlanReclaim(ctx, operation, ms)
}

// DeleteMissionAndReclaim removes a mission along with its worktrees, branches, and session.
//
// Reclaiming happens before the record is forgotten, because the record is what says
// which worktrees and branches belong to this mission. Losing it first would leave them
// orphaned with nothing to associate them back to.
func (s *Service) DeleteMissionAndReclaim(
	ctx context.Context,
	id mission.MissionID,
	force bool,
) (mission.Report, error) {
	report, err := s.ReclaimMission(ctx, id, force)
	if err != nil {
		return report, err
	}

	err = s.DeleteMission(id)
	if err != nil {
		return report, err
	}

	return report, nil
}

// ReclaimMission removes a mission's provisioned resources while retaining its record.
//
// Callers can safely perform their own state transition after this succeeds. A partial
// reclaim is an error because forgetting or finishing the mission would otherwise discard
// the only durable record of resources that still need another cleanup attempt.
func (s *Service) ReclaimMission(
	ctx context.Context,
	id mission.MissionID,
	force bool,
) (mission.Report, error) {
	ms, operation, err := s.missionWithOperation(id)
	if err != nil {
		return mission.Report{}, err
	}

	var report mission.Report

	if s.reclaimer != nil && ms.Launched() {
		report, err = s.reclaimer.Reclaim(ctx, operation, ms, force)
		if err != nil {
			if errors.Is(err, mission.ErrNeedsForce) {
				return report, fmt.Errorf("%w: %w", ErrConflict, err)
			}

			return report, err
		}
	}

	if len(report.Failures) > 0 {
		return report, fmt.Errorf("reclaiming mission resources: %s", strings.Join(report.Failures, "; "))
	}

	return report, nil
}

// FinishMission reclaims a mission's live resources and retains its card in done.
//
// The mission is filed only after every resource was reclaimed. Its launch fields are
// cleared in the same state mutation as the lane change, so a finished card cannot look
// resumable or point at worktrees that no longer exist.
func (s *Service) FinishMission(
	ctx context.Context,
	id mission.MissionID,
	force bool,
) (mission.Mission, mission.Report, error) {
	ms, _, err := s.missionWithOperation(id)
	if err != nil {
		return mission.Mission{}, mission.Report{}, err
	}

	if ms.Status == mission.StatusClosed {
		return ms, mission.Report{}, nil
	}

	report, err := s.ReclaimMission(ctx, id, force)
	if err != nil {
		return mission.Mission{}, report, err
	}

	ms, err = s.setStatus(id, mission.StatusClosed, clearFinishedResources)
	if err != nil {
		return mission.Mission{}, report, err
	}

	return ms, report, nil
}

// clearFinishedResources removes state that described the session and worktrees which
// FinishMission just reclaimed. StartedAt is cleared so a human reopening the card can
// launch fresh resources rather than trying to resume paths that no longer exist.
func clearFinishedResources(ms *mission.Mission) {
	ms.MissionDir = ""
	ms.TmuxSession = ""
	ms.AgentPaneID = ""
	ms.AgentSessionID = ""
	ms.HookEpoch++
	ms.Work = nil
	ms.AgentState = mission.AgentUnknown
	ms.WaitingFor = ""
	ms.PlanPending = false
	ms.Badges = nil
	ms.LaunchError = ""
	ms.StartedAt = nil
}

// missionWithOperation fetches a mission and the operation it belongs to.
//
// A missing operation is not fatal here: a mission can outlive a force-deleted operation, and it
// still needs to be deletable.
func (s *Service) missionWithOperation(id mission.MissionID) (mission.Mission, mission.Operation, error) {
	snap := s.store.Snapshot()

	ms, ok := snap.Mission(id)
	if !ok {
		return mission.Mission{}, mission.Operation{}, fmt.Errorf("%w: mission %s", ErrNotFound, id)
	}

	operation, _ := snap.Operation(ms.OperationID)

	return ms, operation, nil
}

// OpenDebrief arranges the mission's debrief session and attaches to it.
func (s *Service) OpenDebrief(ctx context.Context, id mission.MissionID, mode api.Mode) (api.Result, error) {
	if s.debriefer == nil {
		return api.Result{}, fmt.Errorf("%w: this daemon cannot open debrief sessions", ErrConflict)
	}

	ms, err := s.requireLaunched(id)
	if err != nil {
		return api.Result{}, err
	}

	result, updated, err := s.debriefer.Open(ctx, ms, mode)

	if result.PanesAdded > 0 {
		// Opening can create several panes before a later split fails. Record those
		// successful creations so retrying does not duplicate them.
		s.persistPanes(updated)
	}

	if err != nil {
		return result, err
	}

	return result, nil
}

// Resume continues a mission's agent, reviving the session first if it has died.
//
// This is what a move out of the waiting or debrief lane does. When the session is
// alive the message is delivered to it; when it is not, the agent is relaunched
// against the surviving worktrees and given the message as its prompt.
func (s *Service) Resume(ctx context.Context, id mission.MissionID, message string) (mission.Mission, error) {
	if s.messenger == nil {
		return mission.Mission{}, fmt.Errorf("%w: this daemon cannot talk to agent sessions", ErrConflict)
	}

	ms, err := s.requireLaunched(id)
	if err != nil {
		return mission.Mission{}, err
	}

	if s.sessionAlive(ctx, ms) {
		if message != "" {
			if err := s.messenger.SendMessage(ctx, ms, message); err != nil {
				return mission.Mission{}, err
			}
		}

		return s.SetStatus(id, mission.StatusActive)
	}

	snap := s.store.Snapshot()

	operation, ok := snap.Operation(ms.OperationID)
	if !ok {
		return mission.Mission{}, fmt.Errorf("%w: operation %s", ErrNotFound, ms.OperationID)
	}

	if !s.inflight.claim(id) {
		return mission.Mission{}, fmt.Errorf("%w: mission %s is already starting", ErrConflict, id)
	}
	defer s.inflight.release(id)

	relaunched, err := s.messenger.Relaunch(ctx, operation, ms, message)
	if err != nil {
		return mission.Mission{}, err
	}

	return s.commitRelaunch(relaunched)
}

// sessionAlive reports whether the mission's agent is still there to talk to.
func (s *Service) sessionAlive(ctx context.Context, ms mission.Mission) bool {
	if s.probe == nil || ms.TmuxSession == "" {
		return false
	}

	if !s.probe.HasSession(ctx, ms.TmuxSession) {
		return false
	}

	panes, err := s.probe.ListPanes(ctx, terminal.Session(ms.TmuxSession))
	if err != nil {
		return false
	}

	for _, pane := range panes {
		if pane.ID == ms.AgentPaneID {
			return !pane.Dead && agentCommands[pane.Command]
		}
	}

	return false
}

// requireLaunched fetches a mission that has been started.
func (s *Service) requireLaunched(id mission.MissionID) (mission.Mission, error) {
	ms, ok := s.store.Snapshot().Mission(id)
	if !ok {
		return mission.Mission{}, fmt.Errorf("%w: mission %s", ErrNotFound, id)
	}

	if !ms.Launched() {
		return mission.Mission{}, fmt.Errorf("%w: mission %s has not been launched", ErrConflict, id)
	}

	return ms, nil
}

// persistPanes records debrief pane ids without disturbing anything else.
func (s *Service) persistPanes(ms mission.Mission) {
	if len(ms.Work) == 0 {
		return
	}

	var updated mission.Mission

	err := s.store.Mutate("mission.debrief_panes", func(snap *mission.Snapshot) error {
		stored, ok := snap.Mission(ms.ID)
		if !ok {
			return nil
		}

		for name, work := range ms.Work {
			existing, ok := stored.Work[name]
			if !ok {
				continue
			}

			existing.DebriefPaneID = work.DebriefPaneID
			stored.Work[name] = existing
		}

		stored.UpdatedAt = s.now()
		updated = stored
		snap.PutMission(stored)

		return nil
	})
	if err != nil {
		s.warn("recording debrief panes", "error", err)

		return
	}

	if updated.ID != "" {
		s.publishMission(updated)
	}
}

// commitRelaunch persists a revived session.
func (s *Service) commitRelaunch(relaunched mission.Mission) (mission.Mission, error) {
	var updated mission.Mission

	err := s.store.Mutate("mission.relaunched", func(snap *mission.Snapshot) error {
		stored, ok := snap.Mission(relaunched.ID)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, relaunched.ID)
		}

		now := s.now()

		stored.Status = mission.StatusActive
		stored.StatusChangedAt = now
		stored.Order = snap.NextOrder(mission.StatusActive)
		stored.AgentState = relaunched.AgentState
		stored.TmuxSession = relaunched.TmuxSession
		stored.AgentPaneID = relaunched.AgentPaneID
		stored.AgentSessionID = relaunched.AgentSessionID
		stored.HookEpoch = relaunched.HookEpoch
		stored.Badges = relaunched.Badges
		stored.WaitingFor = ""
		stored.LaunchError = ""
		stored.FinishedAt = nil
		stored.UpdatedAt = now

		updated = stored
		snap.PutMission(stored)

		return nil
	})
	if err != nil {
		return mission.Mission{}, err
	}

	s.publishMission(updated)

	return updated, nil
}

// Diff reports what each of a mission's worktrees has changed.
func (s *Service) Diff(ctx context.Context, id mission.MissionID) ([]api.Touched, error) {
	if s.debriefer == nil {
		return nil, fmt.Errorf("%w: this daemon cannot inspect worktrees", ErrConflict)
	}

	ms, err := s.requireLaunched(id)
	if err != nil {
		return nil, err
	}

	return s.debriefer.Touched(ctx, ms)
}
