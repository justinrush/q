// Keeping a mission's worktrees the same on both paired hosts.
//
// Every running mission has a worktree per repository on each host. One host
// runs the agent; the other holds a mirror, there so the code is local wherever
// a human looks at it and so the agent can be started there if the lease moves.
//
// Each exchange, both hosts snapshot their worktrees and report what they have.
// The primary then moves the snapshots between the two repositories, and each
// host settles its own worktrees against the other's report.

package daemon

import (
	"context"
	"os"
	"slices"
	"strings"

	"github.com/justinrush/q/internal/mission"
)

// Worktrees moves worktree state between this host and its paired host.
//
// It is an interface so the rules for when a worktree may be overwritten can be
// tested without git, and so this package does not have to know how a snapshot
// is made.
type Worktrees interface {
	Capture(ctx context.Context, worktree string, host mission.HostID, id mission.MissionID) (mission.Snap, error)
	Apply(ctx context.Context, worktree string, current, target mission.Snap) error
	Has(ctx context.Context, repo, commit string) bool
	Fetch(ctx context.Context, repo, url string, peer mission.HostID) error
	Push(ctx context.Context, repo, url string, self mission.HostID) error
	Keep(ctx context.Context, repo string, host mission.HostID, id mission.MissionID, commit string) error
	Merge(
		ctx context.Context,
		worktree string,
		ours, theirs mission.Snap,
		bases []string,
	) (mission.Snap, bool, error)
	Preserve(ctx context.Context, repo, branch, commit string) error
	Provision(
		ctx context.Context,
		operation mission.Operation,
		ms mission.Mission,
		starts map[string]string,
	) (mission.Mission, error)
}

// WithWorktrees attaches the component that snapshots and mirrors worktrees.
// Without it, paired hosts share records but not code.
func WithWorktrees(w Worktrees) Option {
	return func(s *Service) { s.worktrees = w }
}

// captureWorktrees snapshots every worktree this host has for a running
// mission and reports them, along with where it keeps each repository.
//
// A repository is reported even when this host has no worktree for it yet. The
// peer cannot send a snapshot to a repository it cannot address, and the path
// is the address.
func (s *Service) captureWorktrees(ctx context.Context) []mission.RepoState {
	if s.worktrees == nil {
		return nil
	}

	var states []mission.RepoState

	for _, ms := range s.store.Snapshot().Missions {
		if !ms.Running() {
			continue
		}

		for _, repo := range ms.LaunchRepos {
			state := mission.RepoState{Mission: ms.ID, Repo: repo.Name, Path: repo.Path}

			if work, ok := liveWork(ms, repo.Name); ok {
				snap, err := s.worktrees.Capture(ctx, work.WorktreePath, s.self, ms.ID)
				if err != nil {
					s.warn("snapshotting a worktree", "mission", ms.ID, "repo", repo.Name, "error", err)
				} else {
					state.Snap, state.Applied, state.Includes = snap, work.SyncBase, work.SyncIncludes
				}
			}

			states = append(states, state)
		}
	}

	return states
}

// liveWork returns a mission's worktree for a repository, if this host has one
// on disk.
func liveWork(ms mission.Mission, repo string) (mission.RepoWork, bool) {
	work, ok := ms.Work[repo]
	if !ok || !work.Created || work.WorktreePath == "" {
		return mission.RepoWork{}, false
	}

	if _, err := os.Stat(work.WorktreePath); err != nil {
		return mission.RepoWork{}, false
	}

	return work, true
}

// transferSnapshots moves snapshots between this host's repositories and the
// peer's. Only the primary does this, because only the primary can dial.
//
// Each repository is contacted only when there is something new to move in
// that direction. Most exchanges find every worktree unchanged, and a git
// connection per repository per exchange would be most of the cost of pairing.
func (s *Service) transferSnapshots(ctx context.Context, mine, theirs []mission.RepoState) {
	if s.worktrees == nil || s.remote == nil {
		return
	}

	peer := s.store.Snapshot().PeerID()
	if peer == "" {
		return
	}

	for local, remotePath := range repoPairs(mine, theirs) {
		url := s.remote.GitURL(remotePath)
		if url == "" {
			continue
		}

		if sig := signature(theirs, remotePath); sig != "" && s.transfers.changed("fetch", local, sig) {
			if err := s.worktrees.Fetch(ctx, local, url, peer); err != nil {
				s.warn("fetching snapshots from the paired q", "repo", local, "error", err)
				s.transfers.forget("fetch", local)
			}
		}

		if sig := signature(mine, local); s.transfers.changed("push", local, sig) {
			if err := s.worktrees.Push(ctx, local, url, s.self); err != nil {
				s.warn("sending snapshots to the paired q", "repo", local, "error", err)
				s.transfers.forget("push", local)
			}
		}
	}
}

// repoPairs maps each of this host's repository paths to the peer's path for
// the same repository, for every repository both hosts have checked out.
func repoPairs(mine, theirs []mission.RepoState) map[string]string {
	remote := make(map[mission.RepoKey]string, len(theirs))
	for _, state := range theirs {
		remote[state.Key()] = state.Path
	}

	pairs := map[string]string{}

	for _, state := range mine {
		if path := remote[state.Key()]; state.Path != "" && path != "" {
			pairs[state.Path] = path
		}
	}

	return pairs
}

// signature summarizes the snapshots one host holds in one repository, so a
// change in any of them is a change in the string.
func signature(states []mission.RepoState, path string) string {
	var parts []string

	for _, state := range states {
		if state.Path == path && !state.Snap.Empty() {
			parts = append(parts, string(state.Mission)+"="+state.Snap.Commit)
		}
	}

	slices.Sort(parts)

	return strings.Join(parts, ",")
}

// transferLog remembers what was last moved for each repository in each
// direction.
type transferLog struct {
	last map[string]string
}

// changed reports whether sig differs from what was last moved, recording it.
func (t *transferLog) changed(direction, repo, sig string) bool {
	if t.last == nil {
		t.last = map[string]string{}
	}

	key := direction + "\x00" + repo
	if previous, ok := t.last[key]; ok && previous == sig {
		return false
	}

	t.last[key] = sig

	return true
}

// forget makes the next exchange retry a transfer that failed.
func (t *transferLog) forget(direction, repo string) {
	delete(t.last, direction+"\x00"+repo)
}

// settleWorktrees brings this host's worktrees into line with the peer's
// report: creating mirrors it does not have yet, taking the peer's state where
// only the peer has changed, and leaving alone anything edited here.
//
// It reports whether any worktree on this host was created or overwritten.
func (s *Service) settleWorktrees(ctx context.Context, mine, theirs []mission.RepoState) bool {
	if s.worktrees == nil {
		return false
	}

	own := indexStates(mine)
	peer := indexStates(theirs)
	changed := false

	for _, ms := range s.store.Snapshot().Missions {
		if !ms.Running() {
			continue
		}

		mirrored := s.provisionMirror(ctx, ms, peer)
		created := mirrored.MissionDir != ms.MissionDir || len(mirrored.Work) != len(ms.Work)

		if s.settleMission(ctx, mirrored, own, peer) || created {
			changed = true
		}
	}

	return changed
}

// indexStates keys reports by mission and repository.
func indexStates(states []mission.RepoState) map[mission.RepoKey]mission.RepoState {
	out := make(map[mission.RepoKey]mission.RepoState, len(states))
	for _, state := range states {
		out[state.Key()] = state
	}

	return out
}

// provisionMirror creates the worktrees this host lacks for a mission the peer
// runs, for every repository whose snapshot has arrived.
func (s *Service) provisionMirror(
	ctx context.Context,
	ms mission.Mission,
	peer map[mission.RepoKey]mission.RepoState,
) mission.Mission {
	if s.holds(ms) {
		return ms
	}

	starts := map[string]string{}

	for _, repo := range ms.LaunchRepos {
		if _, have := liveWork(ms, repo.Name); have {
			continue
		}

		theirs := peer[mission.RepoKey{Mission: ms.ID, Repo: repo.Name}]

		// A mirror starts at the peer's branch tip. The snapshot is laid over it
		// afterwards, exactly as an update to an existing mirror would be.
		if repo.Path == "" || s.worktrees.Has(ctx, repo.Path, theirs.Snap.Head) {
			if !theirs.Snap.Empty() {
				starts[repo.Name] = theirs.Snap.Head
			}
		}
	}

	if len(starts) == 0 {
		return ms
	}

	operation, _ := s.store.Snapshot().Operation(ms.OperationID)

	provisioned, err := s.worktrees.Provision(ctx, operation, ms, starts)
	if err != nil {
		s.warn("mirroring a mission the paired q runs", "mission", ms.ID, "error", err)
	}

	s.updateLocal(ms.ID, "mission.mirrored", func(stored *mission.Mission) {
		stored.MissionDir = provisioned.MissionDir

		for name, work := range provisioned.Work {
			entry := stored.Work[name]
			entry.RepoName = name
			entry.WorktreePath = work.WorktreePath
			entry.Branch = work.Branch
			entry.Created = work.Created
			entry.Error = work.Error

			if stored.Work == nil {
				stored.Work = map[string]mission.RepoWork{}
			}

			stored.Work[name] = entry
		}

		if err != nil {
			stored.LocalBadges = stored.WithLocalBadge(mission.BadgeRepoMissing, firstLine(err.Error()))
		} else {
			stored.LocalBadges = stored.WithoutLocalBadge(mission.BadgeRepoMissing)
		}
	})

	updated, ok := s.store.Snapshot().Mission(ms.ID)
	if !ok {
		return ms
	}

	return updated
}

// firstLine returns the first line of a possibly multi-line message.
func firstLine(text string) string {
	line, _, _ := strings.Cut(text, "\n")

	return line
}

// settleMission settles each of one mission's worktrees, reporting whether any
// was overwritten.
func (s *Service) settleMission(
	ctx context.Context,
	ms mission.Mission,
	own, peer map[mission.RepoKey]mission.RepoState,
) bool {
	holds := s.holds(ms)
	edited, took := false, false

	for _, repo := range ms.LaunchRepos {
		work, have := liveWork(ms, repo.Name)
		if !have {
			continue
		}

		key := mission.RepoKey{Mission: ms.ID, Repo: repo.Name}

		mine, known := own[key]
		if !known || mine.Snap.Empty() {
			// Created during this exchange, so it was not in the report taken at
			// its start. It holds the peer's branch tip and nothing of its own,
			// which is to say it has not been edited here.
			snap, err := s.worktrees.Capture(ctx, work.WorktreePath, s.self, ms.ID)
			if err != nil {
				continue
			}

			mine = mission.RepoState{Mission: ms.ID, Repo: repo.Name, Path: repo.Path, Snap: snap}
			if snap.Commit != peer[key].Snap.Commit {
				mine.Applied = snap.Commit
			}
		}

		switch mission.Settle(mine, peer[key], holds) {
		case mission.SettleAgreed:
			s.recordAgreed(ctx, ms.ID, repo, mine.Snap.Commit)
		case mission.SettleTake:
			took = s.takeSnapshot(ctx, ms, repo, work, mine, peer[key]) || took
		case mission.SettleDiverged:
			if s.resolve(ctx, ms, repo, work, mine, peer[key]) {
				took = true
			} else {
				edited = true
			}
		case mission.SettleNothing:
			// Edited here and not yet matched by the peer: its turn to take it.
			edited = edited || (mine.Edited() && mine.Snap.Commit != peer[key].Snap.Commit)
		}
	}

	s.markEdited(ms, edited && !holds)

	return took
}

// takeSnapshot overwrites a worktree with the peer's snapshot, reporting
// whether it did.
//
// Two things can still stop it. An agent mid-turn on this host must not have
// files change under it, so a holder waits until its agent is idle. And the
// worktree is snapshotted again immediately before being overwritten: the
// report it was judged by is seconds old, and a save made in the meantime
// would otherwise be lost to the very mechanism meant to protect it.
func (s *Service) takeSnapshot(
	ctx context.Context,
	ms mission.Mission,
	repo mission.Repo,
	work mission.RepoWork,
	mine, theirs mission.RepoState,
) bool {
	if s.holds(ms) && ms.Status == mission.StatusActive {
		return false
	}

	if !s.worktrees.Has(ctx, work.WorktreePath, theirs.Snap.Commit) {
		// Not transferred yet. The next exchange will have it.
		return false
	}

	now, err := s.worktrees.Capture(ctx, work.WorktreePath, s.self, ms.ID)
	if err != nil || now.Commit != mine.Snap.Commit {
		return false
	}

	if err := s.worktrees.Apply(ctx, work.WorktreePath, mine.Snap, theirs.Snap); err != nil {
		s.warn("applying the paired q's snapshot", "mission", ms.ID, "repo", repo.Name, "error", err)

		return false
	}

	// Publish the new state under this host's own ref, so its next report and
	// the ref the peer fetches agree.
	if _, err := s.worktrees.Capture(ctx, work.WorktreePath, s.self, ms.ID); err != nil {
		s.warn("recording an applied snapshot", "mission", ms.ID, "repo", repo.Name, "error", err)
	}

	s.recordAgreed(ctx, ms.ID, repo, theirs.Snap.Commit)

	return true
}

// recordAgreed notes the snapshot both hosts now hold for a worktree.
func (s *Service) recordAgreed(ctx context.Context, id mission.MissionID, repo mission.Repo, commit string) {
	if repo.Path != "" {
		if err := s.worktrees.Keep(ctx, repo.Path, s.self, id, commit); err != nil {
			s.warn("keeping an agreed snapshot", "mission", id, "repo", repo.Name, "error", err)
		}
	}

	s.updateLocal(id, "mission.snapshot_agreed", func(ms *mission.Mission) {
		work, ok := ms.Work[repo.Name]
		if !ok {
			return
		}

		work.SyncBase = commit
		// Agreement supersedes any merge still waiting to be taken.
		work.SyncIncludes = ""
		ms.Work[repo.Name] = work
	})
}

// markEdited shows, or clears, the badge for a mirror edited on this host.
func (s *Service) markEdited(ms mission.Mission, edited bool) {
	if ms.HasLocalBadge(mission.BadgeLocalEdits) == edited {
		return
	}

	s.updateLocal(ms.ID, "mission.mirror_edited", func(stored *mission.Mission) {
		if edited {
			stored.LocalBadges = stored.WithLocalBadge(mission.BadgeLocalEdits, "")
		} else {
			stored.LocalBadges = stored.WithoutLocalBadge(mission.BadgeLocalEdits)
		}
	})
}

// resolve deals with a worktree both hosts have changed, reporting whether it
// did.
//
// Only the primary resolves. Two hosts merging the same pair of states at once
// would each produce a result the other then had to merge, and they would chase
// each other. And it waits for the agent to be idle, on whichever host it is:
// the result replaces what is in the worktree, and that must not happen under
// an agent part-way through a turn.
//
// A merge that comes out clean is laid out here, and the peer takes it on the
// next exchange. One that does not is not attempted by halves. This host's
// version stays where it is, the peer's is kept on a branch of its own beside
// it, and the card says so; from there it is an ordinary merge for a person, or
// an agent, to do.
func (s *Service) resolve(
	ctx context.Context,
	ms mission.Mission,
	repo mission.Repo,
	work mission.RepoWork,
	mine, theirs mission.RepoState,
) bool {
	if s.remote == nil || ms.Status == mission.StatusActive {
		return false
	}

	if !s.worktrees.Has(ctx, work.WorktreePath, theirs.Snap.Commit) {
		return false
	}

	// As when taking a snapshot: the report is seconds old, and what is about
	// to be replaced must be what was judged.
	now, err := s.worktrees.Capture(ctx, work.WorktreePath, s.self, ms.ID)
	if err != nil || now.Commit != mine.Snap.Commit {
		return false
	}

	bases := []string{mine.Applied, theirs.Applied, work.BaseSHA}

	merged, clean, err := s.worktrees.Merge(ctx, work.WorktreePath, mine.Snap, theirs.Snap, bases)
	if err != nil {
		s.warn("merging the paired q's work", "mission", ms.ID, "repo", repo.Name, "error", err)

		return false
	}

	if !clean {
		return s.keepBoth(ctx, ms, repo, work, theirs)
	}

	if err := s.worktrees.Apply(ctx, work.WorktreePath, mine.Snap, merged); err != nil {
		s.warn("applying a merge of the paired q's work", "mission", ms.ID, "repo", repo.Name, "error", err)

		return false
	}

	if _, err := s.worktrees.Capture(ctx, work.WorktreePath, s.self, ms.ID); err != nil {
		s.warn("recording a merge", "mission", ms.ID, "repo", repo.Name, "error", err)
	}

	s.logger.Info("merged work from both hosts", "mission", ms.ID, "repo", repo.Name)
	s.recordIncluded(ms.ID, repo.Name, theirs.Snap.Commit, "")

	return true
}

// keepBoth settles a conflict in the primary's favor without losing the other
// side: the peer's state is kept on a branch named for it.
func (s *Service) keepBoth(
	ctx context.Context,
	ms mission.Mission,
	repo mission.Repo,
	work mission.RepoWork,
	theirs mission.RepoState,
) bool {
	snap := s.store.Snapshot()
	branch := work.Branch + "--" + mission.Slug(snap.HostName(snap.PeerID()))

	if err := s.worktrees.Preserve(ctx, work.WorktreePath, branch, theirs.Snap.Commit); err != nil {
		s.warn("keeping the paired q's side of a conflict", "mission", ms.ID, "repo", repo.Name, "error", err)

		return false
	}

	s.logger.Info("both hosts changed the same lines; kept the other side on a branch",
		"mission", ms.ID, "repo", repo.Name, "branch", branch)
	s.recordIncluded(ms.ID, repo.Name, theirs.Snap.Commit, branch)

	return true
}

// recordIncluded notes that this host's worktree now accounts for a snapshot
// of the peer's, and when that was by setting it aside, where it was put.
func (s *Service) recordIncluded(id mission.MissionID, repo, commit, keptOn string) {
	s.updateLocal(id, "mission.snapshot_merged", func(ms *mission.Mission) {
		work, ok := ms.Work[repo]
		if !ok {
			return
		}

		work.SyncIncludes = commit
		ms.Work[repo] = work

		if keptOn != "" {
			ms.LocalBadges = ms.WithLocalBadge(mission.BadgeDiverged, keptOn)
		}
	})
}
