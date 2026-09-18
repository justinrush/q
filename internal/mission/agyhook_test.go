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

func TestOpencodeHooksDriveLifecycle(t *testing.T) {
	ms := Mission{Tool: ToolOpencode, Status: StatusActive, PlanMode: true}

	// 1. Session start
	start, err := ParseToolHookEventBytes(ToolOpencode, []byte(`{"session_id":"ses-123","cwd":"/missions/test"}`), EventSessionStart)
	if err != nil {
		t.Fatal(err)
	}
	if start.SessionID != "ses-123" || start.CWD != "/missions/test" || start.Source != SourceStartup {
		t.Fatalf("start = %+v", start)
	}
	ms = Reduce(ms, start, time.Now()).Mission
	if ms.AgentSessionID != "ses-123" || ms.AgentState != AgentBusy {
		t.Fatalf("ms = %+v", ms)
	}

	// 2. Plan approval permission request (plan_exit)
	planExitReq, err := ParseToolHookEventBytes(ToolOpencode, []byte(`{"sessionID":"ses-123","toolName":"plan_exit"}`), EventPermissionRequest)
	if err != nil {
		t.Fatal(err)
	}
	if !planExitReq.IsPlanApproval() {
		t.Fatal("plan_exit should be plan approval")
	}
	planRes := Reduce(ms, planExitReq, time.Now())
	if planRes.ProposedStatus != StatusDebrief || !planRes.Mission.PlanPending {
		t.Fatalf("plan exit should propose debrief: %+v", planRes)
	}
	ms = planRes.Mission

	// 3. Plan approval post-tool-use (approved and continues)
	postPlan, err := ParseToolHookEventBytes(ToolOpencode, []byte(`{"sessionId":"ses-123","tool":"plan_exit"}`), EventPostToolUse)
	if err != nil {
		t.Fatal(err)
	}
	postRes := Reduce(ms, postPlan, time.Now())
	if postRes.ProposedStatus != StatusActive || postRes.Mission.PlanPending {
		t.Fatalf("post plan_exit should propose active: %+v", postRes)
	}
	ms = postRes.Mission

	// 4. Normal permission request
	cmdReq, err := ParseToolHookEventBytes(ToolOpencode, []byte(`{"session_id":"ses-123","tool_name":"bash"}`), EventPermissionRequest)
	if err != nil {
		t.Fatal(err)
	}
	cmdRes := Reduce(ms, cmdReq, time.Now())
	if cmdRes.ProposedStatus != StatusAwaiting {
		t.Fatalf("bash permission should propose awaiting: %+v", cmdRes)
	}

	// 5. Stop
	stop, err := ParseToolHookEventBytes(ToolOpencode, []byte(`{"session_id":"ses-123"}`), EventStop)
	if err != nil {
		t.Fatal(err)
	}
	stopRes := Reduce(ms, stop, time.Now())
	if stopRes.ProposedStatus != StatusDebrief {
		t.Fatalf("stop should propose debrief: %+v", stopRes)
	}

	// 6. StopFailure
	stopFail, err := ParseToolHookEventBytes(ToolOpencode, []byte(`{"session_id":"ses-123","reason":"rate limit"}`), EventStopFailure)
	if err != nil {
		t.Fatal(err)
	}
	if stopFail.Reason != "rate limit" {
		t.Fatalf("reason = %q", stopFail.Reason)
	}

	// 7. Invalid JSON rejected
	if _, err := ParseToolHookEventBytes(ToolOpencode, []byte(`{invalid`), EventStop); err == nil {
		t.Fatal("expected error on malformed json")
	}
}

