package mission

import (
	"testing"
	"time"
)

// The board sorts by the end of the last turn, so the stamp has to move when a
// turn ends and stay put while the agent sits idle or starts the next one.
func TestLastTurnAtMarksTheEndOfATurn(t *testing.T) {
	ended := Reduce(missionIn(StatusActive), HookEvent{Event: EventStop, SessionID: "sess-1"}, testNow)
	if !ended.Mission.TurnEnded || !ended.Mission.LastTurnAt.Equal(testNow) {
		t.Fatalf("after Stop: TurnEnded=%v LastTurnAt=%v, want true and %v", ended.Mission.TurnEnded, ended.Mission.LastTurnAt, testNow)
	}

	later := testNow.Add(time.Minute)
	idle := Reduce(ended.Mission, HookEvent{Event: EventNotification, SessionID: "sess-1"}, later)
	if !idle.Mission.LastTurnAt.Equal(testNow) {
		t.Errorf("a notification between turns moved LastTurnAt to %v", idle.Mission.LastTurnAt)
	}

	working := Reduce(idle.Mission, HookEvent{Event: EventUserPromptSubmit, SessionID: "sess-1"}, later)
	if working.Mission.TurnEnded || !working.Mission.LastTurnAt.Equal(testNow) {
		t.Errorf("a new turn: TurnEnded=%v LastTurnAt=%v, want false and %v", working.Mission.TurnEnded, working.Mission.LastTurnAt, testNow)
	}
}

func TestSortByTurnRecencyPutsTheNewestTurnFirst(t *testing.T) {
	base := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	started := base.Add(2 * time.Hour)
	missions := []Mission{
		{ID: "ms_old", Order: 0, LastTurnAt: base},
		{ID: "ms_created", Order: 1, CreatedAt: base.Add(30 * time.Minute)},
		{ID: "ms_new", Order: 2, LastTurnAt: base.Add(3 * time.Hour)},
		{ID: "ms_started", Order: 3, CreatedAt: base, StartedAt: &started},
	}

	SortByTurnRecency(missions)

	want := []MissionID{"ms_new", "ms_started", "ms_created", "ms_old"}
	for i, id := range want {
		if missions[i].ID != id {
			t.Fatalf("order = %v, want %v", ids(missions), want)
		}
	}
}

func ids(missions []Mission) []MissionID {
	out := make([]MissionID, len(missions))
	for i, ms := range missions {
		out[i] = ms.ID
	}
	return out
}
