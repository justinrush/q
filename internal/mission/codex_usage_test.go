package mission

import "testing"

func TestCodexCheckpointIsLocalAndCloned(t *testing.T) {
	ms := Mission{Tool: ToolCodex, TranscriptPath: "old"}
	ms.SetTranscriptPath("new")
	ms.CodexUsage["old"] = map[string]CodexResponse{"r": {Model: "planner", Tokens: ModelTokens{Output: 10}}}
	cloned := cloneMission(ms)
	cloned.CodexUsage["old"]["r"] = CodexResponse{}
	cloned.SetTranscriptPath("third")
	if ms.CodexUsage["old"]["r"].Tokens.Output != 10 || len(ms.CodexUsage) != 2 {
		t.Fatal("checkpoint alias mutated original")
	}
	if ms.Shared().CodexUsage != nil {
		t.Fatal("local checkpoint leaked to peer")
	}
	ms.SetTranscriptPath("")
	if ms.TranscriptPath != "new" {
		t.Fatal("empty metadata erased path")
	}
}
