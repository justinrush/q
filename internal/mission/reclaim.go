package mission

import "errors"

// Action is what deleting a mission would do to one repo's worktree and branch.
type Action string

// The dispositions a worktree can receive.
const (
	// ActionDiscard removes the worktree and its branch. Nothing would be lost:
	// the branch holds no commits and the tree is clean.
	ActionDiscard Action = "discard"
	// ActionKeepBranch removes the worktree but leaves the branch, because it
	// carries commits that are not anywhere else yet.
	ActionKeepBranch Action = "keep-branch"
	// ActionNeedsForce means the worktree holds uncommitted work, so git refuses to
	// remove it and q will not overrule that without being told to.
	ActionNeedsForce Action = "needs-force"
	// ActionUnavailable means the repo can no longer be inspected, usually because
	// the user's checkout moved or was deleted.
	ActionUnavailable Action = "unavailable"
)

// RepoDisposition is what would happen to one repo.
type RepoDisposition struct {
	Repo         string `json:"repo"`
	WorktreePath string `json:"worktreePath"`
	Branch       string `json:"branch"`
	Action       Action `json:"action"`
	// Dirty reports uncommitted changes, tracked or not.
	Dirty bool `json:"dirty"`
	// Ahead counts commits made since the branch point.
	Ahead int `json:"ahead"`
	// Pushed reports a remote-tracking ref for the branch, meaning the work exists
	// somewhere other than this machine.
	Pushed bool `json:"pushed"`
	// Reason explains an unavailable repo.
	Reason string `json:"reason,omitempty"`
}

// Plan is what deleting a mission would do.
type Plan struct {
	Repos []RepoDisposition `json:"repos"`
	// NeedsForce reports that at least one worktree holds uncommitted work.
	NeedsForce bool `json:"needsForce"`
	// KeptBranches lists branches that would survive the delete.
	KeptBranches []string `json:"keptBranches,omitempty"`
	// TmuxSession is the session that would be killed, if it is still alive.
	TmuxSession string `json:"tmuxSession,omitempty"`
	// SessionAlive reports that an agent is still running.
	SessionAlive bool `json:"sessionAlive"`
}

// Report is what deleting a mission actually did.
type Report struct {
	Removed []string `json:"removed,omitempty"`
	// KeptBranches lists branches deliberately left behind.
	KeptBranches []string `json:"keptBranches,omitempty"`
	// DeletedBranches lists branches removed because they held nothing.
	DeletedBranches []string `json:"deletedBranches,omitempty"`
	// Failures records what could not be reclaimed, so a partial result is visible
	// rather than silently incomplete.
	Failures []string `json:"failures,omitempty"`
}

// ErrNeedsForce reports that reclaiming would discard uncommitted work.
var ErrNeedsForce = errors.New("worktree holds uncommitted changes")
