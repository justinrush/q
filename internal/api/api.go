// Package api is q's daemon protocol: the request and response types, the
// rendezvous handle that says where a daemon is listening, and the client that
// speaks to one.
//
// Both ends of the protocol live here so an endpoint's request type, the client
// method that sends it, and the handler that answers it stay in sight of one
// another. The server half lives with the service it exposes, in
// internal/daemon, because it is that service's transport rather than a thing
// of its own.
package api

import (
	"encoding/json"
	"time"

	"github.com/justinrush/q/internal/mission"
)

// ClientHeader must be present on every request.
//
// It is a cheap cross-site guard: a browser cannot set a custom header on a
// cross-origin request without a preflight, so a page the user happens to have
// open cannot drive the daemon even though it listens on localhost.
const ClientHeader = "X-Q-Client"

// ClientHeaderValue is the expected value of [ClientHeader].
const ClientHeaderValue = "1"

// Health describes a running daemon.
type Health struct {
	Version   string    `json:"version"`
	PID       int       `json:"pid"`
	StartedAt time.Time `json:"startedAt"`
	Uptime    string    `json:"uptime"`
	// Binary is the executable the daemon is running.
	//
	// It is reported because a daemon keeps serving with whatever it started with,
	// and the path it embeds in generated hook commands is the one that must still
	// exist for running sessions to report status. A rebuilt or moved binary is
	// otherwise invisible.
	Binary     string `json:"binary"`
	Operations int    `json:"operations"`
	Missions   int    `json:"missions"`
	// Subscribers is the number of connected event streams, i.e. open boards.
	Subscribers int `json:"subscribers"`
}

// Error is the body returned with any non-2xx response.
type Error struct {
	Error string `json:"error"`
}

// CreateOperationRequest creates an operation.
type CreateOperationRequest struct {
	Name    string         `json:"name"`
	Summary string         `json:"summary"`
	Repos   []mission.Repo `json:"repos,omitempty"`
}

// UpdateOperationRequest patches an operation. Nil fields are left unchanged, which is
// what lets the TUI send only what the user edited.
type UpdateOperationRequest struct {
	Name     *string         `json:"name,omitempty"`
	Summary  *string         `json:"summary,omitempty"`
	Repos    *[]mission.Repo `json:"repos,omitempty"`
	ColorIdx *int            `json:"colorIdx,omitempty"`
	Archived *bool           `json:"archived,omitempty"`
}

// CreateMissionRequest creates a mission. New missions always land in the draft lane;
// launching is a separate, explicit action.
type CreateMissionRequest struct {
	// OperationID is the operation the mission belongs to. It is required unless
	// InheritFrom names a mission that already has one.
	OperationID mission.OperationID `json:"operationId"`
	Name        string              `json:"name"`
	Prompt      string              `json:"prompt"`
	Tool        mission.Tool        `json:"tool"`
	PlanMode    bool                `json:"planMode"`
	// Model and Effort name the agent model to run on and how hard it should
	// think. Both are empty when the caller has no opinion, in which case q emits
	// no flag and the agent uses its own default.
	Model      string         `json:"model,omitempty"`
	Effort     string         `json:"effort,omitempty"`
	ExtraRepos []mission.Repo `json:"extraRepos,omitempty"`
	// BaseBranches names the branch each repo's worktree is based on, keyed by
	// repo name. Repos it does not mention use their own default branch.
	BaseBranches map[string]string `json:"baseBranches,omitempty"`
	// InheritFrom names a mission whose setup this one copies: its operation,
	// agent, model, effort, repositories, and base branches.
	//
	// It exists so that a mission which designs follow-up work can queue it
	// without restating, and eventually getting subtly wrong, the context it is
	// already running in. Any field the caller sends wins, so it is a default
	// rather than an override. In particular it is what makes OperationID
	// optional.
	//
	// PlanMode is deliberately not among the inherited fields. A bool has no
	// unset state on the wire, so an inherited value could never be switched back
	// off, and whether a mission should stop for approval is a decision for
	// whoever launches it.
	InheritFrom mission.MissionID `json:"inheritFrom,omitempty"`
	// Queued asks the daemon to start the mission on its own once a slot is
	// free. Without it a new mission waits in briefing for a human, as before.
	Queued bool `json:"queued,omitempty"`
	// Pin names the host the mission must run on: "local", "remote", or a
	// host's name or id. Empty leaves the choice to q.
	Pin string `json:"pin,omitempty"`
}

// UpdateMissionRequest patches a mission. Nil fields are left unchanged.
type UpdateMissionRequest struct {
	Name        *string              `json:"name,omitempty"`
	Prompt      *string              `json:"prompt,omitempty"`
	Tool        *mission.Tool        `json:"tool,omitempty"`
	PlanMode    *bool                `json:"planMode,omitempty"`
	Model       *string              `json:"model,omitempty"`
	Effort      *string              `json:"effort,omitempty"`
	OperationID *mission.OperationID `json:"operationId,omitempty"`
	ExtraRepos  *[]mission.Repo      `json:"extraRepos,omitempty"`
	// BaseBranches replaces the whole map rather than merging into it, so that
	// clearing a repo's override is expressible.
	BaseBranches *map[string]string `json:"baseBranches,omitempty"`
	Order        *int               `json:"order,omitempty"`
	// Queued and Pin say when and where an unlaunched mission starts; see
	// [CreateMissionRequest]. An empty Pin clears it.
	Queued *bool   `json:"queued,omitempty"`
	Pin    *string `json:"pin,omitempty"`
}

// SetStatusRequest moves a mission between lanes.
//
// Moving an unlaunched mission into active launches the agent. Moving into
// active from a waiting or debrief lane delivers Message to the live session, if
// one is supplied. Moving to closed reclaims the live session and worktrees first.
type SetStatusRequest struct {
	To      mission.Status `json:"to"`
	Message string         `json:"message,omitempty"`
	// Force permits finishing to discard uncommitted work. Other lane moves
	// ignore it.
	Force bool `json:"force,omitempty"`
}

// MessageRequest sends text to a mission's live agent session, reviving it first if it
// has died.
type MessageRequest struct {
	Text string `json:"text"`
}

// OpenDebriefRequest opens a mission's debrief session.
type OpenDebriefRequest struct {
	// Mode is one of attach, steal, raise, or prepare. Empty means attach.
	//
	// It is a plain string rather than the debrief package's own type so clients do
	// not have to import the machinery that drives tmux.
	Mode string `json:"mode,omitempty"`
}

// Debrief modes, mirroring the debrief package's values.
const (
	DebriefAttach  = "attach"
	DebriefSteal   = "steal"
	DebriefRaise   = "raise"
	DebriefPrepare = "prepare"
)

// BranchesResponse lists the branches one repository's origin offers.
type BranchesResponse struct {
	Branches []string `json:"branches"`
}

// DiscoverReposRequest searches for candidate git repositories.
type DiscoverReposRequest struct {
	Query string `json:"query,omitempty"`
	// Limit caps the number of results. Zero means the server's default.
	Limit int `json:"limit,omitempty"`
}

// DiscoverReposResponse lists candidate repositories.
type DiscoverReposResponse struct {
	Repos []mission.Repo `json:"repos"`
}

// HookRequest reports one agent hook event.
//
// Mission identity travels in the request rather than being inferred, because the agent
// knows it exactly: q put it in the session environment at launch. SessionID
// and CWD are fallbacks for the cases where that environment did not survive.
type HookRequest struct {
	Tool  mission.Tool `json:"tool"`
	Event string       `json:"event"`
	// MissionID comes from the hook's environment and is the primary identity.
	MissionID mission.MissionID `json:"missionId,omitempty"`
	// HookEpoch is the launch generation the hook was configured for, so events
	// from a session q has already abandoned can be discarded.
	HookEpoch int `json:"hookEpoch"`
	// Payload is the raw JSON the agent wrote to the hook's standard input.
	Payload json.RawMessage `json:"payload"`
}

// Limits carries the agent usage windows currently exhausted, for the limits
// event. It is a wrapper rather than a bare slice so the frame stays a JSON
// object like every other one.
type Limits struct {
	Limits []mission.Limit `json:"limits"`
}

// ModelsResponse is what each agent says it can run.
//
// It is a separate endpoint rather than part of the state snapshot because it
// changes on the order of hours while the snapshot changes on the order of
// seconds, and every board would otherwise re-receive an unchanged model list
// with each event.
type ModelsResponse struct {
	Models map[mission.Tool]mission.ModelSet `json:"models"`
}

// Deleted identifies an entity that no longer exists, for the deleted event.
type Deleted struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

// Entity kinds used in [Deleted].
const (
	KindOperation = "operation"
	KindMission   = "mission"
)

// SyncProtocol is the version of the exchange two paired daemons speak.
//
// It is checked on every exchange rather than negotiated. The two machines are
// one person's, upgraded together, so a mismatch means one was missed, and the
// useful response is to say so rather than to guess at a common subset.
const SyncProtocol = 1

// SyncRequest is the primary's half of an exchange.
type SyncRequest struct {
	Protocol int `json:"protocol"`
	// Version is the sender's q version, reported when the protocols disagree
	// so the error names what to upgrade.
	Version string          `json:"version,omitempty"`
	Payload mission.Payload `json:"payload"`
	// TakeoverAfter is how long the secondary should wait, having not heard
	// from the primary, before running the primary's missions itself. It
	// travels with the exchange so the secondary needs no configuration.
	TakeoverAfter time.Duration `json:"takeoverAfter,omitempty"`
	// Commands are requests for missions the secondary runs that could not be
	// delivered when they were made.
	Commands []mission.Command `json:"commands,omitempty"`
	// Refs reports the primary's worktrees. See [mission.RepoState].
	Refs []mission.RepoState `json:"refs,omitempty"`
	// Expect is the host the primary believes it is calling, empty on a first
	// exchange. A secondary that is not that host refuses before recording
	// anything, so a primary pointed at the wrong machine cannot leave it
	// believing in a pairing the primary itself is about to reject.
	Expect mission.HostID `json:"expect,omitempty"`
	// Witness names the witness the primary consults, empty when it has none.
	Witness string `json:"witness,omitempty"`
}

// SyncResponse is the secondary's half.
type SyncResponse struct {
	Protocol int             `json:"protocol"`
	Version  string          `json:"version,omitempty"`
	Payload  mission.Payload `json:"payload"`
	// Results answers each command by id. A command with no result was not
	// attempted and is retried on the next exchange.
	Results []CommandResult `json:"results,omitempty"`
	// Refs reports the secondary's worktrees.
	Refs []mission.RepoState `json:"refs,omitempty"`
	// Witness names the witness the secondary consults, empty when it has none.
	Witness string `json:"witness,omitempty"`
}

// CommandResult is what became of one queued command.
type CommandResult struct {
	ID string `json:"id"`
	// Error is why the peer refused, empty on success.
	Error string `json:"error,omitempty"`
}

// RemoteStatus describes this daemon's pairing with another, for q remote
// status and the board's header.
type RemoteStatus struct {
	Self mission.HostInfo `json:"self"`
	Role mission.Role     `json:"role,omitempty"`
	// Peer is nil until the first exchange has told each side who the other is.
	Peer *mission.HostInfo `json:"peer,omitempty"`
	// LastSyncAt is the last completed exchange, zero if there has been none
	// since this daemon started.
	LastSyncAt time.Time `json:"lastSyncAt,omitzero"`
	// Error is why the most recent exchange failed, empty when it succeeded.
	Error string `json:"error,omitempty"`
	// Pending counts commands waiting to reach the peer.
	Pending int `json:"pending,omitempty"`
	// Witness is nil when neither host of the pair consults one.
	Witness *WitnessStatus `json:"witness,omitempty"`
}

// WitnessStatus describes this daemon's dealings with the pair's witness.
type WitnessStatus struct {
	// Name is the witness this host is configured with, empty when it has none.
	Name string `json:"name,omitempty"`
	// PeerName is the witness the paired host said it uses. The two must match
	// for either to rely on it.
	PeerName string `json:"peerName,omitempty"`
	// Holder is the host the witness named when it was last asked, empty when
	// it has not answered since this daemon started.
	Holder mission.HostID `json:"holder,omitempty"`
	// Standby reports that this host is not running its unpinned missions,
	// because it can reach neither its peer nor a witness that vouches for it.
	Standby bool `json:"standby,omitempty"`
	// AskedAt is when the witness was last asked.
	AskedAt time.Time `json:"askedAt,omitzero"`
	// Error is why the witness could not be asked, empty when it answered.
	Error string `json:"error,omitempty"`
}

// Mismatched reports whether the two hosts name different witnesses, one of
// them possibly none.
func (w WitnessStatus) Mismatched() bool { return w.Name != w.PeerName }

// Linked reports whether an exchange has completed and the latest one worked.
func (r RemoteStatus) Linked() bool { return !r.LastSyncAt.IsZero() && r.Error == "" }

// SettleRequest tells a secondary that the snapshots named in an exchange have
// been transferred, and carries the primary's report to settle against.
//
// It is a second request rather than part of the exchange because of ordering.
// The exchange tells each host what the other has; only then can the primary
// move the snapshots; and only after that can the secondary lay one out.
type SettleRequest struct {
	Refs []mission.RepoState `json:"refs,omitempty"`
}

// SettleResponse reports the secondary's worktrees as they stand once it has
// settled, which is what the primary then settles its own against. Without it
// the primary would judge by a report taken before the secondary acted, and
// spend one more exchange believing an edit it had just delivered was still
// waiting.
type SettleResponse struct {
	Refs []mission.RepoState `json:"refs,omitempty"`
}

// AttachCommand is how to attach a terminal to a mission's agent on the paired
// host.
type AttachCommand struct {
	// Argv is the command to run, with this terminal as its own.
	Argv []string `json:"argv"`
	// Host names the machine the agent is on, for messages.
	Host string `json:"host"`
}

// Forgotten reports what ending a pairing did.
type Forgotten struct {
	// Peer is the host that was forgotten, nil if there was none.
	Peer *mission.HostInfo `json:"peer,omitempty"`
	// PeerTold reports that the other host was reached and ended the pairing on
	// its side too. When false, it still believes in the pairing.
	PeerTold bool `json:"peerTold,omitempty"`
}
