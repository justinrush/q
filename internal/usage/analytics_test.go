package usage

import (
	"github.com/justinrush/q/internal/mission"
	"os"
	"path/filepath"
	"testing"
)

func TestCodexObservedEffortAndCheckpoint(t *testing.T) {
	path := filepath.Join(t.TempDir(), "rollout.jsonl")
	data := `{"type":"session_meta","payload":{"id":"thread"}}
{"type":"turn_context","payload":{"model":"gpt-6-astra","effort":"high","collaboration_mode":{"mode":"plan","settings":{"reasoning_effort":"low"}}}}
{"type":"token_usage_record","payload":{"thread_id":"thread","response_id":"first","usage":{"input_tokens":100,"output_tokens":10}}}
{"type":"turn_context","payload":{"model":"gpt-6-sol","collaboration_mode":{"mode":"default","settings":{"reasoning_effort":"medium"}}}}
{"type":"token_usage_record","payload":{"thread_id":"thread","response_id":"second","usage":{"input_tokens":200,"output_tokens":20}}}
{"type":"token_usage_record","payload":{"thread_id":"thread","response_id":"first","usage":{"input_tokens":100,"output_tokens":10}}}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	meter := NewCodex(DefaultPricing())
	ms := mission.Mission{TranscriptPath: path, Effort: "low"}
	got, ok, err := meter.Meter(ms)
	if err != nil || !ok {
		t.Fatalf("meter: %v %v", ok, err)
	}
	if len(got.Usage.Breakdown) != 2 {
		t.Fatalf("breakdown %+v", got.Usage.Breakdown)
	}
	a, b := got.Usage.Breakdown[0], got.Usage.Breakdown[1]
	if a.Effort != "high" || a.Phase != mission.PhasePlan || b.Effort != "medium" {
		t.Fatalf("attribution %+v %+v", a, b)
	}
	ms.CodexUsage = got.CodexUsage
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	saved, _, _ := meter.Meter(ms)
	if !saved.Usage.Equal(got.Usage) {
		t.Fatal("checkpoint lost attribution")
	}
}

func TestClaudeObservedEffort(t *testing.T) {
	path := filepath.Join(t.TempDir(), "transcript.jsonl")
	data := `{"type":"user","permissionMode":"plan"}
{"type":"assistant","requestId":"a","effort":"low","perTurnEffort":"high","message":{"model":"claude-opus-5","usage":{"input_tokens":100,"output_tokens":10}}}
{"type":"assistant","requestId":"a","effort":"medium","message":{"model":"claude-opus-5","usage":{"input_tokens":100,"output_tokens":10}}}
{"type":"user","permissionMode":"default"}
{"type":"assistant","requestId":"b","effort":"medium","message":{"model":"claude-opus-5","usage":{"input_tokens":200,"output_tokens":20}}}
{"type":"assistant","requestId":"c","message":{"model":"claude-opus-5","usage":{"input_tokens":50,"output_tokens":5}}}
`
	if err := os.WriteFile(path, []byte(data), 0600); err != nil {
		t.Fatal(err)
	}
	got, ok, err := NewClaude(DefaultPricing()).Meter(mission.Mission{TranscriptPath: path, Effort: "low"})
	if err != nil || !ok || len(got.Usage.Breakdown) != 3 {
		t.Fatalf("meter %+v %v", got, err)
	}
	var total float64
	efforts := map[string]bool{}
	for _, b := range got.Usage.Breakdown {
		total += b.USD
		efforts[b.Effort] = true
	}
	if !efforts[""] || !efforts["high"] || !efforts["medium"] {
		t.Fatalf("efforts %v", efforts)
	}
	if total-got.Usage.USD > 1e-9 || got.Usage.USD-total > 1e-9 {
		t.Fatal("dollars do not reconcile")
	}
}
