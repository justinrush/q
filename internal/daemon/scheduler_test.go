package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// recordingLauncher stands in for the real launcher, recording what it was
// asked to start. It fails for any mission named in fail.
type recordingLauncher struct {
	launched []string
	fail     map[string]error
}

func (l *recordingLauncher) Launch(
	_ context.Context,
	_ mission.Operation,
	ms mission.Mission,
) (mission.Mission, error) {
	if err := l.fail[ms.Name]; err != nil {
		return ms, err
	}

	l.launched = append(l.launched, ms.Name)

	started := time.Now()
	ms.StartedAt = &started
	ms.Status = mission.StatusActive
	ms.MissionDir = "/missions/" + ms.Slug
	ms.TmuxSession = "q-" + ms.Slug

	return ms, nil
}

// schedulerService returns a service that can launch, with a queue of the
// given names in board order.
func schedulerService(t *testing.T, queued ...string) (*Service, *recordingLauncher, map[string]mission.MissionID) {
	t.Helper()

	svc := newTestService(t)
	launcher := &recordingLauncher{fail: map[string]error{}}
	svc.apply(WithLauncher(launcher))

	operation := seedOperation(t, svc)
	ids := map[string]mission.MissionID{}

	for _, name := range queued {
		ms, err := svc.CreateMission(api.CreateMissionRequest{
			OperationID: operation.ID, Name: name, Prompt: "do it", Queued: true,
		})
		if err != nil {
			t.Fatalf("CreateMission(%s): %v", name, err)
		}

		ids[name] = ms.ID
	}

	return svc, launcher, ids
}

func TestScheduleStartsQueuedMissionsInBoardOrderUpToTheCap(t *testing.T) {
	svc, launcher, ids := schedulerService(t, "first", "second", "third")
	svc.apply(WithMaxConcurrent(2))

	svc.Schedule(t.Context())

	if !slices.Equal(launcher.launched, []string{"first", "second"}) {
		t.Fatalf("launched %v, want the first two in order", launcher.launched)
	}

	third, _ := svc.Snapshot().Mission(ids["third"])
	if third.Status != mission.StatusBriefing || !third.Queued {
		t.Errorf("third is %s queued=%v, want it still waiting its turn", third.Status, third.Queued)
	}

	// Nothing has finished, so a second pass must not start it either.
	svc.Schedule(t.Context())

	if len(launcher.launched) != 2 {
		t.Errorf("launched %v on a second pass with no free slot", launcher.launched)
	}
}

// A mission that stops to ask, or finishes its turn, is no longer using the
// machine. The queue must move on without a human there to answer.
func TestScheduleFreesASlotWhenAMissionStopsForTheHuman(t *testing.T) {
	for _, lane := range []mission.Status{mission.StatusAwaiting, mission.StatusDebrief} {
		t.Run(string(lane), func(t *testing.T) {
			svc, launcher, ids := schedulerService(t, "first", "second")
			svc.apply(WithMaxConcurrent(1))

			svc.Schedule(t.Context())

			if _, err := svc.SetStatus(ids["first"], lane); err != nil {
				t.Fatalf("SetStatus: %v", err)
			}

			svc.Schedule(t.Context())

			if !slices.Equal(launcher.launched, []string{"first", "second"}) {
				t.Errorf("launched %v, want the second once the first left the active lane", launcher.launched)
			}
		})
	}
}

func TestScheduleLeavesUnqueuedBriefsAlone(t *testing.T) {
	svc, launcher, _ := schedulerService(t)
	operation := seedOperation(t, svc)

	if _, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID, Name: "by hand", Prompt: "do it",
	}); err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	svc.Schedule(t.Context())

	if len(launcher.launched) != 0 {
		t.Errorf("launched %v, want a brief nobody queued left for a human", launcher.launched)
	}
}

// A mission that cannot start must say why once, not be retried every few
// seconds for as long as the daemon runs.
func TestScheduleDoesNotRetryAFailedLaunch(t *testing.T) {
	svc, launcher, ids := schedulerService(t, "broken", "fine")
	svc.apply(WithMaxConcurrent(1))

	launcher.fail["broken"] = errors.New("fetching base branch: no such ref")

	svc.Schedule(t.Context())

	broken, _ := svc.Snapshot().Mission(ids["broken"])

	if broken.Queued || broken.Status != mission.StatusBriefing || broken.LaunchError == "" {
		t.Errorf("broken: queued=%v status=%s error=%q, want it unqueued in briefing with the reason",
			broken.Queued, broken.Status, broken.LaunchError)
	}

	// The failure did not take the slot.
	if !slices.Equal(launcher.launched, []string{"fine"}) {
		t.Errorf("launched %v, want the next mission to take the slot the failure left", launcher.launched)
	}
}

func TestStartingByHandClearsTheQueuedFlag(t *testing.T) {
	svc, _, ids := schedulerService(t, "impatient")

	ms, err := svc.Start(t.Context(), ids["impatient"])
	if err != nil {
		t.Fatalf("Start: %v", err)
	}

	if ms.Queued {
		t.Error("a mission started by hand is still marked queued")
	}
}

func TestQueueingALaunchedMissionIsRefused(t *testing.T) {
	svc, _, ids := schedulerService(t, "running")

	if _, err := svc.Start(t.Context(), ids["running"]); err != nil {
		t.Fatalf("Start: %v", err)
	}

	_, err := svc.UpdateMission(ids["running"], api.UpdateMissionRequest{Queued: new(true)})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("err = %v, want a conflict: a running mission cannot be queued", err)
	}
}

func TestPinResolvesRelativeHostNames(t *testing.T) {
	svc, _, ids := schedulerService(t, "pinned")

	local := "local"

	ms, err := svc.UpdateMission(ids["pinned"], api.UpdateMissionRequest{Pin: &local})
	if err != nil {
		t.Fatalf("pinning to local: %v", err)
	}

	if ms.Pin != svc.self {
		t.Errorf("Pin = %q, want this host %q", ms.Pin, svc.self)
	}

	remote := "remote"

	if _, err := svc.UpdateMission(ids["pinned"], api.UpdateMissionRequest{Pin: &remote}); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want a refusal: there is no peer to pin to", err)
	}
}
