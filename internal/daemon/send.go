// Sending a mission to the paired host, and keeping it there.
//
// Take brings a mission here. Send is its inverse, and is stronger than a lease
// move: it pins the mission to the other host, so that host runs it from then
// on, whoever is around. That is what lets a laptop hand long work to the
// always-on machine and close its lid without the work coming back to it at
// the next turn end.
//
// A pin on a launched mission is acted on only between turns. A mission sent
// mid-turn stays where it is, marked as on its way, and moves when its agent
// next stops for a person. Moving it sooner would throw the turn away.

package daemon

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/remote"
)

// pinnedElsewhere reports whether a mission is pinned to this host's peer.
//
// A pin naming a host that is neither, left over from an old pairing, is no
// instruction anyone can follow and is treated as no pin.
func (s *Service) pinnedElsewhere(ms mission.Mission) bool {
	snap := s.store.Snapshot()

	return snap.Peer != nil && ms.Pin != "" && ms.Pin == snap.Peer.ID
}

// launchOnPeer starts, on the peer, an unlaunched mission pinned to it.
//
// The lease goes first, then the record, then the request: the peer starts
// only what it holds, and can hold only what it has heard of. A peer that
// cannot be reached gets the mission queued instead, and its scheduler starts
// it at the first exchange.
func (s *Service) launchOnPeer(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	peer := s.store.Snapshot().PeerID()

	s.passLease(ms, peer)

	if s.remote != nil {
		err := s.SyncNow(ctx)
		if err == nil {
			started, err := s.remote.SetStatus(ctx, ms.ID, api.SetStatusRequest{To: mission.StatusActive})
			if err == nil {
				s.kickSync()

				return started, nil
			}

			if !errors.Is(err, remote.ErrUnreachable) {
				return mission.Mission{}, peerError(err)
			}
		} else {
			s.warn("exchanging state before starting a mission on the paired q", "mission", ms.ID, "error", err)
		}
	}

	return s.queueForPeer(ms.ID)
}

// queueForPeer marks a mission to be started by whichever host it is pinned
// to, at that host's next look at its queue.
func (s *Service) queueForPeer(id mission.MissionID) (mission.Mission, error) {
	var updated mission.Mission

	err := s.store.Mutate("mission.queue_for_peer", func(snap *mission.Snapshot) error {
		ms, ok := snap.Mission(id)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}

		ms.Queued = true
		ms.LaunchError = ""
		ms.UpdatedAt = s.now()
		updated = ms
		snap.PutMission(ms)

		return nil
	})
	if err != nil {
		return mission.Mission{}, err
	}

	s.announce(updated)

	return updated, nil
}

// Send pins a mission to the paired host and moves it there as soon as that
// costs nothing.
//
// An unlaunched mission is only pinned: it starts there when it is started. A
// launched one this host runs moves now if its agent is between turns, and at
// the end of the current turn if not. One the peer already runs stays there.
func (s *Service) Send(ctx context.Context, id mission.MissionID) (mission.Mission, error) {
	snap := s.store.Snapshot()
	if snap.Peer == nil {
		return mission.Mission{}, fmt.Errorf("%w: this q is not paired, so there is nowhere to send to", ErrConflict)
	}

	ms, ok := snap.Mission(id)
	if !ok {
		return mission.Mission{}, fmt.Errorf("%w: mission %s", ErrNotFound, id)
	}

	if familyHasOpenChildren(&snap, ms) {
		return mission.Mission{}, fmt.Errorf("%w: mission has an open child or borrows a parent workspace", ErrConflict)
	}
	if ms.Status.Terminal() {
		return mission.Mission{}, fmt.Errorf("%w: %s is closed", ErrConflict, ms.Name)
	}

	if ms.ReviewOf != "" {
		return mission.Mission{}, fmt.Errorf(
			"%w: a review runs where its transcript is, beside the mission it reviews", ErrConflict)
	}

	pinned, err := s.setPin(id, snap.Peer.ID)
	if err != nil {
		return mission.Mission{}, err
	}

	if !pinned.Launched() || !s.holds(pinned) || !pinned.Running() {
		return pinned, nil
	}

	s.tendSend(ctx, pinned)

	current, _ := s.store.Snapshot().Mission(id)

	return current, nil
}

// setPin pins a mission to a host, launched or not. The pin is part of the
// brief, so it reaches the peer with the next exchange.
func (s *Service) setPin(id mission.MissionID, to mission.HostID) (mission.Mission, error) {
	var updated mission.Mission

	err := s.store.Mutate("mission.pin", func(snap *mission.Snapshot) error {
		ms, ok := snap.Mission(id)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}

		if ms.Pin == to {
			updated = ms

			return mission.ErrUnchanged
		}

		ms.Pin = to
		ms.UpdatedAt = s.now()
		updated = ms
		snap.PutMission(ms)

		return nil
	})
	if err != nil {
		return mission.Mission{}, err
	}

	s.announce(updated)

	return updated, nil
}

// sendPinned moves to the peer every mission this host runs that is pinned
// there, each at the end of its current turn.
func (s *Service) sendPinned(ctx context.Context, snap mission.Snapshot) {
	peer := snap.PeerID()

	for _, ms := range snap.Missions {
		want := peer != "" && ms.Pin == peer && s.holds(ms) && ms.Running() && !familyHasOpenChildren(&snap, ms)
		if !want {
			if ms.HasLocalBadge(mission.BadgeSending) {
				s.updateLocal(ms.ID, "mission.send.cleared", func(stored *mission.Mission) {
					stored.LocalBadges = stored.WithoutLocalBadge(mission.BadgeSending)
				})
			}

			continue
		}

		s.tendSend(ctx, ms)
	}
}

// tendSend moves one mission pinned to the peer if it can go now, and
// otherwise says on its card what it is waiting for.
func (s *Service) tendSend(ctx context.Context, ms mission.Mission) {
	snap := s.store.Snapshot()
	peer := snap.Peer
	if peer == nil {
		return
	}

	waiting := ""

	switch {
	case !atTurnEnd(ms):
		waiting = "at turn end"
	case !s.peerSeen(snap, wall(s.now())) && s.remote == nil:
		// A secondary cannot ask; it can only hand over to a primary it has
		// recently heard from.
		waiting = peer.Name + " has not been heard from"
	default:
		if reason := s.peerReady(ctx, ms.ID); reason != "" {
			waiting = reason
		}
	}

	if waiting != "" {
		s.markSending(ms, waiting)

		return
	}

	if !s.release(ctx, ms, peer.ID, "mission.lease.send") {
		s.markSending(ms, "could not stop the agent here")

		return
	}

	s.updateLocal(ms.ID, "mission.send.done", func(stored *mission.Mission) {
		stored.LocalBadges = stored.WithoutLocalBadge(mission.BadgeSending)
	})

	s.logger.Info("sent a mission to the paired q", "mission", ms.ID, "to", peer.Name)

	// The final state of the worktrees goes across now, so the agent the peer
	// starts has it and the laptop can sleep as soon as this returns.
	if s.remote != nil {
		if err := s.SyncNow(ctx); err != nil {
			s.warn("exchanging state after sending a mission", "mission", ms.ID, "error", err)
		}
	}
}

// peerReady asks the peer whether it could run a mission now, returning why
// not, or empty when it could.
//
// Only a primary can ask. A secondary hands over as it always has, and the
// primary, which then holds a mission it may be unable to resume, says so on
// its card.
func (s *Service) peerReady(ctx context.Context, id mission.MissionID) string {
	if s.remote == nil {
		return ""
	}

	snap := s.store.Snapshot()
	name := snap.HostName(snap.PeerID())

	ready, err := s.remote.Ready(ctx, id)
	if err != nil {
		if errors.Is(err, remote.ErrUnreachable) {
			return name + " cannot be reached"
		}

		return fmt.Sprintf("asking %s: %v", name, err)
	}

	if !ready.Ready {
		return name + " " + ready.Reason
	}

	return ""
}

// markSending shows on a card that the mission is on its way to the peer and
// what it is waiting for.
func (s *Service) markSending(ms mission.Mission, detail string) {
	for _, badge := range ms.LocalBadges {
		if badge.Kind == mission.BadgeSending && badge.Detail == detail {
			return
		}
	}

	s.updateLocal(ms.ID, "mission.sending", func(stored *mission.Mission) {
		stored.LocalBadges = stored.WithLocalBadge(mission.BadgeSending, detail)
	})
}

// Readiness reports whether this host could run a mission the peer wants to
// send it. It is the peer that asks, before it stops its own agent.
func (s *Service) Readiness(id mission.MissionID) api.Readiness {
	snap := s.store.Snapshot()

	ms, ok := snap.Mission(id)
	if !ok {
		return api.Readiness{Reason: "has not heard of this mission yet"}
	}

	if s.messenger == nil {
		return api.Readiness{Reason: "cannot run agents"}
	}

	if missing := unlocated(snap, ms); len(missing) > 0 {
		return api.Readiness{Reason: "has no checkout of " + strings.Join(missing, ", ")}
	}

	if ms.Launched() && !mirrorComplete(ms) {
		return api.Readiness{Reason: "is still mirroring its worktrees"}
	}

	return api.Readiness{Ready: true}
}

// adoptSent starts an agent for each mission that was just sent here.
//
// The host that sent it stopped its agent between turns, and nothing here is
// running one. Waiting for a person to ask would leave Enter on the other
// machine attaching to nothing, and the point of sending was not to need that
// machine. The agent is told it is continuing from elsewhere, as one started
// for a takeover is.
func (s *Service) adoptSent(ctx context.Context, snap mission.Snapshot) {
	if s.messenger == nil {
		return
	}

	for _, ms := range snap.Missions {
		if !s.holds(ms) || !ms.Running() || ms.Pin != s.self || ms.TmuxSession != "" ||
			!ms.HasBadge(mission.BadgeHandoff) || ms.LaunchError != "" || !mirrorComplete(ms) {
			continue
		}

		if _, err := s.relaunch(ctx, ms.ID, s.continuation(ms, "")); err != nil {
			s.warn("starting an agent for a mission sent here", "mission", ms.ID, "error", err)
			s.updateLease(ms.ID, "mission.adopt_failed", func(stored *mission.Mission) bool {
				stored.LaunchError = err.Error()

				return true
			})
		}
	}
}

// clearOpposingPin removes a pin that names the other host, so a mission taken
// here is not sent straight back.
func (s *Service) clearOpposingPin(id mission.MissionID) {
	ms, ok := s.store.Snapshot().Mission(id)
	if !ok || !s.pinnedElsewhere(ms) {
		return
	}

	if _, err := s.setPin(id, ""); err != nil {
		s.warn("clearing the pin of a mission taken here", "mission", id, "error", err)
	}
}
