package daemon

import (
	"encoding/json"
	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"io"
	"net/http"
	"strings"
	"testing"
)

func TestAnalyticsAPIExcludesConversationAndRetainsDeletion(t *testing.T) {
	server, client, base := newTestServer(t)
	err := server.svc.store.Mutate("seed", func(s *mission.Snapshot) error {
		s.PutMission(mission.Mission{ID: "ms_analytics", Name: "Cost fixture", Prompt: "PRIVATE_PROMPT", TranscriptPath: "PRIVATE_PATH", Usage: mission.Usage{USD: 7, PerModel: map[string]mission.ModelTokens{"sol": {Input: 100}}}})
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := server.svc.store.Mutate("delete", func(s *mission.Snapshot) error { s.DeleteMission("ms_analytics"); return nil }); err != nil {
		t.Fatal(err)
	}
	resp := do(t, client, http.MethodGet, base+"/v1/analytics", nil)
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != 200 || strings.Contains(string(raw), "PRIVATE_") {
		t.Fatalf("bad response: %d %s", resp.StatusCode, raw)
	}
	var snapshot api.AnalyticsSnapshot
	if err := json.Unmarshal(raw, &snapshot); err != nil {
		t.Fatal(err)
	}
	if len(snapshot.Records) != 1 || !snapshot.Records[0].Deleted || snapshot.Records[0].Usage().USD != 7 {
		t.Fatalf("history %+v", snapshot)
	}
	req, err := http.NewRequestWithContext(t.Context(), http.MethodGet, base+"/v1/analytics", nil)
	if err != nil {
		t.Fatal(err)
	}
	unauth, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = unauth.Body.Close() }()
	if unauth.StatusCode != http.StatusForbidden {
		t.Fatal("analytics endpoint bypassed authentication")
	}
}
