package analytics

import (
	"context"
	"net/http/httptest"
	"testing"
	"testing/fstest"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

func slice(model, effort string, phase mission.Phase, usd float64) mission.UsageSlice {
	return mission.UsageSlice{Model: model, Effort: effort, Phase: phase, USD: usd, Tokens: mission.ModelTokens{Input: 100}}
}

func record(id mission.MissionID, slices ...mission.UsageSlice) mission.AnalyticsRecord {
	start := time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC)
	u := mission.Usage{PerModel: map[string]mission.ModelTokens{}, Breakdown: slices}
	for _, s := range slices {
		u.USD += s.USD
		u.PerModel[s.Model] = u.PerModel[s.Model].Add(s.Tokens)
	}
	return mission.AnalyticsRecord{ID: id, StartedAt: &start, Status: mission.StatusClosed, Tool: "claude", Legacy: u}
}

func fixture() api.AnalyticsSnapshot {
	return api.AnalyticsSnapshot{Records: []mission.AnalyticsRecord{
		record("planned", slice("astra", "medium", mission.PhasePlan, 1), slice("astra", "low", mission.PhaseImplement, 3)),
		record("direct", slice("opus", "high", mission.PhaseImplement, 5)),
		record("mixed", slice("astra", "low", mission.PhasePlan, 1), slice("astra", "high", mission.PhasePlan, 1), slice("opus", "high", mission.PhaseImplement, 2)),
		record("legacy", slice("", "", "", 4)),
		{ID: "empty", Status: mission.StatusClosed},
	}}
}

func TestProjectLabelsObservedPhases(t *testing.T) {
	got := Project(fixture())
	if got.Unmeasured != 1 || len(got.Missions) != 4 {
		t.Fatalf("measured %d, unmeasured %d", len(got.Missions), got.Unmeasured)
	}
	want := map[mission.MissionID]string{
		"planned": "astra / medium → astra / low",
		"direct":  "none → opus / high",
		"mixed":   "mixed → opus / high",
		"legacy":  "unknown → unknown",
	}
	for _, f := range got.Missions {
		if f.Workflow != want[f.ID] {
			t.Errorf("%s: workflow %q, want %q", f.ID, f.Workflow, want[f.ID])
		}
		if f.Workflow != f.Plan+" → "+f.Implement {
			t.Errorf("%s: workflow disagrees with its phases", f.ID)
		}
	}
	if got.Missions[0].USD != 4 || len(got.Missions[0].Slices) != 2 {
		t.Fatalf("slices not carried: %+v", got.Missions[0])
	}
}

type fakeSource struct{ err error }

func (f fakeSource) Refresh() (bool, error) { return false, nil }
func (f fakeSource) Analytics(context.Context) (api.AnalyticsSnapshot, error) {
	return fixture(), f.err
}

func TestServerBoundary(t *testing.T) {
	handler := Handler(fakeSource{}, fstest.MapFS{"index.html": {Data: []byte("dashboard")}}, "127.0.0.1:8765")
	for _, tc := range []struct {
		path, method, origin, host string
		code                       int
	}{
		{"/api/facts", "GET", "", "127.0.0.1:8765", 200},
		{"/api/facts?bad=1", "GET", "", "127.0.0.1:8765", 400},
		{"/api/facts", "POST", "", "127.0.0.1:8765", 405},
		{"/api/facts", "GET", "https://evil.example", "127.0.0.1:8765", 403},
		{"/api/facts", "GET", "", "evil.example", 403},
		{"/api/query", "GET", "", "127.0.0.1:8765", 404},
		{"/v1/state", "GET", "", "127.0.0.1:8765", 404},
		{"/", "GET", "", "127.0.0.1:8765", 200},
	} {
		r := httptest.NewRequest(tc.method, tc.path, nil)
		r.Host = tc.host
		r.Header.Set("Origin", tc.origin)
		w := httptest.NewRecorder()
		handler.ServeHTTP(w, r)
		if w.Code != tc.code {
			t.Errorf("%+v: got %d", tc, w.Code)
		}
		if w.Header().Get("Access-Control-Allow-Origin") != "" {
			t.Fatal("CORS exposed")
		}
	}
}
