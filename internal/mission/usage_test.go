package mission

import (
	"strings"
	"testing"
	"time"
)

func TestUsageEmpty(t *testing.T) {
	cases := []struct {
		name  string
		usage Usage
		want  bool
	}{
		{name: "never measured", usage: Usage{}, want: true},
		{
			name:  "measured but nothing consumed yet",
			usage: Usage{PerModel: map[string]ModelTokens{"m": {}}},
			want:  true,
		},
		{
			name:  "consumed",
			usage: Usage{PerModel: map[string]ModelTokens{"m": {Output: 1}}},
		},
		{
			name: "one model idle and another not",
			usage: Usage{PerModel: map[string]ModelTokens{
				"idle": {}, "busy": {CacheRead: 1},
			}},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.usage.Empty(); got != tc.want {
				t.Fatalf("Empty() = %v, want %v", got, tc.want)
			}
		})
	}
}

// Equal ignores when a measurement was taken, so that re-metering an idle
// session on a timer does not look like a change on every tick.
func TestUsageEqual(t *testing.T) {
	base := Usage{
		PerModel: map[string]ModelTokens{"m": {Input: 1, Output: 2}},
		USD:      3,
		At:       time.Date(2026, 9, 5, 16, 0, 0, 0, time.UTC),
	}

	later := base.Clone()
	later.At = base.At.Add(time.Hour)

	grown := base.Clone()
	grown.PerModel["m"] = ModelTokens{Input: 1, Output: 3}

	repriced := base.Clone()
	repriced.USD = 4

	incomplete := base.Clone()
	incomplete.Unpriced = true

	extraModel := base.Clone()
	extraModel.PerModel["n"] = ModelTokens{Input: 1}

	cases := []struct {
		name  string
		other Usage
		want  bool
	}{
		{name: "identical", other: base.Clone(), want: true},
		{name: "same reading taken later", other: later, want: true},
		{name: "more tokens", other: grown},
		{name: "different total", other: repriced},
		{name: "newly incomplete", other: incomplete},
		{name: "a model appeared", other: extraModel},
		{name: "nothing measured", other: Usage{}},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := base.Equal(tc.other); got != tc.want {
				t.Fatalf("Equal() = %v, want %v", got, tc.want)
			}
		})
	}
}

// A clone must not share the map, or a mission handed out of the store could be
// mutated through it while the daemon holds no lock.
func TestUsageCloneDetachesTheMap(t *testing.T) {
	original := Usage{PerModel: map[string]ModelTokens{"m": {Input: 1}}}

	clone := original.Clone()
	clone.PerModel["m"] = ModelTokens{Input: 99}

	if original.PerModel["m"].Input != 1 {
		t.Fatalf("original mutated through its clone: %+v", original.PerModel["m"])
	}
}

func TestLimitLiveAndDescribe(t *testing.T) {
	now := time.Date(2026, 9, 5, 16, 0, 0, 0, time.UTC)

	cases := []struct {
		name     string
		limit    Limit
		wantLive bool
		wantText string
	}{
		{
			name:     "a window that has not reopened",
			limit:    Limit{Kind: "five_hour", ResetsAt: now.Add(time.Hour)},
			wantLive: true,
			wantText: "5-hour limit hit",
		},
		{
			name:  "a window that already reopened",
			limit: Limit{Kind: "five_hour", ResetsAt: now.Add(-time.Second)},
		},
		{name: "no window at all", limit: Limit{}},
		{
			name:     "a weekly window",
			limit:    Limit{Kind: "weekly", ResetsAt: now.Add(time.Hour)},
			wantLive: true,
			wantText: "weekly limit hit",
		},
		{
			name:     "a window this version of q has no name for",
			limit:    Limit{Kind: "monthly", ResetsAt: now.Add(time.Hour)},
			wantLive: true,
			wantText: "monthly limit hit",
		},
		{
			name:     "an unnamed window",
			limit:    Limit{ResetsAt: now.Add(time.Hour)},
			wantLive: true,
			wantText: "usage limit hit",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.limit.Live(now); got != tc.wantLive {
				t.Fatalf("Live() = %v, want %v", got, tc.wantLive)
			}

			if tc.wantText == "" {
				return
			}

			if got := tc.limit.Describe(); !strings.Contains(got, tc.wantText) {
				t.Fatalf("Describe() = %q, want it to contain %q", got, tc.wantText)
			}
		})
	}
}

func TestSnapshotLimits(t *testing.T) {
	now := time.Date(2026, 9, 5, 16, 0, 0, 0, time.UTC)
	live := Limit{Tool: ToolClaude, Kind: "five_hour", ResetsAt: now.Add(time.Hour)}
	expired := Limit{Tool: ToolClaude, Kind: "five_hour", ResetsAt: now.Add(-time.Hour)}

	t.Run("a live limit is found", func(t *testing.T) {
		snap := Snapshot{Limits: []Limit{live}}

		got, ok := snap.Limit(ToolClaude, now)
		if !ok || got.Kind != "five_hour" {
			t.Fatalf("Limit() = %+v, %v", got, ok)
		}
	})

	t.Run("an expired limit is not", func(t *testing.T) {
		snap := Snapshot{Limits: []Limit{expired}}

		if _, ok := snap.Limit(ToolClaude, now); ok {
			t.Fatal("an expired limit should not be reported")
		}
	})

	t.Run("another agent's limit is not", func(t *testing.T) {
		snap := Snapshot{Limits: []Limit{live}}

		if _, ok := snap.Limit(ToolCodex, now); ok {
			t.Fatal("codex should not inherit claude's limit")
		}
	})

	t.Run("a newer limit replaces the agent's old one", func(t *testing.T) {
		snap := Snapshot{Limits: []Limit{live}}
		newer := Limit{Tool: ToolClaude, Kind: "weekly", ResetsAt: now.Add(48 * time.Hour)}

		snap.PutLimit(newer, now)

		if len(snap.Limits) != 1 || snap.Limits[0].Kind != "weekly" {
			t.Fatalf("Limits = %+v, want just the weekly one", snap.Limits)
		}
	})

	t.Run("storing an expired limit stores nothing", func(t *testing.T) {
		var snap Snapshot

		snap.PutLimit(expired, now)

		if len(snap.Limits) != 0 {
			t.Fatalf("Limits = %+v, want none", snap.Limits)
		}
	})

	t.Run("storing prunes another agent's expired limit", func(t *testing.T) {
		codexExpired := Limit{Tool: ToolCodex, ResetsAt: now.Add(-time.Hour)}
		snap := Snapshot{Limits: []Limit{codexExpired}}

		snap.PutLimit(live, now)

		if len(snap.Limits) != 1 || snap.Limits[0].Tool != ToolClaude {
			t.Fatalf("Limits = %+v, want only claude's", snap.Limits)
		}
	})

	t.Run("agents keep their own limits", func(t *testing.T) {
		codexLive := Limit{Tool: ToolCodex, Kind: "five_hour", ResetsAt: now.Add(time.Hour)}
		snap := Snapshot{Limits: []Limit{codexLive}}

		snap.PutLimit(live, now)

		if len(snap.Limits) != 2 {
			t.Fatalf("Limits = %+v, want both agents", snap.Limits)
		}
	})
}
