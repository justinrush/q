// Moving a mission between paired hosts.
//
// A mission is on both hosts all the time: its records, and a worktree per
// repository. What moves is the lease, which says whose agent runs it. The
// rules for moving it are few and are all here.
//
// The primary is preferred. It is the machine with the power and the human, so
// a mission runs there whenever it can. The secondary runs a mission only when
// the primary has gone quiet for long enough to be considered away, and gives
// it back at the first moment that costs nothing: when the agent has finished a
// turn and is waiting for a person anyway.

package daemon

import (
	"context"
	"fmt"

	"github.com/justinrush/q/internal/mission"
)

// tendLeases makes one pass over the rules for moving missions. It runs on the
// scheduler's tick.
func (s *Service) tendLeases(ctx context.Context) {
	snap := s.store.Snapshot()
	now := s.now()

	switch s.roleOf(snap) {
	case mission.RoleSecondary:
		if s.peerSeen(snap, now) {
			s.handBack(ctx, snap)
		} else {
			s.takeOver(ctx, snap)
		}
	case mission.RolePrimary:
		s.markUnconfirmed(snap, !s.peerSeen(snap, now))
	case mission.RoleStandalone:
	}
}

// takeOver runs, on the secondary, missions the primary was running when it
// went quiet.
//
// Only a mission whose agent was mid-turn is taken. One waiting on a human has
// nothing for an agent to do, and a queued one that never started is the
// scheduler's to start. The agent here is a fresh one, started in the mirror
// with the last state that arrived, so work the primary did after its final
// exchange is not in it — at most one interval's worth, and still on the
// primary's disk for when it returns.
//
// A sleeping laptop is not a dead one. Its own agent will wake with it, and the
// two will then both have worked on the mission. That is expected, and is what
// the primary resolves when it next exchanges: it sees it lost the lease, stops
// its agent, and its changes are kept alongside these.
func (s *Service) takeOver(ctx context.Context, snap mission.Snapshot) {
	if s.messenger == nil {
		return
	}

	free := s.maxConcurrent - s.activeHere(snap)

	for _, ms := range snap.MissionsInLane(mission.StatusActive) {
		if free <= 0 {
			return
		}

		if s.holds(ms) || ms.Pin == snap.PeerID() || !ms.Running() {
			continue
		}

		// Without a mirror there is nothing to start an agent in. That means the
		// mission launched after the last exchange, so this host knows nothing
		// of its work either.
		if ms.MissionDir == "" {
			continue
		}

		if s.continueHere(ctx, ms, snap.HostName(ms.Lease.Holder)) {
			free--
		}
	}
}

// continueHere takes a mission and starts an agent for it on this host.
func (s *Service) continueHere(ctx context.Context, ms mission.Mission, from string) bool {
	taken := s.updateLease(ms.ID, "mission.lease.takeover", func(stored *mission.Mission) bool {
		if s.holds(*stored) || !stored.Running() {
			return false
		}

		stored.Lease = stored.Lease.Take(s.self)

		// Until the agent is up, the card must not claim one is working.
		stored.AgentState = mission.AgentUnknown
		stored.WaitingFor = ""
		stored.TurnEnded = false
		stored.Badges = stored.WithBadge(mission.BadgeLaunching, "")

		return true
	})
	if !taken {
		return false
	}

	s.logger.Info("taking over a mission from the paired q", "mission", ms.ID, "from", from)

	message := mission.Continuation(from, ms.LastMessage, "")

	if _, err := s.relaunch(ctx, ms.ID, message); err != nil {
		s.warn("starting an agent for a mission taken over", "mission", ms.ID, "error", err)
		s.updateLease(ms.ID, "mission.takeover_failed", func(stored *mission.Mission) bool {
			stored.Badges = stored.WithoutBadge(mission.BadgeLaunching)
			stored.LaunchError = err.Error()
			orphan(stored, s.now())

			return true
		})

		return false
	}

	return true
}

// handBack returns to the primary every mission the secondary is running whose
// agent has finished a turn.
//
// The moment is chosen so that nothing is interrupted. The agent is at its
// prompt, waiting for a person; the person is at the primary. Stopping the
// agent here and letting the next turn start there costs only the conversation,
// which the primary's agent is told it does not have.
//
// A mission blocked part-way through a turn — on a permission prompt, or a plan
// awaiting approval — is left where it is. Moving it would throw the turn away,
// and it can be answered from the primary without moving.
func (s *Service) handBack(ctx context.Context, snap mission.Snapshot) {
	peer := snap.PeerID()

	for _, ms := range snap.Missions {
		if !s.holds(ms) || !ms.Running() || ms.Pin == s.self || !atTurnEnd(ms) {
			continue
		}

		s.release(ctx, ms, peer, "mission.lease.return")
	}
}

// handedOff marks a mission passed on between turns by the named host. Nothing
// about its run is touched: the agent finished what it was doing, and the card
// should go on saying what it said, plus where it has just been.
func handedOff(ms *mission.Mission, from string) {
	ms.Badges = ms.WithBadge(mission.BadgeHandoff, from)
}

// atTurnEnd reports whether a mission's agent is between turns and waiting on
// a human, which is when it can change hosts without losing work in flight.
func atTurnEnd(ms mission.Mission) bool {
	if ms.Status != mission.StatusAwaiting && ms.Status != mission.StatusDebrief {
		return false
	}

	if ms.PlanPending {
		return false
	}

	return ms.TurnEnded || ms.AgentState == mission.AgentDead
}

// release stops this host's agent for a mission and passes its lease to
// another host.
//
// The agent is stopped first. A lease that moved while the session was still
// alive would leave an agent running on a host that no longer speaks for the
// mission, and whose worktree the other host is about to treat as a mirror.
func (s *Service) release(ctx context.Context, ms mission.Mission, to mission.HostID, label string) bool {
	if to == "" {
		return false
	}

	if s.messenger != nil && ms.TmuxSession != "" {
		if err := s.messenger.Stop(ctx, ms); err != nil {
			s.warn("stopping an agent before its mission moves", "mission", ms.ID, "error", err)

			return false
		}
	}

	// Read before the mutation, which holds the store's lock.
	here := s.store.Snapshot().HostName(s.self)

	return s.updateLease(ms.ID, label, func(stored *mission.Mission) bool {
		if !s.holds(*stored) {
			return false
		}

		stored.Lease = stored.Lease.Take(to)

		// What this host knew about its own session is no longer anyone's concern.
		stored.TmuxSession = ""
		stored.AgentPaneID = ""
		stored.HookEpoch++

		// A mission between turns moves as it is. One stopped part-way through a
		// turn has lost that turn, and the card it hands over has to say so.
		if atTurnEnd(*stored) {
			handedOff(stored, here)
		} else {
			orphan(stored, s.now())
		}

		return true
	})
}

// markUnconfirmed shows, on the primary, that the peer may have taken over the
// missions this host believes it is running.
func (s *Service) markUnconfirmed(snap mission.Snapshot, unconfirmed bool) {
	for _, ms := range snap.Missions {
		want := unconfirmed && s.holds(ms) && ms.Running() && ms.Status == mission.StatusActive

		if ms.HasLocalBadge(mission.BadgeUnconfirmed) == want {
			continue
		}

		s.updateLocal(ms.ID, "mission.unconfirmed", func(stored *mission.Mission) {
			if want {
				stored.LocalBadges = stored.WithLocalBadge(mission.BadgeUnconfirmed, "")
			} else {
				stored.LocalBadges = stored.WithoutLocalBadge(mission.BadgeUnconfirmed)
			}
		})
	}
}

// Take brings a mission the paired q is running to this host.
//
// When the other host can be asked, it is: it stops its agent, and the exchange
// that follows carries its final state here, so nothing is lost. When it cannot
// be asked — or this is the secondary, which cannot ask at all — the lease is
// simply taken. The other host learns of it when the two next speak, stops its
// agent then, and whatever it did in the meantime is kept beside what happens
// here.
func (s *Service) Take(ctx context.Context, id mission.MissionID) (mission.Mission, error) {
	snap := s.store.Snapshot()

	ms, ok := snap.Mission(id)
	if !ok {
		return mission.Mission{}, fmt.Errorf("%w: mission %s", ErrNotFound, id)
	}

	if s.holds(ms) {
		return ms, nil
	}

	if s.remote != nil && ms.Running() {
		if err := s.remote.Release(ctx, id); err == nil {
			if err := s.SyncNow(ctx); err != nil {
				s.warn("exchanging state after taking a mission", "mission", id, "error", err)
			}

			if taken, ok := s.store.Snapshot().Mission(id); ok && s.holds(taken) {
				return taken, nil
			}
		}
	}

	return s.seize(id, snap.HostName(ms.Lease.Holder))
}

// seize takes a lease without the other host's cooperation.
func (s *Service) seize(id mission.MissionID, from string) (mission.Mission, error) {
	var taken mission.Mission

	err := s.store.Mutate("mission.lease.take", func(snap *mission.Snapshot) error {
		ms, ok := snap.Mission(id)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}

		if !s.holds(ms) {
			peer := ms.Lease.Holder
			ms.Lease = ms.Lease.Take(s.self)
			ms.MovedFrom = peer
			orphan(&ms, s.now())
		}

		ms.UpdatedAt = s.now()
		taken = ms
		snap.PutMission(ms)

		return nil
	})
	if err != nil {
		return mission.Mission{}, err
	}

	s.logger.Info("took a mission from the paired q without asking it", "mission", id, "from", from)
	s.publishMission(taken)
	s.kickSync()

	return taken, nil
}

// Release gives a mission this host is running to its peer, at the peer's
// request.
func (s *Service) Release(ctx context.Context, id mission.MissionID) (mission.Mission, error) {
	snap := s.store.Snapshot()

	ms, ok := snap.Mission(id)
	if !ok {
		return mission.Mission{}, fmt.Errorf("%w: mission %s", ErrNotFound, id)
	}

	if snap.Peer == nil {
		return mission.Mission{}, fmt.Errorf("%w: this q is not paired, so there is nobody to release to", ErrConflict)
	}

	if !s.holds(ms) {
		return ms, nil
	}

	if !s.release(ctx, ms, snap.Peer.ID, "mission.lease.release") {
		return mission.Mission{}, fmt.Errorf("%w: %s could not be released", ErrConflict, ms.Name)
	}

	released, _ := s.store.Snapshot().Mission(id)

	return released, nil
}

// relaunch starts an agent for a launched mission against its existing
// worktrees and records the result.
func (s *Service) relaunch(ctx context.Context, id mission.MissionID, message string) (mission.Mission, error) {
	snap := s.store.Snapshot()

	ms, ok := snap.Mission(id)
	if !ok {
		return mission.Mission{}, fmt.Errorf("%w: mission %s", ErrNotFound, id)
	}

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

// continuation wraps a message for an agent that is picking a mission up from
// another host, and returns it unchanged for one that is not.
func (s *Service) continuation(ms mission.Mission, message string) string {
	if ms.MovedFrom == "" {
		return message
	}

	return mission.Continuation(s.store.Snapshot().HostName(ms.MovedFrom), ms.LastMessage, message)
}

// noteMoved records, for a mission whose lease just arrived, which host it came
// from, so the agent started for it can be told.
func (s *Service) noteMoved(id mission.MissionID, from mission.HostID) {
	if from == "" {
		return
	}

	s.updateLocal(id, "mission.moved_here", func(ms *mission.Mission) {
		if ms.Running() {
			ms.MovedFrom = from
		}
	})
}
