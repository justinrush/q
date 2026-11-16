package daemon

import (
	"errors"
	"slices"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// launcherOf returns the recording launcher a paired test service was built with.
func launcherOf(t *testing.T, svc *Service) *recordingLauncher {
	t.Helper()

	l, ok := svc.launcher.(*recordingLauncher)
	if !ok {
		t.Fatalf("launcher is %T, want a recording one", svc.launcher)
	}

	return l
}

// pinnedBrief creates an unlaunched mission on svc pinned to "remote".
func pinnedBrief(t *testing.T, svc *Service, name string) mission.Mission {
	t.Helper()

	ms, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: seedOperation(t, svc).ID, Name: name, Prompt: "do it", Pin: "remote",
	})
	if err != nil {
		t.Fatal(err)
	}

	return ms
}

// Starting by hand a mission pinned to the mini starts it on the mini.
func TestAManualStartOfAPinnedMissionStartsItOnThePeer(t *testing.T) {
	p := newMovablePair(t)
	exchange(t, p.primary)

	ms := pinnedBrief(t, p.primary, "for the mini")

	if _, err := p.primary.Dispatch(t.Context(), ms.ID, api.SetStatusRequest{To: mission.StatusActive}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if got := launcherOf(t, p.primary).launched; len(got) != 0 {
		t.Errorf("the laptop launched %v, want nothing", got)
	}

	if got := launcherOf(t, p.secondary).launched; !slices.Equal(got, []string{"for the mini"}) {
		t.Errorf("the mini launched %v, want the pinned mission", got)
	}

	exchange(t, p.primary)

	if got, _ := p.primary.Snapshot().Mission(ms.ID); !got.Launched() || got.Lease.Holder != p.secondary.self {
		t.Errorf("on the laptop: launched=%v holder=%s, want it launched and the mini's", got.Launched(), got.Lease.Holder)
	}
}

// With the mini out of reach, the start waits for it rather than happening here.
func TestAManualStartOfAPinnedMissionQueuesItWhenThePeerIsAway(t *testing.T) {
	p := newMovablePair(t)
	exchange(t, p.primary)

	ms := pinnedBrief(t, p.primary, "for the mini")
	p.link.down = true

	got, err := p.primary.Dispatch(t.Context(), ms.ID, api.SetStatusRequest{To: mission.StatusActive})
	if err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if !got.Queued || got.Launched() {
		t.Errorf("queued=%v launched=%v, want it queued and not started", got.Queued, got.Launched())
	}

	if l := launcherOf(t, p.primary).launched; len(l) != 0 {
		t.Errorf("the laptop launched %v, want nothing", l)
	}

	p.link.down = false
	exchange(t, p.primary)
	p.secondary.Schedule(t.Context())

	if l := launcherOf(t, p.secondary).launched; !slices.Equal(l, []string{"for the mini"}) {
		t.Errorf("the mini launched %v, want the queued mission once it was heard from", l)
	}
}

// Nothing reaches Start for a mission pinned elsewhere without being refused.
func TestStartRefusesAMissionPinnedToThePeer(t *testing.T) {
	p := newMovablePair(t)
	exchange(t, p.primary)

	ms := pinnedBrief(t, p.primary, "for the mini")

	if _, err := p.primary.Start(t.Context(), ms.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("Start = %v, want a conflict", err)
	}
}

// A mission between turns is sent at once, and the mini starts an agent for it
// so that Enter on the laptop has something to attach to.
func TestSendingAMissionBetweenTurnsMovesItAndStartsItThere(t *testing.T) {
	p := newMovablePair(t)
	ms := runningOn(t, p.primary, "long job")
	endTurn(t, p.primary, ms.ID, "Done with step one.")

	exchange(t, p.primary)
	mirrored(p.secondary, ms.ID)

	sent, err := p.primary.Send(t.Context(), ms.ID)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if sent.Lease.Holder != p.secondary.self || sent.Pin != p.secondary.self {
		t.Fatalf("holder=%s pin=%s, want both the mini", sent.Lease.Holder, sent.Pin)
	}

	if !slices.Equal(p.onPrimary.stopped, []mission.MissionID{ms.ID}) {
		t.Errorf("stopped %v, want the laptop's agent stopped", p.onPrimary.stopped)
	}

	p.secondary.tendLeases(t.Context())

	if len(p.onSecondary.relaunched) != 1 || !strings.Contains(p.onSecondary.relaunched[0], "laptop") {
		t.Fatalf("relaunched %q, want one agent told it continues from the laptop", p.onSecondary.relaunched)
	}

	// Pinned, so the mini keeps it at the next turn end with the laptop present.
	endTurn(t, p.secondary, ms.ID, "Done with step two.")
	exchange(t, p.primary)
	p.secondary.tendLeases(t.Context())

	if got := holder(p.secondary, ms.ID); got != p.secondary.self {
		t.Errorf("holder = %s after a turn end, want the mini still", got)
	}

	if len(p.onSecondary.stopped) != 0 {
		t.Errorf("the mini stopped %v, want its agent left alone", p.onSecondary.stopped)
	}
}

// Sent mid-turn, a mission finishes its turn here and moves after.
func TestSendingAMissionMidTurnWaitsForTheTurnToEnd(t *testing.T) {
	p := newMovablePair(t)
	ms := runningOn(t, p.primary, "long job")

	exchange(t, p.primary)
	mirrored(p.secondary, ms.ID)

	sent, err := p.primary.Send(t.Context(), ms.ID)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if sent.Lease.Holder != p.primary.self || !sent.HasLocalBadge(mission.BadgeSending) {
		t.Fatalf("holder=%s badges=%v, want it still here and marked sending", sent.Lease.Holder, sent.LocalBadges)
	}

	if len(p.onPrimary.stopped) != 0 {
		t.Fatalf("stopped %v mid-turn, want nothing interrupted", p.onPrimary.stopped)
	}

	endTurn(t, p.primary, ms.ID, "Done.")
	p.primary.tendLeases(t.Context())

	got, _ := p.primary.Snapshot().Mission(ms.ID)
	if got.Lease.Holder != p.secondary.self || got.HasLocalBadge(mission.BadgeSending) {
		t.Errorf("holder=%s badges=%v, want it moved and no longer marked", got.Lease.Holder, got.LocalBadges)
	}
}

// A mini that could not run the mission is asked first, and the laptop keeps
// it, saying why.
func TestSendingWaitsForThePeerToBeReady(t *testing.T) {
	p := newMovablePair(t)
	ms := runningOn(t, p.primary, "long job")
	endTurn(t, p.primary, ms.ID, "Done.")

	// Exchanged, but the mini has no worktrees for it yet.
	exchange(t, p.primary)

	sent, err := p.primary.Send(t.Context(), ms.ID)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if sent.Lease.Holder != p.primary.self {
		t.Fatalf("holder = %s, want the laptop to keep a mission the mini cannot run", sent.Lease.Holder)
	}

	if len(p.onPrimary.stopped) != 0 {
		t.Errorf("stopped %v, want the laptop's agent left running", p.onPrimary.stopped)
	}

	var detail string

	for _, badge := range sent.LocalBadges {
		if badge.Kind == mission.BadgeSending {
			detail = badge.Detail
		}
	}

	if !strings.Contains(detail, "mini") || !strings.Contains(detail, "mirroring") {
		t.Errorf("sending badge detail = %q, want it to say the mini is still mirroring", detail)
	}

	mirrored(p.secondary, ms.ID)
	p.primary.tendLeases(t.Context())

	if got := holder(p.primary, ms.ID); got != p.secondary.self {
		t.Errorf("holder = %s once the mini was ready, want the mini", got)
	}
}

// Taking a sent mission back undoes the pin, so it is not sent again.
func TestTakingASentMissionBackClearsItsPin(t *testing.T) {
	p := newMovablePair(t)
	ms := runningOn(t, p.primary, "long job")
	endTurn(t, p.primary, ms.ID, "Done.")

	exchange(t, p.primary)
	mirrored(p.secondary, ms.ID)

	if _, err := p.primary.Send(t.Context(), ms.ID); err != nil {
		t.Fatalf("Send: %v", err)
	}

	p.secondary.tendLeases(t.Context())
	endTurn(t, p.secondary, ms.ID, "Done there.")

	taken, err := p.primary.Take(t.Context(), ms.ID)
	if err != nil {
		t.Fatalf("Take: %v", err)
	}

	if taken.Lease.Holder != p.primary.self || taken.Pin != "" {
		t.Fatalf("holder=%s pin=%q, want it here and unpinned", taken.Lease.Holder, taken.Pin)
	}

	p.primary.tendLeases(t.Context())
	exchange(t, p.primary)

	if got := holder(p.primary, ms.ID); got != p.primary.self {
		t.Errorf("holder = %s, want it to stay on the laptop", got)
	}

	if got, _ := p.secondary.Snapshot().Mission(ms.ID); got.Pin != "" {
		t.Errorf("the mini still has pin %q after an exchange", got.Pin)
	}
}

// A pin set after launch is part of the brief and reaches the peer.
func TestAPinSetAfterLaunchReachesThePeer(t *testing.T) {
	p := newMovablePair(t)
	ms := runningOn(t, p.primary, "long job")

	exchange(t, p.primary)

	if _, err := p.primary.Send(t.Context(), ms.ID); err != nil {
		t.Fatalf("Send: %v", err)
	}

	exchange(t, p.primary)

	if got, _ := p.secondary.Snapshot().Mission(ms.ID); got.Pin != p.secondary.self {
		t.Errorf("pin on the mini = %q, want the mini", got.Pin)
	}
}

// An unlaunched mission is only pinned by a send, and starts there later.
func TestSendingABriefOnlyPinsIt(t *testing.T) {
	p := newMovablePair(t)
	exchange(t, p.primary)

	ms := briefOn(t, p.primary, "brief")

	sent, err := p.primary.Send(t.Context(), ms.ID)
	if err != nil {
		t.Fatalf("Send: %v", err)
	}

	if sent.Pin != p.secondary.self || sent.Launched() {
		t.Errorf("pin=%s launched=%v, want it pinned to the mini and not started", sent.Pin, sent.Launched())
	}
}

func TestSendingNeedsAPeer(t *testing.T) {
	svc := newTestService(t)
	ms := briefOn(t, svc, "alone")

	if _, err := svc.Send(t.Context(), ms.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("Send = %v, want a conflict", err)
	}
}
