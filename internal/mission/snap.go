package mission

// Snap identifies one snapshot of a mission worktree.
//
// A snapshot is a commit whose tree is the worktree exactly as it stood,
// uncommitted work included. Head is the branch tip it was taken on top of, and
// Commit is the snapshot itself. For a clean worktree the two are the same
// commit, because there is nothing to add to the tip.
type Snap struct {
	Head   string `json:"head,omitempty"`
	Commit string `json:"commit,omitempty"`
	// Tree is the snapshot's tree. It is carried so a host can tell whether its
	// worktree still matches a snapshot without asking git about the commit.
	Tree string `json:"tree,omitempty"`
}

// Empty reports whether there is no snapshot.
func (s Snap) Empty() bool { return s.Commit == "" }

// RepoState is one host's report on one repository of one mission, sent with
// every exchange between a pair.
//
// Two of these, one from each host, are all that is needed to decide what
// should happen to a worktree: whether the hosts already agree, which of them
// should take the other's state, or whether both have changed it.
type RepoState struct {
	Mission MissionID `json:"mission"`
	Repo    string    `json:"repo"`
	// Path is where the reporting host keeps its checkout of the repository.
	// The host that dials needs it to address the other's repository; empty
	// means the repository is not checked out there.
	Path string `json:"path,omitempty"`
	// Snap is the host's worktree as it stands, empty when it has none yet.
	Snap Snap `json:"snap,omitzero"`
	// Applied is the last snapshot at which this host's worktree and its peer's
	// were the same: one it took from the peer, or one the peer was seen to
	// hold too. A worktree whose Snap is still its Applied has not been edited
	// here since, which is what makes it safe to overwrite.
	Applied string `json:"applied,omitempty"`
	// Includes is a snapshot of the peer's that this host has already combined
	// with its own, set after it resolved a worktree both had changed. A peer
	// still holding exactly that snapshot can take this host's state outright:
	// everything it had is in there.
	Includes string `json:"includes,omitempty"`
}

// Edited reports whether the worktree has changed since the two hosts last
// agreed.
func (r RepoState) Edited() bool { return r.Snap.Commit != r.Applied }

// RepoKey identifies a repository within a mission.
type RepoKey struct {
	Mission MissionID
	Repo    string
}

// Key returns the state's place in an index.
func (r RepoState) Key() RepoKey { return RepoKey{Mission: r.Mission, Repo: r.Repo} }

// Settlement is what a host should do with one worktree, given both reports.
type Settlement int

// The outcomes of comparing two hosts' reports.
const (
	// SettleNothing means there is nothing to do: the hosts agree, the peer has
	// no worktree to compare against, or it is the peer's turn to act.
	SettleNothing Settlement = iota
	// SettleAgreed means both worktrees are the same and this host should
	// record that.
	SettleAgreed
	// SettleTake means this host's worktree should become the peer's.
	SettleTake
	// SettleDiverged means both have changed since they last agreed.
	SettleDiverged
)

// Settle decides what this host should do with a worktree.
//
// The rule is the one a person would apply. If only one side has changed, the
// other takes the change. If both have, neither may be overwritten. And if
// neither has yet they differ, which only happens when one of them was built
// from an older state of the other, the host that is not running the mission
// follows the one that is.
//
// Once one host has resolved a divergence it says so, by naming the peer
// snapshot its state includes. That is what breaks the tie: without it the
// peer would see two edited worktrees again and the pair would never settle.
//
// holds says whether this host runs the mission.
func Settle(mine, theirs RepoState, holds bool) Settlement {
	switch {
	case theirs.Snap.Empty() || mine.Snap.Empty():
		return SettleNothing
	case mine.Snap.Commit == theirs.Snap.Commit:
		if mine.Applied == mine.Snap.Commit {
			return SettleNothing
		}

		return SettleAgreed
	case theirs.Includes != "" && theirs.Includes == mine.Snap.Commit:
		// The peer merged this exact state into its own. Nothing here is lost
		// by taking the result, whatever this host has done since it last agreed.
		return SettleTake
	case mine.Includes != "" && mine.Includes == theirs.Snap.Commit:
		// The mirror image: this host did the merging, and waits to be taken.
		return SettleNothing
	case mine.Edited() && theirs.Edited():
		return SettleDiverged
	case mine.Edited():
		return SettleNothing
	case theirs.Edited() || !holds:
		return SettleTake
	default:
		return SettleNothing
	}
}
