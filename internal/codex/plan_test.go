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
	path   string
	// createdAt is the recorded thread's creation time.
	createdAt int64
	thread    ThreadSnapshot
	found     bool
	// unpersisted hides thread from FindPersistedThread, as for a loaded
	// thread that has not yet taken a turn.
	unpersisted bool
	plan        planState
}

func (f recoveryThreads) ReadThread(_ context.Context, _ string) (ThreadSnapshot, error) {
	return ThreadSnapshot{ID: "old", Status: f.status, Path: f.path, CreatedAt: f.createdAt}, nil
}
func (f recoveryThreads) FindThread(_ context.Context, _ string) (ThreadSnapshot, bool, error) {
	return f.thread, f.found, nil
}
func (f recoveryThreads) FindPersistedThread(_ context.Context, _ string) (ThreadSnapshot, bool, error) {
	if f.unpersisted {
		return ThreadSnapshot{}, false, nil
	}
	return f.thread, f.found, nil
}
func (f recoveryThreads) ReadPlan(_ context.Context, _ string) (planState, error) { return f.plan, nil }
func (f recoveryThreads) Close() error                                            { return nil }

func TestRuntimeRecoversUnloadedSession(t *testing.T) {
	runtime := NewRuntime(recoveryThreads{status: ThreadStatus{Type: statusNotLoaded}, thread: ThreadSnapshot{ID: "new", Path: "/new.jsonl", Status: ThreadStatus{Type: statusIdle}}, found: true, plan: planState{Known: true, Pending: true}})
	got, found, err := runtime.Read(t.Context(), mission.Mission{Tool: mission.ToolCodex, AgentSessionID: "old", MissionDir: "/mission", HookEpoch: 2, PlanMode: true})
	if err != nil || !found {
		t.Fatalf("found=%v err=%v", found, err)
	}
	if got.TranscriptPath != "/new.jsonl" || got.SessionID != "new" || got.PreviousSessionID != "old" || got.HookEpoch != 2 || !got.PlanPending {
		t.Fatalf("reading: %+v", got)
	}
}

func TestRuntimeRecoversTranscriptForKnownSession(t *testing.T) {
	runtime := NewRuntime(recoveryThreads{status: ThreadStatus{Type: statusActive}, path: "/rollout.jsonl"})
	got, found, err := runtime.Read(t.Context(), mission.Mission{Tool: mission.ToolCodex, AgentSessionID: "old", HookEpoch: 2})
	if err != nil || !found || got.TranscriptPath != "/rollout.jsonl" || got.SessionID != "old" || got.HookEpoch != 2 {
		t.Fatalf("reading=%+v found=%v err=%v", got, found, err)
	}
}

// After a reboot the recorded thread is unloaded until the terminal resumes
// it. A loaded thread that has never taken a turn must not replace it, or the
// next resume targets a thread codex never wrote and the conversation is lost.
func TestRuntimeKeepsUnloadedSessionOverUnpersistedThread(t *testing.T) {
	runtime := NewRuntime(recoveryThreads{status: ThreadStatus{Type: statusNotLoaded}, thread: ThreadSnapshot{ID: "phantom", Status: ThreadStatus{Type: statusIdle}}, found: true, unpersisted: true})
	got, found, err := runtime.Read(t.Context(), mission.Mission{Tool: mission.ToolCodex, AgentSessionID: "old", MissionDir: "/mission"})
	if err != nil || found {
		t.Fatalf("reading=%+v found=%v err=%v, want no reading", got, found, err)
	}
}

func TestRuntimeKeepsUnloadedSessionOverOlderThread(t *testing.T) {
	runtime := NewRuntime(recoveryThreads{status: ThreadStatus{Type: statusNotLoaded}, createdAt: 200, thread: ThreadSnapshot{ID: "older", CreatedAt: 100, Status: ThreadStatus{Type: statusIdle}}, found: true})
	got, found, err := runtime.Read(t.Context(), mission.Mission{Tool: mission.ToolCodex, AgentSessionID: "old", MissionDir: "/mission"})
	if err != nil || found {
		t.Fatalf("reading=%+v found=%v err=%v, want no reading", got, found, err)
	}
}

// A mission with no recorded thread, such as a tool handoff, still adopts the
// loaded thread its terminal owns before the first turn.
func TestRuntimeAdoptsUnpersistedThreadWithoutRecordedSession(t *testing.T) {
	runtime := NewRuntime(recoveryThreads{thread: ThreadSnapshot{ID: "fresh", Status: ThreadStatus{Type: statusIdle}}, found: true, unpersisted: true})
	got, found, err := runtime.Read(t.Context(), mission.Mission{Tool: mission.ToolCodex, MissionDir: "/mission"})
	if err != nil || !found || got.SessionID != "fresh" || got.PreviousSessionID != "" {
		t.Fatalf("reading=%+v found=%v err=%v", got, found, err)
	}
}
