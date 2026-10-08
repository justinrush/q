// Telling a silent peer from an absent one.
//
// The lease rules act on silence: a secondary that has not heard from its
// primary for long enough runs the primary's missions. Silence is all two
// machines can observe of each other, and it has two causes that call for
// opposite responses. A laptop with its lid closed should be covered for. A
// laptop that is awake on a network the secondary is not on should not be, and
// neither should a laptop that has just woken there carry on with missions the
// secondary took while it slept.
//
// A witness is a third place both machines can reach when they cannot reach
// each other. It holds one claim. The primary renews it for as long as it is
// awake, and the secondary may only act on the primary's silence once that
// claim has lapsed and it has replaced it with its own. A primary that finds
// the secondary's claim there, or cannot reach the witness to look, stands by:
// it stops the agents the secondary would have taken over and starts nothing
// until it has spoken to the secondary again.
//
// Without a witness none of this file applies and the rules are as they were.

package daemon

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

const (
	// witnessTimeout bounds one conversation with the witness. It runs on the
	// scheduler's tick, so an unreachable witness delays the queue by this much
	// once per sync interval and no more.
	witnessTimeout = 10 * time.Second
	// claimAttempts is how many times a claim is retried when the record moves
	// between the read and the write. Only two hosts ever write it, so losing
	// more than once means something else is wrong.
	claimAttempts = 3
	// claimMarginDivisor sets the primary's safety margin as a fraction of the
	// takeover window: a tenth. See [claimMargin].
	claimMarginDivisor = 10
)

// Witness keeps the pair's one [mission.Claim] somewhere both hosts can reach.
//
// It is a record with a conditional write and nothing else. The rules for who
// may claim, and when, are the daemon's and are the same whatever keeps the
// record, so an implementation is only the storage: a Kubernetes Lease, a blob
// in a storage account.
type Witness interface {
	// Name identifies the record, in a form two hosts configured separately can
	// compare to learn whether they mean the same one.
	Name() string
	// Read returns the claim as it stands, the zero Claim when there is none.
	Read(ctx context.Context) (mission.Claim, error)
	// Write replaces the claim, provided the record is still at the version the
	// claim carries. It returns [mission.ErrClaimRaced] when it is not.
	Write(ctx context.Context, claim mission.Claim) error
}

// WithWitness gives the daemon a witness to consult. Both hosts of a pair need
// one, naming the same record, or neither does.
func WithWitness(w Witness) Option {
	return func(s *Service) { s.witness = w }
}

// vouch is what this daemon remembers of its dealings with the witness.
//
// Like [link] it is held in memory, and a restart resolves the doubt the same
// way: a daemon that has not yet asked has not been vouched for.
type vouch struct {
	mu sync.Mutex
	// asked is when the witness was last asked, whatever it said.
	asked time.Time
	// until is how long the primary may rely on its last successful claim.
	until time.Time
	// held reports that the witness named this host when it last answered.
	held bool
	// holder is who the witness named when it last answered.
	holder mission.HostID
	err    string
	// standby reports that this primary has stopped its agents and is waiting.
	standby bool
}

// due reports whether enough time has passed to ask again.
func (v *vouch) due(now time.Time, every time.Duration) bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.asked.IsZero() || now.Sub(v.asked) >= every
}

// answered records what the witness said.
func (v *vouch) answered(now time.Time, claim mission.Claim, granted bool, until time.Time) {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.asked, v.err = now, ""
	v.held, v.holder = granted, claim.Holder

	if granted {
		v.until = until
	} else {
		v.until = time.Time{}
	}
}

// failed records that the witness could not be asked, reporting whether that
// is news. What it last said is kept: a claim does not lapse sooner because
// the witness went quiet, only at the time it was always going to.
func (v *vouch) failed(now time.Time, err error) bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	changed := v.err != err.Error()
	v.asked, v.err = now, err.Error()

	return changed
}

// valid reports whether the primary's own claim still stands.
func (v *vouch) valid(now time.Time) bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	return now.Before(v.until)
}

// holding reports whether the witness last named this host.
func (v *vouch) holding() bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	return v.held
}

// forget drops what the witness last said, so the next question is asked fresh.
func (v *vouch) forget() {
	v.mu.Lock()
	defer v.mu.Unlock()

	v.asked, v.until, v.held = time.Time{}, time.Time{}, false
}

// setStandby records whether this host is standing by, reporting a change.
func (v *vouch) setStandby(standby bool) bool {
	v.mu.Lock()
	defer v.mu.Unlock()

	changed := v.standby != standby
	v.standby = standby

	return changed
}

// wall strips a time's monotonic reading.
//
// Every interval here has to count time spent asleep, because a closed lid is
// the case being decided. Go subtracts two times by their monotonic readings
// when both have one, and that clock stops while the machine sleeps: a laptop
// waking after a night would find its claim, by that arithmetic, seconds old.
func wall(t time.Time) time.Time { return t.Round(0) }

// Claim asks a witness to name holder, for ttl from now.
//
// It is granted when the record is free, lapsed, or already holder's. force
// takes it from another host whose claim still stands, and is only for a
// caller with other grounds to know that host agrees, or a person overriding
// it. The claim returned is the one written when granted and the one that
// stood in the way when not.
func Claim(
	ctx context.Context,
	w Witness,
	holder mission.HostID,
	now func() time.Time,
	ttl time.Duration,
	force bool,
) (mission.Claim, bool, error) {
	for range claimAttempts {
		current, err := w.Read(ctx)
		if err != nil {
			return mission.Claim{}, false, err
		}

		at := wall(now()).UTC()

		if current.Holder != holder && !current.Expired(at) && !force {
			return current, false, nil
		}

		next := mission.Claim{Holder: holder, RenewedAt: at, TTL: ttl, Version: current.Version}

		err = w.Write(ctx, next)
		if errors.Is(err, mission.ErrClaimRaced) {
			continue
		}

		if err != nil {
			return mission.Claim{}, false, err
		}

		return next, true, nil
	}

	return mission.Claim{}, false, fmt.Errorf("the witness's record changed %d times while it was being claimed", claimAttempts)
}

// claim makes this host's claim, bounded so a witness that does not answer
// cannot hold up the tick it is asked on.
func (s *Service) claim(ctx context.Context, ttl time.Duration, force bool) (mission.Claim, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, witnessTimeout)
	defer cancel()

	return Claim(ctx, s.witness, s.self, s.now, ttl, force)
}

// ask makes one claim and records the outcome, at most once per sync interval.
func (s *Service) ask(ctx context.Context, now time.Time, ttl time.Duration, force bool) {
	if !s.vouch.due(now, s.syncInterval) {
		return
	}

	claim, granted, err := s.claim(ctx, ttl, force)
	if err != nil {
		if s.vouch.failed(now, err) {
			s.warn("asking the witness", "witness", s.witness.Name(), "error", err)
			s.publishRemote()
		}

		return
	}

	until := claim.RenewedAt.Add(ttl - claimMargin(ttl))

	wasHeld, hadErr := s.vouch.holding(), s.vouchError() != ""
	s.vouch.answered(now, claim, granted, until)

	if wasHeld != granted || hadErr {
		s.publishRemote()
	}
}

// vouchError returns why the witness could not last be asked.
func (s *Service) vouchError() string {
	s.vouch.mu.Lock()
	defer s.vouch.mu.Unlock()

	return s.vouch.err
}

// witnessed reports whether this pair settles silence through a witness: this
// host has one and its peer names the same one.
func (s *Service) witnessed(snap mission.Snapshot) bool {
	return s.witness != nil && snap.Peer != nil && snap.Peer.Witness == s.witness.Name()
}

// keepClaim is the primary's side of the witness, run on every tick: renew the
// claim, and report whether this host has to stand by.
//
// A primary may run its missions while either thing is true: it has spoken to
// the secondary within the window, or the witness still names it. The first is
// direct knowledge of what the secondary holds. The second is the secondary's
// own precondition for taking anything, seen from the other side.
//
// Having just spoken to the secondary is also what entitles the primary to
// take the claim back from it. The secondary's claim says "talk to me before
// you run anything", and the primary has.
func (s *Service) keepClaim(ctx context.Context, snap mission.Snapshot, now time.Time, seen bool) bool {
	if !s.witnessed(snap) {
		return false
	}

	s.ask(ctx, now, s.takeoverAfter, seen)

	// The exchange is trusted for a little less than the window, for the same
	// reason the claim is: the secondary acts when the window closes, and this
	// host has to have stopped by then, not be stopping.
	sure := s.peerSeenWithin(snap, now, s.takeoverAfter-claimMargin(s.takeoverAfter))

	return !sure && !s.vouch.valid(now)
}

// claimMargin is how much earlier than the secondary could act the primary
// stops relying on what it last knew. The secondary judges by its own clock,
// and the two are never exactly agreed.
func claimMargin(window time.Duration) time.Duration { return window / claimMarginDivisor }

// primaryAway is the secondary's side: whether the primary's silence may be
// acted on.
//
// Without a witness silence is all there is, and the window having passed is
// the answer. With one, the secondary must also hold the claim, which it can
// only come to do once the primary has stopped renewing its own. Its claim
// carries no expiry: it stands until the primary has exchanged and taken it
// back, because what it protects is the primary not resuming blind.
//
// A pair that disagrees about the witness is never away. One side would be
// relying on a check the other is not making.
func (s *Service) primaryAway(ctx context.Context, snap mission.Snapshot, now time.Time) bool {
	if s.peerSeen(snap, now) {
		s.vouch.forget()

		return false
	}

	if s.witness == nil {
		return snap.Peer != nil && snap.Peer.Witness == ""
	}

	if !s.witnessed(snap) {
		return false
	}

	if !s.vouch.holding() {
		s.ask(ctx, wall(now), 0, false)
	}

	return s.vouch.holding()
}

// away is primaryAway as last decided, for the scheduler, which runs straight
// after the leases are tended and must not talk to the witness itself.
func (s *Service) away(snap mission.Snapshot, now time.Time) bool {
	if s.peerSeen(snap, now) {
		return false
	}

	if s.witness == nil {
		return snap.Peer != nil && snap.Peer.Witness == ""
	}

	return s.witnessed(snap) && s.vouch.holding()
}

// tendStandby moves the primary into or out of standing by.
//
// Entering is acted on once, at the moment it happens. A mission a person
// starts or resumes by hand while this host stands by is their decision, made
// knowing what the card says, and is not stopped again a tick later.
func (s *Service) tendStandby(ctx context.Context, standby bool) {
	changed := s.vouch.setStandby(standby)

	if standby {
		if changed {
			s.logger.Info("standing by: neither the paired q nor the witness vouches for this host")
			s.standBy(ctx)
			s.publishRemote()
		}

		return
	}

	if changed {
		s.logger.Info("no longer standing by")
		s.publishRemote()
	}

	s.standUp(ctx)
}

// standBy stops the agents the secondary would take over.
//
// The set is the secondary's own: missions mid-turn that are not pinned here.
// A mission waiting on a person has no agent doing anything, and a pinned one
// is never taken. The agent's session id is kept, as in [Service.standDown],
// so the conversation is there to resume if the mission turns out not to have
// moved.
func (s *Service) standBy(ctx context.Context) {
	for _, ms := range s.store.Snapshot().MissionsInLane(mission.StatusActive) {
		if !s.holds(ms) || !ms.Running() || ms.Pin == s.self {
			continue
		}

		if s.messenger != nil && ms.TmuxSession != "" {
			if err := s.messenger.Stop(ctx, ms); err != nil {
				s.warn("stopping an agent to stand by", "mission", ms.ID, "error", err)

				continue
			}
		}

		s.updateLocal(ms.ID, "mission.standby", func(stored *mission.Mission) {
			stored.TmuxSession = ""
			stored.AgentPaneID = ""
			stored.HookEpoch++
			stored.LocalBadges = stored.WithLocalBadge(mission.BadgeStandby, "")
		})
	}
}

// standUp restarts the agents standing by stopped, for the missions that are
// still this host's.
//
// The snapshot is taken here and not by the caller. What ends a standby is
// usually an exchange, and the exchange is also what says which missions the
// secondary took; reading the store after it has committed is what keeps an
// agent from being started for a mission the same exchange gave away.
func (s *Service) standUp(ctx context.Context) {
	for _, ms := range s.store.Snapshot().Missions {
		if !ms.HasLocalBadge(mission.BadgeStandby) {
			continue
		}

		if !s.holds(ms) || !ms.Running() || ms.TmuxSession != "" || s.messenger == nil {
			s.updateLocal(ms.ID, "mission.standby.over", func(stored *mission.Mission) {
				stored.LocalBadges = stored.WithoutLocalBadge(mission.BadgeStandby)
			})

			continue
		}

		_, err := s.relaunch(ctx, ms.ID, mission.Resumption())
		if err != nil {
			s.warn("restarting an agent after standing by", "mission", ms.ID, "error", err)
		}

		s.updateLease(ms.ID, "mission.standby.over", func(stored *mission.Mission) bool {
			stored.LocalBadges = stored.WithoutLocalBadge(mission.BadgeStandby)

			if err != nil {
				stored.LaunchError = err.Error()
				orphan(stored, s.now())
			}

			return true
		})
	}
}

// witnessStatus reports the witness for q remote status, nil when the pair has
// nothing to do with one.
func (s *Service) witnessStatus(snap mission.Snapshot) *api.WitnessStatus {
	status := api.WitnessStatus{}

	if s.witness != nil {
		status.Name = s.witness.Name()
	}

	if snap.Peer != nil {
		status.PeerName = snap.Peer.Witness
	}

	if status.Name == "" && status.PeerName == "" {
		return nil
	}

	s.vouch.mu.Lock()
	defer s.vouch.mu.Unlock()

	status.Holder = s.vouch.holder
	status.Standby = s.vouch.standby
	status.AskedAt = s.vouch.asked
	status.Error = s.vouch.err

	return &status
}
