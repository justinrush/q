package daemon

import (
	"context"
	"errors"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// memoryWitness is a witness two services in one process can share. Each gets
// its own view, so one can be cut off from it while the other is not.
type memoryWitness struct {
	mu      sync.Mutex
	claim   mission.Claim
	version int
}

// view is one host's way of reaching a memoryWitness.
type view struct {
	record *memoryWitness
	name   string
	// down makes every call fail, as a witness on an unreachable network does.
	down bool
	// raced makes the next write lose to a write that did not happen here.
	raced bool
}

func (v *view) Name() string { return v.name }

func (v *view) Read(context.Context) (mission.Claim, error) {
	if v.down {
		return mission.Claim{}, errors.New("witness unreachable")
	}

	v.record.mu.Lock()
	defer v.record.mu.Unlock()

	return v.record.claim, nil
}

func (v *view) Write(_ context.Context, claim mission.Claim) error {
	if v.down {
		return errors.New("witness unreachable")
	}

	v.record.mu.Lock()
	defer v.record.mu.Unlock()

	if v.raced {
		v.raced = false
		v.record.version++
		v.record.claim.Version = strconv.Itoa(v.record.version)
	}

	if claim.Version != v.record.claim.Version {
		return mission.ErrClaimRaced
	}

	v.record.version++
	claim.Version = strconv.Itoa(v.record.version)
	v.record.claim = claim

	return nil
}

// witnessedPair is a movable pair whose hosts consult one witness, already
// exchanged once so each knows the other does.
type witnessedPair struct {
	*movablePair

	record                     *memoryWitness
	fromPrimary, fromSecondary *view
}

func newWitnessedPair(t *testing.T) *witnessedPair {
	t.Helper()

	record := &memoryWitness{}
	p := &witnessedPair{
		movablePair:   newMovablePair(t),
		record:        record,
		fromPrimary:   &view{record: record, name: "test:pair"},
		fromSecondary: &view{record: record, name: "test:pair"},
	}

	p.primary.apply(WithWitness(p.fromPrimary))
	p.secondary.apply(WithWitness(p.fromSecondary))

	// One clock for both. A claim is judged by its reader against the time its
	// writer stamped, so the hosts are assumed to roughly agree what time it is.
	p.secondaryClock = p.primaryClock
	p.secondary.apply(WithClock(p.primaryClock.Now))

	exchange(t, p.primary)

	return p
}

// tick advances the shared clock in sync-interval steps, tending leases on the
// hosts named, as the scheduler would on each that is awake.
func (p *witnessedPair) tick(t *testing.T, d time.Duration, awake ...*Service) {
	t.Helper()

	for range int(d / DefaultSyncInterval) {
		p.primaryClock.advance(DefaultSyncInterval)

		for _, svc := range awake {
			svc.tendLeases(t.Context())
		}
	}
}

func (p *witnessedPair) holder() mission.HostID {
	p.record.mu.Lock()
	defer p.record.mu.Unlock()

	return p.record.claim.Holder
}

// session gives a mission on svc a session to stop.
func session(svc *Service, id mission.MissionID) {
	svc.updateLocal(id, "test.session", func(ms *mission.Mission) { ms.TmuxSession = "q-" + string(id) })
}

// The case a witness exists for. The laptop is awake and working, on a network
// the always-on machine cannot be reached from. Without a witness that is
// indistinguishable from a closed lid, and both would run the mission.
func TestASecondaryDoesNotTakeOverFromAPrimaryTheWitnessStillVouchesFor(t *testing.T) {
	p := newWitnessedPair(t)
	ms := runningOn(t, p.primary, "long job")
	session(p.primary, ms.ID)

	exchange(t, p.primary)
	mirrored(p.secondary, ms.ID)

	p.link.down = true
	p.tick(t, time.Hour, p.primary, p.secondary)

	if got := holder(p.secondary, ms.ID); got != p.primary.self {
		t.Errorf("holder = %s, want the mission left with a primary that is awake", got)
	}

	if len(p.onSecondary.relaunched) != 0 {
		t.Errorf("the secondary started %d agents for a primary that never went away", len(p.onSecondary.relaunched))
	}

	if len(p.onPrimary.stopped) != 0 {
		t.Errorf("the primary stopped %v while the witness vouched for it", p.onPrimary.stopped)
	}

	if got, _ := p.primary.Snapshot().Mission(ms.ID); got.HasLocalBadge(mission.BadgeUnconfirmed) {
		t.Error("a mission the witness vouches for is marked unconfirmed")
	}
}

// The lid closes. The claim lapses with nobody renewing it, the secondary
// replaces it and carries on, and the laptop then wakes somewhere it cannot
// reach the secondary from. It must not pick the mission back up.
func TestAPrimaryThatWakesCutOffStandsByUntilItHasSpokenToItsPeer(t *testing.T) {
	p := newWitnessedPair(t)
	ms := runningOn(t, p.primary, "long job")
	session(p.primary, ms.ID)

	queued, err := p.primary.CreateMission(api.CreateMissionRequest{
		OperationID: seedOperation(t, p.primary).ID, Name: "queued", Prompt: "do it", Queued: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	exchange(t, p.primary)
	mirrored(p.secondary, ms.ID)
	p.primary.tendLeases(t.Context())

	// Asleep: only the secondary's scheduler runs.
	p.link.down = true
	p.tick(t, 10*time.Minute, p.secondary)

	if got := holder(p.secondary, ms.ID); got != p.secondary.self {
		t.Fatalf("holder = %s, want the secondary to have taken over a sleeping primary's mission", got)
	}

	if got := p.holder(); got != p.secondary.self {
		t.Fatalf("the witness names %s, want the secondary", got)
	}

	// Awake, off the VPN: the witness answers and the secondary does not.
	p.tick(t, time.Minute, p.primary, p.secondary)
	p.primary.Schedule(t.Context())

	if !slices.Equal(p.onPrimary.stopped, []mission.MissionID{ms.ID}) {
		t.Fatalf("stopped %v, want the primary's agent stopped once", p.onPrimary.stopped)
	}

	stored, _ := p.primary.Snapshot().Mission(ms.ID)
	if !stored.HasLocalBadge(mission.BadgeStandby) || stored.TmuxSession != "" {
		t.Errorf("badges %v session %q, want the mission standing by with no session", stored.LocalBadges, stored.TmuxSession)
	}

	if got, _ := p.primary.Snapshot().Mission(queued.ID); got.Launched() {
		t.Error("a queued mission was started while standing by")
	}

	if status := p.primary.RemoteStatus().Witness; status == nil || !status.Standby || status.Holder != p.secondary.self {
		t.Errorf("witness status = %+v, want standing by behind the secondary", status)
	}

	// Back on the VPN. The exchange says the mission moved, so nothing is
	// restarted here, and the claim comes back.
	p.link.down = false
	exchange(t, p.primary)
	p.tick(t, DefaultSyncInterval, p.primary, p.secondary)

	stored, _ = p.primary.Snapshot().Mission(ms.ID)
	if stored.Lease.Holder != p.secondary.self || stored.HasLocalBadge(mission.BadgeStandby) {
		t.Errorf("lease %+v badges %v, want the takeover accepted and the standby over", stored.Lease, stored.LocalBadges)
	}

	if len(p.onPrimary.relaunched) != 0 {
		t.Errorf("the primary restarted %d agents for a mission its peer is running", len(p.onPrimary.relaunched))
	}

	if got := p.holder(); got != p.primary.self {
		t.Errorf("the witness names %s after the exchange, want the primary again", got)
	}
}

// A primary that can reach nothing cannot know, and the secondary will soon
// act on its silence. It stops first.
func TestAPrimaryThatCanReachNeitherStandsByBeforeTheSecondaryTakesOver(t *testing.T) {
	p := newWitnessedPair(t)
	ms := runningOn(t, p.primary, "long job")
	session(p.primary, ms.ID)

	exchange(t, p.primary)
	mirrored(p.secondary, ms.ID)
	p.primary.tendLeases(t.Context())

	p.link.down, p.fromPrimary.down = true, true

	var stoppedAt, takenAt time.Time

	for range 40 {
		p.tick(t, DefaultSyncInterval, p.primary, p.secondary)

		if stoppedAt.IsZero() && len(p.onPrimary.stopped) > 0 {
			stoppedAt = p.primaryClock.Now()
		}

		if takenAt.IsZero() && holder(p.secondary, ms.ID) == p.secondary.self {
			takenAt = p.primaryClock.Now()
		}
	}

	if stoppedAt.IsZero() || takenAt.IsZero() {
		t.Fatalf("stopped at %v, taken at %v; want both to have happened", stoppedAt, takenAt)
	}

	if !stoppedAt.Before(takenAt) {
		t.Errorf("the primary stopped at %v and the secondary took over at %v; the stop must come first", stoppedAt, takenAt)
	}
}

// Standing by is a precaution, and sometimes nothing happened: the secondary
// was off too, or had no room. The agent is then started again where it was.
func TestAMissionNobodyTookIsRestartedWhenTheStandbyEnds(t *testing.T) {
	p := newWitnessedPair(t)
	ms := runningOn(t, p.primary, "long job")
	session(p.primary, ms.ID)

	pinned := runningOn(t, p.primary, "pinned here")
	session(p.primary, pinned.ID)
	p.primary.updateLease(pinned.ID, "test.pin", func(stored *mission.Mission) bool {
		stored.Pin = p.primary.self

		return true
	})

	exchange(t, p.primary)
	p.primary.tendLeases(t.Context())

	// Everything unreachable for an hour, and the secondary's daemon not running.
	p.link.down, p.fromPrimary.down = true, true
	p.tick(t, time.Hour, p.primary)

	if !slices.Equal(p.onPrimary.stopped, []mission.MissionID{ms.ID}) {
		t.Fatalf("stopped %v, want only the mission the secondary could have taken", p.onPrimary.stopped)
	}

	// A mission started by hand while standing by is the human's call.
	manual := briefOn(t, p.primary, "by hand")
	if _, err := p.primary.Start(t.Context(), manual.ID); err != nil {
		t.Fatalf("a manual start was refused while standing by: %v", err)
	}

	session(p.primary, manual.ID)
	p.tick(t, time.Minute, p.primary)

	if len(p.onPrimary.stopped) != 1 {
		t.Errorf("stopped %v, want the mission started by hand left running", p.onPrimary.stopped)
	}

	// The witness comes back and still names the primary.
	p.fromPrimary.down = false
	p.tick(t, DefaultSyncInterval, p.primary)

	if len(p.onPrimary.relaunched) != 1 || !strings.Contains(p.onPrimary.relaunched[0], "nothing else worked on this mission") {
		t.Fatalf("relaunched %q, want the one stopped agent restarted and told why", p.onPrimary.relaunched)
	}

	stored, _ := p.primary.Snapshot().Mission(ms.ID)
	if stored.HasLocalBadge(mission.BadgeStandby) || stored.TmuxSession == "" || stored.Status != mission.StatusActive {
		t.Errorf("badges %v session %q status %q, want it running again", stored.LocalBadges, stored.TmuxSession, stored.Status)
	}
}

// With a witness configured, a witness that cannot be reached is not consent.
func TestASecondaryThatCannotReachTheWitnessDoesNotTakeOver(t *testing.T) {
	p := newWitnessedPair(t)
	ms := runningOn(t, p.primary, "long job")

	queued, err := p.primary.CreateMission(api.CreateMissionRequest{
		OperationID: seedOperation(t, p.primary).ID, Name: "queued", Prompt: "do it", Queued: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	exchange(t, p.primary)
	mirrored(p.secondary, ms.ID)

	p.link.down, p.fromSecondary.down = true, true
	p.tick(t, time.Hour, p.secondary)
	p.secondary.Schedule(t.Context())

	if got := holder(p.secondary, ms.ID); got != p.primary.self {
		t.Errorf("holder = %s, want the mission left alone", got)
	}

	if got, _ := p.secondary.Snapshot().Mission(queued.ID); got.Launched() {
		t.Error("a queued mission was started without the witness's say")
	}

	if status := p.secondary.RemoteStatus().Witness; status == nil || status.Error == "" {
		t.Errorf("witness status = %+v, want the failure reported", status)
	}

	// Reachable again: the primary's claim has long lapsed.
	p.fromSecondary.down = false
	p.tick(t, DefaultSyncInterval, p.secondary)
	p.secondary.Schedule(t.Context())

	if got := holder(p.secondary, ms.ID); got != p.secondary.self {
		t.Errorf("holder = %s, want the takeover once the witness answered", got)
	}

	if got, _ := p.secondary.Snapshot().Mission(queued.ID); !got.Launched() {
		t.Error("the queued mission was not started once the primary was known to be away")
	}
}

// Each host is configured separately. If only one consults the witness, or
// they consult different ones, the check protects nothing, and the secondary
// takes nothing rather than rely on it.
func TestAPairThatDisagreesAboutTheWitnessNeverTakesOver(t *testing.T) {
	for name, configure := range map[string]func(p *witnessedPair){
		"only the primary has one":   func(p *witnessedPair) { p.secondary.witness = nil },
		"only the secondary has one": func(p *witnessedPair) { p.primary.witness = nil },
		"they name different ones":   func(p *witnessedPair) { p.fromSecondary.name = "test:other" },
	} {
		t.Run(name, func(t *testing.T) {
			p := newWitnessedPair(t)
			configure(p)

			ms := runningOn(t, p.primary, "long job")

			exchange(t, p.primary)
			mirrored(p.secondary, ms.ID)

			if status := p.secondary.RemoteStatus().Witness; status == nil || !status.Mismatched() {
				t.Fatalf("witness status = %+v, want the mismatch reported", status)
			}

			p.link.down = true
			p.tick(t, time.Hour, p.secondary)

			if got := holder(p.secondary, ms.ID); got != p.primary.self {
				t.Errorf("holder = %s, want no takeover", got)
			}
		})
	}
}

// Both hosts write the one record, and a write that loses is read again
// rather than assumed to have won.
func TestAClaimThatLosesARaceIsDecidedAgainstWhatWon(t *testing.T) {
	p := newWitnessedPair(t)

	p.fromPrimary.raced = true

	claim, granted, err := p.primary.claim(t.Context(), time.Minute, false)
	if err != nil || !granted || claim.Holder != p.primary.self {
		t.Fatalf("claim = %+v granted %v err %v, want it granted on the second attempt", claim, granted, err)
	}

	// A standing claim by the other host is not taken without force.
	if _, granted, err := p.secondary.claim(t.Context(), 0, false); err != nil || granted {
		t.Errorf("granted %v err %v, want the secondary refused while the primary's claim stands", granted, err)
	}

	if _, granted, err := p.secondary.claim(t.Context(), 0, true); err != nil || !granted {
		t.Errorf("granted %v err %v, want a forced claim to take it", granted, err)
	}
}

// The monotonic clock stops while a laptop sleeps. Judged by it, a claim made
// before a night's sleep would look seconds old on waking.
func TestIntervalsCountTimeSpentAsleep(t *testing.T) {
	slept := time.Now()
	woke := slept.Add(time.Second).Round(0).Add(8 * time.Hour)

	if got := wall(woke).Sub(wall(slept)); got < 8*time.Hour {
		t.Errorf("elapsed = %s, want the sleep counted", got)
	}
}
