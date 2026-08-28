package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/justinrush/q/internal/debrief"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/launch"
	"github.com/justinrush/q/internal/state"
	"github.com/justinrush/q/internal/tmuxc"
)

// Debriefer arranges and attaches a mission's debrief session, and reports what changed.
type Debriefer interface {
	Open(ctx context.Context, mission domain.Mission, mode debrief.Mode) (debrief.Result, domain.Mission, error)
	Touched(ctx context.Context, mission domain.Mission) ([]debrief.Touched, error)
}

// Messenger delivers text to a live agent session and revives dead ones.
type Messenger interface {
	SendMessage(ctx context.Context, mission domain.Mission, text string) error
	Relaunch(ctx context.Context, operation domain.Operation, mission domain.Mission, message string) (domain.Mission, error)
}

// Reclaimer removes a mission's worktrees, branches, and tmux session.
type Reclaimer interface {
	PlanReclaim(ctx context.Context, operation domain.Operation, mission domain.Mission) (launch.Plan, error)
	Reclaim(ctx context.Context, operation domain.Operation, mission domain.Mission, force bool) (launch.Report, error)
}

// SetReclaimer attaches the component that reclaims a deleted mission's resources.
func (s *Service) SetReclaimer(r Reclaimer) { s.reclaimer = r }

// PlanDelete reports what deleting a mission would discard.
//
// The board asks for this before confirming, so the dialog can name what is about to be
// lost rather than asking the human to be sure about nothing.
func (s *Service) PlanDelete(ctx context.Context, id domain.MissionID) (launch.Plan, error) {
	mission, operation, err := s.missionWithOperation(id)
	if err != nil {
		return launch.Plan{}, err
	}

	if s.reclaimer == nil || !mission.Launched() {
		// Nothing was ever provisioned, so there is nothing to plan.
		return launch.Plan{}, nil
	}

	return s.reclaimer.PlanReclaim(ctx, operation, mission)
}

// DeleteMissionAndReclaim removes a mission along with its worktrees, branches, and session.
//
// Reclaiming happens before the record is forgotten, because the record is what says
// which worktrees and branches belong to this mission. Losing it first would leave them
// orphaned with nothing to associate them back to.
func (s *Service) DeleteMissionAndReclaim(
	ctx context.Context,
	id domain.MissionID,
	force bool,
) (launch.Report, error) {
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
	id domain.MissionID,
	force bool,
) (launch.Report, error) {
	mission, operation, err := s.missionWithOperation(id)
	if err != nil {
		return launch.Report{}, err
	}

	var report launch.Report

	if s.reclaimer != nil && mission.Launched() {
		report, err = s.reclaimer.Reclaim(ctx, operation, mission, force)
		if err != nil {
			if errors.Is(err, launch.ErrNeedsForce) {
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
	id domain.MissionID,
	force bool,
) (domain.Mission, launch.Report, error) {
	mission, _, err := s.missionWithOperation(id)
	if err != nil {
		return domain.Mission{}, launch.Report{}, err
	}

	if mission.Status == domain.StatusClosed {
		return mission, launch.Report{}, nil
	}

	report, err := s.ReclaimMission(ctx, id, force)
	if err != nil {
		return domain.Mission{}, report, err
	}

	mission, err = s.setStatus(id, domain.StatusClosed, clearFinishedResources)
	if err != nil {
		return domain.Mission{}, report, err
	}

	return mission, report, nil
}

// clearFinishedResources removes state that described the session and worktrees which
// FinishMission just reclaimed. StartedAt is cleared so a human reopening the card can
// launch fresh resources rather than trying to resume paths that no longer exist.
func clearFinishedResources(mission *domain.Mission) {
	mission.MissionDir = ""
	mission.TmuxSession = ""
	mission.AgentPaneID = ""
	mission.AgentSessionID = ""
	mission.HookEpoch++
	mission.Work = nil
	mission.AgentState = domain.AgentUnknown
	mission.WaitingFor = ""
	mission.PlanPending = false
	mission.Badges = nil
	mission.LaunchError = ""
	mission.StartedAt = nil
}

// missionWithOperation fetches a mission and the operation it belongs to.
//
// A missing operation is not fatal here: a mission can outlive a force-deleted operation, and it
// still needs to be deletable.
func (s *Service) missionWithOperation(id domain.MissionID) (domain.Mission, domain.Operation, error) {
	snap := s.store.Snapshot()

	mission, ok := snap.Mission(id)
	if !ok {
		return domain.Mission{}, domain.Operation{}, fmt.Errorf("%w: mission %s", ErrNotFound, id)
	}

	operation, _ := snap.Operation(mission.OperationID)

	return mission, operation, nil
}

// SetDebriefer attaches the component that opens debrief sessions.
func (s *Service) SetDebriefer(r Debriefer) { s.debriefer = r }

// SetMessenger attaches the component that talks to live sessions.
func (s *Service) SetMessenger(m Messenger) { s.messenger = m }

// OpenDebrief arranges the mission's debrief session and attaches to it.
func (s *Service) OpenDebrief(ctx context.Context, id domain.MissionID, mode debrief.Mode) (debrief.Result, error) {
	if s.debriefer == nil {
		return debrief.Result{}, fmt.Errorf("%w: this daemon cannot open debrief sessions", ErrConflict)
	}

	mission, err := s.requireLaunched(id)
	if err != nil {
		return debrief.Result{}, err
	}

	result, updated, err := s.debriefer.Open(ctx, mission, mode)

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
func (s *Service) Resume(ctx context.Context, id domain.MissionID, message string) (domain.Mission, error) {
	if s.messenger == nil {
		return domain.Mission{}, fmt.Errorf("%w: this daemon cannot talk to agent sessions", ErrConflict)
	}

	mission, err := s.requireLaunched(id)
	if err != nil {
		return domain.Mission{}, err
	}

	if s.sessionAlive(ctx, mission) {
		if message != "" {
			if err := s.messenger.SendMessage(ctx, mission, message); err != nil {
				return domain.Mission{}, err
			}
		}

		return s.SetStatus(id, domain.StatusActive)
	}

	snap := s.store.Snapshot()

	operation, ok := snap.Operation(mission.OperationID)
	if !ok {
		return domain.Mission{}, fmt.Errorf("%w: operation %s", ErrNotFound, mission.OperationID)
	}

	if !s.inflight.claim(id) {
		return domain.Mission{}, fmt.Errorf("%w: mission %s is already starting", ErrConflict, id)
	}
	defer s.inflight.release(id)

	relaunched, err := s.messenger.Relaunch(ctx, operation, mission, message)
	if err != nil {
		return domain.Mission{}, err
	}

	return s.commitRelaunch(relaunched)
}

// sessionAlive reports whether the mission's agent is still there to talk to.
func (s *Service) sessionAlive(ctx context.Context, mission domain.Mission) bool {
	if s.probe == nil || mission.TmuxSession == "" {
		return false
	}

	if !s.probe.HasSession(ctx, mission.TmuxSession) {
		return false
	}

	panes, err := s.probe.ListPanes(ctx, tmuxc.Session(mission.TmuxSession))
	if err != nil {
		return false
	}

	for _, pane := range panes {
		if pane.ID == mission.AgentPaneID {
			return !pane.Dead && agentCommands[pane.Command]
		}
	}

	return false
}

// requireLaunched fetches a mission that has been started.
func (s *Service) requireLaunched(id domain.MissionID) (domain.Mission, error) {
	mission, ok := s.store.Snapshot().Mission(id)
	if !ok {
		return domain.Mission{}, fmt.Errorf("%w: mission %s", ErrNotFound, id)
	}

	if !mission.Launched() {
		return domain.Mission{}, fmt.Errorf("%w: mission %s has not been launched", ErrConflict, id)
	}

	return mission, nil
}

// persistPanes records debrief pane ids without disturbing anything else.
func (s *Service) persistPanes(mission domain.Mission) {
	if len(mission.Work) == 0 {
		return
	}

	var updated domain.Mission

	err := s.store.Mutate("mission.debrief_panes", func(snap *state.Snapshot) error {
		stored, ok := snap.Mission(mission.ID)
		if !ok {
			return nil
		}

		for name, work := range mission.Work {
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
func (s *Service) commitRelaunch(relaunched domain.Mission) (domain.Mission, error) {
	var updated domain.Mission

	err := s.store.Mutate("mission.relaunched", func(snap *state.Snapshot) error {
		stored, ok := snap.Mission(relaunched.ID)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, relaunched.ID)
		}

		now := s.now()

		stored.Status = domain.StatusActive
		stored.StatusChangedAt = now
		stored.Order = snap.NextOrder(domain.StatusActive)
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
		return domain.Mission{}, err
	}

	s.publishMission(updated)

	return updated, nil
}

// Diff reports what each of a mission's worktrees has changed.
func (s *Service) Diff(ctx context.Context, id domain.MissionID) ([]debrief.Touched, error) {
	if s.debriefer == nil {
		return nil, fmt.Errorf("%w: this daemon cannot inspect worktrees", ErrConflict)
	}

	mission, err := s.requireLaunched(id)
	if err != nil {
		return nil, err
	}

	return s.debriefer.Touched(ctx, mission)
}
