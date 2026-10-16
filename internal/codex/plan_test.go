package codex

import (
	"context"
	"github.com/justinrush/q/internal/mission"
	"testing"
)

func TestReadPlan(t *testing.T) {
	tests := map[string]struct {
		response string
		want     planState
	}{
		"completed plan":                    {`{"data":[{"status":"completed","items":[{"type":"plan"}]}]}`, planState{Known: true, Pending: true}},
		"unfinished plan":                   {`{"data":[{"status":"inProgress","items":[{"type":"plan"}]}]}`, planState{Known: true}},
		"accepted plan":                     {`{"data":[{"status":"inProgress","items":[{"type":"userMessage","content":[{"type":"text","text":"Implement the plan."}]}]}]}`, planState{Known: true, Approved: true}},
		"ordinary yes is not plan approval": {`{"data":[{"status":"inProgress","items":[{"type":"userMessage","content":[{"type":"text","text":"yes"}]}]}]}`, planState{Known: true}},
		"no turns":                          {`{"data":[]}`, planState{}},
	}
	for name, tc := range tests {
		t.Run(name, func(t *testing.T) {
			stream := newScriptedStream("{\"id\":1,\"result\":" + tc.response + "}\n")
			client := NewClient(stream, stream)
			go func() { _ = client.Run(t.Context(), make(chan Notification)) }()
			got, err := (&Proxy{client: client}).ReadPlan(t.Context(), "thread")
			if err != nil {
				t.Fatal(err)
			}
			if got != tc.want {
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}

type recoveryThreads struct {
	status ThreadStatus
	thread ThreadSnapshot
	found  bool
	plan   planState
}

func (f recoveryThreads) ReadThread(_ context.Context, _ string) (ThreadStatus, error) {
	return f.status, nil
}
func (f recoveryThreads) FindThread(_ context.Context, _ string) (ThreadSnapshot, bool, error) {
	return f.thread, f.found, nil
}
func (f recoveryThreads) ReadPlan(_ context.Context, _ string) (planState, error) { return f.plan, nil }
func (f recoveryThreads) Close() error                                            { return nil }

func TestRuntimeRecoversUnloadedSession(t *testing.T) {
	runtime := NewRuntime(recoveryThreads{status: ThreadStatus{Type: statusNotLoaded}, thread: ThreadSnapshot{ID: "new", Status: ThreadStatus{Type: statusIdle}}, found: true, plan: planState{Known: true, Pending: true}})
	got, found, err := runtime.Read(t.Context(), mission.Mission{Tool: mission.ToolCodex, AgentSessionID: "old", MissionDir: "/mission", HookEpoch: 2, PlanMode: true})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got.SessionID != "new" || got.PreviousSessionID != "old" || got.HookEpoch != 2 || !got.PlanPending {
		t.Fatalf("reading: %+v", got)
	}
}
