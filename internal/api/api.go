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
	Order       *int                 `json:"order,omitempty"`
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
