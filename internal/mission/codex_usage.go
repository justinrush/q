package mission

import "maps"

// CodexResponse is a measured response, retained even if its rollout disappears.
// Response IDs allow replayed records to be counted once across transcripts.
type CodexResponse struct {
	Model  string      `json:"model"`
	Tokens ModelTokens `json:"tokens"`
}

// CodexUsage maps local rollout paths to their measured responses. A nil entry
// remembers a discovered path that has not yet been measured.
type CodexUsage map[string]map[string]CodexResponse

// Clone isolates the checkpoint from concurrent store readers.
func (u CodexUsage) Clone() CodexUsage {
	if u == nil {
		return nil
	}
	out := make(CodexUsage, len(u))
	for path, responses := range u {
		out[path] = maps.Clone(responses)
	}
	return out
}

// Equal compares checkpoints without depending on map iteration order.
func (u CodexUsage) Equal(other CodexUsage) bool {
	return maps.EqualFunc(u, other, maps.Equal[map[string]CodexResponse, map[string]CodexResponse])
}

// SetTranscriptPath remembers both sides of a Codex thread replacement before
// updating the active path. Empty metadata must not erase a known path.
func (t *Mission) SetTranscriptPath(path string) {
	if path == "" {
		return
	}
	if t.Tool == ToolCodex {
		t.CodexUsage = maps.Clone(t.CodexUsage)
		if t.CodexUsage == nil {
			t.CodexUsage = CodexUsage{}
		}
		for _, p := range []string{t.TranscriptPath, path} {
			if p != "" {
				if _, ok := t.CodexUsage[p]; !ok {
					t.CodexUsage[p] = nil
				}
			}
		}
	}
	t.TranscriptPath = path
}
