// Package mission is q's model of the work it manages: the operations and
// missions themselves, the lanes they move through, the state machine that moves
// them, and the store that persists them.
//
// It depends on nothing but the standard library and internal/paths,
// deliberately. Everything an agent, a git checkout, or a terminal has to do for
// a mission is reached through an interface declared here — [Agent], [Runtime],
// [Healer], [Workspace] — so the rules stay testable without a terminal or a
// subprocess, and an implementation of any of them is a package that imports
// this one rather than a branch inside it.
package mission

import (
	"slices"
	"strings"
	"time"
)

// PaletteSize is the number of distinct operation colors available. The concrete
// colors live in the TUI layer; the domain only tracks which index an operation
// owns, so that assignment and recycling are testable without a terminal.
const PaletteSize = 12

// Operation is an area of investigation: a high-level summary plus the git repos it
// spans.
type Operation struct {
	ID      OperationID `json:"id"`
	Name    string      `json:"name"`
	Slug    string      `json:"slug"`
	Summary string      `json:"summary"`
	Repos   []Repo      `json:"repos"`

	// ColorIdx is this operation's slot in the display palette. It is assigned as
	// the lowest unused index rather than hashed from the ID, because hashing
	// collides and two adjacent operations sharing a stripe defeats the purpose of
	// coloring them at all.
	ColorIdx int `json:"colorIdx"`

	Archived  bool      `json:"archived"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// Repo is one of the user's existing git checkouts that an operation touches.
//
// q never modifies this checkout. It is only ever used as the source for
// `git worktree add`, which shares its object database.
type Repo struct {
	// Name is the leaf directory name, e.g. "azure-tf". It may differ from the
	// remote's name, so it is stored rather than derived.
	Name string `json:"name"`
	// Path is the absolute path to the user's checkout.
	Path string `json:"path"`
	// CommonDir is the resolved main .git directory. Path may itself be a
	// worktree, in which case git operations must target the common dir.
	CommonDir string `json:"commonDir"`
	// DefaultBranch is the resolved default branch, e.g. "main". It is stored
	// because it is not always "main" and resolving it costs a subprocess.
	DefaultBranch string `json:"defaultBranch"`
}

// Mission is one unit of agent work within an operation.
type Mission struct {
	ID          MissionID   `json:"id"`
	OperationID OperationID `json:"operationId"`
	Name        string      `json:"name"`
	Slug        string      `json:"slug"`
	Tool        Tool        `json:"tool"`
	Prompt      string      `json:"prompt"`
	PlanMode    bool        `json:"planMode"`
	// ExtraRepos are repositories this mission adds to the repositories inherited
	// from its operation. They are editable until the mission starts.
	ExtraRepos []Repo `json:"extraRepos,omitempty"`
	// LaunchRepos freezes the complete repository set used for a launch.
	LaunchRepos []Repo `json:"launchRepos,omitempty"`
	// LaunchReposFrozen distinguishes a repo-less launch from a draft whose
	// effective repositories have not been captured yet.
	LaunchReposFrozen bool   `json:"launchReposFrozen,omitempty"`
	Status            Status `json:"status"`
	// Order positions the card within its lane.
	Order int `json:"order"`

	// MissionDir is the agent's working directory, containing one worktree per repo
	// plus the generated .q artifacts.
	MissionDir string `json:"missionDir,omitempty"`
	// TmuxSession is the detached session hosting the agent.
	TmuxSession string `json:"tmuxSession,omitempty"`
	// AgentPaneID is the tmux pane id (e.g. "%13") running the agent. Pane ids
	// are captured at creation because pane indices cannot be computed: the
	// user's tmux config may set pane-base-index to 1.
	AgentPaneID string `json:"agentPaneId,omitempty"`
	// AgentSessionID is the agent's own session identifier, used to resume.
	// For claude, q chooses it before launch; for codex, it is learned
	// from the SessionStart hook and is empty until then.
	AgentSessionID string `json:"agentSessionId,omitempty"`
	// HookEpoch increments on every launch or relaunch. Hooks report the epoch
	// they were configured with, so events from a session q has already
	// abandoned can be discarded rather than moving a live card.
	HookEpoch int `json:"hookEpoch"`
	// Work records the worktree provisioned for each repo, keyed by repo name.
	Work map[string]RepoWork `json:"work,omitempty"`

	// AgentState is what the agent process is observably doing, as opposed to
	// the lane the card sits in.
	AgentState AgentState `json:"agentState"`
	// WaitingFor describes what the agent is blocked on, e.g. "Bash(rm -rf …)".
	WaitingFor string `json:"waitingFor,omitempty"`
	// PlanPending is true between an ExitPlanMode request and its resolution.
	// While set, no Stop event may move the card out of debrief, so a plan
	// awaiting approval cannot be silently downgraded to "finished".
	PlanPending bool `json:"planPending,omitempty"`
	// LastMessage is the agent's closing message from its last completed turn.
	LastMessage string `json:"lastMessage,omitempty"`
	// Badges carry nuance the five lanes cannot express.
	Badges []Badge `json:"badges,omitempty"`
	// LaunchError records why the last launch attempt failed.
	LaunchError string `json:"launchError,omitempty"`

	CreatedAt  time.Time  `json:"createdAt"`
	UpdatedAt  time.Time  `json:"updatedAt"`
	StartedAt  *time.Time `json:"startedAt,omitempty"`
	FinishedAt *time.Time `json:"finishedAt,omitempty"`
	// LastEventAt is when the agent last reported anything, used to detect a mission
	// that has gone quiet.
	LastEventAt time.Time `json:"lastEventAt,omitzero"`
	// StatusChangedAt is when the lane last changed.
	//
	// It exists so a lower-precedence proposal arriving moments after a
	// higher-precedence one can be recognized as part of the same racing burst
	// rather than as a genuine later development.
	StatusChangedAt time.Time `json:"statusChangedAt,omitzero"`
}

// RepoWork is the worktree q provisioned for one repo of one mission.
type RepoWork struct {
	RepoName     string `json:"repoName"`
	WorktreePath string `json:"worktreePath"`
	Branch       string `json:"branch"`
	// BaseRef is the ref the branch was cut from, e.g.
	// "refs/remotes/origin/main".
	BaseRef string `json:"baseRef"`
	// BaseSHA pins the exact commit the branch started at.
	//
	// Diffs are computed against this SHA rather than against origin/main,
	// because the user's global git config sets fetch.force, so any unrelated
	// fetch in another terminal can move origin/main and silently pull other
	// people's commits into what q reports as "what this mission changed".
	BaseSHA string `json:"baseSha"`
	// DebriefPaneID is the tmux pane running an editor on this worktree, if one
	// has been opened.
	DebriefPaneID string `json:"debriefPaneId,omitempty"`
	Created       bool   `json:"created"`
	Error         string `json:"error,omitempty"`
}

// Badge is a short marker rendered on a card to convey state the lanes cannot.
type Badge struct {
	// Kind is a stable identifier, e.g. "stale", "tmux-gone", "hooks-silent".
	Kind string `json:"kind"`
	// Detail is optional extra text, e.g. the API failure reason.
	Detail string `json:"detail,omitempty"`
}

// Known badge kinds.
const (
	// BadgeStale marks a mission whose agent has been quiet for an unexpectedly
	// long time.
	BadgeStale = "stale"
	// BadgeTmuxGone marks a mission whose tmux session or pane has died.
	BadgeTmuxGone = "tmux-gone"
	// BadgeHooksSilent marks a launched mission whose SessionStart hook never
	// arrived, which means the status feedback wiring is broken. Surfacing this
	// matters more than it seems: the alternative is a card that quietly stops
	// telling the truth.
	BadgeHooksSilent = "hooks-silent"
	// BadgeBackground marks a mission paused on background work rather than done.
	BadgeBackground = "bg"
	// BadgeAPIError marks a turn that ended in an API failure.
	BadgeAPIError = "api"
	// BadgeLaunching marks a mission whose worktrees are still being provisioned.
	BadgeLaunching = "launching"
	// BadgeCompacting marks a mission compacting its context.
	BadgeCompacting = "compacting"
	// BadgeEnded marks a mission whose agent session exited.
	BadgeEnded = "ended"
	// BadgeRepoMissing marks a mission whose source checkout has disappeared.
	BadgeRepoMissing = "repo-missing"
)

// HasBadge reports whether the mission carries a badge of the given kind.
func (t Mission) HasBadge(kind string) bool {
	for _, b := range t.Badges {
		if b.Kind == kind {
			return true
		}
	}

	return false
}

// WithBadge returns the badge list with kind set to detail, replacing any
// existing badge of that kind. The receiver is not modified.
func (t Mission) WithBadge(kind, detail string) []Badge {
	out := make([]Badge, 0, len(t.Badges)+1)

	for _, b := range t.Badges {
		if b.Kind != kind {
			out = append(out, b)
		}
	}

	return append(out, Badge{Kind: kind, Detail: detail})
}

// WithoutBadge returns the badge list with every badge of the given kind
// removed. The receiver is not modified.
func (t Mission) WithoutBadge(kind string) []Badge {
	if !t.HasBadge(kind) {
		return t.Badges
	}

	out := make([]Badge, 0, len(t.Badges))

	for _, b := range t.Badges {
		if b.Kind != kind {
			out = append(out, b)
		}
	}

	return out
}

// Launched reports whether the mission has ever been started.
func (t Mission) Launched() bool { return t.StartedAt != nil }

// Resumable reports whether q knows enough to resume the agent session
// after its tmux session has gone away.
func (t Mission) Resumable() bool { return t.AgentSessionID != "" }

// Worktrees returns the mission's per-repo work, sorted by repo name.
//
// Work is stored as a map for lookup by repo name, so callers that render or
// iterate need a deterministic order; without one, generated prompts and debrief
// pane layouts would shuffle between runs.
func (t Mission) Worktrees() []RepoWork {
	out := make([]RepoWork, 0, len(t.Work))
	for _, w := range t.Work {
		out = append(out, w)
	}

	slices.SortFunc(out, func(a, b RepoWork) int {
		return strings.Compare(a.RepoName, b.RepoName)
	})

	return out
}
