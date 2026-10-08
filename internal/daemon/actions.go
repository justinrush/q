package daemon

import (
	"context"
	"errors"
	"fmt"
	"github.com/justinrush/q/internal/api"
	"strings"

	"github.com/justinrush/q/internal/launch"
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
	// Stop ends a mission's agent session without touching its worktrees.
	Stop(ctx context.Context, ms mission.Mission) error
	// View starts a session on this host that shows an agent running on
	// another, by running the given command where the agent would be.
	View(ctx context.Context, operation mission.Operation, ms mission.Mission) (mission.Mission, error)
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

	if ms.Running() && !s.holds(ms) {
		if ms, err = s.ensureViewer(ctx, ms); err != nil {
			return api.Result{}, err
		}
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

// ensureViewer makes sure this host has a session to open for a mission the
// paired q is running, starting one if it does not.
//
// A debrief is a tmux session with the agent in one pane and editors beside
// it. This host has the code, mirrored, but not the agent. So the session it
// opens runs, where the agent would be, the command that attaches to the
// agent on the other machine.
func (s *Service) ensureViewer(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	snap := s.store.Snapshot()
	host := snap.HostName(ms.Lease.Holder)

	if s.remote == nil || s.messenger == nil {
		return ms, fmt.Errorf("%w: %s is running on %s; open it from there", ErrConflict, ms.Name, host)
	}

	if ms.MissionDir == "" {
		return ms, fmt.Errorf(
			"%w: %s is running on %s and has not been mirrored here yet; try again in a moment",
			ErrConflict, ms.Name, host)
	}

	if s.sessionAlive(ctx, ms) {
		return ms, nil
	}

	operation, _ := snap.Operation(ms.OperationID)

	if len(s.remote.AttachArgv(ms.ID)) == 0 {
		return ms, fmt.Errorf("%w: there is no way to attach to an agent on %s", ErrConflict, host)
	}

	viewing, err := s.messenger.View(ctx, operation, ms)
	if err != nil {
		return ms, err
	}

	s.updateLocal(ms.ID, "mission.viewer", func(stored *mission.Mission) {
		stored.TmuxSession = viewing.TmuxSession
		stored.AgentPaneID = viewing.AgentPaneID
	})

	return viewing, nil
}

// AttachCommand returns the command that attaches a terminal to a mission's
// agent on the paired host.
//
// It is asked for each time rather than baked into the viewer session, because
// the answer changes: the mission may have come back to this host, or been
// closed, since the session was opened.
func (s *Service) AttachCommand(id mission.MissionID) (api.AttachCommand, error) {
	snap := s.store.Snapshot()

	ms, ok := snap.Mission(id)
	if !ok {
		return api.AttachCommand{}, fmt.Errorf("%w: mission %s", ErrNotFound, id)
	}

	host := snap.HostName(ms.Lease.Holder)

	switch {
	case !ms.Running():
		return api.AttachCommand{}, fmt.Errorf("%w: %s is not running anywhere", ErrConflict, ms.Name)
	case s.holds(ms):
		return api.AttachCommand{}, fmt.Errorf(
			"%w: %s now runs on this machine; close this and open the mission again", ErrConflict, ms.Name)
	case s.remote == nil:
		return api.AttachCommand{}, fmt.Errorf("%w: %s is running on %s; open it from there", ErrConflict, ms.Name, host)
	}

	argv := s.remote.AttachArgv(id)
	if len(argv) == 0 {
		return api.AttachCommand{}, fmt.Errorf("%w: there is no way to attach to an agent on %s", ErrConflict, host)
	}

	return api.AttachCommand{Argv: argv, Host: host}, nil
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

	// An agent started here for a mission that was last worked on elsewhere is
	// told so, and told what the agent there last said.
	return s.relaunch(ctx, id, s.continuation(ms, message))
}

// Dispatch decides what a lane move actually does, and does it.
//
// Moving out of briefing launches the agent. Moving into the active lane from a
// lane that implies the agent is waiting resumes it, delivering the
// accompanying message and reviving the session if it has died. Moving to
// closed reclaims its resources before filing the card. Everything else is
// bookkeeping.
//
// A mission the paired q is running is not this host's to move, so the request
// is passed to the host that can act on it.
//
// This is kept apart from SetStatus so the lane rules stay free of subprocess
// work and remain testable on their own.
func (s *Service) Dispatch(
	ctx context.Context,
	id mission.MissionID,
	req api.SetStatusRequest,
) (mission.Mission, error) {
	current, ok := s.store.Snapshot().Mission(id)
	if !ok {
		return mission.Mission{}, fmt.Errorf("%w: mission %s", ErrNotFound, id)
	}

	if current.Running() && !s.holds(current) {
		return s.dispatchToPeer(ctx, current, req)
	}

	if req.To == mission.StatusClosed {
		ms, _, err := s.FinishMission(ctx, id, req.Force)

		return ms, err
	}

	if req.To != mission.StatusActive {
		return s.SetStatus(id, req.To)
	}

	if !current.Launched() {
		return s.Start(ctx, id)
	}

	switch current.Status {
	case mission.StatusBriefing:
		return s.Start(ctx, id)
	case mission.StatusAwaiting, mission.StatusDebrief:
		return s.Resume(ctx, id, req.Message)
	default:
		// Already active, so there is no lane to change. Text sent along with the
		// move is still meant for the agent, and is how a message reaches one
		// that the paired q is running.
		if req.Message != "" {
			return s.Resume(ctx, id, req.Message)
		}

		return s.SetStatus(id, req.To)
	}
}

// Message sends text to a mission's agent, wherever it is running.
func (s *Service) Message(ctx context.Context, id mission.MissionID, text string) (mission.Mission, error) {
	current, ok := s.store.Snapshot().Mission(id)
	if !ok {
		return mission.Mission{}, fmt.Errorf("%w: mission %s", ErrNotFound, id)
	}

	if current.Running() && !s.holds(current) {
		return s.dispatchToPeer(ctx, current, api.SetStatusRequest{To: mission.StatusActive, Message: text})
	}

	return s.Resume(ctx, id, text)
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
			return !pane.Dead && launch.PaneRunsAgent(pane.Command)
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
		stored.Badges = stored.WithoutBadge(mission.BadgeHandoff)
		stored.Badges = stored.WithoutBadge(mission.BadgeLaunching)
		stored.WaitingFor = ""
		stored.LaunchError = ""
		stored.FinishedAt = nil
		stored.TurnEnded = false
		stored.MovedFrom = ""
		// The agent now starting works from the branch as it stands, which is
		// the moment a note about a conflict set aside stops being news.
		stored.LocalBadges = stored.WithoutLocalBadge(mission.BadgeDiverged)
		// However it came to be started, an agent is running: by q once the
		// standby was over, or by a person who did not want to wait for that.
		stored.LocalBadges = stored.WithoutLocalBadge(mission.BadgeStandby)
		stored.UpdatedAt = now

		updated = stored
		snap.PutMission(stored)

		return nil
	})
	if err != nil {
		return mission.Mission{}, err
	}

	s.announce(updated)

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
