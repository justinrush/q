package git

import (
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

// twoSides returns two worktrees of one repository at the same commit, standing
// in for the two hosts' copies of a mission, and the snapshot they agree on.
func twoSides(t *testing.T) (s *sandbox, ours, theirs string, base mission.Snap) {
	t.Helper()

	s = newSandbox(t)
	ours, theirs = s.worktree("ours"), s.worktree("theirs")

	return s, ours, theirs, s.snap(ours, hostA)
}

func (s *sandbox) commit(dir, message string) {
	s.t.Helper()
	s.run(dir, "add", "-A")
	s.run(dir, "commit", "-q", "-m", message)
}

// merge combines the two worktrees' current states and lays the result out in
// ours, failing the test unless it merged cleanly.
func (s *sandbox) merge(ours, theirs string, bases ...string) mission.Snap {
	s.t.Helper()

	mine, other := s.snap(ours, hostA), s.snap(theirs, hostB)

	merged, clean, err := s.git.MergeSnapshots(s.t.Context(), ours, mine, other, bases)
	if err != nil || !clean {
		s.t.Fatalf("MergeSnapshots: clean=%v err=%v", clean, err)
	}

	if err := s.git.ApplySnapshot(s.t.Context(), ours, mine, merged); err != nil {
		s.t.Fatalf("ApplySnapshot: %v", err)
	}

	return merged
}

// The common case: a human touched one file in the mirror while the agent
// touched another. Both edits survive, and both are still uncommitted, because
// neither of them committed anything.
func TestMergeCombinesUncommittedWorkAndLeavesItUncommitted(t *testing.T) {
	s, ours, theirs, base := twoSides(t)

	s.write(ours, "a.txt", "alpha, by the human\n")
	s.write(theirs, "b.txt", "bravo, by the agent\n")
	s.write(theirs, "new.txt", "added by the agent\n")

	merged := s.merge(ours, theirs, base.Commit)

	if merged.Head != base.Head {
		t.Errorf("head moved to %s, want it left at %s: nobody committed", merged.Head, base.Head)
	}

	if got := s.read(ours, "a.txt") + s.read(ours, "b.txt") + s.read(ours, "new.txt"); got != "alpha, by the human\nbravo, by the agent\nadded by the agent\n" {
		t.Errorf("merged worktree = %q", got)
	}

	status := s.run(ours, "status", "--porcelain=v1")
	// The helper trims its output, which takes the leading space off the first
	// line, so the first entry is matched without it.
	for _, want := range []string{"M a.txt", " M b.txt", "?? new.txt"} {
		if !strings.Contains(status, want) {
			t.Errorf("status lacks %q; the work should still be uncommitted:\n%s", want, status)
		}
	}

	if strings.Contains(status, "A ") || strings.HasPrefix(status, "M  ") {
		t.Errorf("something was staged, so it is no longer simply uncommitted:\n%s", status)
	}
}

// One side committed and the other did not. The branch takes the commit, and
// the other side's work sits on top of it, uncommitted.
func TestMergeKeepsOneSidesCommitAndTheOthersUncommittedWork(t *testing.T) {
	s, ours, theirs, base := twoSides(t)

	s.write(ours, "a.txt", "alpha, uncommitted here\n")
	s.write(theirs, "b.txt", "bravo, committed there\n")
	s.commit(theirs, "agent commit")

	theirHead := s.run(theirs, "rev-parse", "HEAD")
	merged := s.merge(ours, theirs, base.Commit)

	if merged.Head != theirHead {
		t.Errorf("head = %s, want the committed side's tip %s with no merge commit", merged.Head, theirHead)
	}

	if got := s.run(ours, "rev-parse", "HEAD"); got != theirHead {
		t.Errorf("worktree HEAD = %s, want %s", got, theirHead)
	}

	if status := s.run(ours, "status", "--porcelain=v1"); status != "M a.txt" && status != " M a.txt" {
		t.Errorf("status = %q, want only this side's edit uncommitted", status)
	}

	if got := s.read(ours, "b.txt"); got != "bravo, committed there\n" {
		t.Errorf("the other side's committed file = %q", got)
	}
}

// Both sides committed: the one case where q adds a commit to the branch.
func TestMergeJoinsTwoCommittedLinesWithAMergeCommit(t *testing.T) {
	s, ours, theirs, base := twoSides(t)

	s.write(ours, "a.txt", "alpha, committed here\n")
	s.commit(ours, "laptop agent commit")
	s.write(theirs, "b.txt", "bravo, committed there\n")
	s.commit(theirs, "mini agent commit")

	ourHead, theirHead := s.run(ours, "rev-parse", "HEAD"), s.run(theirs, "rev-parse", "HEAD")

	merged := s.merge(ours, theirs, base.Commit)

	if parents := s.run(ours, "log", "-1", "--format=%P", merged.Head); parents != ourHead+" "+theirHead {
		t.Errorf("merge parents = %q, want both hosts' tips", parents)
	}

	if merged.Commit != merged.Head {
		t.Errorf("snapshot %s differs from its head %s, but nothing was uncommitted", merged.Commit, merged.Head)
	}

	if status := s.run(ours, "status", "--porcelain=v1"); status != "" {
		t.Errorf("status = %q, want a clean tree", status)
	}

	if log := s.run(ours, "log", "--format=%s"); !strings.Contains(log, "laptop agent commit") || !strings.Contains(log, "mini agent commit") {
		t.Errorf("history lost a side:\n%s", log)
	}

	if author := s.run(ours, "log", "-1", "--format=%an"); author != "q" {
		t.Errorf("merge commit author = %q, want q", author)
	}
}

// Two different edits to one line. q must not pick, and must not touch
// anything while saying so.
func TestMergeReportsAConflictAndChangesNothing(t *testing.T) {
	s, ours, theirs, base := twoSides(t)

	s.write(ours, "a.txt", "alpha, one way\n")
	s.write(theirs, "a.txt", "alpha, another way\n")

	mine, other := s.snap(ours, hostA), s.snap(theirs, hostB)

	merged, clean, err := s.git.MergeSnapshots(t.Context(), ours, mine, other, []string{base.Commit})
	if err != nil {
		t.Fatalf("MergeSnapshots: %v", err)
	}

	if clean || !merged.Empty() {
		t.Fatalf("clean=%v merged=%+v, want a conflict and no result", clean, merged)
	}

	if got := s.read(ours, "a.txt"); got != "alpha, one way\n" {
		t.Errorf("our file was touched: %q", got)
	}
}

// Conflicting commits are a conflict too, even when the uncommitted work on
// top would have merged.
func TestMergeReportsConflictingCommits(t *testing.T) {
	s, ours, theirs, base := twoSides(t)

	s.write(ours, "a.txt", "alpha, committed one way\n")
	s.commit(ours, "here")
	s.write(theirs, "a.txt", "alpha, committed another way\n")
	s.commit(theirs, "there")

	_, clean, err := s.git.MergeSnapshots(t.Context(), ours, s.snap(ours, hostA), s.snap(theirs, hostB), []string{base.Commit})
	if err != nil || clean {
		t.Errorf("clean=%v err=%v, want a conflict", clean, err)
	}
}

// Each host knows only the last state it saw agreed, and one of them may be out
// of date. A later base that makes the merge clean is found even when an
// earlier one is offered first, and a base that never arrived is skipped.
func TestMergeTriesEachBaseUntilOneIsClean(t *testing.T) {
	s, ours, theirs, old := twoSides(t)

	// Both sides took the same change, then each went its own way from there.
	s.write(ours, "a.txt", "alpha, shared change\n")
	s.write(theirs, "a.txt", "alpha, shared change\n")
	recent := s.snap(ours, hostA)

	s.write(ours, "a.txt", "alpha, shared change, then more here\n")
	s.write(theirs, "b.txt", "bravo, changed there\n")

	mine, other := s.snap(ours, hostA), s.snap(theirs, hostB)

	// Against the old base both sides changed a.txt differently: a conflict.
	if _, clean, _ := s.git.MergeSnapshots(t.Context(), ours, mine, other, []string{old.Commit}); clean {
		t.Fatal("the old base was expected to conflict, or this test proves nothing")
	}

	missing := strings.Repeat("0", 40)

	merged, clean, err := s.git.MergeSnapshots(t.Context(), ours, mine, other, []string{missing, old.Commit, recent.Commit})
	if err != nil || !clean {
		t.Fatalf("clean=%v err=%v, want the recent base to merge", clean, err)
	}

	if got := s.run(ours, "show", merged.Commit+":a.txt"); got != "alpha, shared change, then more here" {
		t.Errorf("merged a.txt = %q", got)
	}
}

// With no base at all there is nothing to merge against, and saying so is
// better than guessing.
func TestMergeWithNoUsableBaseIsNotClean(t *testing.T) {
	s, ours, theirs, _ := twoSides(t)
	s.write(ours, "a.txt", "one\n")
	s.write(theirs, "b.txt", "two\n")

	if _, clean, err := s.git.MergeSnapshots(t.Context(), ours, s.snap(ours, hostA), s.snap(theirs, hostB), nil); clean || err != nil {
		t.Errorf("clean=%v err=%v, want not clean and no error", clean, err)
	}
}

func TestPreserveBranchKeepsTheOtherSide(t *testing.T) {
	s, ours, theirs, _ := twoSides(t)
	s.write(theirs, "b.txt", "their work\n")

	other := s.snap(theirs, hostB)

	if err := s.git.PreserveBranch(t.Context(), ours, "me/ours--mini", other.Commit); err != nil {
		t.Fatalf("PreserveBranch: %v", err)
	}

	if got := s.run(s.dir, "show", "me/ours--mini:b.txt"); got != "their work" {
		t.Errorf("preserved branch holds %q", got)
	}

	// Kept again after the other side moved on, the branch follows it.
	s.write(theirs, "b.txt", "their later work\n")

	if err := s.git.PreserveBranch(t.Context(), ours, "me/ours--mini", s.snap(theirs, hostB).Commit); err != nil {
		t.Fatalf("PreserveBranch again: %v", err)
	}

	if got := s.run(s.dir, "show", "me/ours--mini:b.txt"); got != "their later work" {
		t.Errorf("preserved branch holds %q after an update", got)
	}
}
