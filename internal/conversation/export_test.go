package conversation

import (
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

func TestExportVisibleContextAndLatestPlan(t *testing.T) {
	for _, tc := range []struct {
		name string
		tool mission.Tool
		data string
	}{
		{"claude", mission.ToolClaude, `{"type":"user","message":{"role":"user","content":"Keep existing behavior"}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"thinking","thinking":"private reasoning"},{"type":"tool_use","name":"ExitPlanMode","input":{"plan":"Old plan"}}]}}
{"type":"assistant","message":{"role":"assistant","content":[{"type":"tool_use","name":"Read","input":{"file_path":"main.go"}},{"type":"tool_use","name":"ExitPlanMode","input":{"plan":"Approved revision"}}]}}
{"type":"user","message":{"role":"user","content":[{"type":"tool_result","content":"file contents"}]}}`},
		{"codex", mission.ToolCodex, `{"type":"response_item","payload":{"type":"message","role":"user","content":[{"type":"input_text","text":"Keep existing behavior"}]}}
{"type":"response_item","payload":{"type":"reasoning","summary":[{"text":"private reasoning"}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<proposed_plan>Old plan</proposed_plan>"}]}}
{"type":"response_item","payload":{"type":"message","role":"assistant","content":[{"type":"output_text","text":"<proposed_plan>Approved revision</proposed_plan>"}]}}
{"type":"response_item","payload":{"type":"function_call","name":"Read","arguments":"main.go"}}
{"type":"response_item","payload":{"type":"function_call_output","output":"file contents"}}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, _, err := Read(strings.NewReader(tc.data), tc.tool)
			if err != nil {
				t.Fatal(err)
			}
			if got.Plan != "Approved revision" {
				t.Fatalf("plan: %q", got.Plan)
			}
			for _, want := range []string{"Keep existing behavior", "main.go", "file contents"} {
				if !strings.Contains(got.Conversation, want) {
					t.Fatalf("missing %s: %s", want, got.Conversation)
				}
			}
			if strings.Contains(got.Conversation, "private reasoning") {
				t.Fatal("exported reasoning")
			}
		})
	}
}

func TestMalformedTranscriptIsNotSilentlyTruncated(t *testing.T) {
	if _, _, err := Read(strings.NewReader("{broken"), mission.ToolClaude); err == nil {
		t.Fatal("expected error")
	}
}
