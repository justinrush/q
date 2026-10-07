package daemon

import (
	"context"
	"time"

	"github.com/justinrush/q/internal/mission"
)

// Scheduling defaults.
const (
	// ScheduleInterval is how often the daemon looks for a queued mission it can
	// start. A freed slot is noticed within this long, which is short against the
	// minutes a mission runs for and long against the cost of looking.
	ScheduleInterval = 5 * time.Second
	// DefaultMaxConcurrent is how many missions run at once when the user has
	// not said. Two is enough to make a queue worth having and few enough that a
	// laptop stays usable underneath it.
	DefaultMaxConcurrent = 2
)

// WithMaxConcurrent sets how many queued missions may be active on this host at
// once. A value below one leaves the default in place.
func WithMaxConcurrent(n int) Option {
	return func(s *Service) {
		if n > 0 {
			s.maxConcurrent = n
		}
	}
}

// RunScheduler starts queued missions as slots free up, until ctx is canceled.
//
// This is the part of q that makes "queued" mean something. Without it a brief
// in the briefing lane waits for a human, which is the right default and the
// wrong one for the machine nobody is sitting at.
func (s *Service) RunScheduler(ctx context.Context) {
	ticker := time.NewTicker(ScheduleInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			s.Schedule(ctx)
		}
	}
}

// Schedule makes one pass over the queue, starting what this host should run
// and passing along what it should not.
//
// Missions start in board order, which is the order a human arranged them in
// and the only statement of priority q has.
func (s *Service) Schedule(ctx context.Context) {
	if s.launcher == nil {
		return
	}

	snap := s.store.Snapshot()
	now := s.now()

	// A primary that has not heard from its peer for as long as the peer waits
	// before taking over cannot tell whether a queued mission is already
	// running there. Starting it anyway is how one brief gets two agents.
	if s.unconfirmed(snap, now) {
		return
	}

	free := s.maxConcurrent - s.activeHere(snap)

	for _, ms := range snap.MissionsInLane(mission.StatusBriefing) {
		if !ms.Queued || ms.Launched() {
			continue
		}

		runner := s.runnerFor(snap, ms, now)

		if runner != s.self {
			s.passLease(ms, runner)

			continue
		}

		if free <= 0 {
			continue
		}

		if s.startQueued(ctx, ms) {
			free--
		}
	}
}

// activeHere counts the missions occupying a slot on this host.
//
// Only the active lane counts. A mission waiting on the human or sitting in
// debrief has an agent that is idle, and holding a slot for it would stall the
// queue behind the one thing a queue is meant to survive: nobody being there to
// answer.
func (s *Service) activeHere(snap mission.Snapshot) int {
	n := 0

	for _, ms := range snap.Missions {
		if ms.Status == mission.StatusActive && s.holds(ms) {
			n++
		}
	}

	return n
}

// runnerFor decides which host should start a queued mission.
//
// A pin is an instruction and is followed. Otherwise the primary runs it,
// because that is the machine with the power and the human; the secondary runs
// it only when the primary has been gone long enough to be considered away.
func (s *Service) runnerFor(snap mission.Snapshot, ms mission.Mission, now time.Time) mission.HostID {
	if ms.Pin == s.self || (ms.Pin != "" && ms.Pin == snap.PeerID()) {
		return ms.Pin
	}

	if s.roleOf(snap) == mission.RoleSecondary && s.peerSeen(snap, now) {
		return snap.Peer.ID
	}

	return s.self
}

// unconfirmed reports whether this host is a primary whose peer may have taken
// over its missions without it knowing.
func (s *Service) unconfirmed(snap mission.Snapshot, now time.Time) bool {
	return s.roleOf(snap) == mission.RolePrimary && !s.peerSeen(snap, now)
}

// passLease hands an unlaunched mission to the host that should run it.
//
// Only a mission this host holds is passed. One the other host already holds
// is where it should be.
func (s *Service) passLease(ms mission.Mission, to mission.HostID) {
	if !s.holds(ms) {
		return
	}

	s.updateLease(ms.ID, "mission.lease.pass", func(stored *mission.Mission) bool {
		if stored.Launched() || !s.holds(*stored) {
			return false
		}

		stored.Lease = stored.Lease.Take(to)

		return true
	})
}

// startQueued launches one queued mission, reporting whether it took a slot.
//
// The queued flag is cleared before the launch rather than after it. A mission
// that cannot start records why on its card, and one that stayed queued would
// be retried every few seconds forever, each attempt a fetch and a worktree.
func (s *Service) startQueued(ctx context.Context, ms mission.Mission) bool {
	claimed := s.updateLease(ms.ID, "mission.dequeue", func(stored *mission.Mission) bool {
		if !stored.Queued || stored.Launched() {
			return false
		}

		stored.Queued = false

		if !s.holds(*stored) {
			stored.Lease = stored.Lease.Take(s.self)
		}

		return true
	})
	if !claimed {
		return false
	}

	if _, err := s.Start(ctx, ms.ID); err != nil {
		s.warn("starting a queued mission", "mission", ms.ID, "error", err)

		return false
	}

	return true
}

// updateLease applies fn to a stored mission and publishes the result,
// reporting whether fn changed anything.
func (s *Service) updateLease(id mission.MissionID, label string, fn func(*mission.Mission) bool) bool {
	var (
		updated mission.Mission
		changed bool
	)

	err := s.store.Mutate(label, func(snap *mission.Snapshot) error {
		stored, ok := snap.Mission(id)
		if !ok || !fn(&stored) {
			return nil
		}

		stored.UpdatedAt = s.now()
		updated, changed = stored, true
		snap.PutMission(stored)

		return nil
	})
	if err != nil {
		s.warn("updating a mission lease", "mission", id, "error", err)

		return false
	}

	if changed {
		s.publishMission(updated)
	}

	return changed
}
