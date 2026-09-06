package mission

import (
	"testing"
	"time"
)

func TestAgyHooksDriveLifecycle(t *testing.T) {
	ms := Mission{Tool: ToolAgy, Status: StatusActive}
	start, err := ParseToolHookEventBytes(ToolAgy, []byte(`{"conversationId":"conv-1","workspacePaths":["/missions/one"],"transcriptPath":"/transcript"}`), EventSessionStart)
	if err != nil {
		t.Fatal(err)
	}
	ms = Reduce(ms, start, time.Now()).Mission
	if ms.AgentSessionID != "conv-1" || ms.AgentState != AgentBusy || start.CWD != "/missions/one" {
		t.Fatalf("%+v / %+v", ms, start)
	}
	for _, tc := range []struct {
		payload    string
		event      string
		background int
	}{
		{`{"fullyIdle":true}`, EventStop, 0},
		{`{"fullyIdle":false}`, EventStop, 1},
		{`{}`, EventStop, 1},
		{`{"fullyIdle":true,"terminationReason":"error","error":"failed"}`, EventStopFailure, 0},
	} {
		ev, err := ParseToolHookEventBytes(ToolAgy, []byte(tc.payload), EventStop)
		if err != nil {
			t.Fatal(err)
		}
		if ev.Event != tc.event || ev.BackgroundTasks != tc.background {
			t.Fatalf("%s: %+v", tc.payload, ev)
		}
		res := Reduce(ms, ev, time.Now())
		if tc.background > 0 && res.ProposedStatus == StatusDebrief {
			t.Fatal("background work marked complete")
		}
		if tc.background == 0 && tc.event == EventStop && res.ProposedStatus != StatusDebrief {
			t.Fatalf("idle not debriefed: %+v", res)
		}
	}
	if _, err := ParseToolHookEventBytes(ToolAgy, []byte(`invalid`), EventStop); err == nil {
		t.Fatal("malformed payload accepted")
	}
}
