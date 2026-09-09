package git

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/runner"
)

const provisionGitBin = "/usr/bin/git"

// noSessions stands in for tmux. Provisioning never starts a session; it only
// asks whether one is still holding a worktree it is about to remove.
type noSessions struct{}

func (noSessions) HasSession(context.Context, string) bool { return false }
func (noSessions) KillSession(context.Context, string) error {
	return nil
}

// newTestProvisioner returns a provisioner whose git commands are faked.
func newTestProvisioner(t *testing.T) (*Provisioner, *runner.Fake, paths.Dirs) {
	t.Helper()

	root := t.TempDir()
	dirs := paths.Dirs{Data: filepath.Join(root, "data"), State: filepath.Join(root, "state")}

	if err := dirs.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	fake := runner.NewFake()

	return NewProvisioner(dirs, New(provisionGitBin, fake), noSessions{},
		WithBranchPrefix("jarush")), fake, dirs
}

// seedRepo registers the git answers provisioning one clean repo needs.
func seedRepo(fake *runner.Fake, repoPath, commonDir string) {
	fake.Expect(provisionGitBin+" -C "+repoPath+" rev-parse --git-common-dir", commonDir)
	fake.Expect(provisionGitBin+" -C "+repoPath+" symbolic-ref --short refs/remotes/origin/HEAD", "origin/main")
	fake.Expect(provisionGitBin+" -C "+commonDir+" rev-parse refs/remotes/origin/main", "deadbeefcafebabe")
	fake.ExpectExit(provisionGitBin+" -C "+commonDir+" show-ref --verify --quiet refs/heads/jarush/add-endpoint", 1, "")
}

func provisionOperation(repoPath string) mission.Operation {
	return mission.Operation{
		ID:    "op_aabbccddeeff",
		Name:  "Discussions API",
		Slug:  "discussions-api",
		Repos: []mission.Repo{{Name: "weave", Path: repoPath}},
	}
}

func provisionMission() mission.Mission {
	return mission.Mission{
		ID:     "ms_aabbccddeeff",
		Name:   "add endpoint",
		Slug:   "add-endpoint",
		Tool:   mission.ToolClaude,
		Status: mission.StatusBriefing,
	}
}

// If the daemon exits after git creates a worktree but before it journals that
// repo, the ownership marker makes the existing checkout safe to recover.
func TestPrepareRecoversUnjournaledOwnedWorktree(t *testing.T) {
	provisioner, fake, dirs := newTestProvisioner(t)
	ms := provisionMission()
	operation := provisionOperation("/dev/weave")

	state, err := provisioner.prepareMissionDir(operation, &ms)
	if err != nil {
		t.Fatalf("prepareMissionDir: %v", err)
	}

	if state.MissionID != ms.ID {
		t.Fatalf("provision owner = %q, want %q", state.MissionID, ms.ID)
	}

	worktreePath := filepath.Join(ms.MissionDir, "weave")
	if err := os.MkdirAll(worktreePath, 0o700); err != nil {
		t.Fatalf("creating interrupted worktree: %v", err)
	}

	seedRepo(fake, "/dev/weave", "/dev/weave/.git")
	fake.Expect(provisionGitBin+" -C /dev/weave/.git worktree list --porcelain", strings.Join([]string{
		"worktree " + worktreePath,
		"HEAD originalbase",
		"branch refs/heads/jarush/add-endpoint",
		"",
	}, "\n"))

	provisioned, err := provisioner.Prepare(t.Context(), operation, &ms)
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	work := provisioned.Work["weave"]
	if work.BaseSHA != "originalbase" || work.Branch != "jarush/add-endpoint" || !work.Created {
		t.Errorf("recovered work = %+v", work)
	}

	for _, line := range fake.Argv() {
		if strings.Contains(line, "worktree add") {
			t.Errorf("recovery tried to recreate the worktree: %q", line)
		}
	}

	data, err := os.ReadFile(filepath.Join(dirs.MissionDir("discussions-api--add-endpoint"),
		mission.ArtifactDir, provisionStateFile))
	if err != nil {
		t.Fatalf("reading provision journal: %v", err)
	}

	if !strings.Contains(string(data), "originalbase") {
		t.Errorf("provision journal did not record recovered base:\n%s", data)
	}
}

// Two missions may have the same name in one operation. The ownership marker keeps
// the second from provisioning into the first mission's directory.
func TestPrepareUsesUniqueDirectoryForOwnershipConflict(t *testing.T) {
	provisioner, fake, dirs := newTestProvisioner(t)
	owner := provisionMission()
	owner.ID = "ms_111111111111"
	operation := provisionOperation("/dev/weave")

	if _, err := provisioner.prepareMissionDir(operation, &owner); err != nil {
		t.Fatalf("claiming first mission directory: %v", err)
	}

	seedRepo(fake, "/dev/weave", "/dev/weave/.git")

	ms := provisionMission()
	if _, err := provisioner.Prepare(t.Context(), operation, &ms); err != nil {
		t.Fatalf("Prepare: %v", err)
	}

	wantDir := dirs.MissionDir("discussions-api--add-endpoint--aabbccddeeff")
	if ms.MissionDir != wantDir {
		t.Errorf("MissionDir = %q, want %q", ms.MissionDir, wantDir)
	}
}

func TestDefaultBranchPrefixFallsBackToUser(t *testing.T) {
	t.Setenv("USER", "")
	t.Setenv("LOGNAME", "")

	if got := DefaultBranchPrefix(); got != "q" {
		t.Errorf("DefaultBranchPrefix() = %q, want q", got)
	}
}

// A mission may name the branch each of its repos is cut from. Everything else
// about the worktree — its path and its own branch — is unchanged, so the only
// observable difference is where the base SHA came from.
func TestProvisionBasesWorktreeOnTheMissionsBranch(t *testing.T) {
	cases := []struct {
		name    string
		bases   map[string]string
		wantRef string
	}{
		{
			name:    "no override uses the default branch",
			wantRef: "refs/remotes/origin/main",
		},
		{
			name:    "override bases on the named branch",
			bases:   map[string]string{"weave": "feat/x"},
			wantRef: "refs/remotes/origin/feat/x",
		},
		{
			name:    "an empty override falls back to the default branch",
			bases:   map[string]string{"weave": "  "},
			wantRef: "refs/remotes/origin/main",
		},
		{
			name:    "an override for another repo is ignored",
			bases:   map[string]string{"other": "feat/x"},
			wantRef: "refs/remotes/origin/main",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			provisioner, fake, _ := newTestProvisioner(t)
			operation := provisionOperation("/dev/weave")

			ms := provisionMission()
			ms.BaseBranches = tc.bases

			seedRepo(fake, "/dev/weave", "/dev/weave/.git")
			fake.Expect(provisionGitBin+" -C /dev/weave/.git rev-parse "+tc.wantRef, "basesha1234567")

			provisioned, err := provisioner.Prepare(t.Context(), operation, &ms)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}

			work := provisioned.Work["weave"]
			if work.BaseRef != tc.wantRef {
				t.Errorf("BaseRef = %q, want %q", work.BaseRef, tc.wantRef)
			}

			if work.Branch != "jarush/add-endpoint" {
				t.Errorf("Branch = %q, want the mission's own branch", work.Branch)
			}

			branch := strings.TrimPrefix(tc.wantRef, "refs/remotes/origin/")
			wantFetch := provisionGitBin + " -C /dev/weave/.git fetch --no-tags origin " +
				"+refs/heads/" + branch + ":refs/remotes/origin/" + branch

			if !strings.Contains(fake.Transcript(), wantFetch) {
				t.Errorf("did not fetch the base branch; wanted\n%s\ngot\n%s", wantFetch, fake.Transcript())
			}

			wantAdd := "worktree add -b jarush/add-endpoint " +
				filepath.Join(ms.MissionDir, "weave") + " " + work.BaseSHA

			if !strings.Contains(fake.Transcript(), wantAdd) {
				t.Errorf("did not cut the worktree from the base; wanted\n%s\ngot\n%s", wantAdd, fake.Transcript())
			}
		})
	}
}

// A base branch that origin does not have fails in the fetch. The message has to
// name the branch, or it is indistinguishable from the network being down.
func TestProvisionReportsAMissingBaseBranch(t *testing.T) {
	provisioner, fake, _ := newTestProvisioner(t)
	operation := provisionOperation("/dev/weave")

	ms := provisionMission()
	ms.BaseBranches = map[string]string{"weave": "gone"}

	seedRepo(fake, "/dev/weave", "/dev/weave/.git")
	fake.ExpectExit(provisionGitBin+" -C /dev/weave/.git fetch --no-tags origin "+
		"+refs/heads/gone:refs/remotes/origin/gone", 128,
		"fatal: couldn't find remote ref refs/heads/gone")

	_, err := provisioner.Prepare(t.Context(), operation, &ms)
	if err == nil {
		t.Fatal("Prepare succeeded on a base branch origin does not have")
	}

	for _, want := range []string{"gone", "weave"} {
		if !strings.Contains(err.Error(), want) {
			t.Errorf("error %q does not mention %q", err, want)
		}
	}
}
