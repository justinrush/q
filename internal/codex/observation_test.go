package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRecordedPlanSettings(t *testing.T) {
	var observer rolloutObserver
	got, err := observer.read("testdata/plan-settings.jsonl", "planning-thread")
	if err != nil {
		t.Fatal(err)
	}
	if !got.approved || got.settings.Mode != "default" || got.settings.Model != "gpt-6-astra" || got.settings.Effort != "low" {
		t.Fatalf("observation: %+v", got)
	}
	again, err := observer.read("testdata/plan-settings.jsonl", "planning-thread")
	if err != nil || again != got {
		t.Fatalf("unchanged poll: %+v, %v", again, err)
	}
}

func TestObservationPartialAndOversizedRecords(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	first := `{"timestamp":"2026-10-20T21:19:17Z","type":"turn_context","payload":{"turn_id":"t1","model":"planner","effort":"low","collaboration_mode":{"mode":"plan"}}}`
	second := strings.ReplaceAll(first, "t1", "t2")
	if err := os.WriteFile(path, []byte(first+"\n"+strings.Repeat("x", 3*1024*1024)+"\n"+second[:50]), 0600); err != nil {
		t.Fatal(err)
	}
	var observer rolloutObserver
	got, err := observer.read(path, "session")
	if err != nil || got.settings.TurnID != "t1" {
		t.Fatalf("first read: %+v %v", got, err)
	}
	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString(second[50:] + "\n"); err != nil {
		t.Fatal(err)
	}
	f.Close()
	got, err = observer.read(path, "session")
	if err != nil || got.settings.TurnID != "t2" {
		t.Fatalf("append: %+v %v", got, err)
	}
	if err = os.WriteFile(path, []byte(first+"\n"), 0600); err != nil {
		t.Fatal(err)
	}
	got, err = observer.read(path, "session")
	if err != nil || got.settings.TurnID != "t1" {
		t.Fatalf("truncate: %+v %v", got, err)
	}
}

func TestObservationDoesNotApproveClarification(t *testing.T) {
	var o rolloutObservation
	o.session = "thread"
	o.apply([]byte(`{"type":"turn_context","payload":{"collaboration_mode":{"mode":"plan"}}}`))
	o.apply([]byte(`{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"text":"<proposed_plan>Plan</proposed_plan>"}]}}`))
	o.apply([]byte(`{"type":"response_item","payload":{"type":"message","role":"user","content":[{"text":"yes, also cover migrations"}]}}`))
	o.apply([]byte(`{"type":"turn_context","payload":{"collaboration_mode":{"mode":"plan"}}}`))
	if o.approved {
		t.Fatal("clarification approved plan")
	}
}
