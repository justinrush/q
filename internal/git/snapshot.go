// Carrying a worktree's state between two machines.
//
// A mission's worktree exists on both of a pair of q hosts. The agent runs on
// one of them, and what it has done — commits, and the uncommitted work that is
// most of what matters mid-mission — has to reach the other without the agent
// being asked to push anything.
//
// A snapshot is how. It is an ordinary commit whose tree is the worktree
// exactly as it stands, built without touching the worktree, the index, or the
// branch, and kept under a ref no branch listing shows. Git's own transport
// then moves it, and the other host lays it back out as the same branch tip
// with the same changes still uncommitted.

package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// refNamespace is where q keeps the refs it exchanges with a paired host.
//
// It is outside refs/heads and refs/tags on purpose. Nothing under it is a
// branch, so it does not appear in a branch listing, is not pushed by a bare
// `git push`, and triggers no pipeline if it ever did reach a forge.
const refNamespace = "refs/q/"

// SnapshotRef is where a host publishes its snapshot of a mission's worktree.
func SnapshotRef(host mission.HostID, id mission.MissionID) string {
	return refNamespace + string(host) + "/" + string(id) + "/snap"
}

// BaseRef is where a host keeps the last snapshot it and its peer agreed on.
//
// The ref exists only to keep that commit alive. It is the merge base if the
// two worktrees ever diverge, and a synthetic commit nothing points at is
// exactly what git's garbage collection removes.
func BaseRef(host mission.HostID, id mission.MissionID) string {
	return refNamespace + string(host) + "/" + string(id) + "/base"
}

// hostRefspec matches every ref one host publishes.
func hostRefspec(host mission.HostID) string {
	pattern := refNamespace + string(host) + "/*"

	return "+" + pattern + ":" + pattern
}

// snapshotIdentity is the author recorded on snapshot commits. A snapshot is
// q's bookkeeping rather than anyone's work, and using the user's identity
// would sign their name to commits they never made.
var snapshotIdentity = []string{"-c", "user.name=q", "-c", "user.email=q@localhost"}

// execEnv runs git in dir, with env added to the environment when given, and
// returns its trimmed output.
func (g *Client) execEnv(ctx context.Context, dir string, env []string, args ...string) (string, error) {
	run := g.exec
	if len(env) > 0 {
		run = func(ctx context.Context, dir string, args ...string) (runner.Result, error) {
			return g.execWith(ctx, dir, env, args...)
		}
	}

	res, err := run(ctx, dir, args...)
	if err != nil {
		return "", err
	}

	return res.Out(), nil
}

// Snapshot records a worktree's current state as a commit and points ref at it.
//
// The commit's tree is everything git would track: modified files, new files,
// and deletions, with ignored files left out. Its parent is the branch tip, so
// the committed and uncommitted parts stay distinguishable on the other side. A
// clean worktree's snapshot is simply its tip, and no commit is made.
//
// The commit is a pure function of the tree and the tip: its author, message,
// and dates are all fixed. So the same state snapshotted twice, or on two
// machines, is the same commit id. That is what makes "the two hosts agree" a
// comparison of two ids, and what keeps an idle worktree from minting a new
// commit on every exchange.
//
// The work is done against a private copy of the index. The real index is
// never written, so a `git add` the agent has half finished is undisturbed,
// and the copy starts from the real one so that only changed files are hashed.
func (g *Client) Snapshot(ctx context.Context, worktree, ref string) (mission.Snap, error) {
	head, err := g.RevParse(ctx, worktree, "HEAD")
	if err != nil {
		return mission.Snap{}, err
	}

	tree, err := g.worktreeTree(ctx, worktree)
	if err != nil {
		return mission.Snap{}, err
	}

	commit, err := g.snapshotCommit(ctx, worktree, head, tree)
	if err != nil {
		return mission.Snap{}, err
	}

	if err := g.updateRef(ctx, worktree, ref, commit); err != nil {
		return mission.Snap{}, err
	}

	return mission.Snap{Head: head, Commit: commit, Tree: tree}, nil
}

// worktreeTree writes a tree object for the worktree as it stands.
func (g *Client) worktreeTree(ctx context.Context, worktree string) (string, error) {
	index, cleanup, err := g.scratchIndex(ctx, worktree)
	if err != nil {
		return "", err
	}
	defer cleanup()

	env := []string{"GIT_INDEX_FILE=" + index}

	if _, err := g.execEnv(ctx, worktree, env, "add", "--all"); err != nil {
		return "", fmt.Errorf("staging a snapshot of %s: %w", worktree, err)
	}

	tree, err := g.execEnv(ctx, worktree, env, "write-tree")
	if err != nil {
		return "", fmt.Errorf("writing a snapshot tree for %s: %w", worktree, err)
	}

	return tree, nil
}

// scratchIndex copies the worktree's index somewhere private.
func (g *Client) scratchIndex(ctx context.Context, worktree string) (string, func(), error) {
	real, err := g.execEnv(ctx, worktree, nil, "rev-parse", "--git-path", "index")
	if err != nil {
		return "", nil, fmt.Errorf("locating the index of %s: %w", worktree, err)
	}

	// --git-path answers relative to the directory git ran in when the git
	// directory is below it, which is the case for a repository's main worktree.
	if !filepath.IsAbs(real) {
		real = filepath.Join(worktree, real)
	}

	scratch, err := os.CreateTemp("", "q-snapshot-index-*")
	if err != nil {
		return "", nil, fmt.Errorf("creating a scratch index: %w", err)
	}

	path := scratch.Name()
	cleanup := func() { _ = os.Remove(path) }

	data, err := os.ReadFile(real)

	switch {
	case errors.Is(err, os.ErrNotExist):
		// No index yet. git treats a missing file as an empty index, but an
		// empty file as a corrupt one, so the placeholder has to go.
		_ = scratch.Close()
		cleanup()

		return path, cleanup, nil
	case err != nil:
		_ = scratch.Close()
		cleanup()

		return "", nil, fmt.Errorf("reading %s: %w", real, err)
	}

	_, writeErr := scratch.Write(data)
	closeErr := scratch.Close()

	if err := errors.Join(writeErr, closeErr); err != nil {
		cleanup()

		return "", nil, fmt.Errorf("copying the index of %s: %w", worktree, err)
	}

	return path, cleanup, nil
}

// snapshotMessage is the message on every snapshot commit. It is constant
// because it is part of what makes the commit id reproducible.
const snapshotMessage = "q: snapshot of uncommitted work"

// snapshotCommit returns the commit that describes tree on top of head.
func (g *Client) snapshotCommit(ctx context.Context, worktree, head, tree string) (string, error) {
	out, err := g.execEnv(ctx, worktree, nil, "log", "-1", "--format=%T %ct", head)
	if err != nil {
		return "", fmt.Errorf("reading the tip of %s: %w", worktree, err)
	}

	headTree, stamp, ok := strings.Cut(out, " ")
	if !ok {
		return "", fmt.Errorf("reading the tip of %s: unexpected output %q", worktree, out)
	}

	if tree == headTree {
		return head, nil
	}

	// The tip's own date, rather than now. A snapshot has no meaningful time of
	// its own, and borrowing one that both hosts already agree on is what lets
	// them arrive at the same commit independently.
	date := stamp + " +0000"
	env := []string{"GIT_AUTHOR_DATE=" + date, "GIT_COMMITTER_DATE=" + date}

	args := append(append([]string{}, snapshotIdentity...), "commit-tree", tree, "-p", head, "-m", snapshotMessage)

	commit, err := g.execEnv(ctx, worktree, env, args...)
	if err != nil {
		return "", fmt.Errorf("committing a snapshot of %s: %w", worktree, err)
	}

	return commit, nil
}

// updateRef points ref at commit.
func (g *Client) updateRef(ctx context.Context, dir, ref, commit string) error {
	if _, err := g.execEnv(ctx, dir, nil, "update-ref", ref, commit); err != nil {
		return fmt.Errorf("recording %s in %s: %w", ref, dir, err)
	}

	return nil
}

// KeepBase points a host's base ref for a mission at commit, so the commit
// outlives garbage collection for as long as it may be needed as a merge base.
func (g *Client) KeepBase(ctx context.Context, dir string, host mission.HostID, id mission.MissionID, commit string) error {
	return g.updateRef(ctx, dir, BaseRef(host, id), commit)
}

// HasCommit reports whether a commit is present in the repository.
func (g *Client) HasCommit(ctx context.Context, dir, commit string) bool {
	if commit == "" {
		return false
	}

	_, err := g.exec(ctx, dir, "cat-file", "-e", commit+"^{commit}")

	return err == nil
}

// ApplySnapshot lays a snapshot out in a worktree: the branch at its head, the
// files as its tree, and the difference between the two left uncommitted.
//
// current is the snapshot the worktree holds now, and it matters. A file that
// was new and uncommitted in current is untracked on disk, and git's reset
// leaves untracked files alone, so if the incoming snapshot no longer has it
// the file would simply stay. The worktree would then differ from the snapshot
// it was just given, which the next exchange would read as an edit made here.
// Loading current's tree into the index first makes every such file tracked,
// so the reset that follows removes exactly the ones that are gone.
//
// The caller must have checked that the worktree still matches current. This
// overwrites it.
func (g *Client) ApplySnapshot(ctx context.Context, worktree string, current, target mission.Snap) error {
	if current.Commit != "" {
		if _, err := g.exec(ctx, worktree, "read-tree", current.Commit); err != nil {
			return fmt.Errorf("preparing %s for a snapshot: %w", worktree, err)
		}
	}

	if _, err := g.exec(ctx, worktree, "reset", "--quiet", "--hard", target.Commit); err != nil {
		return fmt.Errorf("applying a snapshot to %s: %w", worktree, err)
	}

	if target.Commit == target.Head {
		return nil
	}

	// The snapshot commit itself must not end up on the branch. Moving the
	// branch back to the real tip, and the index with it, leaves the files where
	// they are, which is what makes the snapshot's extra changes uncommitted.
	if _, err := g.exec(ctx, worktree, "reset", "--quiet", target.Head); err != nil {
		return fmt.Errorf("restoring the branch tip in %s: %w", worktree, err)
	}

	return nil
}

// extAllowed lets git run the command a paired host is reached by. git refuses
// the ext transport unless told otherwise, because a URL from an untrusted
// source could name any program. This URL is built by q from the user's own
// configuration, and the permission is given for the one invocation.
var extAllowed = []string{"-c", "protocol.ext.allow=always"}

// FetchHostRefs brings a host's published refs for this repository from url.
//
// Like [Client.FetchBranch] it names its remote and its refspec, so the user's
// fetch.all cannot widen it. --prune removes refs for missions the host has
// finished with, which is how the namespace is kept from growing forever.
func (g *Client) FetchHostRefs(ctx context.Context, dir, url string, host mission.HostID) error {
	args := append(append([]string{}, extAllowed...),
		"fetch", "--no-tags", "--prune", "--no-write-fetch-head", url, hostRefspec(host))

	if _, err := g.exec(ctx, dir, args...); err != nil {
		return fmt.Errorf("fetching %s's snapshots into %s: %w", host, dir, err)
	}

	return nil
}

// PushHostRefs sends a host's published refs for this repository to url.
//
// --no-verify skips the repository's pre-push hook. The hook is there to vet
// work headed for a shared remote; this is a private copy between two of one
// person's machines, sent many times a minute.
func (g *Client) PushHostRefs(ctx context.Context, dir, url string, host mission.HostID) error {
	args := append(append([]string{}, extAllowed...),
		"push", "--quiet", "--no-verify", "--prune", url, hostRefspec(host))

	if _, err := g.exec(ctx, dir, args...); err != nil {
		return fmt.Errorf("sending %s's snapshots from %s: %w", host, dir, err)
	}

	return nil
}

// ForgetMission removes every ref q keeps for a mission in this repository,
// both hosts' alike.
func (g *Client) ForgetMission(ctx context.Context, dir string, id mission.MissionID) error {
	res, err := g.exec(ctx, dir, "for-each-ref", "--format=%(refname)", refNamespace)
	if err != nil {
		return fmt.Errorf("listing q's refs in %s: %w", dir, err)
	}

	marker := "/" + string(id) + "/"

	for _, ref := range res.Lines() {
		if !strings.Contains(ref, marker) {
			continue
		}

		if _, err := g.exec(ctx, dir, "update-ref", "-d", ref); err != nil {
			return fmt.Errorf("removing %s from %s: %w", ref, dir, err)
		}
	}

	return nil
}
