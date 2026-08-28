package state

import (
	"testing"
	"time"

	"github.com/justinrush/q/internal/domain"
)

// mission builds a minimal mission for query tests.
func mission(id string, status domain.Status, order int) domain.Mission {
	return domain.Mission{
		ID:          domain.MissionID(id),
		OperationID: domain.OperationID("op_aabbccddeeff"),
		Status:      status,
		Order:       order,
		Tool:        domain.ToolClaude,
	}
}

func TestOperationAndMissionLookup(t *testing.T) {
	snap := Snapshot{
		Operations: []domain.Operation{{ID: "op_aabbccddeeff", Name: "one"}},
		Missions:   []domain.Mission{mission("ms_111111111111", domain.StatusBriefing, 0)},
	}

	if got, ok := snap.Operation("op_aabbccddeeff"); !ok || got.Name != "one" {
		t.Errorf("Operation lookup failed: %+v %v", got, ok)
	}

	if _, ok := snap.Operation("op_000000000000"); ok {
		t.Error("expected miss for unknown operation")
	}

	if got, ok := snap.Mission("ms_111111111111"); !ok || got.ID != "ms_111111111111" {
		t.Errorf("Mission lookup failed: %+v %v", got, ok)
	}

	if _, ok := snap.Mission("ms_000000000000"); ok {
		t.Error("expected miss for unknown mission")
	}
}

// The reducer identifies a mission by session id when the hook environment is
// unavailable, and must not match across tools.
func TestMissionBySessionIsToolScoped(t *testing.T) {
	claude := mission("ms_111111111111", domain.StatusActive, 0)
	claude.AgentSessionID = "shared-id"

	codex := mission("ms_222222222222", domain.StatusActive, 1)
	codex.Tool = domain.ToolCodex
	codex.AgentSessionID = "shared-id"

	snap := Snapshot{Missions: []domain.Mission{claude, codex}}

	got, ok := snap.MissionBySession(domain.ToolCodex, "shared-id")
	if !ok || got.ID != "ms_222222222222" {
		t.Errorf("MissionBySession returned %q, want the codex mission", got.ID)
	}

	// An empty session id must never match, or every codex mission that has not yet
	// reported its SessionStart would collide.
	if _, ok := snap.MissionBySession(domain.ToolClaude, ""); ok {
		t.Error("empty session id must not match any mission")
	}
}

func TestMissionByDir(t *testing.T) {
	withDir := mission("ms_111111111111", domain.StatusActive, 0)
	withDir.MissionDir = "/data/missions/operation--mission"

	snap := Snapshot{Missions: []domain.Mission{withDir, mission("ms_222222222222", domain.StatusBriefing, 1)}}

	if got, ok := snap.MissionByDir("/data/missions/operation--mission"); !ok || got.ID != "ms_111111111111" {
		t.Errorf("MissionByDir returned %q, %v", got.ID, ok)
	}

	// Draft missions have no directory yet; an empty query must not match them.
	if _, ok := snap.MissionByDir(""); ok {
		t.Error("empty dir must not match any mission")
	}
}

func TestMissionsInLaneOrdering(t *testing.T) {
	snap := Snapshot{Missions: []domain.Mission{
		mission("ms_333333333333", domain.StatusDebrief, 2),
		mission("ms_111111111111", domain.StatusDebrief, 0),
		mission("ms_222222222222", domain.StatusDebrief, 1),
		mission("ms_444444444444", domain.StatusClosed, 0),
	}}

	got := snap.MissionsInLane(domain.StatusDebrief)
	if len(got) != 3 {
		t.Fatalf("len = %d, want 3", len(got))
	}

	for i, want := range []string{"ms_111111111111", "ms_222222222222", "ms_333333333333"} {
		if string(got[i].ID) != want {
			t.Errorf("MissionsInLane[%d] = %q, want %q", i, got[i].ID, want)
		}
	}
}

// Order ties must resolve deterministically, or cards would shuffle between
// renders and between processes.
func TestMissionsInLaneBreaksTiesStably(t *testing.T) {
	base := time.Date(2026, 8, 17, 0, 0, 0, 0, time.UTC)

	older := mission("ms_bbbbbbbbbbbb", domain.StatusBriefing, 0)
	older.CreatedAt = base

	newer := mission("ms_aaaaaaaaaaaa", domain.StatusBriefing, 0)
	newer.CreatedAt = base.Add(time.Hour)

	snap := Snapshot{Missions: []domain.Mission{newer, older}}

	got := snap.MissionsInLane(domain.StatusBriefing)
	if got[0].ID != "ms_bbbbbbbbbbbb" {
		t.Errorf("expected the older mission first, got %q", got[0].ID)
	}

	// Identical timestamps fall back to id order.
	sameA := mission("ms_aaaaaaaaaaaa", domain.StatusDebrief, 0)
	sameB := mission("ms_bbbbbbbbbbbb", domain.StatusDebrief, 0)
	snap = Snapshot{Missions: []domain.Mission{sameB, sameA}}

	if got := snap.MissionsInLane(domain.StatusDebrief); got[0].ID != "ms_aaaaaaaaaaaa" {
		t.Errorf("expected id tiebreak, got %q first", got[0].ID)
	}
}

func TestActiveMissionsForOperationExcludesDone(t *testing.T) {
	snap := Snapshot{Missions: []domain.Mission{
		mission("ms_111111111111", domain.StatusClosed, 0),
		mission("ms_222222222222", domain.StatusDebrief, 1),
		mission("ms_333333333333", domain.StatusBriefing, 2),
	}}

	got := snap.ActiveMissionsForOperation("op_aabbccddeeff")
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	for _, mission := range got {
		if mission.Status == domain.StatusClosed {
			t.Errorf("mission %q should have been excluded", mission.ID)
		}
	}
}

func TestNextOrderAppendsWithinLane(t *testing.T) {
	snap := Snapshot{Missions: []domain.Mission{
		mission("ms_111111111111", domain.StatusDebrief, 0),
		mission("ms_222222222222", domain.StatusDebrief, 4),
		mission("ms_333333333333", domain.StatusBriefing, 9),
	}}

	if got := snap.NextOrder(domain.StatusDebrief); got != 5 {
		t.Errorf("NextOrder(debrief) = %d, want 5", got)
	}

	if got := snap.NextOrder(domain.StatusClosed); got != 0 {
		t.Errorf("NextOrder(done) = %d, want 0 for an empty lane", got)
	}
}

// Colors are assigned as the lowest unused index rather than hashed, so adjacent
// operations never share a stripe.
func TestNextColorIdxAssignsLowestUnused(t *testing.T) {
	snap := Snapshot{Operations: []domain.Operation{
		{ID: "op_111111111111", ColorIdx: 0},
		{ID: "op_222222222222", ColorIdx: 2},
	}}

	if got := snap.NextColorIdx(); got != 1 {
		t.Errorf("NextColorIdx() = %d, want the gap at 1", got)
	}
}

func TestNextColorIdxRecyclesFreedIndices(t *testing.T) {
	snap := Snapshot{Operations: []domain.Operation{
		{ID: "op_111111111111", ColorIdx: 0},
		{ID: "op_222222222222", ColorIdx: 1},
	}}

	snap.DeleteOperation("op_111111111111")

	if got := snap.NextColorIdx(); got != 0 {
		t.Errorf("NextColorIdx() = %d, want the recycled 0", got)
	}
}

func TestNextColorIdxWrapsWhenPaletteExhausted(t *testing.T) {
	var snap Snapshot

	for i := range domain.PaletteSize {
		snap.Operations = append(snap.Operations, domain.Operation{
			ID:       domain.OperationID("op_" + string(rune('a'+i)) + "00000000000"),
			ColorIdx: i,
		})
	}

	got := snap.NextColorIdx()
	if got < 0 || got >= domain.PaletteSize {
		t.Errorf("NextColorIdx() = %d, want a value inside the palette", got)
	}
}

func TestPutReplacesRatherThanDuplicates(t *testing.T) {
	var snap Snapshot

	snap.PutOperation(domain.Operation{ID: "op_aabbccddeeff", Name: "first"})
	snap.PutOperation(domain.Operation{ID: "op_aabbccddeeff", Name: "second"})

	if len(snap.Operations) != 1 {
		t.Fatalf("len(Operations) = %d, want 1", len(snap.Operations))
	}

	if snap.Operations[0].Name != "second" {
		t.Errorf("Name = %q, want %q", snap.Operations[0].Name, "second")
	}

	snap.PutMission(mission("ms_111111111111", domain.StatusBriefing, 0))
	snap.PutMission(mission("ms_111111111111", domain.StatusDebrief, 0))

	if len(snap.Missions) != 1 {
		t.Fatalf("len(Missions) = %d, want 1", len(snap.Missions))
	}

	if snap.Missions[0].Status != domain.StatusDebrief {
		t.Errorf("Status = %q, want %q", snap.Missions[0].Status, domain.StatusDebrief)
	}
}

func TestDeleteReportsPresence(t *testing.T) {
	snap := Snapshot{
		Operations: []domain.Operation{{ID: "op_aabbccddeeff"}},
		Missions:   []domain.Mission{mission("ms_111111111111", domain.StatusBriefing, 0)},
	}

	if !snap.DeleteOperation("op_aabbccddeeff") {
		t.Error("DeleteOperation should report the operation was present")
	}

	if snap.DeleteOperation("op_aabbccddeeff") {
		t.Error("DeleteOperation should report a second delete as absent")
	}

	if !snap.DeleteMission("ms_111111111111") {
		t.Error("DeleteMission should report the mission was present")
	}

	if snap.DeleteMission("ms_111111111111") {
		t.Error("DeleteMission should report a second delete as absent")
	}
}
