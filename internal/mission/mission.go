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
	ID      OperationID `json:"id" q:"key"`
	Name    string      `json:"name" q:"spec"`
	Slug    string      `json:"slug" q:"spec"`
	Summary string      `json:"summary" q:"spec"`
	Repos   []Repo      `json:"repos" q:"spec"`

	// ColorIdx is this operation's slot in the display palette. It is assigned as
	// the lowest unused index rather than hashed from the ID, because hashing
	// collides and two adjacent operations sharing a stripe defeats the purpose of
	// coloring them at all.
	ColorIdx int `json:"colorIdx" q:"spec"`

	Archived  bool      `json:"archived" q:"spec"`
	CreatedAt time.Time `json:"createdAt" q:"spec"`
	UpdatedAt time.Time `json:"updatedAt" q:"spec"`

	// Rev counts the edits made to this operation, on whichever host made them.
	//
	// It is how two paired hosts decide whose copy is newer without comparing
	// clocks: the higher revision wins and the primary wins a tie. See
	// [Mission.SpecRev], which does the same job for a mission's brief.
	Rev int `json:"rev,omitempty" q:"meta"`
}

// Repo is one of the user's existing git checkouts that an operation touches.
//
// q never modifies this checkout. It is only ever used as the source for
// `git worktree add`, which shares its object database.
type Repo struct {
	// Name is the leaf directory name, e.g. "azure-tf". It may differ from the
	// remote's name, so it is stored rather than derived.
	Name string `json:"name" q:"spec"`
	// Path is the absolute path to the user's checkout.
	Path string `json:"path" q:"local"`
	// CommonDir is the resolved main .git directory. Path may itself be a
	// worktree, in which case git operations must target the common dir.
	CommonDir string `json:"commonDir" q:"local"`
	// DefaultBranch is the resolved default branch, e.g. "main". It is stored
	// because it is not always "main" and resolving it costs a subprocess.
	DefaultBranch string `json:"defaultBranch" q:"spec"`
	// URL is the repository's origin, normalized, e.g.
	// "gitlab.com/justinarush/mac".
	//
	// It is what identifies a repository across machines. Name is only the leaf
	// directory and Path is only true on one host, so without this a paired host
	// has no way to tell which of its own checkouts an operation means. Empty
	// until q has asked git, and for a repository with no origin.
	URL string `json:"url,omitempty" q:"spec"`
}

// Mission is one unit of agent work within an operation.
type Mission struct {
	ID          MissionID   `json:"id" q:"key"`
	OperationID OperationID `json:"operationId" q:"spec"`
	Name        string      `json:"name" q:"spec"`
	Slug        string      `json:"slug" q:"spec"`
	Tool        Tool        `json:"tool" q:"spec"`
	Prompt      string      `json:"prompt" q:"spec"`
	PlanMode    bool        `json:"planMode" q:"spec"`
	// Model is the agent model this mission runs on, named as that agent's own
	// CLI names it. Empty means q passes no model flag and the agent uses
	// whatever it would have used anyway, which is what a mission created before
	// model selection existed still does.
	Model string `json:"model,omitempty" q:"spec"`
	// Effort is the reasoning effort level, for a model that accepts one. Empty
	// means q passes no effort flag.
	Effort string `json:"effort,omitempty" q:"spec"`
	// ExtraRepos are repositories this mission adds to the repositories inherited
	// from its operation. They are editable until the mission starts.
	ExtraRepos []Repo `json:"extraRepos,omitempty" q:"spec"`
	// BaseBranches overrides the branch a repo's worktree is based on, keyed by
	// repo name. It covers the operation's repos as well as this mission's own,
	// which is why it lives here rather than on Repo: the operation owns its
	// repositories and a mission must not edit them to choose its own base.
	//
	// An absent or empty entry means the repo's default branch, so a mission that
	// never touched it provisions exactly as it did before this existed.
	BaseBranches map[string]string `json:"baseBranches,omitempty" q:"spec"`
	// Queued asks q to start this mission on its own once a slot is free, rather
	// than waiting for a human to move it to active.
	//
	// It is cleared the moment a launch is attempted, successful or not, so a
	// mission that cannot start reports why once instead of retrying forever.
	Queued bool `json:"queued,omitempty" q:"spec"`
	// Pin names the one host this mission must run on. Empty means q decides:
	// the primary when it is around, the secondary when it is not.
	Pin HostID `json:"pin,omitempty" q:"spec"`
	// LaunchRepos freezes the complete repository set used for a launch.
	LaunchRepos []Repo `json:"launchRepos,omitempty" q:"run"`
	// LaunchReposFrozen distinguishes a repo-less launch from a draft whose
	// effective repositories have not been captured yet.
	LaunchReposFrozen bool   `json:"launchReposFrozen,omitempty" q:"run"`
	Status            Status `json:"status" q:"run"`
	// Order positions the card within its lane.
	Order int `json:"order" q:"run"`

	// MissionDir is the agent's working directory, containing one worktree per repo
	// plus the generated .q artifacts.
	MissionDir string `json:"missionDir,omitempty" q:"local"`
	// TmuxSession is the detached session hosting the agent.
	TmuxSession string `json:"tmuxSession,omitempty" q:"local"`
	// AgentPaneID is the tmux pane id (e.g. "%13") running the agent. Pane ids
	// are captured at creation because pane indices cannot be computed: the
	// user's tmux config may set pane-base-index to 1.
	AgentPaneID string `json:"agentPaneId,omitempty" q:"local"`
	// AgentSessionID is the agent's own session identifier, used to resume.
	// For claude, q chooses it before launch; for codex, it is learned
	// from the SessionStart hook and is empty until then.
	AgentSessionID string `json:"agentSessionId,omitempty" q:"local"`
	// HookEpoch increments on every launch or relaunch. Hooks report the epoch
	// they were configured with, so events from a session q has already
	// abandoned can be discarded rather than moving a live card.
	HookEpoch int `json:"hookEpoch" q:"local"`
	// Work records the worktree provisioned for each repo, keyed by repo name.
	Work map[string]RepoWork `json:"work,omitempty" q:"run"`

	// AgentState is what the agent process is observably doing, as opposed to
	// the lane the card sits in.
	AgentState AgentState `json:"agentState" q:"run"`
	// WaitingFor describes what the agent is blocked on, e.g. "Bash(rm -rf …)".
	WaitingFor string `json:"waitingFor,omitempty" q:"run"`
	// PlanPending is true between an ExitPlanMode request and its resolution.
	// While set, no Stop event may move the card out of debrief, so a plan
	// awaiting approval cannot be silently downgraded to "finished".
	PlanPending bool `json:"planPending,omitempty" q:"run"`
	// LastMessage is the agent's closing message from its last completed turn.
	LastMessage string `json:"lastMessage,omitempty" q:"run"`
	// Badges carry nuance the five lanes cannot express.
	Badges []Badge `json:"badges,omitempty" q:"run"`
	// LaunchError records why the last launch attempt failed.
	LaunchError string `json:"launchError,omitempty" q:"run"`
	// TranscriptPath is the agent's own conversation log, learned from any hook
	// event that carries one.
	//
	// It is stored rather than used and discarded because metering runs on a
	// timer as well as on a hook: a turn that takes ten minutes should show a
	// growing total, and the reconciler has no hook payload to read the path out
	// of. Empty for an agent that keeps no transcript.
	TranscriptPath string `json:"transcriptPath,omitempty" q:"local"`
	// Usage is what the session has consumed, as last measured by a [Meter].
	Usage Usage `json:"usage,omitzero" q:"run"`

	CreatedAt  time.Time  `json:"createdAt" q:"spec"`
	UpdatedAt  time.Time  `json:"updatedAt" q:"run"`
	StartedAt  *time.Time `json:"startedAt,omitempty" q:"run"`
	FinishedAt *time.Time `json:"finishedAt,omitempty" q:"run"`
	// LastEventAt is when the agent last reported anything, used to detect a mission
	// that has gone quiet.
	LastEventAt time.Time `json:"lastEventAt,omitzero" q:"run"`
	// StatusChangedAt is when the lane last changed.
	//
	// It exists so a lower-precedence proposal arriving moments after a
	// higher-precedence one can be recognized as part of the same racing burst
	// rather than as a genuine later development.
	StatusChangedAt time.Time `json:"statusChangedAt,omitzero" q:"run"`

	// Lease says which host runs this mission. See [Lease].
	Lease Lease `json:"lease,omitzero" q:"meta"`
	// SpecRev counts the edits made to the brief: the fields a human writes, as
	// opposed to the ones a running agent produces. The higher revision wins
	// when two hosts disagree, and the primary wins a tie.
	SpecRev int `json:"specRev,omitempty" q:"meta"`
	// HolderSession is the tmux session the lease holder runs the agent in.
	//
	// TmuxSession is this host's own session and is never sent. This is the same
	// name as the holder publishes it, which is what lets the other host attach
	// to an agent it is not running.
	HolderSession string `json:"holderSession,omitempty" q:"run"`
	// TurnEnded reports that the agent's last turn is over and it is sitting at
	// its prompt, as opposed to being stopped part-way through one.
	//
	// The lane alone cannot say this. A mission is awaiting orders both when the
	// agent asked a question and stopped, and when it is blocked on a permission
	// prompt in the middle of a tool call. Moving a mission between hosts is safe
	// in the first case and loses the turn in the second.
	TurnEnded bool `json:"turnEnded,omitempty" q:"run"`
	// UsageByHost is what the mission has consumed on each host that has run it.
	// Usage is their sum, and is what a card shows.
	//
	// A meter reads the agent's own transcript, which lives on the host the agent
	// ran on. A mission that moves therefore has a transcript on each, and
	// keeping the readings apart is what stops the total from resetting, or
	// doubling, every time the lease changes hands.
	UsageByHost map[HostID]Usage `json:"usageByHost,omitempty" q:"run"`
	// MovedFrom names the host that was running this mission when it came to
	// this one, until an agent has been started here. It is how the agent is told
	// that the conversation it is resuming, or starting, is missing part of the
	// story.
	MovedFrom HostID `json:"movedFrom,omitempty" q:"local"`
	// LocalBadges are badges this host raised about its own copy of the mission,
	// such as edits in a mirror worktree that have not been merged. They are kept
	// apart from Badges because those belong to the lease holder and are
	// replaced wholesale whenever its state arrives.
	LocalBadges []Badge `json:"localBadges,omitempty" q:"local"`
}

// RepoWork is the worktree q provisioned for one repo of one mission.
type RepoWork struct {
	RepoName     string `json:"repoName" q:"key"`
	WorktreePath string `json:"worktreePath" q:"local"`
	Branch       string `json:"branch" q:"local"`
	// BaseRef is the ref the branch was cut from, e.g.
	// "refs/remotes/origin/main".
	BaseRef string `json:"baseRef" q:"run"`
	// BaseSHA pins the exact commit the branch started at.
	//
	// Diffs are computed against this SHA rather than against origin/main,
	// because the user's global git config sets fetch.force, so any unrelated
	// fetch in another terminal can move origin/main and silently pull other
	// people's commits into what q reports as "what this mission changed".
	BaseSHA string `json:"baseSha" q:"run"`
	// DebriefPaneID is the tmux pane running an editor on this worktree, if one
	// has been opened.
	DebriefPaneID string `json:"debriefPaneId,omitempty" q:"local"`
	Created       bool   `json:"created" q:"local"`
	Error         string `json:"error,omitempty" q:"local"`
	// SyncBase is the last snapshot of this worktree that both paired hosts
	// agreed on. It is the merge base when both sides have since changed, and
	// what tells "the peer moved on" apart from "I did".
	SyncBase string `json:"syncBase,omitempty" q:"local"`
	// SyncIncludes is a snapshot of the peer's that this worktree has had merged
	// into it, or been ruled to supersede. It is set when both hosts changed a
	// worktree and this one resolved it, and is what tells the peer that taking
	// this host's state loses nothing of its own.
	SyncIncludes string `json:"syncIncludes,omitempty" q:"local"`
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
	// BadgeQueued marks a briefed mission q will start on its own.
	BadgeQueued = "queued"
	// BadgePending marks a mission with a request waiting to reach its peer.
	BadgePending = "pending"
	// BadgeLocalEdits marks a mirror worktree edited here while the agent runs
	// on the other host. The edits are safe and are merged when the turn ends.
	BadgeLocalEdits = "local-edits"
	// BadgeDiverged marks a mission whose two worktrees changed the same lines.
	// The primary's version is on the branch and the other is kept beside it.
	BadgeDiverged = "diverged"
	// BadgeHandoff marks a mission that finished a turn on one host and was
	// passed to the other, where its next turn will start. Its detail names
	// the host it came from.
	BadgeHandoff = "handoff"
	// BadgeUnconfirmed marks a mission this host believes it is running but has
	// not been able to confirm with its peer for long enough that the peer may
	// have taken it over.
	BadgeUnconfirmed = "unconfirmed"
	// BadgeStandby marks a mission whose agent this host stopped because it
	// could reach neither its peer nor the witness, and so could not tell
	// whether the peer had taken the mission over. The agent is started again
	// when one of them answers and the mission is still this host's.
	BadgeStandby = "standby"
)

// AllBadges returns the holder's badges followed by this host's own.
func (t Mission) AllBadges() []Badge {
	if len(t.LocalBadges) == 0 {
		return t.Badges
	}

	return append(slices.Clone(t.Badges), t.LocalBadges...)
}

// WithLocalBadge returns the host-local badge list with kind set to detail.
// The receiver is not modified.
func (t Mission) WithLocalBadge(kind, detail string) []Badge {
	out := make([]Badge, 0, len(t.LocalBadges)+1)

	for _, b := range t.LocalBadges {
		if b.Kind != kind {
			out = append(out, b)
		}
	}

	return append(out, Badge{Kind: kind, Detail: detail})
}

// WithoutLocalBadge returns the host-local badge list without kind. The
// receiver is not modified.
func (t Mission) WithoutLocalBadge(kind string) []Badge {
	var out []Badge

	for _, b := range t.LocalBadges {
		if b.Kind != kind {
			out = append(out, b)
		}
	}

	return out
}

// HasLocalBadge reports whether this host raised a badge of the given kind.
func (t Mission) HasLocalBadge(kind string) bool {
	for _, b := range t.LocalBadges {
		if b.Kind == kind {
			return true
		}
	}

	return false
}

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

// Running reports whether an agent may be at work on the mission: it has been
// launched and has not been filed. It says nothing about which host.
func (t Mission) Running() bool { return t.Launched() && !t.Status.Terminal() }

// Resumable reports whether q knows enough to resume the agent session
// after its tmux session has gone away.
func (t Mission) Resumable() bool { return t.AgentSessionID != "" }

// BaseBranch returns the branch repoName's worktree should be based on, falling
// back to the repository's own default branch when the mission names none.
func (t Mission) BaseBranch(repoName, defaultBranch string) string {
	if override := strings.TrimSpace(t.BaseBranches[repoName]); override != "" {
		return override
	}

	return defaultBranch
}

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
