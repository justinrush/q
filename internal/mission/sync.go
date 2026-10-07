package mission

import (
	"slices"
	"time"
)

// Payload is one host's view of everything it shares with its peer.
//
// It is the whole shared state rather than a delta. The state of a busy board
// is tens of kilobytes, an exchange happens a few times a minute, and a full
// copy cannot drift the way a missed delta can.
type Payload struct {
	From       HostInfo    `json:"from"`
	Operations []Operation `json:"operations,omitempty"`
	Missions   []Mission   `json:"missions,omitempty"`
	Tombstones []Tombstone `json:"tombstones,omitempty"`
}

// Payload projects the snapshot for the peer, with everything host-local
// cleared.
func (s Snapshot) Payload() Payload {
	out := Payload{From: s.Self, Tombstones: slices.Clone(s.Tombstones)}

	for _, op := range s.Operations {
		out.Operations = append(out.Operations, op.Shared())
	}

	for _, ms := range s.Missions {
		out.Missions = append(out.Missions, ms.Shared())
	}

	return out
}

// Command is a request for a mission the peer is running.
//
// A host that does not hold a mission's lease must not change its run: it has
// no agent to deliver a message to and no session to stop. It asks the holder
// instead. When the holder is reachable the request is answered at once; when
// it is not, the request waits here and rides along with the next exchange.
type Command struct {
	// ID makes delivery idempotent, since an exchange that failed after the peer
	// acted on it will be retried.
	ID      string    `json:"id"`
	Mission MissionID `json:"mission"`
	// To is the lane to move the mission to, as in a status request.
	To Status `json:"to"`
	// Message accompanies a move to active, and is what resumes an agent.
	Message string    `json:"message,omitempty"`
	At      time.Time `json:"at"`
}

// Merge reports what applying a peer's payload changed, so the daemon can
// publish it and act on the parts that have consequences outside the store.
type Merge struct {
	// Operations and Missions are the records that now differ.
	Operations []OperationID
	Missions   []MissionID
	// RemovedOperations and Removed are the records the peer deleted. Removed
	// carries each mission as it was, because the record is the only thing that
	// says which worktrees on this host belonged to it.
	RemovedOperations []OperationID
	Removed           []Mission
	// Lost are missions this host held and no longer does; an agent it is
	// running for one of them must stop. Gained are the reverse.
	Lost   []MissionID
	Gained []MissionID
}

// Changed reports whether the merge altered anything.
func (m Merge) Changed() bool {
	return len(m.Operations)+len(m.Missions)+len(m.RemovedOperations)+len(m.Removed) > 0
}

// MergePeer folds the peer's shared state into this snapshot.
//
// The rules are the whole of q's conflict resolution, and there are only three.
// A brief or an operation carries a revision counter: the higher wins. A run
// belongs to whoever holds the lease, and a lease carries an epoch: the higher
// wins. Where either comparison ties, the primary wins. None of them consults a
// clock, so two machines that disagree about the time still agree about the
// outcome.
//
// primary says whether this host is the one that wins those ties.
func (s *Snapshot) MergePeer(p Payload, primary bool, now time.Time) Merge {
	var out Merge

	s.mergeTombstones(p.Tombstones, now, &out)

	for _, in := range p.Operations {
		if s.mergeOperation(in, primary) {
			out.Operations = append(out.Operations, in.ID)
		}
	}

	for _, in := range p.Missions {
		s.mergeMission(in, primary, &out)
	}

	return out
}

// mergeTombstones applies the peer's deletes.
func (s *Snapshot) mergeTombstones(tombstones []Tombstone, now time.Time, out *Merge) {
	for _, t := range tombstones {
		switch t.Kind {
		case TombstoneOperation:
			if s.DeleteOperation(OperationID(t.ID)) {
				out.RemovedOperations = append(out.RemovedOperations, OperationID(t.ID))
			}
		case TombstoneMission:
			if ms, ok := s.Mission(MissionID(t.ID)); ok {
				s.DeleteMission(ms.ID)
				out.Removed = append(out.Removed, ms)
			}
		}

		s.bury(t.Kind, t.ID, now)
	}
}

// mergeOperation applies one of the peer's operations, reporting a change.
func (s *Snapshot) mergeOperation(in Operation, primary bool) bool {
	local, ok := s.Operation(in.ID)
	if !ok {
		// A delete on this side beats an edit on the other. The peer hears of it
		// through the tombstone on the way back.
		if s.Tombstoned(TombstoneOperation, string(in.ID)) {
			return false
		}

		s.PutOperation(in.Shared())

		return true
	}

	if !incomingWins(local.Rev, in.Rev, primary) || operationEqual(local, in) {
		if in.Rev > local.Rev {
			local.Rev = in.Rev
			s.PutOperation(local)
		}

		return false
	}

	repos := adoptRepos(local.Repos, in.Repos)

	copyClass(&local, in, classSpec)
	local.Repos = repos
	local.Rev = in.Rev

	s.PutOperation(local)

	return true
}

// incomingWins applies the revision rule: higher wins, the primary wins a tie.
func incomingWins(localRev, incomingRev int, primary bool) bool {
	if incomingRev != localRev {
		return incomingRev > localRev
	}

	return !primary
}

// mergeMission applies one of the peer's missions.
func (s *Snapshot) mergeMission(in Mission, primary bool, out *Merge) {
	local, ok := s.Mission(in.ID)
	if !ok {
		if s.Tombstoned(TombstoneMission, string(in.ID)) {
			return
		}

		s.PutMission(in.Shared())
		out.Missions = append(out.Missions, in.ID)

		if in.Lease.Holder == s.Self.ID {
			out.Gained = append(out.Gained, in.ID)
		}

		return
	}

	merged := cloneMission(local)
	known := s.knownRepos(local)

	if incomingWins(local.SpecRev, in.SpecRev, primary) && !specEqual(local, in) {
		adoptSpec(&merged, in, known)
	} else if in.SpecRev > merged.SpecRev {
		merged.SpecRev = in.SpecRev
	}

	if takeRun(local.Lease, in.Lease, s.Self.ID, primary) {
		merged.Lease = in.Lease
		adoptRun(&merged, in, known)
	}

	held, holds := local.Lease.HeldBy(s.Self.ID), merged.Lease.HeldBy(s.Self.ID)

	switch {
	case held && !holds:
		out.Lost = append(out.Lost, in.ID)
	case !held && holds:
		out.Gained = append(out.Gained, in.ID)
	}

	if merged.Lease == local.Lease && merged.SpecRev == local.SpecRev &&
		specEqual(local, merged) && runEqual(local, merged) {
		return
	}

	s.PutMission(merged)
	out.Missions = append(out.Missions, in.ID)
}

// takeRun decides whether the peer's copy of a mission's run replaces this
// host's.
//
// When the two agree on the lease, the run belongs to whoever it names, so the
// peer's copy is taken exactly when the peer is the holder. When they disagree,
// the lease itself is in dispute: the higher epoch wins, the primary wins a
// tie, and the run that travels with the winning lease is the one that stands,
// because it was written by whoever most recently had the right to.
func takeRun(local, incoming Lease, self HostID, primary bool) bool {
	if local == incoming {
		return !local.HeldBy(self)
	}

	if incoming.Epoch != local.Epoch {
		return incoming.Epoch > local.Epoch
	}

	return !primary
}

// knownRepos gathers every repository this host has a path for that a mission
// might name, so an incoming list can be matched back to local checkouts.
func (s Snapshot) knownRepos(ms Mission) []Repo {
	known := slices.Concat(ms.LaunchRepos, ms.ExtraRepos)

	if op, ok := s.Operation(ms.OperationID); ok {
		known = append(known, op.Repos...)
	}

	return known
}

// stamp records, on a snapshot about to be persisted, the bookkeeping a peer
// needs to make sense of what changed: revisions, leases, and deletes.
//
// It runs inside every ordinary mutation and compares the state before with the
// state after, rather than being called from each place that edits something.
// There are dozens of those and there will be more; a rule that depends on all
// of them remembering to bump a counter is a rule that will be broken.
func stamp(old Snapshot, next *Snapshot, now time.Time) {
	stampOperations(old, next, now)
	stampMissions(old, next, now)
	next.expireTombstones(now)
}

// stampOperations bumps the revision of every edited operation and buries the
// deleted ones.
func stampOperations(old Snapshot, next *Snapshot, now time.Time) {
	for i := range next.Operations {
		op := &next.Operations[i]

		before, existed := old.Operation(op.ID)

		switch {
		case !existed:
			op.Rev = max(op.Rev, 1)
		case !operationEqual(before, *op):
			op.Rev = before.Rev + 1
		}
	}

	for _, before := range old.Operations {
		if _, ok := next.Operation(before.ID); !ok {
			next.bury(TombstoneOperation, string(before.ID), now)
		}
	}
}

// stampMissions does the same for missions, and settles who holds each one.
func stampMissions(old Snapshot, next *Snapshot, now time.Time) {
	self := next.Self.ID

	for i := range next.Missions {
		ms := &next.Missions[i]

		before, existed := old.Mission(ms.ID)
		if !existed {
			ms.SpecRev = max(ms.SpecRev, 1)

			if ms.Lease.Holder == "" {
				ms.Lease = Lease{Holder: self, Epoch: 1}
			}

			continue
		}

		if !specEqual(before, *ms) {
			ms.SpecRev = before.SpecRev + 1
		}

		stampLease(before, ms, self)

		if ms.Lease.HeldBy(self) {
			ms.HolderSession = ms.TmuxSession
		}
	}

	for _, before := range old.Missions {
		if _, ok := next.Mission(before.ID); !ok {
			next.bury(TombstoneMission, string(before.ID), now)
		}
	}
}

// stampLease makes a change to a mission's run consistent with its lease.
//
// A host that holds the lease may write the run freely. One that does not has
// two cases. If no agent is running anywhere — the mission is still a brief, or
// already closed — then writing the run is how this host comes to run it, and
// the lease moves here. If an agent is running on the peer, the write is
// discarded: this host has nothing to base it on, and letting it stand would
// either be overwritten by the next exchange or, worse, quietly steal a mission
// out from under a working agent.
//
// A mutation that changes the lease itself is taken at its word.
func stampLease(before Mission, ms *Mission, self HostID) {
	if before.Lease != ms.Lease {
		return
	}

	if before.Lease.HeldBy(self) {
		if ms.Lease.Holder == "" {
			ms.Lease = Lease{Holder: self, Epoch: max(ms.Lease.Epoch, 1)}
		}

		return
	}

	if runEqual(before, *ms) {
		return
	}

	if before.Running() {
		work := ms.Work
		copyClass(ms, cloneMission(before), classRun)
		ms.Work = adoptWork(work, before.Work)

		return
	}

	ms.Lease = before.Lease.Take(self)
}
