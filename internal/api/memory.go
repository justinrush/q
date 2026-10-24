package api

import (
	"context"
	"net/http"
	"net/url"

	"github.com/justinrush/q/internal/memory"
	"github.com/justinrush/q/internal/mission"
)

type SaveMemoryRequest struct {
	Mission mission.MissionID `json:"mission"`
	Capture string            `json:"capture,omitempty"`
	Records []memory.Input    `json:"records"`
}

type MemorySyncRequest struct {
	From    mission.HostID  `json:"from"`
	Expect  mission.HostID  `json:"expect"`
	After   string          `json:"after,omitempty"`
	Want    []string        `json:"want,omitempty"`
	Records []memory.Record `json:"records,omitempty"`
}

type MemorySyncResponse struct {
	Manifest []memory.Entry  `json:"manifest"`
	Next     string          `json:"next,omitempty"`
	Records  []memory.Record `json:"records,omitempty"`
}

func (c *Client) SaveMemory(ctx context.Context, req SaveMemoryRequest) ([]memory.Record, error) {
	return send[[]memory.Record](ctx, c, http.MethodPost, "/v1/memory", req)
}
func (c *Client) SearchMemory(ctx context.Context, q memory.Query) ([]memory.Result, error) {
	return send[[]memory.Result](ctx, c, http.MethodPost, "/v1/memory/search", q)
}
func (c *Client) ShowMemory(ctx context.Context, id string) (memory.Record, error) {
	return send[memory.Record](ctx, c, http.MethodGet, "/v1/memory/"+url.PathEscape(id), nil)
}
func (c *Client) UpdateMemory(ctx context.Context, id string, in memory.Input) (memory.Record, error) {
	return send[memory.Record](ctx, c, http.MethodPatch, "/v1/memory/"+url.PathEscape(id), in)
}
func (c *Client) DeleteMemory(ctx context.Context, id string) (memory.Record, error) {
	return send[memory.Record](ctx, c, http.MethodDelete, "/v1/memory/"+url.PathEscape(id), nil)
}
