package codex

import (
	"context"

	"github.com/justinrush/q/internal/mission"
)

type planState struct {
	Known    bool
	Pending  bool
	Approved bool
}

type planReader interface {
	ReadPlan(context.Context, string) (planState, error)
}

// ReadPlan reads only the newest turn. A proposed plan is a structured item,
// not an inference from an empty closing message. The terminal's explicit
// implementation prompt is the same signal used by the hook reducer.
func (p *Proxy) ReadPlan(ctx context.Context, id string) (planState, error) {
	params := struct {
		ThreadID      string `json:"threadId"`
		Limit         int    `json:"limit"`
		SortDirection string `json:"sortDirection"`
		ItemsView     string `json:"itemsView"`
	}{id, 1, "desc", "full"}
	var result struct {
		Data []struct {
			Status string `json:"status"`
			Items  []struct {
				Type    string `json:"type"`
				Content []struct {
					Type string `json:"type"`
					Text string `json:"text"`
				} `json:"content"`
			} `json:"items"`
		} `json:"data"`
	}
	err := p.client.Request(ctx, "thread/turns/list", params, &result)
	if err != nil {
		return planState{}, err
	}
	if len(result.Data) == 0 {
		return planState{}, nil
	}
	turn := result.Data[0]
	state := planState{Known: true}
	for _, item := range turn.Items {
		if item.Type == modePlan && turn.Status == "completed" {
			state.Pending = true
		}
		if item.Type == "userMessage" {
			for _, content := range item.Content {
				if content.Type == inputText && (mission.HookEvent{Prompt: content.Text}).IsPlanGoAhead() {
					state.Approved = true
				}
			}
		}
	}
	return state, nil
}

func (m *Manager) ReadPlan(ctx context.Context, id string) (planState, error) {
	var state planState
	err := m.withProxy(func(p *Proxy) error {
		var readErr error
		state, readErr = p.ReadPlan(ctx, id)
		return readErr
	})
	return state, err
}
