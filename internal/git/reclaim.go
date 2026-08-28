package git

import (
	"context"
	"fmt"
	"os"

	"github.com/justinrush/q/internal/mission"
)

// PlanReclaim reports what deleting a mission would do, without doing any of it.
//
// This exists so a confirmation can name what is about to be lost. Deleting a mission can
// throw away hours of an agent's uncommitted work, and a dialog that only says "are you
// sure" gives the human nothing to be sure about.
func (p *Provisioner) PlanReclaim(ctx context.Context, operation mission.Operation, ms mission.Mission) (mission.Plan, error) {
	plan := mission.Plan{TmuxSession: ms.TmuxSession}

	if ms.TmuxSession != "" {
		plan.SessionAlive = p.sessions.HasSession(ctx, ms.TmuxSession)
	}

	repos, err := reposByName(operation, ms)
	if err != nil {
		return plan, err
	}

	for _, work := range ms.Worktrees() {
		if !work.Created {
			continue
		}

		plan.Repos = append(plan.Repos, p.disposeOf(ctx, repos[work.RepoName], work))
	}

	for _, repo := range plan.Repos {
		switch repo.Action {
		case mission.ActionNeedsForce:
			plan.NeedsForce = true
		case mission.ActionKeepBranch:
			plan.KeptBranches = append(plan.KeptBranches, repo.Branch)
		}
	}

	return plan, nil
}

// disposeOf decides what should happen to one worktree.
func (p *Provisioner) disposeOf(ctx context.Context, repo mission.Repo, work mission.RepoWork) mission.RepoDisposition {
	out := mission.RepoDisposition{
		Repo:         work.RepoName,
		WorktreePath: work.WorktreePath,
		Branch:       work.Branch,
	}

	if _, err := os.Stat(work.WorktreePath); err != nil {
		// Already gone, so there is nothing to remove and nothing to lose.
		out.Action = mission.ActionDiscard

		return out
	}

	commonDir, _, err := p.resolveRepo(ctx, repo)
	if err != nil {
		out.Action = mission.ActionUnavailable
		out.Reason = err.Error()

		return out
	}

	summary, err := p.git.TouchedSummary(ctx, work.WorktreePath, work.BaseSHA)
	if err != nil {
		out.Action = mission.ActionUnavailable
		out.Reason = err.Error()

		return out
	}

	out.Dirty = summary.Dirty
	out.Ahead = summary.Ahead
	out.Pushed = work.Branch != "" && p.git.RefExists(ctx, commonDir, "refs/remotes/origin/"+work.Branch)

	switch {
	case summary.Dirty:
		// git refuses to remove a worktree with modified or untracked files, and that
		// refusal is the last thing standing between a keystroke and lost work.
		out.Action = mission.ActionNeedsForce
	case summary.Ahead > 0 && !out.Pushed:
		// Commits that exist only here are worth keeping even though the worktree is
		// not; the branch costs nothing to leave behind.
		out.Action = mission.ActionKeepBranch
	default:
		out.Action = mission.ActionDiscard
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
func (p *Provisioner) Reclaim(
	ctx context.Context,
	operation mission.Operation,
	ms mission.Mission,
	force bool,
) (mission.Report, error) {
	plan, err := p.PlanReclaim(ctx, operation, ms)
	if err != nil {
		return mission.Report{}, err
	}

	if plan.NeedsForce && !force {
		return mission.Report{}, fmt.Errorf("%w: %s", mission.ErrNeedsForce, describeDirty(plan))
	}

	if ms.TmuxSession != "" {
		if err := p.sessions.KillSession(ctx, ms.TmuxSession); err != nil {
			return mission.Report{}, err
		}
	}

	report, err := p.reclaimRepos(ctx, operation, ms, plan, force)
	if err != nil {
		return mission.Report{}, err
	}

	// The mission directory holds the generated artifacts and the now-empty worktree
	// mount points, and is only q's to remove once the worktrees are gone.
	if len(report.Failures) == 0 && ms.MissionDir != "" {
		if err := os.RemoveAll(ms.MissionDir); err != nil {
			report.Failures = append(report.Failures, fmt.Sprintf("removing %s: %v", ms.MissionDir, err))
		}
	}

	return report, nil
}

// reclaimRepos removes each worktree and disposes of its branch.
func (p *Provisioner) reclaimRepos(
	ctx context.Context,
	operation mission.Operation,
	ms mission.Mission,
	plan mission.Plan,
	force bool,
) (mission.Report, error) {
	var report mission.Report

	repos, err := reposByName(operation, ms)
	if err != nil {
		return report, err
	}

	for _, disposition := range plan.Repos {
		if disposition.Action == mission.ActionUnavailable {
			report.Failures = append(report.Failures,
				fmt.Sprintf("%s: %s", disposition.Repo, disposition.Reason))

			continue
		}

		commonDir, _, err := p.resolveRepo(ctx, repos[disposition.Repo])
		if err != nil {
			report.Failures = append(report.Failures, fmt.Sprintf("%s: %v", disposition.Repo, err))

			continue
		}

		unlock := p.git.Lock(commonDir)
		p.reclaimOne(ctx, commonDir, disposition, force, &report)
		unlock()
	}

	return report, nil
}

// reclaimOne removes one worktree and decides its branch's fate.
func (p *Provisioner) reclaimOne(
	ctx context.Context,
	commonDir string,
	disposition mission.RepoDisposition,
	force bool,
	report *mission.Report,
) {
	if _, err := os.Stat(disposition.WorktreePath); err == nil {
		if err := p.git.WorktreeRemove(ctx, commonDir, disposition.WorktreePath, force); err != nil {
			report.Failures = append(report.Failures, fmt.Sprintf("%s: %v", disposition.Repo, err))

			return
		}
	}

	report.Removed = append(report.Removed, disposition.WorktreePath)

	if err := p.git.WorktreePrune(ctx, commonDir); err != nil {
		p.logger.Warn("pruning worktrees", "repo", commonDir, "error", err)
	}

	p.disposeBranch(ctx, commonDir, disposition, force, report)
}

// disposeBranch deletes a branch only when nothing would be lost with it.
func (p *Provisioner) disposeBranch(
	ctx context.Context,
	commonDir string,
	disposition mission.RepoDisposition,
	force bool,
	report *mission.Report,
) {
	if disposition.Branch == "" {
		return
	}

	// A branch is kept whenever it holds work that is not somewhere else, and when
	// forcing past a dirty tree, since that is the case most likely to be regretted.
	if disposition.Action == mission.ActionKeepBranch || (disposition.Dirty && force && disposition.Ahead > 0) {
		report.KeptBranches = append(report.KeptBranches, disposition.Branch)

		return
	}

	if err := p.git.DeleteBranch(ctx, commonDir, disposition.Branch, false); err != nil {
		// An unmerged branch is kept rather than forced away; git refusing here means
		// it holds something.
		report.KeptBranches = append(report.KeptBranches, disposition.Branch)

		return
	}

	report.DeletedBranches = append(report.DeletedBranches, disposition.Branch)
}

// reposByName indexes the repositories frozen for a mission's launch.
func reposByName(operation mission.Operation, ms mission.Mission) (map[string]mission.Repo, error) {
	repos, err := mission.MissionRepos(operation, ms)
	if err != nil {
		return nil, err
	}

	out := make(map[string]mission.Repo, len(repos))
	for _, repo := range repos {
		out[repo.Name] = repo
	}

	return out, nil
}

// describeDirty names the worktrees holding uncommitted work.
func describeDirty(plan mission.Plan) string {
	var names []string

	for _, repo := range plan.Repos {
		if repo.Action == mission.ActionNeedsForce {
			names = append(names, repo.Repo)
		}
	}

	if len(names) == 1 {
		return names[0]
	}

	return fmt.Sprintf("%d worktrees (%v)", len(names), names)
}
