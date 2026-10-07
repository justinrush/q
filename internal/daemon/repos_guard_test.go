package daemon

import (
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// unlocatedBrief stores an operation naming one repository this host has and
// one it does not, with a queued brief under it, as an exchange would leave it.
func unlocatedBrief(t *testing.T, svc *Service, queued bool) mission.Mission {
	t.Helper()

	operation := mission.Operation{
		ID: "op_aabbccddeeff", Name: "Two repos", Slug: "two-repos", Rev: 1,
		Repos: []mission.Repo{
			{Name: "weave", Path: "/dev/weave", URL: "example.com/acme/weave"},
			{Name: "loom", URL: "example.com/acme/loom"},
		},
	}

	ms := mission.Mission{
		ID: "ms_aabbccddeeff", OperationID: operation.ID, Name: "needs both", Slug: "needs-both",
		Tool: mission.ToolClaude, Prompt: "do it", Status: mission.StatusBriefing, Queued: queued,
		Lease: mission.Lease{Holder: svc.self, Epoch: 1}, SpecRev: 1,
	}

	if err := svcStore(svc).Apply("test.unlocated", func(snap *mission.Snapshot) error {
		snap.PutOperation(operation)
		snap.PutMission(ms)

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	return ms
}

// An agent given a workspace with a repository missing does not stop and say
// so. It works around the gap. So the mission is not started at all, and the
// refusal names what to clone.
func TestStartIsRefusedWhenARepositoryIsNotCheckedOutHere(t *testing.T) {
	svc, launcher, _ := schedulerService(t)
	ms := unlocatedBrief(t, svc, false)

	_, err := svc.Start(t.Context(), ms.ID)
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want a refusal", err)
	}

	for _, want := range []string{"loom", "example.com/acme/loom", "repos.roots"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("the refusal should mention %q: %v", want, err)
		}
	}

	if strings.Contains(err.Error(), "weave") {
		t.Errorf("the refusal names a repository that is here: %v", err)
	}

	if len(launcher.launched) != 0 {
		t.Errorf("launched %v, want nothing provisioned", launcher.launched)
	}

	if got, _ := svc.Snapshot().Mission(ms.ID); got.Status != mission.StatusBriefing {
		t.Errorf("status = %s, want the card left in briefing", got.Status)
	}
}

// A queued mission this host cannot run waits, visibly, and starts by itself
// once the repository turns up. It is not started and left to fail, which
// would unqueue it over something a clone fixes.
func TestAQueuedMissionWaitsForAMissingRepositoryAndThenStarts(t *testing.T) {
	svc, launcher, _ := schedulerService(t)
	ms := unlocatedBrief(t, svc, true)

	svc.Schedule(t.Context())

	waiting, _ := svc.Snapshot().Mission(ms.ID)

	if waiting.Launched() || !waiting.Queued {
		t.Fatalf("launched=%v queued=%v, want it still queued and unstarted", waiting.Launched(), waiting.Queued)
	}

	if !waiting.HasLocalBadge(mission.BadgeRepoMissing) || !strings.Contains(waiting.LocalBadges[0].Detail, "loom") {
		t.Errorf("badges = %v, want the card to say which repository is missing", waiting.LocalBadges)
	}

	// It does not hold up the missions behind it.
	other, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: seedOperation(t, svc).ID, Name: "runnable", Prompt: "do it", Queued: true,
	})
	if err != nil {
		t.Fatal(err)
	}

	svc.Schedule(t.Context())

	if got, _ := svc.Snapshot().Mission(other.ID); !got.Launched() {
		t.Error("a runnable mission was held up behind one that cannot run here")
	}

	// The repository is cloned and found.
	if err := svcStore(svc).Apply("test.located", func(snap *mission.Snapshot) error {
		op, _ := snap.Operation(ms.OperationID)
		op.Repos[1].Path = "/dev/loom"
		snap.PutOperation(op)

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if _, err := svc.SetStatus(other.ID, mission.StatusDebrief); err != nil {
		t.Fatal(err)
	}

	svc.Schedule(t.Context())

	started, _ := svc.Snapshot().Mission(ms.ID)
	if !started.Launched() || started.HasLocalBadge(mission.BadgeRepoMissing) {
		t.Errorf("launched=%v badges=%v, want it started and the badge gone", started.Launched(), started.LocalBadges)
	}

	if len(launcher.launched) != 2 {
		t.Errorf("launched %v, want both missions", launcher.launched)
	}
}

// The always-on machine must not carry on a mission it only has part of.
func TestTakeoverIsRefusedWhenTheMirrorIsMissingARepository(t *testing.T) {
	p := newMovablePair(t)
	ms := runningOn(t, p.primary, "spans two repos")

	exchange(t, p.primary)

	// The secondary mirrored one repository and could not find the other.
	p.secondary.updateLocal(ms.ID, "test.partial", func(stored *mission.Mission) {
		stored.MissionDir = t.TempDir()
		stored.LaunchRepos = []mission.Repo{
			{Name: "weave", Path: "/dev/weave", URL: "example.com/acme/weave"},
			{Name: "loom", URL: "example.com/acme/loom"},
		}
		stored.Work = map[string]mission.RepoWork{
			"weave": {RepoName: "weave", WorktreePath: stored.MissionDir, Created: true},
		}
	})

	p.secondaryClock.advance(10 * time.Minute)
	p.secondary.tendLeases(t.Context())

	if got := holder(p.secondary, ms.ID); got != p.primary.self {
		t.Errorf("holder = %s, want the mission left with the primary", got)
	}

	if len(p.onSecondary.relaunched) != 0 {
		t.Errorf("relaunched %v, want no agent started in a partial workspace", p.onSecondary.relaunched)
	}
}
