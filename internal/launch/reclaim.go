package launch

import (
	"context"
	"errors"
	"fmt"
	"os"

	"github.com/justinrush/q/internal/domain"
)

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

// PlanReclaim reports what deleting a mission would do, without doing any of it.
//
// This exists so a confirmation can name what is about to be lost. Deleting a mission can
// throw away hours of an agent's uncommitted work, and a dialog that only says "are you
// sure" gives the human nothing to be sure about.
func (l *Launcher) PlanReclaim(ctx context.Context, operation domain.Operation, mission domain.Mission) (Plan, error) {
	plan := Plan{TmuxSession: mission.TmuxSession}

	if mission.TmuxSession != "" {
		plan.SessionAlive = l.tmux.HasSession(ctx, mission.TmuxSession)
	}

	repos, err := reposByName(operation, mission)
	if err != nil {
		return plan, err
	}

	for _, work := range mission.Worktrees() {
		if !work.Created {
			continue
		}

		plan.Repos = append(plan.Repos, l.disposeOf(ctx, repos[work.RepoName], work))
	}

	for _, repo := range plan.Repos {
		switch repo.Action {
		case ActionNeedsForce:
			plan.NeedsForce = true
		case ActionKeepBranch:
			plan.KeptBranches = append(plan.KeptBranches, repo.Branch)
		}
	}

	return plan, nil
}

// disposeOf decides what should happen to one worktree.
func (l *Launcher) disposeOf(ctx context.Context, repo domain.Repo, work domain.RepoWork) RepoDisposition {
	out := RepoDisposition{
		Repo:         work.RepoName,
		WorktreePath: work.WorktreePath,
		Branch:       work.Branch,
	}

	if _, err := os.Stat(work.WorktreePath); err != nil {
		// Already gone, so there is nothing to remove and nothing to lose.
		out.Action = ActionDiscard

		return out
	}

	commonDir, _, err := l.resolveRepo(ctx, repo)
	if err != nil {
		out.Action = ActionUnavailable
		out.Reason = err.Error()

		return out
	}

	summary, err := l.git.TouchedSummary(ctx, work.WorktreePath, work.BaseSHA)
	if err != nil {
		out.Action = ActionUnavailable
		out.Reason = err.Error()

		return out
	}

	out.Dirty = summary.Dirty
	out.Ahead = summary.Ahead
	out.Pushed = work.Branch != "" && l.git.RefExists(ctx, commonDir, "refs/remotes/origin/"+work.Branch)

	switch {
	case summary.Dirty:
		// git refuses to remove a worktree with modified or untracked files, and that
		// refusal is the last thing standing between a keystroke and lost work.
		out.Action = ActionNeedsForce
	case summary.Ahead > 0 && !out.Pushed:
		// Commits that exist only here are worth keeping even though the worktree is
		// not; the branch costs nothing to leave behind.
		out.Action = ActionKeepBranch
	default:
		out.Action = ActionDiscard
	}

	return out
}

// Reclaim removes a mission's worktrees and branches.
//
// The tmux session is killed first: removing a worktree that is some process's working
// directory leaves git and the shell disagreeing about what exists.
//
// Without force, a worktree holding uncommitted work is refused and nothing about it is
// touched. Other repos are still reclaimed, and the refusal is reported, so one dirty
// worktree does not block the rest.
func (l *Launcher) Reclaim(
	ctx context.Context,
	operation domain.Operation,
	mission domain.Mission,
	force bool,
) (Report, error) {
	plan, err := l.PlanReclaim(ctx, operation, mission)
	if err != nil {
		return Report{}, err
	}

	if plan.NeedsForce && !force {
		return Report{}, fmt.Errorf("%w: %s", ErrNeedsForce, describeDirty(plan))
	}

	if mission.TmuxSession != "" {
		if err := l.tmux.KillSession(ctx, mission.TmuxSession); err != nil {
			return Report{}, err
		}
	}

	report, err := l.reclaimRepos(ctx, operation, mission, plan, force)
	if err != nil {
		return Report{}, err
	}

	// The mission directory holds the generated artifacts and the now-empty worktree
	// mount points, and is only q's to remove once the worktrees are gone.
	if len(report.Failures) == 0 && mission.MissionDir != "" {
		if err := os.RemoveAll(mission.MissionDir); err != nil {
			report.Failures = append(report.Failures, fmt.Sprintf("removing %s: %v", mission.MissionDir, err))
		}
	}

	return report, nil
}

// reclaimRepos removes each worktree and disposes of its branch.
func (l *Launcher) reclaimRepos(
	ctx context.Context,
	operation domain.Operation,
	mission domain.Mission,
	plan Plan,
	force bool,
) (Report, error) {
	var report Report

	repos, err := reposByName(operation, mission)
	if err != nil {
		return report, err
	}

	for _, disposition := range plan.Repos {
		if disposition.Action == ActionUnavailable {
			report.Failures = append(report.Failures,
				fmt.Sprintf("%s: %s", disposition.Repo, disposition.Reason))

			continue
		}

		commonDir, _, err := l.resolveRepo(ctx, repos[disposition.Repo])
		if err != nil {
			report.Failures = append(report.Failures, fmt.Sprintf("%s: %v", disposition.Repo, err))

			continue
		}

		unlock := l.git.Lock(commonDir)
		l.reclaimOne(ctx, commonDir, disposition, force, &report)
		unlock()
	}

	return report, nil
}

// reclaimOne removes one worktree and decides its branch's fate.
func (l *Launcher) reclaimOne(
	ctx context.Context,
	commonDir string,
	disposition RepoDisposition,
	force bool,
	report *Report,
) {
	if _, err := os.Stat(disposition.WorktreePath); err == nil {
		if err := l.git.WorktreeRemove(ctx, commonDir, disposition.WorktreePath, force); err != nil {
			report.Failures = append(report.Failures, fmt.Sprintf("%s: %v", disposition.Repo, err))

			return
		}
	}

	report.Removed = append(report.Removed, disposition.WorktreePath)

	if err := l.git.WorktreePrune(ctx, commonDir); err != nil {
		l.logger.Warn("pruning worktrees", "repo", commonDir, "error", err)
	}

	l.disposeBranch(ctx, commonDir, disposition, force, report)
}

// disposeBranch deletes a branch only when nothing would be lost with it.
func (l *Launcher) disposeBranch(
	ctx context.Context,
	commonDir string,
	disposition RepoDisposition,
	force bool,
	report *Report,
) {
	if disposition.Branch == "" {
		return
	}

	// A branch is kept whenever it holds work that is not somewhere else, and when
	// forcing past a dirty tree, since that is the case most likely to be regretted.
	if disposition.Action == ActionKeepBranch || (disposition.Dirty && force && disposition.Ahead > 0) {
		report.KeptBranches = append(report.KeptBranches, disposition.Branch)

		return
	}

	if err := l.git.DeleteBranch(ctx, commonDir, disposition.Branch, false); err != nil {
		// An unmerged branch is kept rather than forced away; git refusing here means
		// it holds something.
		report.KeptBranches = append(report.KeptBranches, disposition.Branch)

		return
	}

	report.DeletedBranches = append(report.DeletedBranches, disposition.Branch)
}

// reposByName indexes the repositories frozen for a mission's launch.
func reposByName(operation domain.Operation, mission domain.Mission) (map[string]domain.Repo, error) {
	repos, err := domain.MissionRepos(operation, mission)
	if err != nil {
		return nil, err
	}

	out := make(map[string]domain.Repo, len(repos))
	for _, repo := range repos {
		out[repo.Name] = repo
	}

	return out, nil
}

// describeDirty names the worktrees holding uncommitted work.
func describeDirty(plan Plan) string {
	var names []string

	for _, repo := range plan.Repos {
		if repo.Action == ActionNeedsForce {
			names = append(names, repo.Repo)
		}
	}

	if len(names) == 1 {
		return names[0]
	}

	return fmt.Sprintf("%d worktrees (%v)", len(names), names)
}
