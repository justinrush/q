package daemon

import (
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/usage"
)

// fakeMeter records what it was asked and answers with a fixed reading.
type fakeMeter struct {
	metering mission.Metering
	measured bool
	err      error
	calls    int
}

func (f *fakeMeter) Tool() mission.Tool { return mission.ToolClaude }

func (f *fakeMeter) Meter(mission.Mission) (mission.Metering, bool, error) {
	f.calls++

	return f.metering, f.measured, f.err
}

// usageOf builds a reading worth a given number of dollars.
func usageOf(usd float64) mission.Usage {
	return mission.Usage{
		PerModel: map[string]mission.ModelTokens{"model-a": {Output: 1}},
		USD:      usd,
	}
}

// hookPayload renders the JSON an agent writes to a hook's standard input.
func hookPayload(t *testing.T, sessionID, transcript string) json.RawMessage {
	t.Helper()

	data, err := json.Marshal(map[string]any{
		"session_id":      sessionID,
		"transcript_path": transcript,
	})
	if err != nil {
		t.Fatal(err)
	}

	return data
}

// Metering is deliberately not on the per-tool-call path: a claude session
// reports two of those per tool, and re-reading a transcript on each would cost
// more than the work it is accounting for.
func TestApplyHookMetersOnlyWhenATurnEnds(t *testing.T) {
	cases := []struct {
		name      string
		event     string
		wantCalls int
	}{
		{name: "a turn ending", event: mission.EventStop, wantCalls: 1},
		{name: "a turn ending in an API error", event: mission.EventStopFailure, wantCalls: 1},
		{name: "the session exiting", event: mission.EventSessionEnd, wantCalls: 1},
		{name: "a tool call finishing", event: mission.EventPostToolUse},
		{name: "a tool call starting", event: mission.EventPreToolUse},
		{name: "a prompt being submitted", event: mission.EventUserPromptSubmit},
		{name: "the session starting", event: mission.EventSessionStart},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			ms := launchedServiceMission(t, svc)
			meter := &fakeMeter{metering: mission.Metering{Usage: usageOf(1)}, measured: true}
			svc.apply(WithMeter(meter))

			svc.ApplyHook(api.HookRequest{
				Tool:      mission.ToolClaude,
				Event:     tc.event,
				MissionID: ms.ID,
				Payload:   hookPayload(t, "", "/transcript.jsonl"),
			})

			if meter.calls != tc.wantCalls {
				t.Fatalf("meter called %d times, want %d", meter.calls, tc.wantCalls)
			}
		})
	}
}

// The transcript path is what the meter needs and the only place q is ever told
// it, so a hook that carries one must leave it on the mission — including on
// the timer path, where there is no hook payload to read it out of.
func TestApplyHookRecordsTheTranscriptPath(t *testing.T) {
	cases := []struct {
		name  string
		event string
	}{
		{name: "at session start", event: mission.EventSessionStart},
		{name: "on a tool call", event: mission.EventPostToolUse},
		{name: "when a turn ends", event: mission.EventStop},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			ms := launchedServiceMission(t, svc)

			// A mission in briefing ignores every agent event but this one, so
			// the later events need a session to have started first.
			svc.ApplyHook(api.HookRequest{
				Tool:      mission.ToolClaude,
				Event:     mission.EventSessionStart,
				MissionID: ms.ID,
				Payload:   hookPayload(t, "", ""),
			})

			svc.ApplyHook(api.HookRequest{
				Tool:      mission.ToolClaude,
				Event:     tc.event,
				MissionID: ms.ID,
				Payload:   hookPayload(t, "", "/tmp/session.jsonl"),
			})

			stored, _ := svc.Snapshot().Mission(ms.ID)
			if stored.TranscriptPath != "/tmp/session.jsonl" {
				t.Fatalf("TranscriptPath = %q, want /tmp/session.jsonl", stored.TranscriptPath)
			}
		})
	}
}

func TestMeterMissionStoresAndPublishes(t *testing.T) {
	svc := newTestService(t)
	ms := launchedServiceMission(t, svc)
	svc.apply(WithMeter(&fakeMeter{metering: mission.Metering{Usage: usageOf(2.5)}, measured: true}))

	svc.meterMission(ms.ID)

	stored, ok := svc.Snapshot().Mission(ms.ID)
	if !ok {
		t.Fatal("mission disappeared")
	}

	if stored.Usage.USD != 2.5 {
		t.Fatalf("USD = %v, want 2.5", stored.Usage.USD)
	}

	if stored.Usage.At.IsZero() {
		t.Fatal("a stored measurement should be stamped with when it was taken")
	}
}

// Exercise the actual Codex meter across the daemon's relaunch boundary. A
// planning/implementation model split must preserve both the transcript path
// and the planning cost, then re-read the appended implementation responses.
func TestCodexMeterAcrossPlanModelRelaunch(t *testing.T) {
	svc := newTestService(t)
	ms := launchedServiceMission(t, svc)
	ms.Tool = mission.ToolCodex
	ms.Model, ms.PlanModel = "gpt-6.1-sol", "gpt-6-astra"
	ms.PlanMode = true
	ms.HookEpoch = 1
	if err := svcStore(svc).Mutate("test.codex", func(snap *mission.Snapshot) error {
		snap.PutMission(ms)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	svc.apply(WithMeter(usage.NewCodex(usage.DefaultPricing())))

	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	plan := `{"type":"turn_context","payload":{"model":"gpt-6-astra"}}` + "\n" +
		`{"type":"token_usage_record","payload":{"thread_id":"thread","response_id":"plan","usage":{"input_tokens":1000000,"cached_input_tokens":500000,"output_tokens":100000}}}` + "\n"
	if err := os.WriteFile(path, []byte(plan), 0o600); err != nil {
		t.Fatal(err)
	}
	hook := func(event string, epoch int, transcript string) {
		t.Helper()
		svc.ApplyHook(api.HookRequest{
			Tool: mission.ToolCodex, MissionID: ms.ID, Event: event,
			HookEpoch: epoch, Payload: hookPayload(t, "thread", transcript),
		})
	}
	hook(mission.EventSessionStart, 1, path)
	hook(mission.EventStop, 1, path)
	stored, _ := svc.Snapshot().Mission(ms.ID)
	if math.Abs(stored.Usage.USD-10.5) > 1e-9 {
		t.Fatalf("planning usage = %+v, want $10.50", stored.Usage)
	}

	if err := svc.setPlanApproved(ms.ID, true, false); err != nil {
		t.Fatal(err)
	}
	stored, _ = svc.Snapshot().Mission(ms.ID)
	stored.HookEpoch++
	if _, err := svc.commitRelaunch(stored); err != nil {
		t.Fatal(err)
	}
	// A resumed hook need not repeat the path: the existing one must survive.
	hook(mission.EventSessionStart, 2, "")
	build := `{"type":"turn_context","payload":{"model":"gpt-6.1-sol"}}` + "\n" +
		`{"type":"token_usage_record","payload":{"thread_id":"thread","response_id":"build","usage":{"input_tokens":1000000,"cached_input_tokens":500000,"output_tokens":100000}}}` + "\n"
	if err := os.WriteFile(path, []byte(plan+build), 0o600); err != nil {
		t.Fatal(err)
	}
	for range 2 { // Repeated reads must not add the planning cost again.
		svc.meterMission(ms.ID)
		stored, _ = svc.Snapshot().Mission(ms.ID)
		if stored.TranscriptPath != path || len(stored.Usage.PerModel) != 2 ||
			stored.Usage.Unpriced || math.Abs(stored.Usage.USD-12.55) > 1e-9 {
			t.Fatalf("after relaunch: path=%q usage=%+v, want both models and $12.55", stored.TranscriptPath, stored.Usage)
		}
	}
}

// The reconciler meters every running mission every fifteen seconds. A reading
// that says what the last one said must not rewrite the state file, or a board
// left open overnight records thousands of non-events.
func TestMeterMissionWritesNothingWhenNothingChanged(t *testing.T) {
	svc := newTestService(t)
	ms := launchedServiceMission(t, svc)
	svc.apply(WithMeter(&fakeMeter{metering: mission.Metering{Usage: usageOf(2.5)}, measured: true}))

	svc.meterMission(ms.ID)

	before := stateWrittenAt(t, svc)

	for range 3 {
		svc.meterMission(ms.ID)
	}

	if after := stateWrittenAt(t, svc); !after.Equal(before) {
		t.Fatalf("state file rewritten at %v, was %v", after, before)
	}
}

func TestMeterMissionRecordsALimit(t *testing.T) {
	svc := newTestService(t)
	ms := launchedServiceMission(t, svc)
	resets := time.Now().Add(time.Hour)

	svc.apply(WithMeter(&fakeMeter{
		metering: mission.Metering{Limit: mission.Limit{
			Tool:     mission.ToolClaude,
			Kind:     "five_hour",
			ResetsAt: resets,
		}},
	}))

	svc.meterMission(ms.ID)

	limit, ok := svc.Snapshot().Limit(mission.ToolClaude, time.Now())
	if !ok {
		t.Fatal("the limit should have been recorded")
	}

	if limit.Kind != "five_hour" {
		t.Fatalf("Kind = %q, want five_hour", limit.Kind)
	}
}

// A meter is a convenience. Losing one must cost the board a cost figure and
// nothing else, which matters most on the hook path, where failing loudly would
// block a tool call in the agent.
func TestMeterMissionToleratesAFailingMeter(t *testing.T) {
	svc := newTestService(t)
	ms := launchedServiceMission(t, svc)
	svc.apply(WithMeter(&fakeMeter{err: errors.New("transcript is gibberish")}))

	svc.ApplyHook(api.HookRequest{
		Tool:      mission.ToolClaude,
		Event:     mission.EventStop,
		MissionID: ms.ID,
		Payload:   hookPayload(t, "", "/transcript.jsonl"),
	})

	stored, _ := svc.Snapshot().Mission(ms.ID)
	if !stored.Usage.Empty() {
		t.Fatalf("Usage = %+v, want nothing recorded", stored.Usage)
	}
}

// An agent with no meter is the normal case for codex, and must not be an error
// or a reason to skip the rest of a hook.
func TestMeterMissionSkipsAnUnmeteredAgent(t *testing.T) {
	svc := newTestService(t)
	ms := launchedServiceMission(t, svc)

	meter := &fakeMeter{metering: mission.Metering{Usage: usageOf(1)}, measured: true}
	svc.apply(WithMeter(meter))

	err := svcStore(svc).Mutate("test.tool", func(snap *mission.Snapshot) error {
		stored, _ := snap.Mission(ms.ID)
		stored.Tool = mission.ToolCodex
		snap.PutMission(stored)

		return nil
	})
	if err != nil {
		t.Fatal(err)
	}

	svc.meterMission(ms.ID)

	if meter.calls != 0 {
		t.Fatalf("the claude meter was asked about a codex mission %d times", meter.calls)
	}
}

// stateWrittenAt reports when the store last persisted.
func stateWrittenAt(t *testing.T, svc *Service) time.Time {
	t.Helper()

	info, err := os.Stat(svc.dirs.StateFile())
	if err != nil {
		t.Fatal(err)
	}

	return info.ModTime()
}

// Missing path-bearing hooks must not leave costs blank when runtime status works.
func TestCodexMeterRecoversFromMissingTranscriptHook(t *testing.T) {
	svc := newTestService(t)
	ms := seedCodexMission(t, svc, mission.StatusActive, "")
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	records := `{"type":"turn_context","payload":{"model":"gpt-6-astra"}}` + "\n" +
		`{"type":"token_usage_record","payload":{"thread_id":"thr-1","response_id":"plan","usage":{"input_tokens":1000000,"cached_input_tokens":500000,"output_tokens":100000}}}` + "\n"
	if err := os.WriteFile(path, []byte(records), 0o600); err != nil {
		t.Fatal(err)
	}
	svc.apply(WithMeter(usage.NewCodex(usage.DefaultPricing())))
	// Start with an otherwise identical runtime reading: a path alone is a change.
	reading := waitingReading(mission.ActivityBusy)
	svc.applyRuntimeReading(ms.ID, reading)
	reading.TranscriptPath = path
	for range 2 {
		svc.applyRuntimeReading(ms.ID, reading)
		svc.meterMission(ms.ID)
		got, _ := svc.Snapshot().Mission(ms.ID)
		if got.TranscriptPath != path || math.Abs(got.Usage.USD-10.5) > 1e-9 {
			t.Fatalf("path=%q usage=%+v, want recovered path and $10.50", got.TranscriptPath, got.Usage)
		}
	}
	// Missing optional metadata on subsequent readings must retain the path.
	reading.TranscriptPath = ""
	svc.applyRuntimeReading(ms.ID, reading)
	got, _ := svc.Snapshot().Mission(ms.ID)
	if got.TranscriptPath != path {
		t.Fatalf("path lost: %q", got.TranscriptPath)
	}
}
