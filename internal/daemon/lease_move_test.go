package daemon

import (
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// testClock is a clock a test can move.
type testClock struct {
	mu  sync.Mutex
	now time.Time
}

func newTestClock() *testClock { return &testClock{now: time.Now()} }

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.now
}

func (c *testClock) advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.now = c.now.Add(d)
}

// movablePair is a pair whose hosts can both start and stop agents, with a
// clock each so one can be made to fall silent.
type movablePair struct {
	primary, secondary           *Service
	onPrimary, onSecondary       *recordingMessenger
	primaryClock, secondaryClock *testClock
	link                         *wire
}

func newMovablePair(t *testing.T) *movablePair {
	t.Helper()

	p := &movablePair{
		onPrimary: &recordingMessenger{}, onSecondary: &recordingMessenger{},
		primaryClock: newTestClock(), secondaryClock: newTestClock(),
	}

	p.primary, p.secondary, p.link = pairedServices(t)
	p.primary.apply(WithMessenger(p.onPrimary), WithClock(p.primaryClock.Now), WithTakeoverAfter(5*time.Minute))
	p.secondary.apply(WithMessenger(p.onSecondary), WithClock(p.secondaryClock.Now))

	return p
}

// mirrored pretends a mission's worktrees exist on svc, which is all the lease
// rules ask about them.
func mirrored(svc *Service, id mission.MissionID) {
	svc.updateLocal(id, "test.mirrored", func(ms *mission.Mission) { ms.MissionDir = "/mirror/" + string(id) })
}

// runningOn briefs and starts a mission on svc, mid-turn.
func runningOn(t *testing.T, svc *Service, name string) mission.Mission {
	t.Helper()

	ms := briefOn(t, svc, name)

	if _, err := svc.Start(t.Context(), ms.ID); err != nil {
		t.Fatalf("Start: %v", err)
	}

	svc.updateLease(ms.ID, "test.busy", func(stored *mission.Mission) bool {
		stored.AgentState = mission.AgentBusy

		return true
	})

	return ms
}

// endTurn puts a mission svc is running at its prompt, as a Stop would.
func endTurn(t *testing.T, svc *Service, id mission.MissionID, lastMessage string) {
	t.Helper()

	svc.updateLease(id, "test.turn_end", func(ms *mission.Mission) bool {
		ms.Status = mission.StatusDebrief
		ms.AgentState = mission.AgentIdle
		ms.TurnEnded = true
		ms.LastMessage = lastMessage

		return true
	})
}

func holder(svc *Service, id mission.MissionID) mission.HostID {
	ms, _ := svc.Snapshot().Mission(id)

	return ms.Lease.Holder
}

// The laptop closes mid-turn. After the window, the always-on machine carries
// the mission on.
func TestTheSecondaryTakesOverAMissionWhenThePrimaryGoesQuiet(t *testing.T) {
	p := newMovablePair(t)
	ms := runningOn(t, p.primary, "long job")

	exchange(t, p.primary)
	mirrored(p.secondary, ms.ID)

	// Still inside the window: the laptop is only between exchanges.
	p.secondaryClock.advance(4 * time.Minute)
	p.secondary.tendLeases(t.Context())

	if got := holder(p.secondary, ms.ID); got != p.primary.self {
		t.Fatalf("holder = %s before the window closed, want the primary still", got)
	}

	p.secondaryClock.advance(2 * time.Minute)
	p.secondary.tendLeases(t.Context())

	taken, _ := p.secondary.Snapshot().Mission(ms.ID)

	if taken.Lease.Holder != p.secondary.self || taken.Lease.Epoch != ms.Lease.Epoch+1 {
		t.Fatalf("lease = %+v, want it taken one epoch on", taken.Lease)
	}

	if len(p.onSecondary.relaunched) != 1 {
		t.Fatalf("relaunched %d agents, want one", len(p.onSecondary.relaunched))
	}

	// The new agent has the code and not the conversation, and must be told.
	if prompt := p.onSecondary.relaunched[0]; !strings.Contains(prompt, "laptop") || !strings.Contains(prompt, "git status") {
		t.Errorf("the continuation should name where the work came from and where to look:\n%s", prompt)
	}
}

// A laptop that was only asleep wakes with its agent still running. It must
// stand down rather than fight the machine that took over.
func TestThePrimaryStandsDownWhenItFindsItsMissionWasTaken(t *testing.T) {
	p := newMovablePair(t)
	ms := runningOn(t, p.primary, "long job")

	p.primary.updateLocal(ms.ID, "test.session", func(stored *mission.Mission) { stored.TmuxSession = "q-long-job" })

	exchange(t, p.primary)
	mirrored(p.secondary, ms.ID)

	p.secondaryClock.advance(10 * time.Minute)
	p.secondary.tendLeases(t.Context())

	exchange(t, p.primary)

	if got := holder(p.primary, ms.ID); got != p.secondary.self {
		t.Fatalf("holder on the primary = %s, want it to accept the takeover", got)
	}

	if !slices.Equal(p.onPrimary.stopped, []mission.MissionID{ms.ID}) {
		t.Errorf("stopped %v, want the primary's own agent stopped", p.onPrimary.stopped)
	}

	if stored, _ := p.primary.Snapshot().Mission(ms.ID); stored.TmuxSession != "" {
		t.Errorf("the primary still records session %q for a mission it no longer runs", stored.TmuxSession)
	}
}

// Missions waiting on a person, and ones pinned elsewhere, are not taken.
func TestTakeoverLeavesWhatItShouldNotRun(t *testing.T) {
	p := newMovablePair(t)

	waiting := runningOn(t, p.primary, "waiting on the human")
	endTurn(t, p.primary, waiting.ID, "what next?")

	pinned := runningOn(t, p.primary, "pinned to the laptop")
	local := "local"

	// Pinned before launch, as the API requires.
	unlaunched := briefOn(t, p.primary, "pinned brief")
	if _, err := p.primary.UpdateMission(unlaunched.ID, api.UpdateMissionRequest{Pin: &local}); err != nil {
		t.Fatal(err)
	}

	p.primary.updateLease(pinned.ID, "test.pin", func(ms *mission.Mission) bool {
		ms.Pin = p.primary.self

		return true
	})

	unmirrored := runningOn(t, p.primary, "launched after the last exchange")

	exchange(t, p.primary)
	mirrored(p.secondary, waiting.ID)
	mirrored(p.secondary, pinned.ID)

	p.secondaryClock.advance(10 * time.Minute)
	p.secondary.tendLeases(t.Context())

	for name, id := range map[string]mission.MissionID{
		"waiting on a human": waiting.ID, "pinned to the primary": pinned.ID, "not mirrored": unmirrored.ID,
	} {
		if got := holder(p.secondary, id); got != p.primary.self {
			t.Errorf("the mission %s was taken", name)
		}
	}

	if len(p.onSecondary.relaunched) != 0 {
		t.Errorf("relaunched %v, want nothing started", p.onSecondary.relaunched)
	}
}

// The mission ran on the mini overnight and has finished a turn. The laptop is
// back, so the next turn belongs there.
func TestTheSecondaryHandsAMissionBackAtTurnEnd(t *testing.T) {
	p := newMovablePair(t)
	ms := runningOn(t, p.secondary, "overnight")

	p.secondary.updateLocal(ms.ID, "test.session", func(stored *mission.Mission) { stored.TmuxSession = "q-overnight" })
	exchange(t, p.primary)

	// Mid-turn: nothing moves, however present the primary is.
	p.secondary.tendLeases(t.Context())

	if got := holder(p.secondary, ms.ID); got != p.secondary.self {
		t.Fatal("a mission was handed back in the middle of a turn")
	}

	endTurn(t, p.secondary, ms.ID, "Done. Should I also update the docs?")
	p.secondary.tendLeases(t.Context())

	if !slices.Equal(p.onSecondary.stopped, []mission.MissionID{ms.ID}) {
		t.Fatalf("stopped %v, want the secondary's agent stopped before the lease moved", p.onSecondary.stopped)
	}

	exchange(t, p.primary)
	mirrored(p.primary, ms.ID)

	back, _ := p.primary.Snapshot().Mission(ms.ID)

	if back.Lease.Holder != p.primary.self {
		t.Fatalf("holder = %s, want the primary", back.Lease.Holder)
	}

	// Handed over between turns, the card says what it said, not that something died.
	if back.Status != mission.StatusDebrief || back.AgentState != mission.AgentIdle || back.HasBadge(mission.BadgeTmuxGone) {
		t.Errorf("card = %s/%s badges %v, want it unchanged apart from where it has been",
			back.Status, back.AgentState, back.Badges)
	}

	if !back.HasBadge(mission.BadgeHandoff) || back.MovedFrom != p.secondary.self {
		t.Errorf("badges %v movedFrom %q, want the handoff recorded", back.Badges, back.MovedFrom)
	}

	// The human answers on the laptop, and the agent there is told the rest.
	if _, err := p.primary.Dispatch(t.Context(), ms.ID, api.SetStatusRequest{To: mission.StatusActive, Message: "yes, update them"}); err != nil {
		t.Fatalf("resuming on the primary: %v", err)
	}

	if len(p.onPrimary.relaunched) != 1 {
		t.Fatalf("relaunched %d agents on the primary, want one", len(p.onPrimary.relaunched))
	}

	prompt := p.onPrimary.relaunched[0]
	for _, want := range []string{"mini", "Should I also update the docs?", "yes, update them"} {
		if !strings.Contains(prompt, want) {
			t.Errorf("the continuation lacks %q:\n%s", want, prompt)
		}
	}

	resumed, _ := p.primary.Snapshot().Mission(ms.ID)
	if resumed.MovedFrom != "" || resumed.HasBadge(mission.BadgeHandoff) {
		t.Errorf("movedFrom %q badges %v, want both cleared once an agent is running here", resumed.MovedFrom, resumed.Badges)
	}
}

// A turn interrupted by a question to the human is over. One blocked on a
// permission prompt or a plan approval is not, and moving it would lose it.
func TestHandBackWaitsForARealTurnEnd(t *testing.T) {
	cases := []struct {
		name  string
		setup func(*mission.Mission)
		moves bool
	}{
		{"asked a question and stopped", func(ms *mission.Mission) {
			ms.Status, ms.AgentState, ms.TurnEnded = mission.StatusAwaiting, mission.AgentWaiting, true
		}, true},
		{"blocked on a permission prompt", func(ms *mission.Mission) {
			ms.Status, ms.AgentState, ms.WaitingFor = mission.StatusAwaiting, mission.AgentWaiting, "Bash"
		}, false},
		{"plan awaiting approval", func(ms *mission.Mission) {
			ms.Status, ms.AgentState, ms.TurnEnded, ms.PlanPending = mission.StatusDebrief, mission.AgentWaiting, true, true
		}, false},
		{"agent exited", func(ms *mission.Mission) {
			ms.Status, ms.AgentState = mission.StatusDebrief, mission.AgentDead
		}, true},
		{"pinned to the secondary", func(ms *mission.Mission) {
			ms.Status, ms.AgentState, ms.TurnEnded = mission.StatusDebrief, mission.AgentIdle, true
			ms.Pin = ms.Lease.Holder
		}, false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			p := newMovablePair(t)
			ms := runningOn(t, p.secondary, "mission")
			exchange(t, p.primary)

			p.secondary.updateLease(ms.ID, "test.state", func(stored *mission.Mission) bool {
				tc.setup(stored)

				return true
			})

			p.secondary.tendLeases(t.Context())

			if moved := holder(p.secondary, ms.ID) == p.primary.self; moved != tc.moves {
				t.Errorf("handed back = %v, want %v", moved, tc.moves)
			}
		})
	}
}

// Taking a mission asks the host running it to stop first, so nothing it did
// is lost. Only when it cannot be asked is the lease simply seized.
func TestTakeAsksThePeerAndSeizesOnlyWhenItCannot(t *testing.T) {
	t.Run("the peer is reachable", func(t *testing.T) {
		p := newMovablePair(t)
		ms := runningOn(t, p.secondary, "mine now")

		p.secondary.updateLocal(ms.ID, "test.session", func(stored *mission.Mission) { stored.TmuxSession = "q-mine" })
		exchange(t, p.primary)

		taken, err := p.primary.Take(t.Context(), ms.ID)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}

		if taken.Lease.Holder != p.primary.self || holder(p.secondary, ms.ID) != p.primary.self {
			t.Errorf("holders: primary says %s, secondary says %s; want both to say the primary",
				taken.Lease.Holder, holder(p.secondary, ms.ID))
		}

		if !slices.Equal(p.onSecondary.stopped, []mission.MissionID{ms.ID}) {
			t.Errorf("stopped %v, want the secondary's agent stopped", p.onSecondary.stopped)
		}

		// Interrupted mid-turn, so the card asks to be looked at.
		if taken.Status != mission.StatusDebrief {
			t.Errorf("status = %s, want debrief for a mission stopped part-way through a turn", taken.Status)
		}
	})

	t.Run("the peer is unreachable", func(t *testing.T) {
		p := newMovablePair(t)
		ms := runningOn(t, p.secondary, "mine anyway")
		exchange(t, p.primary)

		p.link.down = true

		taken, err := p.primary.Take(t.Context(), ms.ID)
		if err != nil {
			t.Fatalf("Take: %v", err)
		}

		if taken.Lease.Holder != p.primary.self || taken.Lease.Epoch != ms.Lease.Epoch+1 {
			t.Errorf("lease = %+v, want it seized one epoch on", taken.Lease)
		}

		if taken.MovedFrom != p.secondary.self || taken.Status != mission.StatusDebrief {
			t.Errorf("movedFrom %q status %s, want the origin recorded and the card in debrief", taken.MovedFrom, taken.Status)
		}

		// When the two speak again, the secondary accepts it and stops its agent.
		p.secondary.updateLocal(ms.ID, "test.session", func(stored *mission.Mission) { stored.TmuxSession = "q-mine" })
		p.link.down = false
		exchange(t, p.primary)

		if holder(p.secondary, ms.ID) != p.primary.self || len(p.onSecondary.stopped) != 1 {
			t.Errorf("secondary: holder %s stopped %v, want it to stand down", holder(p.secondary, ms.ID), p.onSecondary.stopped)
		}
	})
}

// A queued mission runs on the laptop when the laptop is there, and on the
// always-on machine when it is not.
func TestQueuedMissionsRunOnThePrimaryWhenItIsAround(t *testing.T) {
	p := newMovablePair(t)

	ms, err := p.secondary.CreateMission(api.CreateMissionRequest{
		OperationID: seedOperation(t, p.secondary).ID, Name: "queued on the mini", Prompt: "do it", Queued: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	exchange(t, p.primary)

	// The primary is present, so the secondary passes it on rather than starting it.
	p.secondary.Schedule(t.Context())

	if got, _ := p.secondary.Snapshot().Mission(ms.ID); got.Launched() || got.Lease.Holder != p.primary.self {
		t.Fatalf("launched=%v holder=%s, want it passed to the primary unstarted", got.Launched(), got.Lease.Holder)
	}

	exchange(t, p.primary)
	p.primary.Schedule(t.Context())

	if got, _ := p.primary.Snapshot().Mission(ms.ID); !got.Launched() || got.Lease.Holder != p.primary.self {
		t.Errorf("launched=%v holder=%s, want the primary to have started it", got.Launched(), got.Lease.Holder)
	}
}

func TestQueuedMissionsRunOnTheSecondaryWhenThePrimaryIsAway(t *testing.T) {
	p := newMovablePair(t)

	ms, err := p.primary.CreateMission(api.CreateMissionRequest{
		OperationID: seedOperation(t, p.primary).ID, Name: "queued before bed", Prompt: "do it", Queued: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	exchange(t, p.primary)

	p.secondaryClock.advance(10 * time.Minute)
	p.secondary.Schedule(t.Context())

	got, _ := p.secondary.Snapshot().Mission(ms.ID)
	if !got.Launched() || got.Lease.Holder != p.secondary.self {
		t.Errorf("launched=%v holder=%s, want the secondary to have started it", got.Launched(), got.Lease.Holder)
	}
}

// A pin is an instruction, whoever is around.
func TestAPinnedQueuedMissionRunsWhereItIsPinned(t *testing.T) {
	p := newMovablePair(t)
	exchange(t, p.primary)

	ms, err := p.primary.CreateMission(api.CreateMissionRequest{
		OperationID: seedOperation(t, p.primary).ID, Name: "for the mini", Prompt: "do it", Queued: true, Pin: "remote",
	})
	if err != nil {
		t.Fatal(err)
	}

	p.primary.Schedule(t.Context())

	if got, _ := p.primary.Snapshot().Mission(ms.ID); got.Launched() {
		t.Fatal("the primary started a mission pinned to the secondary")
	}

	exchange(t, p.primary)
	p.secondary.Schedule(t.Context())

	if got, _ := p.secondary.Snapshot().Mission(ms.ID); !got.Launched() || got.Lease.Holder != p.secondary.self {
		t.Errorf("launched=%v holder=%s, want it started on the secondary", got.Launched(), got.Lease.Holder)
	}
}

// A laptop that has just woken cannot know what the other machine did while it
// slept. Until they have spoken, it starts nothing on its own and says so.
func TestAnUnconfirmedPrimaryStartsNothingAndSaysSo(t *testing.T) {
	p := newMovablePair(t)
	running := runningOn(t, p.primary, "was running")

	queued, err := p.primary.CreateMission(api.CreateMissionRequest{
		OperationID: seedOperation(t, p.primary).ID, Name: "queued", Prompt: "do it", Queued: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	exchange(t, p.primary)

	// Asleep for an hour, and now off the network.
	p.primaryClock.advance(time.Hour)
	p.link.down = true

	p.primary.tendLeases(t.Context())
	p.primary.Schedule(t.Context())

	if got, _ := p.primary.Snapshot().Mission(queued.ID); got.Launched() {
		t.Error("a queued mission was started without knowing whether the peer already had")
	}

	if got, _ := p.primary.Snapshot().Mission(running.ID); !got.HasLocalBadge(mission.BadgeUnconfirmed) {
		t.Errorf("badges %v, want the running mission marked unconfirmed", got.LocalBadges)
	}

	// A start made by hand is the human's call and is not refused.
	if _, err := p.primary.Start(t.Context(), queued.ID); err != nil {
		t.Errorf("a manual start was refused: %v", err)
	}

	p.link.down = false
	exchange(t, p.primary)
	p.primary.tendLeases(t.Context())

	if got, _ := p.primary.Snapshot().Mission(running.ID); got.HasLocalBadge(mission.BadgeUnconfirmed) {
		t.Error("the mission is still marked unconfirmed after an exchange")
	}
}
