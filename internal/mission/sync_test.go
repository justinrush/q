package mission

import (
	"testing"
	"time"
)

const (
	laptop HostID = "h_aaaaaaaaaaaa"
	mini   HostID = "h_bbbbbbbbbbbb"
)

var syncNow = time.Date(2026, 10, 7, 12, 0, 0, 0, time.UTC)

// hostSnapshot returns an empty snapshot belonging to host.
func hostSnapshot(host HostID) Snapshot {
	return Snapshot{SchemaVersion: SchemaVersion, Self: HostInfo{ID: host}}
}

// briefed returns an unlaunched mission held by holder.
func briefed(holder HostID) Mission {
	return Mission{
		ID:          "ms_aabbccddeeff",
		OperationID: "op_aabbccddeeff",
		Name:        "Fix it",
		Slug:        "fix-it",
		Tool:        ToolClaude,
		Prompt:      "fix it",
		Status:      StatusBriefing,
		Lease:       Lease{Holder: holder, Epoch: 1},
		SpecRev:     1,
	}
}

// running returns a launched mission held by holder.
func running(holder HostID) Mission {
	started := syncNow.Add(-time.Hour)

	ms := briefed(holder)
	ms.Status = StatusActive
	ms.AgentState = AgentBusy
	ms.StartedAt = &started

	return ms
}

// mutated applies fn through stamp, as Store.Mutate does.
func mutated(snap Snapshot, fn func(*Snapshot)) Snapshot {
	next := snap.Clone()
	fn(&next)
	stamp(snap, &next, syncNow)

	return next
}

func TestStampGivesANewMissionToThisHost(t *testing.T) {
	snap := mutated(hostSnapshot(laptop), func(s *Snapshot) {
		ms := briefed("")
		ms.Lease, ms.SpecRev = Lease{}, 0
		s.PutMission(ms)
	})

	got, _ := snap.Mission("ms_aabbccddeeff")

	if got.Lease != (Lease{Holder: laptop, Epoch: 1}) || got.SpecRev != 1 {
		t.Errorf("lease %+v rev %d, want held here at epoch 1, rev 1", got.Lease, got.SpecRev)
	}
}

func TestStampBumpsTheRevisionOnlyWhenTheBriefChanges(t *testing.T) {
	snap := hostSnapshot(laptop)
	snap.PutMission(running(laptop))

	run := mutated(snap, func(s *Snapshot) {
		ms, _ := s.Mission("ms_aabbccddeeff")
		ms.LastMessage = "done"
		ms.UpdatedAt = syncNow
		s.PutMission(ms)
	})

	if got, _ := run.Mission("ms_aabbccddeeff"); got.SpecRev != 1 {
		t.Errorf("SpecRev = %d after a run-only change, want 1", got.SpecRev)
	}

	spec := mutated(snap, func(s *Snapshot) {
		ms, _ := s.Mission("ms_aabbccddeeff")
		ms.Prompt = "fix it properly"
		s.PutMission(ms)
	})

	if got, _ := spec.Mission("ms_aabbccddeeff"); got.SpecRev != 2 {
		t.Errorf("SpecRev = %d after editing the prompt, want 2", got.SpecRev)
	}
}

// Writing the run of a brief nobody is running is how a host comes to run it.
func TestStampTakesTheLeaseOfAnUnlaunchedMission(t *testing.T) {
	snap := hostSnapshot(laptop)
	snap.PutMission(briefed(mini))

	next := mutated(snap, func(s *Snapshot) {
		ms, _ := s.Mission("ms_aabbccddeeff")
		ms.Status = StatusActive
		s.PutMission(ms)
	})

	got, _ := next.Mission("ms_aabbccddeeff")

	if got.Lease != (Lease{Holder: laptop, Epoch: 2}) || got.Status != StatusActive {
		t.Errorf("lease %+v status %s, want taken at epoch 2 and active", got.Lease, got.Status)
	}
}

// Writing the run of a mission the peer's agent is working on is discarded,
// which is what keeps a missed guard from stealing it.
func TestStampDiscardsARunWriteToAMissionThePeerRuns(t *testing.T) {
	snap := hostSnapshot(laptop)
	snap.PutMission(running(mini))

	next := mutated(snap, func(s *Snapshot) {
		ms, _ := s.Mission("ms_aabbccddeeff")
		ms.Status = StatusDebrief
		ms.AgentState = AgentDead
		ms.Name = "Renamed"
		ms.MissionDir = "/mirror"
		s.PutMission(ms)
	})

	got, _ := next.Mission("ms_aabbccddeeff")

	if got.Lease != (Lease{Holder: mini, Epoch: 1}) {
		t.Errorf("lease = %+v, want it left with the peer", got.Lease)
	}

	if got.Status != StatusActive || got.AgentState != AgentBusy {
		t.Errorf("run = %s/%s, want the write discarded", got.Status, got.AgentState)
	}

	if got.Name != "Renamed" || got.SpecRev != 2 || got.MissionDir != "/mirror" {
		t.Errorf("brief and local fields should still be written: %+v", got)
	}
}

func TestStampHonorsAnExplicitLeaseChange(t *testing.T) {
	snap := hostSnapshot(laptop)
	snap.PutMission(running(mini))

	next := mutated(snap, func(s *Snapshot) {
		ms, _ := s.Mission("ms_aabbccddeeff")
		ms.Lease = ms.Lease.Take(laptop)
		ms.Status = StatusDebrief
		s.PutMission(ms)
	})

	got, _ := next.Mission("ms_aabbccddeeff")

	if got.Lease != (Lease{Holder: laptop, Epoch: 2}) || got.Status != StatusDebrief {
		t.Errorf("lease %+v status %s, want the take and its run write kept", got.Lease, got.Status)
	}
}

func TestStampBuriesDeletes(t *testing.T) {
	snap := hostSnapshot(laptop)
	snap.PutOperation(sampleOperation("Area"))
	snap.PutMission(briefed(laptop))

	next := mutated(snap, func(s *Snapshot) {
		s.DeleteMission("ms_aabbccddeeff")
		s.DeleteOperation("op_aabbccddeeff")
	})

	if !next.Tombstoned(TombstoneMission, "ms_aabbccddeeff") ||
		!next.Tombstoned(TombstoneOperation, "op_aabbccddeeff") {
		t.Errorf("tombstones = %+v, want one for each delete", next.Tombstones)
	}
}

func TestTombstonesExpire(t *testing.T) {
	snap := hostSnapshot(laptop)
	snap.Tombstones = []Tombstone{
		{Kind: TombstoneMission, ID: "old", At: syncNow.Add(-tombstoneTTL - time.Hour)},
		{Kind: TombstoneMission, ID: "new", At: syncNow.Add(-time.Hour)},
	}

	snap.expireTombstones(syncNow)

	if snap.Tombstoned(TombstoneMission, "old") || !snap.Tombstoned(TombstoneMission, "new") {
		t.Errorf("tombstones = %+v, want only the recent one", snap.Tombstones)
	}
}

func TestMergeAdoptsAMissionThisHostHasNeverSeen(t *testing.T) {
	theirs := hostSnapshot(mini)
	theirs.PutMission(launchedMission())

	mine := hostSnapshot(laptop)
	merge := mine.MergePeer(theirs.Payload(), true, syncNow)

	got, ok := mine.Mission("ms_aabbccddeeff")
	if !ok || len(merge.Missions) != 1 {
		t.Fatalf("mission not adopted: %+v", merge)
	}

	if got.MissionDir != "" || got.Work["weave"].WorktreePath != "" {
		t.Errorf("host-local fields crossed the wire: %+v", got)
	}
}

func TestMergeBriefHigherRevisionWins(t *testing.T) {
	mine, theirs := hostSnapshot(laptop), hostSnapshot(mini)

	local := briefed(laptop)
	local.ExtraRepos = []Repo{{Name: "extra", Path: "/Users/me/dev/extra"}}
	mine.PutMission(local)

	remote := briefed(laptop)
	remote.Prompt, remote.SpecRev = "fix it properly", 2
	remote.ExtraRepos = []Repo{{Name: "extra"}}
	theirs.PutMission(remote)

	mine.MergePeer(theirs.Payload(), true, syncNow)

	got, _ := mine.Mission("ms_aabbccddeeff")

	if got.Prompt != "fix it properly" || got.SpecRev != 2 {
		t.Errorf("prompt %q rev %d, want the newer brief", got.Prompt, got.SpecRev)
	}

	if got.ExtraRepos[0].Path != "/Users/me/dev/extra" {
		t.Errorf("repo path = %q, want this host's own kept", got.ExtraRepos[0].Path)
	}
}

func TestMergeBriefTieGoesToThePrimary(t *testing.T) {
	for _, tc := range []struct {
		name    string
		primary bool
		want    string
	}{
		{"primary keeps its own", true, "laptop's"},
		{"secondary takes the primary's", false, "mini's"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			mine, theirs := hostSnapshot(laptop), hostSnapshot(mini)

			local := briefed(laptop)
			local.Prompt, local.SpecRev = "laptop's", 2
			mine.PutMission(local)

			remote := briefed(laptop)
			remote.Prompt, remote.SpecRev = "mini's", 2
			theirs.PutMission(remote)

			mine.MergePeer(theirs.Payload(), tc.primary, syncNow)

			if got, _ := mine.Mission("ms_aabbccddeeff"); got.Prompt != tc.want {
				t.Errorf("prompt = %q, want %q", got.Prompt, tc.want)
			}
		})
	}
}

// The holder's run replaces this host's copy, and this host's own worktree
// record survives it.
func TestMergeTakesTheRunFromTheHolder(t *testing.T) {
	mine, theirs := hostSnapshot(laptop), hostSnapshot(mini)

	local := running(mini)
	local.MissionDir = "/mirror"
	local.Work = map[string]RepoWork{"weave": {RepoName: "weave", WorktreePath: "/mirror/weave", Created: true}}
	mine.PutMission(local)

	remote := running(mini)
	remote.Status, remote.AgentState, remote.LastMessage = StatusDebrief, AgentIdle, "all green"
	remote.Work = map[string]RepoWork{"weave": {RepoName: "weave", WorktreePath: "/theirs/weave", BaseSHA: "abc", Created: true}}
	theirs.PutMission(remote)

	merge := mine.MergePeer(theirs.Payload(), true, syncNow)

	got, _ := mine.Mission("ms_aabbccddeeff")

	if got.Status != StatusDebrief || got.LastMessage != "all green" {
		t.Errorf("run = %s %q, want the holder's", got.Status, got.LastMessage)
	}

	if got.MissionDir != "/mirror" || got.Work["weave"].WorktreePath != "/mirror/weave" || got.Work["weave"].BaseSHA != "abc" {
		t.Errorf("work = %+v dir %q, want local paths with the holder's base", got.Work, got.MissionDir)
	}

	if len(merge.Lost)+len(merge.Gained) != 0 {
		t.Errorf("no lease moved, got %+v", merge)
	}
}

// A host never lets the peer's stale copy overwrite the run it is producing.
func TestMergeKeepsTheRunThisHostHolds(t *testing.T) {
	mine, theirs := hostSnapshot(laptop), hostSnapshot(mini)

	local := running(laptop)
	local.LastMessage = "fresh"
	mine.PutMission(local)

	remote := running(laptop)
	remote.LastMessage = "stale"
	theirs.PutMission(remote)

	merge := mine.MergePeer(theirs.Payload(), false, syncNow)

	if got, _ := mine.Mission("ms_aabbccddeeff"); got.LastMessage != "fresh" || merge.Changed() {
		t.Errorf("message %q changed %v, want this host's run untouched", got.LastMessage, merge.Changed())
	}
}

func TestMergeHigherEpochTakesTheLease(t *testing.T) {
	mine, theirs := hostSnapshot(laptop), hostSnapshot(mini)
	mine.PutMission(running(laptop))

	remote := running(mini)
	remote.Lease = Lease{Holder: mini, Epoch: 2}
	remote.LastMessage = "took over"
	theirs.PutMission(remote)

	merge := mine.MergePeer(theirs.Payload(), true, syncNow)

	got, _ := mine.Mission("ms_aabbccddeeff")

	if got.Lease.Holder != mini || got.LastMessage != "took over" {
		t.Errorf("lease %+v message %q, want the takeover to stand even against the primary", got.Lease, got.LastMessage)
	}

	if len(merge.Lost) != 1 {
		t.Errorf("Lost = %v, want the mission reported so its agent is stopped", merge.Lost)
	}
}

// Both hosts took the mission at the same epoch: the classic split brain after
// a takeover of a laptop that was only asleep.
func TestMergeLeaseTieGoesToThePrimary(t *testing.T) {
	primarySide, secondarySide := hostSnapshot(laptop), hostSnapshot(mini)

	onLaptop := running(laptop)
	onLaptop.Lease = Lease{Holder: laptop, Epoch: 2}
	primarySide.PutMission(onLaptop)

	onMini := running(mini)
	onMini.Lease = Lease{Holder: mini, Epoch: 2}
	secondarySide.PutMission(onMini)

	laptopPayload, miniPayload := primarySide.Payload(), secondarySide.Payload()

	atPrimary := primarySide.MergePeer(miniPayload, true, syncNow)
	atSecondary := secondarySide.MergePeer(laptopPayload, false, syncNow)

	p, _ := primarySide.Mission("ms_aabbccddeeff")
	s, _ := secondarySide.Mission("ms_aabbccddeeff")

	if p.Lease.Holder != laptop || s.Lease.Holder != laptop {
		t.Errorf("holders = %s and %s, want both to settle on the primary", p.Lease.Holder, s.Lease.Holder)
	}

	if len(atPrimary.Lost) != 0 || len(atSecondary.Lost) != 1 {
		t.Errorf("lost: primary %v secondary %v, want only the secondary to stand down", atPrimary.Lost, atSecondary.Lost)
	}
}

func TestMergeReportsAGainedLease(t *testing.T) {
	mine, theirs := hostSnapshot(laptop), hostSnapshot(mini)
	mine.PutMission(running(mini))

	remote := running(mini)
	remote.Lease = Lease{Holder: laptop, Epoch: 2}
	remote.Status, remote.AgentState = StatusDebrief, AgentIdle
	theirs.PutMission(remote)

	merge := mine.MergePeer(theirs.Payload(), true, syncNow)

	got, _ := mine.Mission("ms_aabbccddeeff")

	if len(merge.Gained) != 1 || got.Status != StatusDebrief {
		t.Errorf("gained %v status %s, want the lease handed back with its final run", merge.Gained, got.Status)
	}
}

func TestMergeAppliesAndRemembersADelete(t *testing.T) {
	mine, theirs := hostSnapshot(laptop), hostSnapshot(mini)

	local := running(mini)
	local.MissionDir = "/mirror"
	mine.PutMission(local)

	theirs.Tombstones = []Tombstone{{Kind: TombstoneMission, ID: "ms_aabbccddeeff", At: syncNow}}

	merge := mine.MergePeer(theirs.Payload(), true, syncNow)

	if _, ok := mine.Mission("ms_aabbccddeeff"); ok {
		t.Fatal("deleted mission survived the merge")
	}

	if len(merge.Removed) != 1 || merge.Removed[0].MissionDir != "/mirror" {
		t.Errorf("Removed = %+v, want the mission as it was so its worktree can be reclaimed", merge.Removed)
	}

	if !mine.Tombstoned(TombstoneMission, "ms_aabbccddeeff") {
		t.Error("the delete should be remembered here too")
	}
}

// The peer's payload was built before it heard of this host's delete, so it
// still lists the mission. It must not come back.
func TestMergeDoesNotResurrectALocallyDeletedMission(t *testing.T) {
	mine, theirs := hostSnapshot(laptop), hostSnapshot(mini)
	mine.Tombstones = []Tombstone{{Kind: TombstoneMission, ID: "ms_aabbccddeeff", At: syncNow}}
	theirs.PutMission(running(mini))

	mine.MergePeer(theirs.Payload(), true, syncNow)

	if _, ok := mine.Mission("ms_aabbccddeeff"); ok {
		t.Error("a mission deleted here was handed back by the peer")
	}
}

func TestMergeOperationKeepsLocalRepoPaths(t *testing.T) {
	mine, theirs := hostSnapshot(laptop), hostSnapshot(mini)

	local := sampleOperation("Area")
	local.Rev = 1
	mine.PutOperation(local)

	remote := sampleOperation("Area renamed")
	remote.Rev = 2
	remote.Repos = []Repo{{Name: "weave", Path: "/home/me/weave"}, {Name: "loom", Path: "/home/me/loom"}}
	theirs.PutOperation(remote)

	merge := mine.MergePeer(theirs.Payload(), true, syncNow)

	got, _ := mine.Operation("op_aabbccddeeff")

	if len(merge.Operations) != 1 || got.Name != "Area renamed" || got.Rev != 2 {
		t.Fatalf("operation = %+v, want the newer one", got)
	}

	if got.Repos[0].Path != "/dev/weave" || got.Repos[1].Path != "" {
		t.Errorf("repos = %+v, want this host's path kept and the new repo unresolved", got.Repos)
	}
}

// Merging the same payload twice must settle: a merge that kept reporting
// changes would rewrite the state file on every exchange forever.
func TestMergeIsIdempotent(t *testing.T) {
	mine, theirs := hostSnapshot(laptop), hostSnapshot(mini)
	theirs.PutOperation(sampleOperation("Area"))
	theirs.PutMission(launchedMission())

	mine.MergePeer(theirs.Payload(), true, syncNow)

	if again := mine.MergePeer(theirs.Payload(), true, syncNow); again.Changed() {
		t.Errorf("second merge reported changes: %+v", again)
	}
}
