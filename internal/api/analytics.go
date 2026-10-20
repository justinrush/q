package api

import (
	"context"
	"github.com/justinrush/q/internal/mission"
	"time"
)

// AnalyticsSnapshot exposes retained metadata and usage, never conversation data.
type AnalyticsSnapshot struct {
	Records   []mission.AnalyticsRecord `json:"records"`
	UpdatedAt time.Time                 `json:"updatedAt"`
}

func (c *Client) Analytics(ctx context.Context) (AnalyticsSnapshot, error) {
	return get[AnalyticsSnapshot](ctx, c, "/v1/analytics")
}
