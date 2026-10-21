package codex

import (
	"bytes"
	"context"
	"encoding/json"
	"os"
	"testing"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// Opt-in: creates only a scratch, read-only thread on the installed managed
// server. It never reads or interrupts a pre-existing thread. Three tiny model
// calls verify the wire contract that fake RPC tests cannot establish.
func TestLivePlanHandoff(t *testing.T) {
	if os.Getenv("Q_CODEX_PLAN_SMOKE") != "1" {
		t.Skip("set Q_CODEX_PLAN_SMOKE=1 to exercise installed Codex")
	}
	ctx, cancel := context.WithTimeout(t.Context(), 2*time.Minute)
	defer cancel()
	owner, err := StartProxy(ctx, "codex", "test", runner.OS{})
	if err != nil {
		t.Fatal(err)
	}
	defer owner.Close()
	observer, err := StartProxy(ctx, "codex", "test", runner.OS{})
	if err != nil {
		t.Fatal(err)
	}
	defer observer.Close()
	var result struct {
		Thread ThreadSnapshot `json:"thread"`
	}
	err = owner.client.Request(ctx, "thread/start", map[string]any{
		"cwd": t.TempDir(), "model": "gpt-6-astra", "sandbox": "read-only",
		"config": map[string]any{"model_reasoning_effort": "low", "plan_mode_reasoning_effort": "low"},
	}, &result)
	if err != nil {
		t.Fatal(err)
	}
	ms := mission.Mission{AgentSessionID: result.Thread.ID, Model: "gpt-6-sol", Effort: "low"}
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		_ = observer.Interrupt(cleanup, ms.AgentSessionID)
		_ = owner.client.Request(cleanup, "thread/archive", map[string]string{"threadId": ms.AgentSessionID}, nil)
	}()
	var turn struct {
		Turn struct {
			ID string `json:"id"`
		} `json:"turn"`
	}
	err = owner.client.Request(ctx, "turn/start", map[string]any{
		"threadId":          ms.AgentSessionID,
		"collaborationMode": map[string]any{"mode": "plan", "settings": map[string]any{"model": "gpt-6-astra", "reasoning_effort": "low", "developer_instructions": nil}},
		"input":             []map[string]string{{"type": "text", "text": "Protocol smoke test: do not use tools. Propose a one-sentence plan to reply OK. No questions needed."}},
	}, &turn)
	if err != nil {
		t.Fatal(err)
	}
	waitLiveTurn(t, ctx, owner, ms.AgentSessionID, turn.Turn.ID)
	assertLiveSettings(t, ctx, observer, ms.AgentSessionID, "plan", "gpt-6-astra")
	// Match q's terminal restart: detach the planning subscriber, then resume
	// the thread on the implementation configuration before updating its mode.
	if err = owner.client.Request(ctx, "thread/unsubscribe", map[string]string{"threadId": ms.AgentSessionID}, nil); err != nil {
		t.Fatal(err)
	}
	if err = owner.client.Request(ctx, "thread/resume", map[string]any{"threadId": ms.AgentSessionID, "model": ms.Model, "config": map[string]string{"model_reasoning_effort": ms.Effort}, "excludeTurns": true}, nil); err != nil {
		t.Fatal(err)
	}
	if err = observer.ConfigureImplementation(ctx, ms); err != nil {
		t.Fatal(err)
	}
	if turn.Turn.ID, err = observer.StartImplementation(ctx, ms, "Implement the plan. Reply OK without tools."); err != nil {
		t.Fatal(err)
	}
	waitLiveTurn(t, ctx, owner, ms.AgentSessionID, turn.Turn.ID)
	assertLiveSettings(t, ctx, observer, ms.AgentSessionID, "default", "gpt-6-sol")
	if err = owner.client.Request(ctx, "turn/start", map[string]any{"threadId": ms.AgentSessionID, "input": []map[string]string{{"type": "text", "text": "Reply OK again without tools."}}}, &turn); err != nil {
		t.Fatal(err)
	}
	waitLiveTurn(t, ctx, owner, ms.AgentSessionID, turn.Turn.ID)
	assertLiveSettings(t, ctx, observer, ms.AgentSessionID, "default", "gpt-6-sol")
}

func assertLiveSettings(t *testing.T, ctx context.Context, p *Proxy, id, mode, model string) {
	t.Helper()
	thread, err := p.ReadThread(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	var observations rolloutObserver
	observed, err := observations.read(thread.Path, id)
	if err != nil {
		t.Fatal(err)
	}
	got := observed.settings
	if got.Mode != mode || got.Model != model || got.Effort != "low" {
		t.Fatalf("wanted %s %s/low; observed %+v", mode, model, got)
	}
	t.Logf("observed %s %s/%s", got.Mode, got.Model, got.Effort)
}

func waitLiveTurn(t *testing.T, ctx context.Context, p *Proxy, id, turnID string) {
	t.Helper()
	ticker := time.NewTicker(200 * time.Millisecond)
	defer ticker.Stop()
	for {
		thread, err := p.ReadThread(ctx, id)
		if err == nil {
			data, readErr := os.ReadFile(thread.Path)
			if readErr == nil {
				for _, line := range bytes.Split(data, []byte("\n")) {
					var record observationRecord
					if json.Unmarshal(line, &record) == nil && record.Type == "event_msg" &&
						record.Payload.Type == "task_complete" && record.Payload.TurnID == turnID {
						return
					}
				}
			}
		}

		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
}
