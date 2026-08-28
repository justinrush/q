package mission

import "testing"

func TestParseClaudeStopPayload(t *testing.T) {
	body := `{
	  "session_id": "3f2a1b4c",
	  "cwd": "/missions/t",
	  "transcript_path": "/Users/j/.claude/projects/slug/3f2a1b4c.jsonl",
	  "hook_event_name": "Stop",
	  "permission_mode": "auto",
	  "stop_hook_active": false,
	  "last_assistant_message": "All done.",
	  "background_tasks": []
	}`

	got, err := ParseHookEventBytes([]byte(body), EventStop)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}

	if got.SessionID != "3f2a1b4c" || got.CWD != "/missions/t" {
		t.Errorf("identity fields = %+v", got)
	}

	if got.LastAssistantMessage != "All done." {
		t.Errorf("LastAssistantMessage = %q", got.LastAssistantMessage)
	}

	// An empty array means nothing is in flight, which is what distinguishes a
	// finished turn from a paused one.
	if got.BackgroundTasks != 0 {
		t.Errorf("BackgroundTasks = %d, want 0", got.BackgroundTasks)
	}
}

// A non-empty background_missions array means the session is paused, not finished.
func TestParseCountsBackgroundTasks(t *testing.T) {
	body := `{"session_id":"s","cwd":"/","hook_event_name":"Stop",
	  "background_tasks":[{"id":"a"},{"id":"b"}]}`

	got, err := ParseHookEventBytes([]byte(body), EventStop)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}

	if got.BackgroundTasks != 2 {
		t.Errorf("BackgroundTasks = %d, want 2", got.BackgroundTasks)
	}
}

// codex sends an explicit null for transcript_path when running ephemerally, which
// must not be mistaken for a usable path.
func TestParseHandlesNullTranscriptPath(t *testing.T) {
	body := `{"session_id":"019fe","cwd":"/missions/t","transcript_path":null,
	  "hook_event_name":"Stop","last_assistant_message":null,"turn_id":"t1"}`

	got, err := ParseHookEventBytes([]byte(body), EventStop)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}

	if got.TranscriptPath != "" {
		t.Errorf("TranscriptPath = %q, want empty", got.TranscriptPath)
	}

	if got.LastAssistantMessage != "" {
		t.Errorf("LastAssistantMessage = %q, want empty", got.LastAssistantMessage)
	}
}

// Several fields are documented as optional yet absent in practice on
// session-lifecycle events, so parsing must not require them.
func TestParseToleratesMissingOptionalFields(t *testing.T) {
	body := `{"session_id":"s","cwd":"/","transcript_path":"/t","hook_event_name":"SessionStart","source":"startup"}`

	got, err := ParseHookEventBytes([]byte(body), EventSessionStart)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}

	if got.Source != SourceStartup {
		t.Errorf("Source = %q", got.Source)
	}

	if got.PermissionMode != "" || got.ToolName != "" {
		t.Errorf("absent fields should be empty: %+v", got)
	}
}

// The caller already knows the event from its own arguments, so a payload claiming
// otherwise must not be able to reroute it.
func TestParsePrefersTheCallerSuppliedEvent(t *testing.T) {
	body := `{"session_id":"s","cwd":"/","hook_event_name":"Stop"}`

	got, err := ParseHookEventBytes([]byte(body), EventPermissionRequest)
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}

	if got.Event != EventPermissionRequest {
		t.Errorf("Event = %q, want the caller's %q", got.Event, EventPermissionRequest)
	}
}

func TestParseFallsBackToPayloadEvent(t *testing.T) {
	body := `{"session_id":"s","cwd":"/","hook_event_name":"Stop"}`

	got, err := ParseHookEventBytes([]byte(body), "")
	if err != nil {
		t.Fatalf("ParseBytes: %v", err)
	}

	if got.Event != EventStop {
		t.Errorf("Event = %q", got.Event)
	}
}

func TestParseRejectsEventlessPayload(t *testing.T) {
	if _, err := ParseHookEventBytes([]byte(`{"session_id":"s"}`), ""); err == nil {
		t.Error("expected an error when no event can be determined")
	}
}

func TestParseRejectsMalformedJSON(t *testing.T) {
	if _, err := ParseHookEventBytes([]byte("{not json"), EventStop); err == nil {
		t.Error("expected an error for malformed JSON")
	}
}

func TestIsPlanApproval(t *testing.T) {
	if !(HookEvent{ToolName: ExitPlanModeTool}).IsPlanApproval() {
		t.Error("ExitPlanMode should be recognized")
	}

	if (HookEvent{ToolName: "Bash"}).IsPlanApproval() {
		t.Error("Bash is not a plan approval")
	}
}

// The slug and canonical forms must round-trip, since the hook command line uses one
// and the state machine dispatches on the other.
func TestEventSlugRoundTrip(t *testing.T) {
	for _, event := range AllHookEvents {
		slug := HookEventSlug(event)

		got, err := CanonicalHookEvent(slug)
		if err != nil {
			t.Errorf("CanonicalEvent(%q): %v", slug, err)

			continue
		}

		if got != event {
			t.Errorf("round trip of %q gave %q via %q", event, got, slug)
		}
	}
}

func TestEventSlug(t *testing.T) {
	for _, tc := range []struct{ event, want string }{
		{EventSessionStart, "session-start"},
		{EventPermissionRequest, "permission-request"},
		{EventStop, "stop"},
		{EventUserPromptSubmit, "user-prompt-submit"},
	} {
		if got := HookEventSlug(tc.event); got != tc.want {
			t.Errorf("EventSlug(%q) = %q, want %q", tc.event, got, tc.want)
		}
	}
}

func TestCanonicalEventRejectsUnknown(t *testing.T) {
	if _, err := CanonicalHookEvent("not-an-event"); err == nil {
		t.Error("expected an error for an unknown event")
	}
}
