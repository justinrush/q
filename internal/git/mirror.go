package git

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/justinrush/q/internal/mission"
)

// Mirror creates this host's worktrees for a mission its paired host launched.
//
// A mirror is laid out exactly like the worktrees a launch provisions — same
// mission directory, one worktree per repository — because it may become the
// real thing: when the lease moves here, the agent is started in it. What
// differs is where it starts from. A launch fetches the base branch from origin
// and cuts a new branch at it; a mirror starts at the commit the other host is
// already on, which arrived with its snapshot, so origin is not consulted at
// all and a laptop with no route to the forge can still mirror.
//
// starts maps a repository's name to the commit its worktree should begin at.
// A repository with no entry is skipped: its snapshot has not arrived yet, and
// it is picked up on a later exchange.
//
// The mission is returned with its directory and worktrees recorded. On a
// partial failure it carries whatever was created, alongside the error, so the
// caller can record it and the next attempt resumes rather than colliding.
func (p *Provisioner) Mirror(
	ctx context.Context,
	operation mission.Operation,
	ms mission.Mission,
	starts map[string]string,
) (mission.Mission, error) {
	repos, err := mission.MissionRepos(operation, ms)
	if err != nil {
		return ms, err
	}

	if ms.MissionDir == "" {
		state, err := p.prepareMissionDir(operation, &ms)
		if err != nil {
			return ms, err
		}

		// A directory claimed by an earlier attempt already journals its worktrees.
		for name, saved := range state.Work {
			if _, known := ms.Work[name]; !known {
				ms.Work = withWork(ms.Work, name, saved)
			}
		}
	}

	var failures []error

	for _, repo := range repos {
		start, ok := starts[repo.Name]
		if !ok {
			continue
		}

		work, err := p.mirrorRepo(ctx, repo, ms, start)
		if err != nil {
			failures = append(failures, fmt.Errorf("mirroring %s: %w", repo.Name, err))

			continue
		}

		ms.Work = withWork(ms.Work, repo.Name, work)
	}

	if err := writeProvisionState(ms.MissionDir, provisionState{MissionID: ms.ID, Work: ms.Work}); err != nil {
		failures = append(failures, err)
	}

	return ms, errors.Join(failures...)
}

// withWork returns work with one entry set, allocating the map if needed.
func withWork(work map[string]mission.RepoWork, name string, entry mission.RepoWork) map[string]mission.RepoWork {
	if work == nil {
		work = make(map[string]mission.RepoWork)
	}

	work[name] = entry

	return work
}

// mirrorRepo creates one repository's mirror worktree at start.
func (p *Provisioner) mirrorRepo(
	ctx context.Context,
	repo mission.Repo,
	ms mission.Mission,
	start string,
) (mission.RepoWork, error) {
	// What the holder recorded about the branch point is kept: diffs on this
	// host are measured from the same commit as on the other.
	work := ms.Work[repo.Name]
	work.RepoName = repo.Name
	work.WorktreePath = filepath.Join(ms.MissionDir, repo.Name)

	if repo.Path == "" {
		return work, fmt.Errorf("%s is not checked out on this machine; clone it under one of repos.roots", repoLabel(repo))
	}

	commonDir, err := p.resolveCommonDir(ctx, repo)
	if err != nil {
		return work, err
	}

	unlock := p.git.Lock(commonDir)
	defer unlock()

	existing, found, err := p.worktreeAt(ctx, commonDir, work.WorktreePath)
	if err != nil {
		return work, err
	}

	if found {
		work.Branch = existing.Branch
		work.Created = true
		work.Error = ""

		return work, nil
	}

	if _, statErr := os.Stat(work.WorktreePath); statErr == nil {
		return work, fmt.Errorf("%s exists but is not a registered git worktree", work.WorktreePath)
	}

	branch, err := p.pickBranch(ctx, commonDir, ms.Slug)
	if err != nil {
		return work, err
	}

	if err := p.git.WorktreeAdd(ctx, commonDir, work.WorktreePath, branch, start); err != nil {
		return work, err
	}

	work.Branch = branch
	work.Created = true
	work.Error = ""

	return work, nil
}

// resolveCommonDir resolves a repository's git directory without asking for its
// default branch, which a mirror has no use for and which can cost a network
// round trip to learn.
func (p *Provisioner) resolveCommonDir(ctx context.Context, repo mission.Repo) (string, error) {
	if repo.CommonDir != "" {
		return repo.CommonDir, nil
	}

	return p.git.CommonDir(ctx, repo.Path)
}

// repoLabel names a repository for an error, preferring its origin.
func repoLabel(repo mission.Repo) string {
	if repo.URL != "" {
		return repo.Name + " (" + repo.URL + ")"
	}

	return repo.Name
}

// Mirrors moves worktree state between this host and its paired host.
//
// It is the git half of sharing a mission: snapshots in, snapshots out, and the
// worktrees they are laid out in. Deciding which of those to do belongs to the
// daemon, which knows who is running what.
type Mirrors struct {
	git         *Client
	provisioner *Provisioner
}

// NewMirrors returns a Mirrors working through the given client and
// provisioner.
func NewMirrors(client *Client, provisioner *Provisioner) *Mirrors {
	return &Mirrors{git: client, provisioner: provisioner}
}

// Capture snapshots a worktree and publishes it under this host's ref.
func (m *Mirrors) Capture(
	ctx context.Context,
	worktree string,
	host mission.HostID,
	id mission.MissionID,
) (mission.Snap, error) {
	return m.git.Snapshot(ctx, worktree, SnapshotRef(host, id))
}

// Apply lays a snapshot out in a worktree. See [Client.ApplySnapshot].
func (m *Mirrors) Apply(ctx context.Context, worktree string, current, target mission.Snap) error {
	return m.git.ApplySnapshot(ctx, worktree, current, target)
}

// Has reports whether a commit has arrived in a repository.
func (m *Mirrors) Has(ctx context.Context, repo, commit string) bool {
	return m.git.HasCommit(ctx, repo, commit)
}

// Fetch brings the peer's published snapshots for one repository.
func (m *Mirrors) Fetch(ctx context.Context, repo, url string, peer mission.HostID) error {
	return m.git.FetchHostRefs(ctx, repo, url, peer)
}

// Push sends this host's published snapshots for one repository.
func (m *Mirrors) Push(ctx context.Context, repo, url string, self mission.HostID) error {
	return m.git.PushHostRefs(ctx, repo, url, self)
}

// Keep records the snapshot two hosts last agreed on, so it survives garbage
// collection for as long as it may be needed as a merge base.
func (m *Mirrors) Keep(ctx context.Context, repo string, host mission.HostID, id mission.MissionID, commit string) error {
	return m.git.KeepBase(ctx, repo, host, id, commit)
}

// Provision creates this host's worktrees for a mission the peer launched.
func (m *Mirrors) Provision(
	ctx context.Context,
	operation mission.Operation,
	ms mission.Mission,
	starts map[string]string,
) (mission.Mission, error) {
	return m.provisioner.Mirror(ctx, operation, ms, starts)
}
