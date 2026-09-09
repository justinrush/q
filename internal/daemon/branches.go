// The branch list a mission's briefing form picks a base from.
//
// Like the model catalog, this is advisory: nothing validates a mission's base
// branch against it, and a daemon with no git still serves missions — their
// forms simply offer nothing to pick from and the branch can still be typed. The
// authoritative check happens where it matters, when the provisioner fetches it.

package daemon

import (
	"context"
	"slices"
	"sync"
	"time"
)

// branchTTL is how long a remote answer is reused.
//
// A form is opened and reopened in quick succession while a mission is being
// written, and asking origin every time would put a network round trip in front
// of a keystroke. Minutes rather than hours because a branch pushed from another
// checkout is exactly what the refresh exists to find.
const branchTTL = 2 * time.Minute

// Brancher lists the branches a repository's origin offers.
//
// The daemon names the behavior it needs rather than importing git's client, so
// that a service under test can answer with a fixed list.
type Brancher interface {
	CommonDir(ctx context.Context, repoPath string) (string, error)
	LocalBranches(ctx context.Context, dir string) ([]string, error)
	RemoteBranches(ctx context.Context, dir string) ([]string, error)
}

// WithBrancher attaches the git client that answers branch queries.
func WithBrancher(b Brancher) Option {
	return func(s *Service) { s.brancher = b }
}

// branchCache remembers what origin last said, keyed by common directory.
//
// Keying on the common directory rather than the path the caller passed means
// two operations naming the same repository through different worktrees share
// one entry.
type branchCache struct {
	mu      sync.Mutex
	entries map[string]branchEntry
}

type branchEntry struct {
	branches []string
	at       time.Time
}

func newBranchCache() *branchCache {
	return &branchCache{entries: map[string]branchEntry{}}
}

// get returns the cached branches for dir if they are younger than ttl.
func (c *branchCache) get(dir string, now time.Time, ttl time.Duration) ([]string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()

	entry, ok := c.entries[dir]
	if !ok || now.Sub(entry.at) > ttl {
		return nil, false
	}

	return slices.Clone(entry.branches), true
}

func (c *branchCache) put(dir string, branches []string, now time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()

	c.entries[dir] = branchEntry{branches: slices.Clone(branches), at: now}
}

// Branches lists the branches repoPath's origin offers, best-effort.
//
// With refresh false it reads only the remote-tracking refs already on disk,
// which is instant. With it true it also asks origin and merges the answer in,
// so a branch pushed since the last fetch appears. A failure either way is not
// an error: a picker that offers a short list is more useful than a dialog
// reporting that git could not be reached, and the branch can still be typed.
func (s *Service) Branches(ctx context.Context, repoPath string, refresh bool) []string {
	if s.brancher == nil || repoPath == "" {
		return nil
	}

	commonDir, err := s.brancher.CommonDir(ctx, repoPath)
	if err != nil {
		s.logger.Warn("resolving the git directory for branches", "repo", repoPath, "error", err)

		return nil
	}

	local, err := s.brancher.LocalBranches(ctx, commonDir)
	if err != nil {
		s.logger.Warn("listing local branches", "repo", repoPath, "error", err)
	}

	if !refresh {
		return sortedUnion(local, nil)
	}

	if cached, ok := s.branches.get(commonDir, s.now(), branchTTL); ok {
		return sortedUnion(local, cached)
	}

	remote, err := s.brancher.RemoteBranches(ctx, commonDir)
	if err != nil {
		s.logger.Warn("listing branches on origin", "repo", repoPath, "error", err)

		return sortedUnion(local, nil)
	}

	s.branches.put(commonDir, remote, s.now())

	return sortedUnion(local, remote)
}

// sortedUnion merges two branch lists into one sorted, deduplicated list.
func sortedUnion(a, b []string) []string {
	out := make([]string, 0, len(a)+len(b))
	out = append(out, a...)
	out = append(out, b...)

	slices.Sort(out)

	return slices.Compact(out)
}
