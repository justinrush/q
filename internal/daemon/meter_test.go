package daemon

import (
	"encoding/json"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
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
