package daemon

import (
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// The Codex usage checkpoint is megabytes on a long mission and only the
// daemon's meter reads it. A client is sent the board on every change, so the
// checkpoint must stay out of everything a client is sent, without being lost
// from the daemon's own copy.
func TestClientsAreNotSentTheCodexUsageCheckpoint(t *testing.T) {
	ms := mission.Mission{
		ID: "ms_111111111111", Tool: mission.ToolCodex, Status: mission.StatusDebrief,
		CodexUsage: mission.CodexUsage{"/rollout.jsonl": {"resp_1": {Model: "gpt"}}},
	}
	snap := mission.Snapshot{Missions: []mission.Mission{ms}}

	sent := map[string]func() ([]byte, error){
		"mission frame":  func() ([]byte, error) { return encodeFrame(api.EventMission, ms) },
		"snapshot frame": func() ([]byte, error) { return encodeFrame(api.EventSnapshot, snap) },
		"state reply": func() ([]byte, error) {
			rec := httptest.NewRecorder()
			writeJSON(rec, 200, snap)

			return rec.Body.Bytes(), nil
		},
		"missions reply": func() ([]byte, error) {
			rec := httptest.NewRecorder()
			writeJSON(rec, 200, snap.Missions)

			return rec.Body.Bytes(), nil
		},
	}

	for name, send := range sent {
		data, err := send()
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}

		if strings.Contains(string(data), "codexUsage") || strings.Contains(string(data), "resp_1") {
			t.Errorf("%s carries the checkpoint: %s", name, data)
		}

		if !strings.Contains(string(data), string(ms.ID)) {
			t.Errorf("%s lost the mission: %s", name, data)
		}
	}

	if len(snap.Missions[0].CodexUsage) == 0 || len(ms.CodexUsage) == 0 {
		t.Error("projecting for a client modified the daemon's copy")
	}
}
