// Package git is every git operation q performs, from the primitives up to
// provisioning and reclaiming a mission's worktrees.
//
// [Client] is the low-level driver; [Provisioner] sequences it into the
// worktree-per-repo layout a mission works in; [Scan] finds the checkouts an
// operation can span.
//
// Two invariants are enforced by construction rather than by convention, because
// both have consequences that are easy to miss and hard to debug:
//
// There is no general Fetch. Only [Git.FetchBranch] exists, and it always names a
// single remote and a single explicit refspec. The user's global git config sets
// fetch.all, fetch.force, and fetch.prune, so a bare `git fetch` would contact
// every remote and force-move every tracking ref.
//
// Operations on one repository are serialized on its common directory. Concurrent
// worktree and fetch operations contend on index.lock and ref locks, and two
// missions provisioning worktrees from the same repo at the same time is the normal
// case, not the exception.
package git

import (
	"context"
	"fmt"
	"strconv"
	"strings"
	"sync"

	"github.com/justinrush/q/internal/runner"
)

// Client runs git commands.
type Client struct {
	bin string
	run runner.Runner

	// lockMu guards the map; each entry serializes work on one repository.
	lockMu sync.Mutex
	locks  map[string]*sync.Mutex
}

// New returns a Git that invokes the binary at bin through run.
func New(bin string, run runner.Runner) *Client {
	return &Client{bin: bin, run: run, locks: map[string]*sync.Mutex{}}
}

// Lock serializes operations on one repository, returning the release function.
//
// Callers that perform a multi-step sequence against a single repo (fetch, then
// resolve, then add a worktree) should hold this across the whole sequence.
func (g *Client) Lock(commonDir string) func() {
	g.lockMu.Lock()

	mu, ok := g.locks[commonDir]
	if !ok {
		mu = &sync.Mutex{}
		g.locks[commonDir] = mu
	}

	g.lockMu.Unlock()

	mu.Lock()

	return mu.Unlock
}

// exec runs git with the given arguments in dir.
func (g *Client) exec(ctx context.Context, dir string, args ...string) (runner.Result, error) {
	spec := runner.Spec{Name: g.bin, Args: args}
	if dir != "" {
		spec.Args = append([]string{"-C", dir}, args...)
	}

	return g.run.Run(ctx, spec)
}

// CommonDir resolves the main .git directory for a path.
//
// The path may itself be a worktree, in which case its own .git is a file
// pointing elsewhere and ref operations must target the common directory.
func (g *Client) CommonDir(ctx context.Context, repoPath string) (string, error) {
	res, err := g.exec(ctx, repoPath, "rev-parse", "--git-common-dir")
	if err != nil {
		return "", fmt.Errorf("resolving the git directory for %s: %w", repoPath, err)
	}

	out := res.Out()
	if out == "" {
		return "", fmt.Errorf("%s does not look like a git repository", repoPath)
	}

	// --git-common-dir answers relatively when run from the repo root.
	if !strings.HasPrefix(out, "/") {
		return repoPath + "/" + out, nil
	}

	return out, nil
}

// DefaultBranch resolves the repository's default branch.
//
// It is not always "main": one of the user's own repos is on master, so this
// asks rather than assumes. The fallbacks matter because origin/HEAD is only set
// if the repo was cloned normally, and a worktree-heavy repo may be missing it.
func (g *Client) DefaultBranch(ctx context.Context, dir string) (string, error) {
	if res, err := g.exec(ctx, dir, "symbolic-ref", "--short", "refs/remotes/origin/HEAD"); err == nil {
		if branch := strings.TrimPrefix(res.Out(), "origin/"); branch != "" {
			return branch, nil
		}
	}

	if res, err := g.exec(ctx, dir, "ls-remote", "--symref", "origin", "HEAD"); err == nil {
		if branch := parseSymrefHead(res.Out()); branch != "" {
			return branch, nil
		}
	}

	if res, err := g.exec(ctx, dir, "config", "--get", "init.defaultBranch"); err == nil {
		if branch := res.Out(); branch != "" {
			return branch, nil
		}
	}

	for _, candidate := range []string{"main", "master"} {
		if g.RefExists(ctx, dir, "refs/remotes/origin/"+candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("could not determine the default branch for %s", dir)
}

// parseSymrefHead extracts the branch from `ls-remote --symref` output.
func parseSymrefHead(out string) string {
	for line := range strings.SplitSeq(out, "\n") {
		rest, ok := strings.CutPrefix(line, "ref:")
		if !ok {
			continue
		}

		fields := strings.Fields(rest)
		if len(fields) == 0 {
			continue
		}

		return strings.TrimPrefix(fields[0], "refs/heads/")
	}

	return ""
}

// LocalBranches lists the branches origin is known to have, from the
// remote-tracking refs already on disk.
//
// It reads only local refs, so it answers instantly and works offline. The
// answer is as fresh as the last fetch, which is why [Client.RemoteBranches]
// exists to catch up with anything pushed since.
func (g *Client) LocalBranches(ctx context.Context, dir string) ([]string, error) {
	res, err := g.exec(ctx, dir, "for-each-ref", "--format=%(refname)", "refs/remotes/origin/")
	if err != nil {
		return nil, fmt.Errorf("listing branches in %s: %w", dir, err)
	}

	return parseTrackingRefs(res.Out()), nil
}

// parseTrackingRefs turns `for-each-ref --format=%(refname)` output over
// refs/remotes/origin into plain branch names.
//
// The full refname is asked for rather than the short one because %(refname:short)
// renders refs/remotes/origin/HEAD as bare "origin", which is indistinguishable
// from a branch and cannot be filtered out by name.
//
// HEAD is dropped: it is a symbolic alias for another entry in the same list, so
// offering it would put one branch in the picker twice under two names.
func parseTrackingRefs(out string) []string {
	var branches []string

	for line := range strings.SplitSeq(out, "\n") {
		name, ok := strings.CutPrefix(strings.TrimSpace(line), "refs/remotes/origin/")
		if !ok || name == "" || name == "HEAD" {
			continue
		}

		branches = append(branches, name)
	}

	return branches
}

// RemoteBranches asks origin what branches it has.
//
// This is a network round trip, so it belongs behind an explicit refresh rather
// than on the path of opening a picker. It writes nothing: unlike a fetch it
// moves no refs, which is what keeps it clear of the user's fetch.all and
// fetch.force settings.
func (g *Client) RemoteBranches(ctx context.Context, dir string) ([]string, error) {
	res, err := g.exec(ctx, dir, "ls-remote", "--heads", "origin")
	if err != nil {
		return nil, fmt.Errorf("listing branches on origin for %s: %w", dir, err)
	}

	return parseLsRemoteHeads(res.Out()), nil
}

// parseLsRemoteHeads extracts branch names from `ls-remote --heads` output,
// whose lines are "<sha>\trefs/heads/<name>".
func parseLsRemoteHeads(out string) []string {
	var branches []string

	for line := range strings.SplitSeq(out, "\n") {
		fields := strings.Fields(line)
		if len(fields) < 2 {
			continue
		}

		name, ok := strings.CutPrefix(fields[1], "refs/heads/")
		if !ok || name == "" {
			continue
		}

		branches = append(branches, name)
	}

	return branches
}

// FetchBranch updates one remote-tracking ref from origin.
//
// The refspec is explicit and the remote is named, so this cannot be widened by
// the user's fetch.all or fetch.force settings. --no-tags keeps tag churn out of
// an operation that only needs one branch.
func (g *Client) FetchBranch(ctx context.Context, dir, branch string) error {
	refspec := fmt.Sprintf("+refs/heads/%s:refs/remotes/origin/%s", branch, branch)

	if _, err := g.exec(ctx, dir, "fetch", "--no-tags", "origin", refspec); err != nil {
		return fmt.Errorf("fetching origin/%s in %s: %w", branch, dir, err)
	}

	return nil
}

// RevParse resolves a revision to a full SHA.
func (g *Client) RevParse(ctx context.Context, dir, rev string) (string, error) {
	res, err := g.exec(ctx, dir, "rev-parse", rev)
	if err != nil {
		return "", fmt.Errorf("resolving %s in %s: %w", rev, dir, err)
	}

	return res.Out(), nil
}

// RefExists reports whether a ref is present.
func (g *Client) RefExists(ctx context.Context, dir, ref string) bool {
	_, err := g.exec(ctx, dir, "show-ref", "--verify", "--quiet", ref)

	return err == nil
}

// BranchExists reports whether a local branch is present.
func (g *Client) BranchExists(ctx context.Context, dir, branch string) bool {
	return g.RefExists(ctx, dir, "refs/heads/"+branch)
}

// WorktreeAdd creates a worktree at path on a new branch cut from startPoint.
//
// startPoint should be a pinned SHA rather than a symbolic ref, so that every
// repo in one mission starts from a coherent moment and later diffs cannot drift
// when someone else's fetch moves origin/main.
func (g *Client) WorktreeAdd(ctx context.Context, dir, path, branch, startPoint string) error {
	if _, err := g.exec(ctx, dir, "worktree", "add", "-b", branch, path, startPoint); err != nil {
		return fmt.Errorf("creating worktree %s on %s: %w", path, branch, err)
	}

	return nil
}

// WorktreeAddExisting creates a worktree at path checked out to an existing
// branch, which is how a previously created branch is reattached.
func (g *Client) WorktreeAddExisting(ctx context.Context, dir, path, branch string) error {
	if _, err := g.exec(ctx, dir, "worktree", "add", path, branch); err != nil {
		return fmt.Errorf("attaching worktree %s to %s: %w", path, branch, err)
	}

	return nil
}

// WorktreeRemove deletes a worktree.
//
// Without force, git refuses when the worktree holds modified or untracked
// files. That refusal is load-bearing: it is what stops a single keystroke from
// discarding hours of uncommitted agent work, so callers must surface it rather
// than reaching for force.
func (g *Client) WorktreeRemove(ctx context.Context, dir, path string, force bool) error {
	args := []string{"worktree", "remove"}
	if force {
		args = append(args, "--force")
	}

	args = append(args, path)

	if _, err := g.exec(ctx, dir, args...); err != nil {
		return fmt.Errorf("removing worktree %s: %w", path, err)
	}

	return nil
}

// WorktreePrune clears administrative entries for worktrees whose directories
// are gone.
func (g *Client) WorktreePrune(ctx context.Context, dir string) error {
	if _, err := g.exec(ctx, dir, "worktree", "prune"); err != nil {
		return fmt.Errorf("pruning worktrees in %s: %w", dir, err)
	}

	return nil
}

// DeleteBranch removes a local branch. It refuses to delete unmerged work unless
// force is set.
func (g *Client) DeleteBranch(ctx context.Context, dir, branch string, force bool) error {
	flag := "-d"
	if force {
		flag = "-D"
	}

	if _, err := g.exec(ctx, dir, "branch", flag, branch); err != nil {
		return fmt.Errorf("deleting branch %s: %w", branch, err)
	}

	return nil
}

// Worktree is one entry from `git worktree list`.
type Worktree struct {
	Path   string
	Branch string
	Head   string
	// Detached reports a worktree not on any branch.
	Detached bool
}

// WorktreeList reports every worktree registered against a repository.
func (g *Client) WorktreeList(ctx context.Context, dir string) ([]Worktree, error) {
	res, err := g.exec(ctx, dir, "worktree", "list", "--porcelain")
	if err != nil {
		return nil, fmt.Errorf("listing worktrees in %s: %w", dir, err)
	}

	return parseWorktreeList(string(res.Stdout)), nil
}

// parseWorktreeList decodes `worktree list --porcelain` output, in which records
// are separated by blank lines.
func parseWorktreeList(out string) []Worktree {
	var (
		list    []Worktree
		current Worktree
		open    bool
	)

	flush := func() {
		if open {
			list = append(list, current)
			current = Worktree{}
			open = false
		}
	}

	for line := range strings.SplitSeq(out, "\n") {
		line = strings.TrimSpace(line)

		switch {
		case line == "":
			flush()
		case strings.HasPrefix(line, "worktree "):
			flush()

			current.Path = strings.TrimPrefix(line, "worktree ")
			open = true
		case strings.HasPrefix(line, "HEAD "):
			current.Head = strings.TrimPrefix(line, "HEAD ")
		case strings.HasPrefix(line, "branch "):
			current.Branch = strings.TrimPrefix(strings.TrimPrefix(line, "branch "), "refs/heads/")
		case line == "detached":
			current.Detached = true
		}
	}

	flush()

	return list
}

// Touched summarizes what a mission changed in one worktree.
type Touched struct {
	// Dirty reports uncommitted changes, tracked or not.
	Dirty bool
	// Ahead is the number of commits made since the branch point.
	Ahead int
	// ShortStat is git's one-line summary of the committed diff.
	ShortStat string
}

// Any reports whether the worktree has any work worth a look.
func (t Touched) Any() bool { return t.Dirty || t.Ahead > 0 }

// TouchedSummary reports what changed in a worktree relative to its branch point.
//
// The comparison is against the pinned base SHA rather than origin/<default>,
// because the user's fetch.force setting means any unrelated fetch elsewhere can
// move that ref, which would retroactively pull other people's commits into what
// q reports as this mission's work. Using the SHA also needs no network.
func (g *Client) TouchedSummary(ctx context.Context, worktree, baseSHA string) (Touched, error) {
	var touched Touched

	status, err := g.exec(ctx, worktree, "status", "--porcelain=v1")
	if err != nil {
		return touched, fmt.Errorf("checking status of %s: %w", worktree, err)
	}

	touched.Dirty = status.Out() != ""

	if baseSHA == "" {
		return touched, nil
	}

	countRes, err := g.exec(ctx, worktree, "rev-list", "--count", baseSHA+"..HEAD")
	if err != nil {
		return touched, fmt.Errorf("counting commits in %s: %w", worktree, err)
	}

	ahead, err := strconv.Atoi(countRes.Out())
	if err != nil {
		return touched, fmt.Errorf("parsing commit count %q for %s: %w", countRes.Out(), worktree, err)
	}

	touched.Ahead = ahead

	if ahead > 0 {
		if stat, err := g.exec(ctx, worktree, "diff", "--shortstat", baseSHA+"..HEAD"); err == nil {
			touched.ShortStat = stat.Out()
		}
	}

	return touched, nil
}
