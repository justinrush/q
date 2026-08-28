// Package launch provisions a mission's git worktrees and starts its agent.
//
// The sequence is: create one worktree per repo branched from a freshly fetched
// default branch, write the generated artifacts into the mission directory, then
// start the agent in a detached tmux session.
//
// Partial failure rolls back. A mission whose worktrees exist for two of three repos
// is worse than one that failed outright, because the agent would start, find the
// third repo missing, and improvise.
package launch

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/gadgets"
	"github.com/justinrush/q/internal/gitx"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/settings"
	"github.com/justinrush/q/internal/tmuxc"
)

// maxBranchAttempts bounds the search for an unused branch name.
const maxBranchAttempts = 20

// agentWindow is the name of the window the agent runs in.
const agentWindow = "agent"

// Config configures a Launcher.
type Config struct {
	Dirs   paths.Dirs
	Git    *gitx.Git
	Tmux   *tmuxc.Tmux
	Bins   *gadgets.Resolver
	Logger *slog.Logger
	// BranchPrefix namespaces created branches, e.g. "jarush". Empty means $USER.
	BranchPrefix string
	// Agents carries the user's per-agent configuration: extra arguments, the
	// codex profile name, and where codex keeps its configuration.
	Agents settings.Agents
	// Now supplies the current time, for tests.
	Now func() time.Time
}

// Launcher provisions and starts missions.
type Launcher struct {
	dirs         paths.Dirs
	git          *gitx.Git
	tmux         *tmuxc.Tmux
	bins         *gadgets.Resolver
	logger       *slog.Logger
	branchPrefix string
	agents       settings.Agents
	now          func() time.Time
}

// New returns a Launcher.
func New(cfg Config) *Launcher {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	prefix := cfg.BranchPrefix
	if prefix == "" {
		prefix = defaultBranchPrefix()
	}

	return &Launcher{
		dirs:         cfg.Dirs,
		git:          cfg.Git,
		tmux:         cfg.Tmux,
		bins:         cfg.Bins,
		logger:       logger,
		branchPrefix: prefix,
		agents:       cfg.Agents,
		now:          now,
	}
}

// defaultBranchPrefix derives a branch namespace from the environment, matching
// the user's existing convention of prefixing branches with their username.
func defaultBranchPrefix() string {
	for _, key := range []string{"USER", "LOGNAME"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}

	return "q"
}

// Launch provisions worktrees and starts the mission's agent.
//
// It returns the mission with its runtime fields populated. Persisting the result is
// the caller's responsibility, so that a launch failure and its rollback are a
// single state transition rather than several.
func (l *Launcher) Launch(ctx context.Context, operation domain.Operation, mission domain.Mission) (domain.Mission, error) {
	mission.HookEpoch++

	repos, err := domain.MissionRepos(operation, mission)
	if err != nil {
		return mission, err
	}

	mission.LaunchRepos = repos
	mission.LaunchReposFrozen = true
	operation.Repos = repos

	provisioned, err := l.prepareMissionDir(operation, &mission)
	if err != nil {
		return mission, err
	}

	// claude accepts a session id chosen in advance, so q picks one here and
	// the session is resumable from the instant it starts. codex generates its own
	// internally, so its id has to be learned from its SessionStart hook instead,
	// and stays empty until then.
	if mission.Tool.SupportsPresetSessionID() && mission.AgentSessionID == "" {
		sessionID, err := domain.NewSessionUUID()
		if err != nil {
			return mission, err
		}

		mission.AgentSessionID = sessionID
	}

	// Refuse to adopt an existing session: it belongs to something else, and
	// launching into it would put two agents in one window.
	if session := domain.TmuxSessionName(operation.Slug, mission.Slug, mission.ID); l.tmux.HasSession(ctx, session) {
		return mission, fmt.Errorf("tmux session %s already exists", session)
	}

	work, created, err := l.provision(ctx, operation, mission, &provisioned)
	if err != nil {
		l.rollback(ctx, operation, created)

		return mission, err
	}

	mission.Work = work

	if err := l.writeArtifacts(operation, mission); err != nil {
		l.rollback(ctx, operation, created)

		return mission, err
	}

	if err := l.startSession(ctx, operation, &mission); err != nil {
		l.rollback(ctx, operation, created)

		return mission, err
	}

	started := l.now()
	mission.StartedAt = &started
	mission.Status = domain.StatusActive
	mission.AgentState = domain.AgentUnknown
	mission.LaunchError = ""

	return mission, nil
}

// provision creates one worktree per repo in the operation.
func (l *Launcher) provision(
	ctx context.Context,
	operation domain.Operation,
	mission domain.Mission,
	state *provisionState,
) (map[string]domain.RepoWork, map[string]domain.RepoWork, error) {
	work := make(map[string]domain.RepoWork, len(operation.Repos))
	created := make(map[string]domain.RepoWork)

	for _, repo := range operation.Repos {
		result, resumed, err := l.provisionRepo(ctx, repo, mission, state.Work[repo.Name])
		work[repo.Name] = result

		if err != nil {
			return work, created, fmt.Errorf("provisioning %s: %w", repo.Name, err)
		}

		if !resumed {
			created[repo.Name] = result
		}

		state.Work[repo.Name] = result
		err = writeProvisionState(mission.MissionDir, *state)
		if err != nil {
			return work, created, fmt.Errorf("recording provisioned %s: %w", repo.Name, err)
		}
	}

	return work, created, nil
}

// provisionRepo creates the worktree for one repo.
//
// The whole sequence is held under the repository's lock: fetching and adding a
// worktree both take git's index and ref locks, and two missions provisioning from
// the same repo concurrently is the normal case.
func (l *Launcher) provisionRepo(
	ctx context.Context,
	repo domain.Repo,
	mission domain.Mission,
	saved domain.RepoWork,
) (domain.RepoWork, bool, error) {
	result := domain.RepoWork{
		RepoName:     repo.Name,
		WorktreePath: filepath.Join(mission.MissionDir, repo.Name),
	}

	commonDir, defaultBranch, err := l.resolveRepo(ctx, repo)
	if err != nil {
		result.Error = err.Error()

		return result, false, err
	}

	unlock := l.git.Lock(commonDir)
	defer unlock()

	err = l.git.FetchBranch(ctx, commonDir, defaultBranch)
	if err != nil {
		result.Error = err.Error()

		return result, false, err
	}

	baseRef := "refs/remotes/origin/" + defaultBranch

	baseSHA, err := l.git.RevParse(ctx, commonDir, baseRef)
	if err != nil {
		result.Error = err.Error()

		return result, false, err
	}

	result.BaseRef = baseRef
	result.BaseSHA = baseSHA

	existing, found, err := l.worktreeAt(ctx, commonDir, result.WorktreePath)
	if err != nil {
		result.Error = err.Error()

		return result, false, err
	}

	if found {
		resumed, err := l.resumeWorktree(mission, result, saved, existing)
		if err != nil {
			result.Error = err.Error()

			return result, false, err
		}

		return resumed, true, nil
	}

	_, statErr := os.Stat(result.WorktreePath)
	if statErr == nil {
		err = fmt.Errorf("%s exists but is not a registered git worktree", result.WorktreePath)
		result.Error = err.Error()

		return result, false, err
	}

	if !errors.Is(statErr, os.ErrNotExist) {
		err = fmt.Errorf("checking worktree path %s: %w", result.WorktreePath, statErr)
		result.Error = err.Error()

		return result, false, err
	}

	branch, err := l.pickBranch(ctx, commonDir, mission.Slug)
	if err != nil {
		result.Error = err.Error()

		return result, false, err
	}

	result.Branch = branch

	err = l.git.WorktreeAdd(ctx, commonDir, result.WorktreePath, branch, baseSHA)
	if err != nil {
		result.Error = err.Error()

		return result, false, err
	}

	result.Created = true

	return result, false, nil
}

func (l *Launcher) worktreeAt(ctx context.Context, commonDir, path string) (gitx.Worktree, bool, error) {
	worktrees, err := l.git.WorktreeList(ctx, commonDir)
	if err != nil {
		return gitx.Worktree{}, false, err
	}

	cleanPath := filepath.Clean(path)
	for _, worktree := range worktrees {
		if filepath.Clean(worktree.Path) == cleanPath {
			return worktree, true, nil
		}
	}

	return gitx.Worktree{}, false, nil
}

func (l *Launcher) resumeWorktree(
	mission domain.Mission,
	current domain.RepoWork,
	saved domain.RepoWork,
	existing gitx.Worktree,
) (domain.RepoWork, error) {
	if !missionBranch(existing.Branch, l.branchPrefix, mission.Slug) {
		return current, fmt.Errorf("worktree %s is on unexpected branch %s", existing.Path, existing.Branch)
	}

	if saved.Created {
		if filepath.Clean(saved.WorktreePath) != filepath.Clean(existing.Path) || saved.Branch != existing.Branch {
			return current, fmt.Errorf("worktree %s does not match its recorded provisioning state", existing.Path)
		}

		saved.Error = ""

		return saved, nil
	}

	// The ownership marker is written before any worktree is created, and the
	// agent starts only after all worktrees are journaled. An unjournaled
	// worktree can therefore only be from an interrupted git add; its HEAD is
	// the pinned branch point.
	current.BaseSHA = existing.Head
	current.Branch = existing.Branch
	current.Created = true

	return current, nil
}

func missionBranch(branch, branchPrefix, missionSlug string) bool {
	base := domain.BranchName(branchPrefix, missionSlug)
	if branch == base {
		return true
	}

	suffix, ok := strings.CutPrefix(branch, base+"-")
	if !ok {
		return false
	}

	number, err := strconv.Atoi(suffix)

	return err == nil && number >= 2
}

// resolveRepo fills in the repository details q needs, asking git for
// anything the operation does not already record.
func (l *Launcher) resolveRepo(ctx context.Context, repo domain.Repo) (commonDir, defaultBranch string, err error) {
	commonDir = repo.CommonDir
	if commonDir == "" {
		if commonDir, err = l.git.CommonDir(ctx, repo.Path); err != nil {
			return "", "", err
		}
	}

	defaultBranch = repo.DefaultBranch
	if defaultBranch == "" {
		if defaultBranch, err = l.git.DefaultBranch(ctx, repo.Path); err != nil {
			return "", "", err
		}
	}

	return commonDir, defaultBranch, nil
}

// pickBranch returns an unused branch name for the mission.
//
// Reusing a name would either fail or, worse, attach the mission to an unrelated
// branch left over from a previous attempt, so a numeric suffix is appended until
// a free name is found.
func (l *Launcher) pickBranch(ctx context.Context, commonDir, missionSlug string) (string, error) {
	base := domain.BranchName(l.branchPrefix, missionSlug)

	for attempt := range maxBranchAttempts {
		candidate := base
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%d", base, attempt+1)
		}

		if !l.git.BranchExists(ctx, commonDir, candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("no unused branch name near %s after %d attempts", base, maxBranchAttempts)
}

// rollback removes worktrees created by a failed launch.
func (l *Launcher) rollback(ctx context.Context, operation domain.Operation, work map[string]domain.RepoWork) {
	byName := make(map[string]domain.Repo, len(operation.Repos))
	for _, repo := range operation.Repos {
		byName[repo.Name] = repo
	}

	for name, created := range work {
		if !created.Created {
			continue
		}

		repo, ok := byName[name]
		if !ok {
			continue
		}

		commonDir, _, err := l.resolveRepo(ctx, repo)
		if err != nil {
			l.logger.Warn("rolling back", "repo", name, "error", err)

			continue
		}

		unlock := l.git.Lock(commonDir)

		if err := l.git.WorktreeRemove(ctx, commonDir, created.WorktreePath, true); err != nil {
			l.logger.Warn("rolling back a worktree", "worktree", created.WorktreePath, "error", err)
		}

		if created.Branch != "" {
			if err := l.git.DeleteBranch(ctx, commonDir, created.Branch, true); err != nil {
				l.logger.Warn("rolling back a branch", "branch", created.Branch, "error", err)
			}
		}

		unlock()
	}
}

// worktreePaths returns the created worktree paths, in repo-name order.
func worktreePaths(mission domain.Mission) []string {
	var paths []string

	for _, work := range mission.Worktrees() {
		if work.Created {
			paths = append(paths, work.WorktreePath)
		}
	}

	return paths
}

// errNoAgentBinary reports that the selected agent could not be found.
var errNoAgentBinary = errors.New("agent binary not found")
