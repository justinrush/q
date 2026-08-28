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
