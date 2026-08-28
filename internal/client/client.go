// Package client talks to the q daemon over HTTP.
//
// The TUI, every CLI subcommand, and the agent hook bridge all mutate state
// through this client rather than touching the state file, which is what keeps
// the daemon the single writer.
package client

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/daemon"
	"github.com/justinrush/q/internal/debrief"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/hookspec"
	"github.com/justinrush/q/internal/launch"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/state"
)

// requestTimeout bounds ordinary requests. The event stream bypasses it.
const requestTimeout = 10 * time.Second

// Client is a connection to a running daemon.
type Client struct {
	handle daemon.Handle
	http   *http.Client
}

// New returns a client for an already-known daemon.
func New(handle daemon.Handle) *Client {
	return &Client{
		handle: handle,
		// No timeout on the transport itself, because the event stream is a
		// long-lived response; ordinary calls get a per-request context deadline.
		http: &http.Client{},
	}
}

// Open reads the daemon handle and returns a client, or [daemon.ErrNoDaemon].
func Open(dirs paths.Dirs) (*Client, error) {
	handle, err := daemon.ReadHandle(dirs.DaemonFile())
	if err != nil {
		return nil, err
	}

	return New(handle), nil
}

// Handle returns the daemon handle this client uses.
func (c *Client) Handle() daemon.Handle { return c.handle }

// Health fetches daemon status.
func (c *Client) Health(ctx context.Context) (api.Health, error) {
	return get[api.Health](ctx, c, "/v1/health")
}

// State fetches the full snapshot.
func (c *Client) State(ctx context.Context) (state.Snapshot, error) {
	return get[state.Snapshot](ctx, c, "/v1/state")
}

// Operations lists every operation.
func (c *Client) Operations(ctx context.Context) ([]domain.Operation, error) {
	return get[[]domain.Operation](ctx, c, "/v1/operations")
}

// CreateOperation adds an operation.
func (c *Client) CreateOperation(ctx context.Context, req api.CreateOperationRequest) (domain.Operation, error) {
	return send[domain.Operation](ctx, c, http.MethodPost, "/v1/operations", req)
}

// UpdateOperation patches an operation.
func (c *Client) UpdateOperation(
	ctx context.Context,
	id domain.OperationID,
	req api.UpdateOperationRequest,
) (domain.Operation, error) {
	return send[domain.Operation](ctx, c, http.MethodPatch, "/v1/operations/"+string(id), req)
}

// DeleteOperation removes an operation. Set force to remove one that still has
// unfinished missions.
func (c *Client) DeleteOperation(ctx context.Context, id domain.OperationID, force bool) error {
	path := "/v1/operations/" + string(id)
	if force {
		path += "?force=true"
	}

	_, err := send[struct{}](ctx, c, http.MethodDelete, path, nil)

	return err
}

// Missions lists every mission.
func (c *Client) Missions(ctx context.Context) ([]domain.Mission, error) {
	return get[[]domain.Mission](ctx, c, "/v1/missions")
}

// CreateMission adds a mission in the draft lane.
func (c *Client) CreateMission(ctx context.Context, req api.CreateMissionRequest) (domain.Mission, error) {
	return send[domain.Mission](ctx, c, http.MethodPost, "/v1/missions", req)
}

// UpdateMission patches a mission.
func (c *Client) UpdateMission(ctx context.Context, id domain.MissionID, req api.UpdateMissionRequest) (domain.Mission, error) {
	return send[domain.Mission](ctx, c, http.MethodPatch, "/v1/missions/"+string(id), req)
}

// DeleteMission removes a mission and reclaims its worktrees, branches, and session.
//
// Set force to remove a worktree holding uncommitted work; without it, such a mission is
// refused so the human can decide.
func (c *Client) DeleteMission(ctx context.Context, id domain.MissionID, force bool) (launch.Report, error) {
	path := "/v1/missions/" + string(id)
	if force {
		path += "?force=true"
	}

	return send[launch.Report](ctx, c, http.MethodDelete, path, nil)
}

// DeletePlan reports what deleting a mission would discard.
func (c *Client) DeletePlan(ctx context.Context, id domain.MissionID) (launch.Plan, error) {
	return get[launch.Plan](ctx, c, "/v1/missions/"+string(id)+"/delete-plan")
}

// SetStatus moves a mission to another lane, applying lifecycle effects such as launching,
// resuming, or reclaiming it.
func (c *Client) SetStatus(ctx context.Context, id domain.MissionID, req api.SetStatusRequest) (domain.Mission, error) {
	return send[domain.Mission](ctx, c, http.MethodPost, "/v1/missions/"+string(id)+"/status", req)
}

// OpenDebrief arranges a mission's debrief session and attaches to it.
func (c *Client) OpenDebrief(ctx context.Context, id domain.MissionID, mode string) (debrief.Result, error) {
	return send[debrief.Result](ctx, c, http.MethodPost, "/v1/missions/"+string(id)+"/open",
		api.OpenDebriefRequest{Mode: mode})
}

// Message sends text to a mission's live agent, reviving the session if it has died.
func (c *Client) Message(ctx context.Context, id domain.MissionID, text string) (domain.Mission, error) {
	return send[domain.Mission](ctx, c, http.MethodPost, "/v1/missions/"+string(id)+"/message",
		api.MessageRequest{Text: text})
}

// Diff reports what each of a mission's worktrees has changed.
func (c *Client) Diff(ctx context.Context, id domain.MissionID) ([]debrief.Touched, error) {
	return get[[]debrief.Touched](ctx, c, "/v1/missions/"+string(id)+"/diff")
}

// PostHook reports an agent hook event.
func (c *Client) PostHook(ctx context.Context, req api.HookRequest) error {
	path := "/v1/hooks/" + string(req.Tool) + "/" + hookspec.EventSlug(req.Event)

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

	req, err := http.NewRequestWithContext(ctx, method, c.handle.BaseURL()+path, reader)
	if err != nil {
		return nil, fmt.Errorf("building %s %s: %w", method, path, err)
	}

	req.Header.Set("Authorization", "Bearer "+c.handle.Token)
	req.Header.Set(api.ClientHeader, api.ClientHeaderValue)

	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}

	return req, nil
}

// decodeError turns a non-2xx response into an error, preferring the daemon's own
// message over the bare status.
func decodeError(resp *http.Response, method, path string) error {
	data, _ := io.ReadAll(io.LimitReader(resp.Body, 64<<10))

	var payload api.Error
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
