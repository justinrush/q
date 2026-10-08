// Combining two hosts' work on one mission.
//
// Most of the time only one of a pair of hosts changes a mission's worktree,
// and the other simply takes its state. When both have — a human edited the
// mirror while the agent kept going, or a laptop woke to find its mission had
// been taken over while its own agent slept — neither may be overwritten.
//
// The two states are merged the way git would merge two branches, except that
// nothing is checked out to do it. The result is computed as objects, and only
// laid out in a worktree once it is known to be clean.

package git

import (
	"context"
	"fmt"
	"strings"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// mergeTree merges two commits against a base without a working tree,
// returning the resulting tree and whether it merged cleanly.
//
// git exits 1 for a merge that has conflicts, which here is an answer and not
// a failure.
func (g *Client) mergeTree(ctx context.Context, dir, base, ours, theirs string) (string, bool, error) {
	res, err := g.exec(ctx, dir, "merge-tree", "--write-tree", "--merge-base="+base, ours, theirs)
	if err != nil {
		if runner.IsExit(err) && res.ExitCode == 1 {
			return "", false, nil
		}

		return "", false, fmt.Errorf("merging %s and %s in %s: %w", ours, theirs, dir, err)
	}

	tree, _, _ := strings.Cut(res.Out(), "\n")

	return tree, true, nil
}

// isAncestor reports whether commit a is b or one of its ancestors.
func (g *Client) isAncestor(ctx context.Context, dir, a, b string) bool {
	_, err := g.exec(ctx, dir, "merge-base", "--is-ancestor", a, b)

	return err == nil
}

// mergeMessage is the message on the commit that joins two hosts' commits.
const mergeMessage = "q: merge work from the paired machine"

// commitMerge creates a commit with the given tree and parents, dated from its
// first parent so that it, like a snapshot, has no time of its own.
func (g *Client) commitMerge(ctx context.Context, dir, tree string, parents ...string) (string, error) {
	stamp, err := g.execEnv(ctx, dir, nil, "log", "-1", "--format=%ct", parents[0])
	if err != nil {
		return "", fmt.Errorf("reading %s in %s: %w", parents[0], dir, err)
	}

	date := stamp + " +0000"
	env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}

	args := append(append([]string{}, snapshotIdentity...), "commit-tree", tree)
	for _, parent := range parents {
		args = append(args, "-p", parent)
	}

	args = append(args, "-m", mergeMessage)

	commit, err := g.execEnv(ctx, dir, env, args...)
	if err != nil {
		return "", fmt.Errorf("committing a merge in %s: %w", dir, err)
	}

	return commit, nil
}

// mergeHeads returns the branch tip that carries both hosts' commits.
//
// Usually one tip already contains the other, because only one host committed,
// and that tip is the answer. When both committed, the two are joined by a
// merge commit, which is the one case where q adds a commit of its own to a
// mission's branch.
func (g *Client) mergeHeads(ctx context.Context, dir, ours, theirs string) (string, bool, error) {
	switch {
	case ours == theirs || g.isAncestor(ctx, dir, theirs, ours):
		return ours, true, nil
	case g.isAncestor(ctx, dir, ours, theirs):
		return theirs, true, nil
	}

	base, err := g.execEnv(ctx, dir, nil, "merge-base", ours, theirs)
	if err != nil {
		return "", false, fmt.Errorf("finding where %s and %s diverged in %s: %w", ours, theirs, dir, err)
	}

	tree, clean, err := g.mergeTree(ctx, dir, base, ours, theirs)
	if err != nil || !clean {
		return "", false, err
	}

	head, err := g.commitMerge(ctx, dir, tree, ours, theirs)

	return head, err == nil, err
}

// MergeSnapshots combines two snapshots of the same mission worktree.
//
// bases are the candidates for what the two had in common, most likely first.
// There can be more than one because each host only knows the last state it
// saw agreed, and they can have seen different ones. Each is tried in turn and
// the first to merge cleanly is used; an older base is still a correct one, it
// only makes a conflict more likely.
//
// The result is a snapshot like any other: a branch tip carrying both hosts'
// commits, and on top of it a tree holding both hosts' uncommitted work, still
// uncommitted. It reports false, with no snapshot, when the two cannot be
// combined without a person choosing.
func (g *Client) MergeSnapshots(
	ctx context.Context,
	dir string,
	ours, theirs mission.Snap,
	bases []string,
) (mission.Snap, bool, error) {
	head, clean, err := g.mergeHeads(ctx, dir, ours.Head, theirs.Head)
	if err != nil || !clean {
		return mission.Snap{}, false, err
	}

	for _, base := range bases {
		if base == "" || !g.HasCommit(ctx, dir, base) {
			continue
		}

		tree, clean, err := g.mergeTree(ctx, dir, base, ours.Commit, theirs.Commit)
		if err != nil {
			return mission.Snap{}, false, err
		}

		if !clean {
			continue
		}

		commit, err := g.snapshotCommit(ctx, dir, head, tree)
		if err != nil {
			return mission.Snap{}, false, err
		}

		return mission.Snap{Head: head, Commit: commit, Tree: tree}, true, nil
	}

	return mission.Snap{}, false, nil
}

// PreserveBranch points a local branch at a commit, creating it if needed.
//
// It is how one host's side of a conflict is kept. The other side stays on the
// mission's branch; this one goes beside it under a name that says where it
// came from, so nothing either agent did is lost and a person can combine them.
func (g *Client) PreserveBranch(ctx context.Context, dir, branch, commit string) error {
	if _, err := g.exec(ctx, dir, "branch", "--force", "--no-track", branch, commit); err != nil {
		return fmt.Errorf("keeping %s as %s in %s: %w", commit, branch, dir, err)
	}

	return nil
}
