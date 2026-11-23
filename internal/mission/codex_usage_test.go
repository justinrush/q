package mission

import "testing"

// Snapshot copies share a checkpoint, so every way of changing one must leave
// the original as it was: SetTranscriptPath replaces the outer map, and an
// edit goes through Clone.
func TestCodexCheckpointIsLocalAndCopiedOnWrite(t *testing.T) {
	ms := Mission{Tool: ToolCodex, TranscriptPath: "old"}
	ms.SetTranscriptPath("new")
	ms.CodexUsage["old"] = map[string]CodexResponse{"r": {Model: "planner", Tokens: ModelTokens{Output: 10}}}

	cloned := cloneMission(ms)
	cloned.SetTranscriptPath("third")

	edited := cloned.CodexUsage.Clone()
	edited["old"]["r"] = CodexResponse{}
	cloned.CodexUsage = edited

	if ms.CodexUsage["old"]["r"].Tokens.Output != 10 || len(ms.CodexUsage) != 2 {
		t.Fatal("changing a copy's checkpoint changed the original")
	}

	if !ms.CodexUsage.Equal(cloneMission(ms).CodexUsage) || ms.CodexUsage.Equal(cloned.CodexUsage) {
		t.Fatal("Equal disagrees with the checkpoints' contents")
	}

	if ms.Shared().CodexUsage != nil {
		t.Fatal("local checkpoint leaked to peer")
	}

	ms.SetTranscriptPath("")

	if ms.TranscriptPath != "new" {
		t.Fatal("empty metadata erased path")
	}
}
