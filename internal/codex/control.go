package codex

import (
	"context"
	"fmt"
	"time"

	"github.com/justinrush/q/internal/mission"
)

const (
	modePlan    = "plan"
	modeDefault = "default"
	inputText   = "text"
	threadIDKey = "threadId"
)

type runtimeController interface {
	Interrupt(context.Context, string) error
	ConfigureImplementation(context.Context, mission.Mission) error
	StartImplementation(context.Context, mission.Mission, string) (string, error)
}

func (r *Runtime) Interrupt(ctx context.Context, id string) error {
	c, ok := r.threads.(runtimeController)
	if !ok {
		return fmt.Errorf("codex managed-server controls are unavailable; update Codex and retry")
	}
	return c.Interrupt(ctx, id)
}
func (r *Runtime) ConfigureImplementation(ctx context.Context, ms mission.Mission) error {
	c, ok := r.threads.(runtimeController)
	if !ok {
		return fmt.Errorf("codex managed-server controls are unavailable; update Codex and retry")
	}
	return c.ConfigureImplementation(ctx, ms)
}
func (r *Runtime) StartImplementation(ctx context.Context, ms mission.Mission, message string) (string, error) {
	c, ok := r.threads.(runtimeController)
	if !ok {
		return "", fmt.Errorf("codex managed-server controls are unavailable; update Codex and retry")
	}
	return c.StartImplementation(ctx, ms, message)
}

// Interrupt waits for server-side generation to stop. Closing a terminal does
// not stop a turn owned by the managed daemon.
func (p *Proxy) Interrupt(ctx context.Context, id string) error {
	if id == "" {
		return fmt.Errorf("codex thread has not been identified")
	}
	var observer rolloutObserver
	for {
		thread, err := p.ReadThread(ctx, id)
		if err != nil {
			return err
		}
		if thread.Status.Type == statusNotLoaded || thread.Status.Type == statusIdle {
			return nil
		}
		turnID, err := p.activeTurn(ctx, thread, &observer)
		if err != nil {
			return err
		}
		if turnID != "" {
			if err = p.client.Request(ctx, "turn/interrupt", map[string]string{threadIDKey: id, "turnId": turnID}, nil); err != nil {
				return err
			}
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}
	}
}

// History can briefly lag an active turn (or be unsupported by an older
// storage backend). A task_started rollout record is an independent source of
// the active turn ID. Never treat missing history as proof that generation stopped.
func (p *Proxy) activeTurn(ctx context.Context, thread ThreadSnapshot, observer *rolloutObserver) (string, error) {
	var result struct {
		Data []struct {
			ID     string `json:"id"`
			Status string `json:"status"`
		} `json:"data"`
	}
	err := p.client.Request(ctx, "thread/turns/list", map[string]any{threadIDKey: thread.ID, "limit": 1, "sortDirection": "desc"}, &result)
	if err == nil && len(result.Data) > 0 && result.Data[0].Status == "inProgress" {
		return result.Data[0].ID, nil
	}
	if thread.Path != "" {
		observed, readErr := observer.read(thread.Path, thread.ID)
		if readErr == nil && observed.active {
			return observed.activeTurn, nil
		}
	}
	if err != nil {
		return "", err
	}
	return "", nil
}

// implementationMode uses the current model only when the mission has no
// explicit implementation model. A null effort leaves Codex's default intact.
func implementationMode(ms mission.Mission) (map[string]any, error) {
	model := ms.Model
	if model == "" {
		model = ms.ObservedSettings.Model
	}
	if model == "" {
		return nil, fmt.Errorf("cannot resolve Codex implementation model until execution settings are observed")
	}
	var effort any
	if ms.Effort != "" {
		effort = ms.Effort
	}
	return map[string]any{"mode": modeDefault, "settings": map[string]any{
		"model": model, "reasoning_effort": effort, "developer_instructions": nil,
	}}, nil
}

func (p *Proxy) ConfigureImplementation(ctx context.Context, ms mission.Mission) error {
	mode, err := implementationMode(ms)
	if err != nil {
		return err
	}
	return p.client.Request(ctx, "thread/settings/update", map[string]any{threadIDKey: ms.AgentSessionID, "collaborationMode": mode}, nil)
}

func (p *Proxy) StartImplementation(ctx context.Context, ms mission.Mission, message string) (string, error) {
	mode, err := implementationMode(ms)
	if err != nil {
		return "", err
	}
	var result struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	err = p.client.Request(ctx, "turn/start", map[string]any{
		threadIDKey: ms.AgentSessionID, "collaborationMode": mode,
		"input": []map[string]string{{"type": inputText, inputText: message}},
	}, &result)
	if err != nil {
		return "", err
	}
	if result.Turn.ID == "" {
		return "", fmt.Errorf("codex turn/start returned no turn ID")
	}
	return result.Turn.ID, nil
}

func (m *Manager) Interrupt(ctx context.Context, id string) error {
	return m.withProxy(func(p *Proxy) error { return p.Interrupt(ctx, id) })
}
func (m *Manager) ConfigureImplementation(ctx context.Context, ms mission.Mission) error {
	return m.withProxy(func(p *Proxy) error { return p.ConfigureImplementation(ctx, ms) })
}

// turn/start must never use withProxy's retry: the server may have accepted
// a turn before its response was lost. Verification/retry belongs to the daemon.
func (m *Manager) StartImplementation(ctx context.Context, ms mission.Mission, message string) (string, error) {
	p, err := m.connect()
	if err != nil {
		return "", err
	}
	return p.StartImplementation(ctx, ms, message)
}
