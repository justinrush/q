// The client half of the protocol.
//
// The TUI, every CLI subcommand, and the agent hook bridge all mutate state
// through this client rather than touching the state file, which is what keeps
// the daemon the single writer.

package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"sync"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
)

// requestTimeout bounds ordinary requests. The event stream bypasses it.
const requestTimeout = 10 * time.Second

// Client is a connection to a running daemon.
//
// The handle is re-read rather than fixed for the client's lifetime. The daemon
// binds an ephemeral port and mints a fresh token on every start, so a long-lived
// client that kept its first handle would dial an address nobody is listening on
// for as long as it ran. See [Client.Refresh].
type Client struct {
	// mu guards handle, which the event-stream goroutine reads while the caller
	// may be replacing it.
	mu     sync.RWMutex
	handle Handle
	// handlePath is daemon.json. It is empty for a client built from a handle the
	// caller already had, which therefore cannot refresh.
	handlePath string

	http *http.Client
}

// New returns a client for an already-known daemon.
func NewClient(handle Handle) *Client {
	return &Client{
		handle: handle,
		// No timeout on the transport itself, because the event stream is a
		// long-lived response; ordinary calls get a per-request context deadline.
		http: &http.Client{},
	}
}

// Open reads the daemon handle and returns a client, or [ErrNoDaemon].
func OpenClient(dirs paths.Dirs) (*Client, error) {
	path := dirs.DaemonFile()

	handle, err := ReadHandle(path)
	if err != nil {
		return nil, err
	}

	c := NewClient(handle)
	c.handlePath = path

	return c, nil
}

// Handle returns the daemon handle this client uses.
func (c *Client) Handle() Handle {
	c.mu.RLock()
	defer c.mu.RUnlock()

	return c.handle
}

// Refresh re-reads daemon.json and adopts it, reporting whether it now names a
// different daemon.
//
// This is what lets a running board survive a daemon restart. Without it a client
// retries the address and token it started with, which no longer exist, and goes
// on rendering the last state it saw: a board that has stopped listening looks
// exactly like one where nothing is happening.
//
// A client built from a caller-supplied handle has nowhere to re-read from and
// reports no change.
func (c *Client) Refresh() (bool, error) {
	if c.handlePath == "" {
		return false, nil
	}

	handle, err := ReadHandle(c.handlePath)
	if err != nil {
		return false, err
	}

	c.mu.Lock()
	defer c.mu.Unlock()

	// Only the reachability fields matter: a daemon that rewrote its handle with
	// the same address, token, and pid is the one already connected to.
	if handle.Addr == c.handle.Addr &&
		handle.Token == c.handle.Token &&
		handle.PID == c.handle.PID {
		return false, nil
	}

	c.handle = handle

	return true, nil
}

// Health fetches daemon status.
func (c *Client) Health(ctx context.Context) (Health, error) {
	return get[Health](ctx, c, "/v1/health")
}

// State fetches the full snapshot.
func (c *Client) State(ctx context.Context) (mission.Snapshot, error) {
	return get[mission.Snapshot](ctx, c, "/v1/state")
}

// Operations lists every operation.
func (c *Client) Operations(ctx context.Context) ([]mission.Operation, error) {
	return get[[]mission.Operation](ctx, c, "/v1/operations")
}

// CreateOperation adds an operation.
func (c *Client) CreateOperation(ctx context.Context, req CreateOperationRequest) (mission.Operation, error) {
	return send[mission.Operation](ctx, c, http.MethodPost, "/v1/operations", req)
}

// UpdateOperation patches an operation.
func (c *Client) UpdateOperation(
	ctx context.Context,
	id mission.OperationID,
	req UpdateOperationRequest,
) (mission.Operation, error) {
	return send[mission.Operation](ctx, c, http.MethodPatch, "/v1/operations/"+string(id), req)
}

// DeleteOperation removes an operation. Set force to remove one that still has
// unfinished missions.
func (c *Client) DeleteOperation(ctx context.Context, id mission.OperationID, force bool) error {
	path := "/v1/operations/" + string(id)
	if force {
		path += "?force=true"
	}

	_, err := send[struct{}](ctx, c, http.MethodDelete, path, nil)

	return err
}

// Missions lists every mission.
func (c *Client) Missions(ctx context.Context) ([]mission.Mission, error) {
	return get[[]mission.Mission](ctx, c, "/v1/missions")
}

// CreateMission adds a mission in the draft lane.
func (c *Client) CreateMission(ctx context.Context, req CreateMissionRequest) (mission.Mission, error) {
	return send[mission.Mission](ctx, c, http.MethodPost, "/v1/missions", req)
}

// UpdateMission patches a mission.
func (c *Client) UpdateMission(ctx context.Context, id mission.MissionID, req UpdateMissionRequest) (mission.Mission, error) {
	return send[mission.Mission](ctx, c, http.MethodPatch, "/v1/missions/"+string(id), req)
}

// DeleteMission removes a mission and reclaims its worktrees, branches, and session.
//
// Set force to remove a worktree holding uncommitted work; without it, such a mission is
// refused so the human can decide.
func (c *Client) DeleteMission(ctx context.Context, id mission.MissionID, force bool) (mission.Report, error) {
	path := "/v1/missions/" + string(id)
	if force {
		path += "?force=true"
	}

	return send[mission.Report](ctx, c, http.MethodDelete, path, nil)
}

// DeletePlan reports what deleting a mission would discard.
func (c *Client) DeletePlan(ctx context.Context, id mission.MissionID) (mission.Plan, error) {
	return get[mission.Plan](ctx, c, "/v1/missions/"+string(id)+"/delete-plan")
}

// SetStatus moves a mission to another lane, applying lifecycle effects such as launching,
// resuming, or reclaiming it.
func (c *Client) SetStatus(ctx context.Context, id mission.MissionID, req SetStatusRequest) (mission.Mission, error) {
	return send[mission.Mission](ctx, c, http.MethodPost, "/v1/missions/"+string(id)+"/status", req)
}

// OpenDebrief arranges a mission's debrief session and attaches to it.
func (c *Client) OpenDebrief(ctx context.Context, id mission.MissionID, mode string) (Result, error) {
	return send[Result](ctx, c, http.MethodPost, "/v1/missions/"+string(id)+"/open",
		OpenDebriefRequest{Mode: mode})
}

// Message sends text to a mission's live agent, reviving the session if it has died.
func (c *Client) Message(ctx context.Context, id mission.MissionID, text string) (mission.Mission, error) {
	return send[mission.Mission](ctx, c, http.MethodPost, "/v1/missions/"+string(id)+"/message",
		MessageRequest{Text: text})
}

// Diff reports what each of a mission's worktrees has changed.
func (c *Client) Diff(ctx context.Context, id mission.MissionID) ([]Touched, error) {
	return get[[]Touched](ctx, c, "/v1/missions/"+string(id)+"/diff")
}

// PostHook reports an agent hook event.
func (c *Client) PostHook(ctx context.Context, req HookRequest) error {
	path := "/v1/hooks/" + string(req.Tool) + "/" + mission.HookEventSlug(req.Event)

	_, err := send[struct{}](ctx, c, http.MethodPost, path, req)

	return err
}

// get performs a GET and decodes the response.
func get[T any](ctx context.Context, c *Client, path string) (T, error) {
	return send[T](ctx, c, http.MethodGet, path, nil)
}

// send performs a request with an optional JSON body and decodes the response.
func send[T any](ctx context.Context, c *Client, method, path string, body any) (T, error) {
	var zero T

	ctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()

	req, err := c.newRequest(ctx, method, path, body)
	if err != nil {
		return zero, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return zero, fmt.Errorf("%s %s: %w", method, path, err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode >= http.StatusBadRequest {
		return zero, decodeError(resp, method, path)
	}

	if resp.StatusCode == http.StatusNoContent {
		return zero, nil
	}

	var out T
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		return zero, fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}

	return out, nil
}

// newRequest builds an authenticated request.
func (c *Client) newRequest(ctx context.Context, method, path string, body any) (*http.Request, error) {
	var reader io.Reader

	if body != nil {
		encoded, err := json.Marshal(body)
		if err != nil {
			return nil, fmt.Errorf("encoding %s %s body: %w", method, path, err)
		}

		reader = bytes.NewReader(encoded)
	}

	handle := c.Handle()

	req, err := http.NewRequestWithContext(ctx, method, handle.BaseURL()+path, reader)
	if err != nil {
		return nil, fmt.Errorf("building %s %s: %w", method, path, err)
	}

	req.Header.Set("Authorization", "Bearer "+handle.Token)
	req.Header.Set(ClientHeader, ClientHeaderValue)

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return req, nil
}

// decodeError turns a non-2xx response into an error, preferring the daemon's own
// message over the bare status.
func decodeError(resp *http.Response, method, path string) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var payload Error
	if err := json.Unmarshal(data, &payload); err == nil && payload.Error != "" {
		return &StatusError{Code: resp.StatusCode, Message: payload.Error}
	}

	return &StatusError{
		Code:    resp.StatusCode,
		Message: fmt.Sprintf("%s %s: %s", method, path, resp.Status),
	}
}

// StatusError is a non-2xx response from the daemon.
type StatusError struct {
	Code    int
	Message string
}

// Error implements error.
func (e *StatusError) Error() string { return e.Message }

// NotFound reports whether the daemon said the entity does not exist.
func (e *StatusError) NotFound() bool { return e.Code == http.StatusNotFound }

// IsNotFound reports whether err is a 404 from the daemon.
func IsNotFound(err error) bool {
	statusErr, ok := errors.AsType[*StatusError](err)

	return ok && statusErr.NotFound()
}
