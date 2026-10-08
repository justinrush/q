package mission

import (
	"testing"
	"time"
)

// Every persisted field has to say what it means to a paired host. A field
// added without a class would be silently left out of both the wire projection
// and the merge, which is how a worktree path ends up being used on the wrong
// machine.
func TestEveryPersistedFieldDeclaresAClass(t *testing.T) {
	for _, field := range unclassified() {
		t.Errorf("%s has no sync class; tag it q:\"key|spec|run|local|meta\"", field)
	}
}

// launchedMission returns a mission as it looks on the host running it, with
// every host-local field populated.
func launchedMission() Mission {
	started := time.Date(2026, 10, 7, 9, 0, 0, 0, time.UTC)

	return Mission{
		ID:             "ms_aabbccddeeff",
		OperationID:    "op_aabbccddeeff",
		Name:           "Fix the flaky test",
		Slug:           "fix-the-flaky-test",
		Tool:           ToolClaude,
		Prompt:         "make it pass",
		ExtraRepos:     []Repo{{Name: "extra", Path: "/Users/me/dev/extra", CommonDir: "/Users/me/dev/extra/.git", URL: "example.com/me/extra"}},
		LaunchRepos:    []Repo{{Name: "weave", Path: "/Users/me/dev/weave", URL: "example.com/me/weave"}},
		Status:         StatusActive,
		MissionDir:     "/Users/me/.local/share/q/missions/op--fix",
		TmuxSession:    "q-op--fix-aabbccddeeff",
		AgentPaneID:    "%13",
		AgentSessionID: "3d0c8f0e-1111-4222-8333-444455556666",
		HookEpoch:      2,
		TranscriptPath: "/Users/me/.claude/projects/x/y.jsonl",
		LocalBadges:    []Badge{{Kind: BadgeLocalEdits}},
		Work: map[string]RepoWork{
			"weave": {
				RepoName:      "weave",
				WorktreePath:  "/Users/me/.local/share/q/missions/op--fix/weave",
				Branch:        "me/fix-the-flaky-test",
				BaseRef:       "refs/remotes/origin/main",
				BaseSHA:       "1111111111111111111111111111111111111111",
				DebriefPaneID: "%14",
				Created:       true,
				SyncBase:      "2222222222222222222222222222222222222222",
			},
		},
		AgentState: AgentBusy,
		StartedAt:  &started,
		Lease:      Lease{Holder: "h_aaaaaaaaaaaa", Epoch: 1},
		SpecRev:    1,
	}
}

// The projection sent to a peer must carry nothing that is only true here.
func TestSharedClearsEverythingHostLocal(t *testing.T) {
	shared := launchedMission().Shared()

	for name, got := range map[string]string{
		"MissionDir":       shared.MissionDir,
		"TmuxSession":      shared.TmuxSession,
		"AgentPaneID":      shared.AgentPaneID,
		"AgentSessionID":   shared.AgentSessionID,
		"TranscriptPath":   shared.TranscriptPath,
		"ExtraRepos path":  shared.ExtraRepos[0].Path + shared.ExtraRepos[0].CommonDir,
		"LaunchRepos path": shared.LaunchRepos[0].Path,
		"WorktreePath":     shared.Work["weave"].WorktreePath,
		"Branch":           shared.Work["weave"].Branch,
		"DebriefPaneID":    shared.Work["weave"].DebriefPaneID,
		"SyncBase":         shared.Work["weave"].SyncBase,
	} {
		if got != "" {
			t.Errorf("%s = %q, want it cleared", name, got)
		}
	}

	if shared.HookEpoch != 0 || len(shared.LocalBadges) != 0 || shared.Work["weave"].Created {
		t.Errorf("host-local values survived: epoch %d, badges %v, created %v",
			shared.HookEpoch, shared.LocalBadges, shared.Work["weave"].Created)
	}

	// What the peer does need must still be there.
	if shared.Work["weave"].BaseSHA == "" || shared.ExtraRepos[0].URL == "" || shared.Status != StatusActive {
		t.Errorf("shared fields were cleared too: %+v", shared)
	}
}

// Projecting must not reach back into the mission it was given.
func TestSharedDoesNotMutateItsReceiver(t *testing.T) {
	ms := launchedMission()
	_ = ms.Shared()

	if ms.Work["weave"].WorktreePath == "" || ms.ExtraRepos[0].Path == "" || ms.MissionDir == "" {
		t.Error("Shared cleared fields on the original mission")
	}
}

// Two hosts holding the same brief at different checkout paths agree.
func TestSpecEqualIgnoresHostLocalPaths(t *testing.T) {
	here, there := launchedMission(), launchedMission()
	there.ExtraRepos = []Repo{{Name: "extra", Path: "/home/me/src/extra", URL: "example.com/me/extra"}}
	there.MissionDir = "/home/me/.local/share/q/missions/op--fix"

	if !specEqual(here, there) {
		t.Error("briefs differing only in host-local paths should be equal")
	}

	there.Prompt = "make it pass, properly"

	if specEqual(here, there) {
		t.Error("a changed prompt should make the briefs differ")
	}
}

// A time that has been through JSON is the same instant in a different
// representation, and must not read as a change.
func TestSpecEqualComparesTimesAsInstants(t *testing.T) {
	here, there := launchedMission(), launchedMission()
	here.CreatedAt = time.Date(2026, 10, 7, 9, 0, 0, 0, time.FixedZone("x", 3600))
	there.CreatedAt = here.CreatedAt.UTC()

	if !specEqual(here, there) {
		t.Error("the same instant in two zones should be equal")
	}
}

func TestAdoptReposKeepsLocalPaths(t *testing.T) {
	local := []Repo{{Name: "weave", Path: "/home/me/src/weave", CommonDir: "/home/me/src/weave/.git", URL: "example.com/me/weave"}}
	incoming := []Repo{
		{Name: "weave", URL: "example.com/me/weave", DefaultBranch: "main"},
		{Name: "loom", URL: "example.com/me/loom"},
	}

	got := adoptRepos(local, incoming)

	if len(got) != 2 {
		t.Fatalf("got %d repos, want the incoming two", len(got))
	}

	if got[0].Path != "/home/me/src/weave" || got[0].DefaultBranch != "main" {
		t.Errorf("known repo = %+v, want local path and incoming default branch", got[0])
	}

	if got[1].Path != "" {
		t.Errorf("unknown repo path = %q, want it left for the daemon to resolve", got[1].Path)
	}
}

// A same-named checkout of a different repository is not the one meant.
func TestAdoptReposDoesNotMatchADifferentOrigin(t *testing.T) {
	local := []Repo{{Name: "weave", Path: "/home/me/src/weave", URL: "example.com/someone-else/weave"}}

	got := adoptRepos(local, []Repo{{Name: "weave", URL: "example.com/me/weave"}})

	if got[0].Path != "" {
		t.Errorf("path = %q, want none: the origins differ", got[0].Path)
	}
}

// A mirror worktree the holder no longer lists must stay on record until this
// host has removed it.
func TestAdoptWorkKeepsAnUnlistedLocalWorktree(t *testing.T) {
	local := map[string]RepoWork{"weave": {RepoName: "weave", WorktreePath: "/w", Created: true}}

	got := adoptWork(local, nil)

	if got["weave"].WorktreePath != "/w" {
		t.Errorf("work = %+v, want the local worktree kept", got)
	}
}
