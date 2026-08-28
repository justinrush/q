package git

import (
	"strings"
	"sync"
	"testing"

	"github.com/justinrush/q/internal/runner"
)

const gitBin = "/usr/bin/git"

// newTestGit returns a Git backed by a fake runner.
func newTestGit() (*Client, *runner.Fake) {
	fake := runner.NewFake()

	return New(gitBin, fake), fake
}

// The user's global config sets fetch.all, fetch.force, and fetch.prune, so a bare
// `git fetch` would contact every remote and force-move every tracking ref. This is
// the regression test for that: every fetch this package emits must name origin and
// one explicit refspec.
func TestFetchIsAlwaysExplicit(t *testing.T) {
	git, fake := newTestGit()

	if err := git.FetchBranch(t.Context(), "/repo/.git", "main"); err != nil {
		t.Fatalf("FetchBranch: %v", err)
	}

	argv := fake.Argv()
	if len(argv) != 1 {
		t.Fatalf("expected one call, got %q", argv)
	}

	want := gitBin + " -C /repo/.git fetch --no-tags origin +refs/heads/main:refs/remotes/origin/main"
	if argv[0] != want {
		t.Errorf("argv = %q, want %q", argv[0], want)
	}
}

// Nothing in this package may emit a fetch without both a remote and a refspec.
func TestNoCodePathEmitsABareFetch(t *testing.T) {
	git, fake := newTestGit()
	ctx := t.Context()

	fake.Expect(gitBin+" -C /repo rev-parse --git-common-dir", "/repo/.git")
	fake.Expect(gitBin+" -C /repo symbolic-ref --short refs/remotes/origin/HEAD", "origin/main")
	fake.Expect(gitBin+" -C /repo rev-parse refs/remotes/origin/main", "abc123")

	_, _ = git.CommonDir(ctx, "/repo")
	_, _ = git.DefaultBranch(ctx, "/repo")
	_ = git.FetchBranch(ctx, "/repo", "main")
	_, _ = git.RevParse(ctx, "/repo", "refs/remotes/origin/main")
	_ = git.WorktreeAdd(ctx, "/repo", "/wt", "branch", "abc123")
	_, _ = git.TouchedSummary(ctx, "/wt", "abc123")

	for _, line := range fake.Argv() {
		if !strings.Contains(line, " fetch") {
			continue
		}

		if !strings.Contains(line, " origin ") {
			t.Errorf("fetch without an explicit remote: %q", line)
		}

		if !strings.Contains(line, "refs/heads/") {
			t.Errorf("fetch without an explicit refspec: %q", line)
		}
	}
}

func TestDefaultBranchPrefersOriginHead(t *testing.T) {
	git, fake := newTestGit()
	fake.Expect(gitBin+" -C /repo symbolic-ref --short refs/remotes/origin/HEAD", "origin/master")

	got, err := git.DefaultBranch(t.Context(), "/repo")
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}

	// Not every repo is on main; one of the user's own is on master.
	if got != "master" {
		t.Errorf("DefaultBranch = %q, want %q", got, "master")
	}
}

func TestDefaultBranchFallsBackToLsRemote(t *testing.T) {
	git, fake := newTestGit()
	fake.ExpectExit(gitBin+" -C /repo symbolic-ref --short refs/remotes/origin/HEAD", 1, "not a symbolic ref")
	fake.Expect(gitBin+" -C /repo ls-remote --symref origin HEAD",
		"ref: refs/heads/trunk\tHEAD\nabc123\tHEAD")

	got, err := git.DefaultBranch(t.Context(), "/repo")
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}

	if got != "trunk" {
		t.Errorf("DefaultBranch = %q, want %q", got, "trunk")
	}
}

func TestDefaultBranchFallsBackToProbingRefs(t *testing.T) {
	git, fake := newTestGit()
	fake.ExpectExit(gitBin+" -C /repo symbolic-ref --short refs/remotes/origin/HEAD", 1, "")
	fake.ExpectExit(gitBin+" -C /repo ls-remote --symref origin HEAD", 128, "")
	fake.ExpectExit(gitBin+" -C /repo config --get init.defaultBranch", 1, "")
	fake.ExpectExit(gitBin+" -C /repo show-ref --verify --quiet refs/remotes/origin/main", 1, "")
	fake.Expect(gitBin+" -C /repo show-ref --verify --quiet refs/remotes/origin/master", "")

	got, err := git.DefaultBranch(t.Context(), "/repo")
	if err != nil {
		t.Fatalf("DefaultBranch: %v", err)
	}

	if got != "master" {
		t.Errorf("DefaultBranch = %q, want %q", got, "master")
	}
}

func TestDefaultBranchFailsWhenNothingResolves(t *testing.T) {
	git, fake := newTestGit()
	fake.Default = runner.Result{ExitCode: 1}
	fake.ExpectExit(gitBin+" -C /repo symbolic-ref --short refs/remotes/origin/HEAD", 1, "")
	fake.ExpectExit(gitBin+" -C /repo ls-remote --symref origin HEAD", 128, "")
	fake.ExpectExit(gitBin+" -C /repo config --get init.defaultBranch", 1, "")
	fake.ExpectExit(gitBin+" -C /repo show-ref --verify --quiet refs/remotes/origin/main", 1, "")
	fake.ExpectExit(gitBin+" -C /repo show-ref --verify --quiet refs/remotes/origin/master", 1, "")

	if _, err := git.DefaultBranch(t.Context(), "/repo"); err == nil {
		t.Error("expected an error when no default branch can be determined")
	}
}

// A worktree's own .git is a file pointing elsewhere, so ref operations have to
// target the common directory. A relative answer must be resolved against the repo.
func TestCommonDirResolvesRelativeAnswers(t *testing.T) {
	git, fake := newTestGit()
	fake.Expect(gitBin+" -C /repo rev-parse --git-common-dir", ".git")

	got, err := git.CommonDir(t.Context(), "/repo")
	if err != nil {
		t.Fatalf("CommonDir: %v", err)
	}

	if got != "/repo/.git" {
		t.Errorf("CommonDir = %q, want %q", got, "/repo/.git")
	}
}

func TestCommonDirRejectsNonRepository(t *testing.T) {
	git, fake := newTestGit()
	fake.Expect(gitBin+" -C /tmp rev-parse --git-common-dir", "")

	if _, err := git.CommonDir(t.Context(), "/tmp"); err == nil {
		t.Error("expected an error for a directory that is not a repository")
	}
}

// Branching from a pinned SHA rather than a symbolic ref is what keeps a mission's
// diff stable: origin/main can move under it at any time.
func TestWorktreeAddUsesPinnedStartPoint(t *testing.T) {
	git, fake := newTestGit()

	err := git.WorktreeAdd(t.Context(), "/repo/.git", "/missions/t/weave", "jarush/mission", "deadbeef")
	if err != nil {
		t.Fatalf("WorktreeAdd: %v", err)
	}

	want := gitBin + " -C /repo/.git worktree add -b jarush/mission /missions/t/weave deadbeef"
	if got := fake.Argv()[0]; got != want {
		t.Errorf("argv = %q, want %q", got, want)
	}
}

// git refuses to remove a worktree holding modified or untracked files, and that
// refusal is what stops one keystroke from discarding hours of agent work. Force
// must be opt-in and visible in the argv.
func TestWorktreeRemoveForceIsExplicit(t *testing.T) {
	git, fake := newTestGit()
	ctx := t.Context()

	if err := git.WorktreeRemove(ctx, "/repo/.git", "/wt", false); err != nil {
		t.Fatalf("WorktreeRemove: %v", err)
	}

	if got := fake.Argv()[0]; strings.Contains(got, "--force") {
		t.Errorf("argv = %q, must not force by default", got)
	}

	fake.Reset()

	if err := git.WorktreeRemove(ctx, "/repo/.git", "/wt", true); err != nil {
		t.Fatalf("WorktreeRemove: %v", err)
	}

	if got := fake.Argv()[0]; !strings.Contains(got, "--force") {
		t.Errorf("argv = %q, want --force", got)
	}
}

func TestDeleteBranchForceFlag(t *testing.T) {
	git, fake := newTestGit()
	ctx := t.Context()

	if err := git.DeleteBranch(ctx, "/repo/.git", "jarush/x", false); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}

	if got := fake.Argv()[0]; !strings.HasSuffix(got, "branch -d jarush/x") {
		t.Errorf("argv = %q, want -d", got)
	}

	fake.Reset()

	if err := git.DeleteBranch(ctx, "/repo/.git", "jarush/x", true); err != nil {
		t.Fatalf("DeleteBranch: %v", err)
	}

	if got := fake.Argv()[0]; !strings.HasSuffix(got, "branch -D jarush/x") {
		t.Errorf("argv = %q, want -D", got)
	}
}

func TestParseWorktreeList(t *testing.T) {
	out := strings.Join([]string{
		"worktree /repo",
		"HEAD abc123",
		"branch refs/heads/main",
		"",
		"worktree /missions/t1/weave",
		"HEAD def456",
		"branch refs/heads/jarush/mission",
		"",
		"worktree /missions/t2/weave",
		"HEAD 789abc",
		"detached",
		"",
	}, "\n")

	list := parseWorktreeList(out)
	if len(list) != 3 {
		t.Fatalf("len = %d, want 3: %+v", len(list), list)
	}

	if list[0].Branch != "main" || list[0].Path != "/repo" {
		t.Errorf("first entry = %+v", list[0])
	}

	if list[1].Branch != "jarush/mission" {
		t.Errorf("second branch = %q", list[1].Branch)
	}

	if !list[2].Detached || list[2].Branch != "" {
		t.Errorf("third entry should be detached: %+v", list[2])
	}
}

func TestParseWorktreeListEmpty(t *testing.T) {
	if got := parseWorktreeList(""); len(got) != 0 {
		t.Errorf("parseWorktreeList(\"\") = %+v, want empty", got)
	}
}

func TestTouchedSummary(t *testing.T) {
	for _, tc := range []struct {
		name      string
		status    string
		count     string
		wantDirty bool
		wantAhead int
		wantAny   bool
	}{
		{name: "clean and level", status: "", count: "0"},
		{name: "dirty only", status: " M main.go", count: "0", wantDirty: true, wantAny: true},
		{name: "committed only", status: "", count: "3", wantAhead: 3, wantAny: true},
		{name: "both", status: "?? new.go", count: "2", wantDirty: true, wantAhead: 2, wantAny: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			git, fake := newTestGit()
			fake.Expect(gitBin+" -C /wt status --porcelain=v1", tc.status)
			fake.Expect(gitBin+" -C /wt rev-list --count base..HEAD", tc.count)
			fake.Expect(gitBin+" -C /wt diff --shortstat base..HEAD", " 1 file changed")

			got, err := git.TouchedSummary(t.Context(), "/wt", "base")
			if err != nil {
				t.Fatalf("TouchedSummary: %v", err)
			}

			if got.Dirty != tc.wantDirty {
				t.Errorf("Dirty = %v, want %v", got.Dirty, tc.wantDirty)
			}

			if got.Ahead != tc.wantAhead {
				t.Errorf("Ahead = %d, want %d", got.Ahead, tc.wantAhead)
			}

			if got.Any() != tc.wantAny {
				t.Errorf("Any() = %v, want %v", got.Any(), tc.wantAny)
			}
		})
	}
}

// Without a branch point there is nothing to compare against, so only the dirty
// check is meaningful.
func TestTouchedSummaryWithoutBaseSHASkipsCommitCount(t *testing.T) {
	git, fake := newTestGit()
	fake.Expect(gitBin+" -C /wt status --porcelain=v1", " M main.go")

	got, err := git.TouchedSummary(t.Context(), "/wt", "")
	if err != nil {
		t.Fatalf("TouchedSummary: %v", err)
	}

	if !got.Dirty || got.Ahead != 0 {
		t.Errorf("got %+v", got)
	}

	for _, line := range fake.Argv() {
		if strings.Contains(line, "rev-list") {
			t.Errorf("should not count commits without a base SHA: %q", line)
		}
	}
}

func TestTouchedSummaryReportsUnparseableCount(t *testing.T) {
	git, fake := newTestGit()
	fake.Expect(gitBin+" -C /wt status --porcelain=v1", "")
	fake.Expect(gitBin+" -C /wt rev-list --count base..HEAD", "not a number")

	if _, err := git.TouchedSummary(t.Context(), "/wt", "base"); err == nil {
		t.Error("expected an error for an unparseable commit count")
	}
}

func TestRefAndBranchExistence(t *testing.T) {
	git, fake := newTestGit()
	fake.Expect(gitBin+" -C /repo show-ref --verify --quiet refs/heads/present", "")
	fake.ExpectExit(gitBin+" -C /repo show-ref --verify --quiet refs/heads/absent", 1, "")

	if !git.BranchExists(t.Context(), "/repo", "present") {
		t.Error("BranchExists should be true for an existing branch")
	}

	if git.BranchExists(t.Context(), "/repo", "absent") {
		t.Error("BranchExists should be false for a missing branch")
	}
}

// Two missions provisioning worktrees from one repo at the same time is the normal
// case, and git's index and ref locks do not tolerate it.
func TestLockSerializesPerRepository(t *testing.T) {
	git, _ := newTestGit()

	var (
		mu       sync.Mutex
		active   int
		maxSeen  int
		wg       sync.WaitGroup
		observed = func(n int) {
			mu.Lock()
			defer mu.Unlock()

			if n > maxSeen {
				maxSeen = n
			}
		}
	)

	for range 8 {
		wg.Go(func() {
			unlock := git.Lock("/repo/.git")
			defer unlock()

			mu.Lock()
			active++
			current := active
			mu.Unlock()

			observed(current)

			mu.Lock()
			active--
			mu.Unlock()
		})
	}

	wg.Wait()

	if maxSeen != 1 {
		t.Errorf("saw %d concurrent holders of one repo lock, want 1", maxSeen)
	}
}

// Different repositories must not block each other, or a multi-repo mission would
// provision serially for no reason.
func TestLockIsPerRepository(t *testing.T) {
	git, _ := newTestGit()

	releaseA := git.Lock("/a/.git")
	defer releaseA()

	done := make(chan struct{})

	go func() {
		release := git.Lock("/b/.git")
		release()
		close(done)
	}()

	<-done
}
