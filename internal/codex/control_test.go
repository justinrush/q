package codex

import (
	"encoding/json"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

func TestImplementationControls(t *testing.T) {
	for _, method := range []string{"thread/settings/update", "turn/start"} {
		t.Run(method, func(t *testing.T) {
			stream := newScriptedStream("{\"id\":1,\"result\":{\"turn\":{\"id\":\"turn-1\"}}}\n")
			client := NewClient(stream, stream)
			go func() { _ = client.Run(t.Context(), make(chan Notification)) }()
			p := &Proxy{client: client}
			ms := mission.Mission{AgentSessionID: "thread-1", Model: "gpt-6-sol", Effort: "low"}
			var err error
			if method == "turn/start" {
				var id string
				id, err = p.StartImplementation(t.Context(), ms, "Implement the plan.")
				if id != "turn-1" {
					t.Fatalf("turn=%s", id)
				}
			} else {
				err = p.ConfigureImplementation(t.Context(), ms)
			}
			if err != nil {
				t.Fatal(err)
			}
			var sent struct {
				Method string
				Params map[string]json.RawMessage
			}
			if err = json.Unmarshal(stream.output.Bytes(), &sent); err != nil {
				t.Fatal(err)
			}
			if sent.Method != method {
				t.Fatal(sent.Method)
			}
			for _, key := range []string{"approvalPolicy", "sandboxPolicy", "permissions", "developerInstructions"} {
				if _, exists := sent.Params[key]; exists {
					t.Fatalf("overrode %s", key)
				}
			}
			var mode struct {
				Mode     string
				Settings struct {
					Model        string
					Effort       string `json:"reasoning_effort"`
					Instructions any    `json:"developer_instructions"`
				}
			}
			if err = json.Unmarshal(sent.Params["collaborationMode"], &mode); err != nil {
				t.Fatal(err)
			}
			if mode.Mode != "default" || mode.Settings.Model != "gpt-6-sol" || mode.Settings.Effort != "low" || mode.Settings.Instructions != nil {
				t.Fatalf("mode=%+v", mode)
			}
		})
	}
}

func TestPlanningEffortAndControlledPrompt(t *testing.T) {
	a := New("codex", Options{})
	for _, resume := range []bool{false, true} {
		inv := mission.Invocation{PlanMode: true, Model: "gpt-6-astra", Effort: "low", Resume: resume, SessionID: "thread"}
		args := a.Args(inv)
		found := false
		for _, arg := range args {
			if arg == "'plan_mode_reasoning_effort=low'" {
				found = true
			}
		}
		if !found {
			t.Fatalf("missing planning effort: %v", args)
		}
		inv.PlanMode = false
		inv.ControlledTurn = true
		for _, arg := range a.Args(inv) {
			if arg == mission.PromptArg {
				t.Fatal("controlled handoff auto-submits prompt")
			}
		}
	}
}

func TestPlanLaunchRequiresManagedServer(t *testing.T) {
	a := New("codex", Options{})
	for _, inv := range []mission.Invocation{{PlanMode: true}, {ControlledTurn: true}} {
		if !strings.Contains(a.Prologue(inv), "requires the managed app-server") {
			t.Fatal("planning silently falls back")
		}
	}
	if strings.Contains(a.Prologue(mission.Invocation{}), "requires the managed app-server") {
		t.Fatal("ordinary launch lost compatibility fallback")
	}
}

func TestInterruptUsesRolloutWhenHistoryLags(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	if err := os.WriteFile(path, []byte(`{"type":"event_msg","payload":{"type":"task_started","turn_id":"live-turn"}}`+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	conn, server := net.Pipe()
	defer conn.Close()
	defer server.Close()
	done := make(chan error, 1)
	go func() {
		decoder := json.NewDecoder(server)
		encoder := json.NewEncoder(server)
		for i, method := range []string{"thread/read", "thread/turns/list", "turn/interrupt", "thread/read"} {
			var request struct {
				ID     int
				Method string
				Params map[string]any
			}
			if err := decoder.Decode(&request); err != nil {
				done <- err
				return
			}
			if request.Method != method {
				done <- fmt.Errorf("method=%s, want %s", request.Method, method)
				return
			}
			var result any
			switch i {
			case 0:
				result = map[string]any{"thread": map[string]any{"id": "live", "path": path, "status": map[string]string{"type": "active"}}}
			case 1:
				result = map[string]any{"data": []map[string]string{{"id": "live-turn", "status": "interrupted"}}}
			case 2:
				if request.Params["turnId"] != "live-turn" {
					done <- fmt.Errorf("wrong interruption: %v", request.Params)
					return
				}
				result = map[string]any{}
			case 3:
				result = map[string]any{"thread": map[string]any{"id": "live", "status": map[string]string{"type": "idle"}}}
			}
			if err := encoder.Encode(map[string]any{"id": request.ID, "result": result}); err != nil {
				done <- err
				return
			}
		}
		done <- nil
	}()
	client := NewClient(conn, conn)
	go func() { _ = client.Run(t.Context(), make(chan Notification)) }()
	if err := (&Proxy{client: client}).Interrupt(t.Context(), "live"); err != nil {
		t.Fatal(err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
