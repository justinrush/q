package launch

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// reclaimMission returns a launched mission whose worktree exists on disk.
func reclaimMission(t *testing.T) (mission.Mission, mission.Operation) {
	t.Helper()

	dir := t.TempDir()

	if err := os.MkdirAll(filepath.Join(dir, "weave"), 0o700); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	ms := launchedMission(t, dir)
	ms.Work = map[string]mission.RepoWork{
		"weave": {
			RepoName:     "weave",
			WorktreePath: filepath.Join(dir, "weave"),
			Branch:       "jarush/add-endpoint",
			BaseSHA:      "deadbeef",
			Created:      true,
		},
	}

	return ms, testOperation("/dev/weave")
}

// expectRepoState registers the git answers describing one worktree.
func expectRepoState(fake *runner.Fake, ms mission.Mission, status, ahead string, pushed bool) {
	path := ms.Work["weave"].WorktreePath

	fake.Expect(gitBin+" -C /dev/weave rev-parse --git-common-dir", "/dev/weave/.git")
	fake.Expect(gitBin+" -C /dev/weave symbolic-ref --short refs/remotes/origin/HEAD", "origin/main")
	fake.Expect(gitBin+" -C "+path+" status --porcelain=v1", status)
	fake.Expect(gitBin+" -C "+path+" rev-list --count deadbeef..HEAD", ahead)
	fake.Expect(gitBin+" -C "+path+" diff --shortstat deadbeef..HEAD", " 1 file changed")

	remoteRef := gitBin + " -C /dev/weave/.git show-ref --verify --quiet refs/remotes/origin/jarush/add-endpoint"
	if pushed {
		fake.Expect(remoteRef, "")
	} else {
		fake.ExpectExit(remoteRef, 1, "")
	}
}

// A clean worktree on a branch with no commits has nothing worth keeping.
func TestPlanDiscardsACleanUnusedBranch(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms, operation := reclaimMission(t)

	expectRepoState(fake, ms, "", "0", false)
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "")

	plan, err := launcher.PlanReclaim(t.Context(), operation, ms)
	if err != nil {
		t.Fatalf("PlanReclaim: %v", err)
	}

	if len(plan.Repos) != 1 || plan.Repos[0].Action != mission.ActionDiscard {
		t.Fatalf("plan = %+v, want discard", plan.Repos)
	}

	if plan.NeedsForce {
		t.Error("a clean worktree needs no force")
	}
}

func TestPlanUsesFrozenReposAfterOperationChanges(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms, operation := reclaimMission(t)
	ms.LaunchRepos = operation.Repos
	ms.LaunchReposFrozen = true
	operation.Repos = []mission.Repo{{Name: "different", Path: "/dev/different"}}

	expectRepoState(fake, ms, "", "0", false)
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "")

	plan, err := launcher.PlanReclaim(t.Context(), operation, ms)
	if err != nil {
		t.Fatalf("PlanReclaim: %v", err)
	}

	if len(plan.Repos) != 1 || plan.Repos[0].Repo != "weave" {
		t.Fatalf("plan = %+v, want frozen weave repo", plan.Repos)
	}
}

// Commits that exist only here are worth keeping even though the worktree is not.
func TestPlanKeepsABranchHoldingUnpushedCommits(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms, operation := reclaimMission(t)

	expectRepoState(fake, ms, "", "3", false)
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "")

	plan, err := launcher.PlanReclaim(t.Context(), operation, ms)
	if err != nil {
		t.Fatalf("PlanReclaim: %v", err)
	}

	if plan.Repos[0].Action != mission.ActionKeepBranch {
		t.Errorf("mission.Action = %q, want keep-branch", plan.Repos[0].Action)
	}

	if len(plan.KeptBranches) != 1 {
		t.Errorf("KeptBranches = %v, want the branch named", plan.KeptBranches)
	}
}

// Once the work is pushed it exists somewhere other than this machine, so the local
// branch is not the last copy of anything.
func TestPlanDiscardsAPushedBranch(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms, operation := reclaimMission(t)

	expectRepoState(fake, ms, "", "3", true)
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "")

	plan, err := launcher.PlanReclaim(t.Context(), operation, ms)
	if err != nil {
		t.Fatalf("PlanReclaim: %v", err)
	}

	if plan.Repos[0].Action != mission.ActionDiscard {
		t.Errorf("mission.Action = %q, want discard for pushed work", plan.Repos[0].Action)
	}

	if !plan.Repos[0].Pushed {
		t.Error("Pushed should be reported")
	}
}

// This is the case that cannot be undone, so it is never done without being asked.
func TestReclaimRefusesUncommittedWorkWithoutForce(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms, operation := reclaimMission(t)

	expectRepoState(fake, ms, " M main.go", "0", false)
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "")

	plan, err := launcher.PlanReclaim(t.Context(), operation, ms)
	if err != nil {
		t.Fatalf("PlanReclaim: %v", err)
	}

	if !plan.NeedsForce || plan.Repos[0].Action != mission.ActionNeedsForce {
		t.Fatalf("plan = %+v, want needs-force", plan)
	}

	_, err = launcher.Reclaim(t.Context(), operation, ms, false)
	if !errors.Is(err, mission.ErrNeedsForce) {
		t.Fatalf("err = %v, want mission.ErrNeedsForce", err)
	}

	// Nothing may be touched by a refused reclaim.
	for _, line := range fake.Argv() {
		if strings.Contains(line, "worktree remove") || strings.Contains(line, "kill-session") {
			t.Errorf("a refused reclaim acted anyway: %q", line)
		}
	}

	if _, statErr := os.Stat(ms.MissionDir); statErr != nil {
		t.Error("the mission directory should still exist after a refusal")
	}
}

// The tmux session is killed first: removing a worktree that is a process's working
// directory leaves git and the shell disagreeing about what exists.
func TestReclaimKillsTheSessionBeforeRemovingWorktrees(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms, operation := reclaimMission(t)

	expectRepoState(fake, ms, "", "0", false)
	fake.Expect(tmuxBin+" has-session -t ="+testSession, "")

	if _, err := launcher.Reclaim(t.Context(), operation, ms, false); err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	transcript := fake.Argv()

	kill, remove := -1, -1

	for i, line := range transcript {
		if strings.Contains(line, "kill-session") && kill < 0 {
			kill = i
		}

		if strings.Contains(line, "worktree remove") && remove < 0 {
			remove = i
		}
	}

	if kill < 0 {
		t.Fatalf("the session was not killed:\n%s", fake.Transcript())
	}

	if remove < 0 {
		t.Fatalf("the worktree was not removed:\n%s", fake.Transcript())
	}

	if kill > remove {
		t.Errorf("the session must be killed before the worktree is removed:\n%s", fake.Transcript())
	}
}

func TestReclaimRemovesTheMissionDirectory(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms, operation := reclaimMission(t)

	expectRepoState(fake, ms, "", "0", false)
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "")

	report, err := launcher.Reclaim(t.Context(), operation, ms, false)
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	if len(report.Failures) != 0 {
		t.Fatalf("Failures = %v", report.Failures)
	}

	if _, err := os.Stat(ms.MissionDir); !os.IsNotExist(err) {
		t.Errorf("mission directory should be gone, stat err = %v", err)
	}
}

// A partial result has to be visible rather than silently incomplete, so the mission
// directory survives when something could not be reclaimed.
func TestReclaimKeepsTheMissionDirectoryOnFailure(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms, operation := reclaimMission(t)

	expectRepoState(fake, ms, "", "0", false)
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "")
	fake.ExpectExit(gitBin+" -C /dev/weave/.git worktree remove "+ms.Work["weave"].WorktreePath,
		1, "worktree is locked")

	report, err := launcher.Reclaim(t.Context(), operation, ms, false)
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	if len(report.Failures) == 0 {
		t.Error("the failure should be reported")
	}

	if _, err := os.Stat(ms.MissionDir); err != nil {
		t.Error("the mission directory should survive a failed reclaim")
	}
}

// git refusing to delete a branch means it holds something, so it is kept rather than
// forced away.
func TestReclaimKeepsABranchGitRefusesToDelete(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms, operation := reclaimMission(t)

	expectRepoState(fake, ms, "", "0", false)
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "")
	fake.ExpectExit(gitBin+" -C /dev/weave/.git branch -d jarush/add-endpoint", 1, "not fully merged")

	report, err := launcher.Reclaim(t.Context(), operation, ms, false)
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	if len(report.KeptBranches) != 1 {
		t.Errorf("KeptBranches = %v, want the unmerged branch kept", report.KeptBranches)
	}

	if len(report.DeletedBranches) != 0 {
		t.Errorf("DeletedBranches = %v, want none", report.DeletedBranches)
	}
}

// Forcing past a dirty tree is the case most likely to be regretted, so the branch is
// kept when it holds commits even though the worktree is discarded.
func TestForcedReclaimKeepsABranchWithCommits(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms, operation := reclaimMission(t)

	expectRepoState(fake, ms, " M main.go", "2", false)
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "")

	report, err := launcher.Reclaim(t.Context(), operation, ms, true)
	if err != nil {
		t.Fatalf("Reclaim: %v", err)
	}

	if len(report.KeptBranches) != 1 {
		t.Errorf("KeptBranches = %v, want the branch kept", report.KeptBranches)
	}

	if !strings.Contains(fake.Transcript(), "worktree remove --force") {
		t.Errorf("a forced reclaim should force the removal:\n%s", fake.Transcript())
	}
}

// A worktree already gone by hand is not a failure; there is nothing to remove and
// nothing to lose.
func TestPlanTreatsAMissingWorktreeAsDiscardable(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms, operation := reclaimMission(t)

	work := ms.Work["weave"]
	work.WorktreePath = filepath.Join(ms.MissionDir, "gone")
	ms.Work["weave"] = work

	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "")

	plan, err := launcher.PlanReclaim(t.Context(), operation, ms)
	if err != nil {
		t.Fatalf("PlanReclaim: %v", err)
	}

	if plan.Repos[0].Action != mission.ActionDiscard {
		t.Errorf("mission.Action = %q, want discard", plan.Repos[0].Action)
	}
}

// A checkout the user moved or deleted must be reported rather than crashing the delete.
func TestPlanReportsAnUninspectableRepo(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms, operation := reclaimMission(t)

	fake.ExpectExit(gitBin+" -C /dev/weave rev-parse --git-common-dir", 128, "not a git repository")
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "")

	plan, err := launcher.PlanReclaim(t.Context(), operation, ms)
	if err != nil {
		t.Fatalf("PlanReclaim: %v", err)
	}

	if plan.Repos[0].Action != mission.ActionUnavailable {
		t.Errorf("mission.Action = %q, want unavailable", plan.Repos[0].Action)
	}

	if plan.Repos[0].Reason == "" {
		t.Error("the reason should be reported")
	}
}

// A running agent is worth mentioning before it is stopped.
func TestPlanReportsALiveSession(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	ms, operation := reclaimMission(t)

	expectRepoState(fake, ms, "", "0", false)
	fake.Expect(tmuxBin+" has-session -t ="+testSession, "")

	plan, err := launcher.PlanReclaim(t.Context(), operation, ms)
	if err != nil {
		t.Fatalf("PlanReclaim: %v", err)
	}

	if !plan.SessionAlive {
		t.Error("SessionAlive should be set")
	}

	if plan.TmuxSession != testSession {
		t.Errorf("TmuxSession = %q", plan.TmuxSession)
	}
}
