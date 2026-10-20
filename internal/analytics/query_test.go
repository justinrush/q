package analytics

import (
	"context"
	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"net/http/httptest"
	"net/url"
	"testing"
	"testing/fstest"
	"time"
)

func fixture() api.AnalyticsSnapshot {
	start := time.Date(2026, 10, 2, 1, 0, 0, 0, time.UTC)
	records := []mission.AnalyticsRecord{}
	for i, id := range []mission.MissionID{"a", "b"} {
		plan := float64(i + 1)
		implement := plan * 3
		u := mission.Usage{USD: plan + implement, PerModel: map[string]mission.ModelTokens{"astra": {Input: 100}, "sol": {Output: 100}}, Breakdown: []mission.UsageSlice{{Model: "astra", Effort: "high", Phase: mission.PhasePlan, USD: plan, Tokens: mission.ModelTokens{Input: 100}}, {Model: "sol", Effort: "low", Phase: mission.PhaseImplement, USD: implement, Tokens: mission.ModelTokens{Output: 100}}}}
		records = append(records, mission.AnalyticsRecord{ID: id, StartedAt: &start, Status: mission.StatusClosed, Legacy: u})
	}
	return api.AnalyticsSnapshot{Records: records, UpdatedAt: start}
}
func TestWholeWorkflowVsContributionFilters(t *testing.T) {
	got := Query(fixture(), Filter{Page: 1, Model: "astra", Phase: "plan"})
	if got.TotalUSD != 12 || len(got.Workflows) != 1 || got.Workflows[0].Missions != 2 || got.Workflows[0].Median != 6 || got.Workflows[0].Mean != 6 {
		t.Fatalf("workflows %+v", got)
	}
	if len(got.Breakdown) != 1 || got.Breakdown[0].USD != 3 {
		t.Fatalf("filtered spend %+v", got.Breakdown)
	}
	if got.Workflows[0].PlanningShare == nil || *got.Workflows[0].PlanningShare != .25 {
		t.Fatal("planning share")
	}
}
func TestDatesUnknownAndUnpriced(t *testing.T) {
	snapshot := fixture()
	snapshot.Records[0].StartedAt = nil
	snapshot.Records[0].Legacy.Unpriced = true
	snapshot.Records[0].Legacy.Breakdown[0].Unpriced = true
	snapshot.Records[0].Legacy.Breakdown[0].Effort = ""
	got := Query(snapshot, Filter{Page: 1})
	if got.Matched != 1 {
		t.Fatal("undated included")
	}
	got = Query(snapshot, Filter{Page: 1, Undated: true})
	if got.UnknownEffort != 1 || got.Unpriced != 1 {
		t.Fatal("coverage wrong")
	}
	for _, g := range got.Workflows {
		if g.Incomplete > 0 && g.PlanningShare != nil {
			t.Fatal("share shown for incomplete usage")
		}
	}
	got = Query(snapshot, Filter{Page: 1, From: snapshot.UpdatedAt.Add(time.Second)})
	if got.Matched != 0 {
		t.Fatal("lower bound ignored")
	}
	got = Query(snapshot, Filter{Page: 1, To: snapshot.UpdatedAt})
	if got.Matched != 0 {
		t.Fatal("upper bound not exclusive")
	}
}
func TestFiltersRejectUnsupported(t *testing.T) {
	for _, q := range []string{"surprise=x", "page=0", "from=2026-01-01", "status=madeup", "phase=other", "undated=maybe", "model=a&model=b"} {
		if _, err := ParseFilter(mustQuery(q)); err == nil {
			t.Errorf("accepted %s", q)
		}
	}
}
func mustQuery(q string) url.Values { v, _ := url.ParseQuery(q); return v }

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
		{"/api/query", "GET", "", "127.0.0.1:8765", 200},
		{"/api/query?bad=1", "GET", "", "127.0.0.1:8765", 400},
		{"/api/query", "POST", "", "127.0.0.1:8765", 405},
		{"/api/query", "GET", "https://evil.example", "127.0.0.1:8765", 403},
		{"/api/query", "GET", "", "evil.example", 403},
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
