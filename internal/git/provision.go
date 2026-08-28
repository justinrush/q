package git

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

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
)

// maxBranchAttempts bounds the search for an unused branch name.
const maxBranchAttempts = 20

// Sessions reports on and ends the terminal sessions a mission's worktrees are
// open in.
//
// A worktree cannot be removed while a session still holds it as a working
// directory, so reclaiming one has to be sequenced against the session that uses
// it. This is declared here rather than imported so the provisioner can be tested
// without a terminal.
type Sessions interface {
	HasSession(ctx context.Context, session string) bool
	KillSession(ctx context.Context, session string) error
}

// Provisioner creates and reclaims the git worktrees a mission works in.
//
// One worktree is created per repo, branched from a freshly fetched default
// branch, and journaled as it goes so an interrupted run can be resumed rather
// than restarted. Partial failure rolls back: a mission whose worktrees exist for
// two of three repos is worse than one that failed outright, because the agent
// would start, find the third repo missing, and improvise.
type Provisioner struct {
	dirs         paths.Dirs
	git          *Client
	sessions     Sessions
	logger       *slog.Logger
	branchPrefix string
	now          func() time.Time
}

// ProvisionerOption configures a Provisioner.
type ProvisionerOption func(*Provisioner)

// WithBranchPrefix namespaces created branches, e.g. "jane" gives
// "jane/add-endpoint".
func WithBranchPrefix(prefix string) ProvisionerOption {
	return func(p *Provisioner) {
		if prefix != "" {
			p.branchPrefix = prefix
		}
	}
}

// WithLogger sets where the provisioner reports problems it does not fail on.
func WithLogger(logger *slog.Logger) ProvisionerOption {
	return func(p *Provisioner) { p.logger = logger }
}

// WithClock replaces the time source, for tests.
func WithClock(now func() time.Time) ProvisionerOption {
	return func(p *Provisioner) { p.now = now }
}

// NewProvisioner returns a provisioner working under dirs.
func NewProvisioner(dirs paths.Dirs, client *Client, sessions Sessions, opts ...ProvisionerOption) *Provisioner {
	p := &Provisioner{
		dirs:         dirs,
		git:          client,
		sessions:     sessions,
		logger:       slog.Default(),
		branchPrefix: DefaultBranchPrefix(),
		now:          time.Now,
	}

	for _, opt := range opts {
		opt(p)
	}

	return p
}

// DefaultBranchPrefix derives a branch namespace from the environment, matching
// the common convention of prefixing branches with a username.
func DefaultBranchPrefix() string {
	for _, key := range []string{"USER", "LOGNAME"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}

	return "q"
}

// provision creates one worktree per repo in the operation.
func (p *Provisioner) provision(
	ctx context.Context,
	operation mission.Operation,
	ms mission.Mission,
	state *provisionState,
) (map[string]mission.RepoWork, map[string]mission.RepoWork, error) {
	work := make(map[string]mission.RepoWork, len(operation.Repos))
	created := make(map[string]mission.RepoWork)

	for _, repo := range operation.Repos {
		result, resumed, err := p.provisionRepo(ctx, repo, ms, state.Work[repo.Name])
		work[repo.Name] = result

		if err != nil {
			return work, created, fmt.Errorf("provisioning %s: %w", repo.Name, err)
		}

		if !resumed {
			created[repo.Name] = result
		}

		state.Work[repo.Name] = result
		err = writeProvisionState(ms.MissionDir, *state)
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
func (p *Provisioner) provisionRepo(
	ctx context.Context,
	repo mission.Repo,
	ms mission.Mission,
	saved mission.RepoWork,
) (mission.RepoWork, bool, error) {
	result := mission.RepoWork{
		RepoName:     repo.Name,
		WorktreePath: filepath.Join(ms.MissionDir, repo.Name),
	}

	commonDir, defaultBranch, err := p.resolveRepo(ctx, repo)
	if err != nil {
		result.Error = err.Error()

		return result, false, err
	}

	unlock := p.git.Lock(commonDir)
	defer unlock()

	err = p.git.FetchBranch(ctx, commonDir, defaultBranch)
	if err != nil {
		result.Error = err.Error()

		return result, false, err
	}

	baseRef := "refs/remotes/origin/" + defaultBranch

	baseSHA, err := p.git.RevParse(ctx, commonDir, baseRef)
	if err != nil {
		result.Error = err.Error()

		return result, false, err
	}

	result.BaseRef = baseRef
	result.BaseSHA = baseSHA

	existing, found, err := p.worktreeAt(ctx, commonDir, result.WorktreePath)
	if err != nil {
		result.Error = err.Error()

		return result, false, err
	}

	if found {
		resumed, err := p.resumeWorktree(ms, result, saved, existing)
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

	branch, err := p.pickBranch(ctx, commonDir, ms.Slug)
	if err != nil {
		result.Error = err.Error()

		return result, false, err
	}

	result.Branch = branch

	err = p.git.WorktreeAdd(ctx, commonDir, result.WorktreePath, branch, baseSHA)
	if err != nil {
		result.Error = err.Error()

		return result, false, err
	}

	result.Created = true

	return result, false, nil
}

func (p *Provisioner) worktreeAt(ctx context.Context, commonDir, path string) (Worktree, bool, error) {
	worktrees, err := p.git.WorktreeList(ctx, commonDir)
	if err != nil {
		return Worktree{}, false, err
	}

	cleanPath := filepath.Clean(path)
	for _, worktree := range worktrees {
		if filepath.Clean(worktree.Path) == cleanPath {
			return worktree, true, nil
		}
	}

	return Worktree{}, false, nil
}

func (p *Provisioner) resumeWorktree(
	ms mission.Mission,
	current mission.RepoWork,
	saved mission.RepoWork,
	existing Worktree,
) (mission.RepoWork, error) {
	if !missionBranch(existing.Branch, p.branchPrefix, ms.Slug) {
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
	base := mission.BranchName(branchPrefix, missionSlug)
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
func (p *Provisioner) resolveRepo(ctx context.Context, repo mission.Repo) (commonDir, defaultBranch string, err error) {
	commonDir = repo.CommonDir
	if commonDir == "" {
		if commonDir, err = p.git.CommonDir(ctx, repo.Path); err != nil {
			return "", "", err
		}
	}

	defaultBranch = repo.DefaultBranch
	if defaultBranch == "" {
		if defaultBranch, err = p.git.DefaultBranch(ctx, repo.Path); err != nil {
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
func (p *Provisioner) pickBranch(ctx context.Context, commonDir, missionSlug string) (string, error) {
	base := mission.BranchName(p.branchPrefix, missionSlug)

	for attempt := range maxBranchAttempts {
		candidate := base
		if attempt > 0 {
			candidate = fmt.Sprintf("%s-%d", base, attempt+1)
		}

		if !p.git.BranchExists(ctx, commonDir, candidate) {
			return candidate, nil
		}
	}

	return "", fmt.Errorf("no unused branch name near %s after %d attempts", base, maxBranchAttempts)
}

// rollback removes worktrees created by a failed launch.
func (p *Provisioner) rollback(ctx context.Context, operation mission.Operation, work map[string]mission.RepoWork) {
	byName := make(map[string]mission.Repo, len(operation.Repos))
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

		commonDir, _, err := p.resolveRepo(ctx, repo)
		if err != nil {
			p.logger.Warn("rolling back", "repo", name, "error", err)

			continue
		}

		unlock := p.git.Lock(commonDir)

		if err := p.git.WorktreeRemove(ctx, commonDir, created.WorktreePath, true); err != nil {
			p.logger.Warn("rolling back a worktree", "worktree", created.WorktreePath, "error", err)
		}

		if created.Branch != "" {
			if err := p.git.DeleteBranch(ctx, commonDir, created.Branch, true); err != nil {
				p.logger.Warn("rolling back a branch", "branch", created.Branch, "error", err)
			}
		}

		unlock()
	}
}

// worktreePaths returns the created worktree paths, in repo-name order.
func WorktreePaths(ms mission.Mission) []string {
	var paths []string

	for _, work := range ms.Worktrees() {
		if work.Created {
			paths = append(paths, work.WorktreePath)
		}
	}

	return paths
}

// Provisioned is the outcome of preparing a mission's worktrees.
type Provisioned struct {
	// Work is the journal of what each repo got: its worktree, branch, and base.
	Work map[string]mission.RepoWork

	provisioner *Provisioner
	operation   mission.Operation
	created     map[string]mission.RepoWork
}

// Rollback removes everything this provisioning created, leaving anything it
// resumed alone.
//
// It is the caller's to invoke, because a launch that fails after provisioning —
// while writing artifacts, or starting a session — must leave no half-provisioned
// worktrees behind either.
func (p *Provisioned) Rollback(ctx context.Context) {
	p.provisioner.rollback(ctx, p.operation, p.created)
}

// Prepare claims the mission directory and creates one worktree per repo.
//
// The mission is updated in place with the directory it was given. Provisioning
// is journaled as it goes, so an interrupted run resumes rather than restarting.
func (p *Provisioner) Prepare(
	ctx context.Context,
	operation mission.Operation,
	ms *mission.Mission,
) (*Provisioned, error) {
	state, err := p.prepareMissionDir(operation, ms)
	if err != nil {
		return nil, err
	}

	work, created, err := p.provision(ctx, operation, *ms, &state)
	out := &Provisioned{Work: work, provisioner: p, operation: operation, created: created}

	if err != nil {
		return out, err
	}

	return out, nil
}

// SessionExists reports whether a terminal session of the given name is already
// running.
func (p *Provisioner) SessionExists(ctx context.Context, session string) bool {
	return p.sessions.HasSession(ctx, session)
}
