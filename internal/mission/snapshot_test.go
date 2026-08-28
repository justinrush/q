package mission

import (
	"testing"
	"time"
)

// mission builds a minimal mission for query tests.
func ms(id string, status Status, order int) Mission {
	return Mission{
		ID:          MissionID(id),
		OperationID: OperationID("op_aabbccddeeff"),
		Status:      status,
		Order:       order,
		Tool:        ToolClaude,
	}
}

func TestOperationAndMissionLookup(t *testing.T) {
	snap := Snapshot{
		Operations: []Operation{{ID: "op_aabbccddeeff", Name: "one"}},
		Missions:   []Mission{ms("ms_111111111111", StatusBriefing, 0)},
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
	claude := ms("ms_111111111111", StatusActive, 0)
	claude.AgentSessionID = "shared-id"

	codex := ms("ms_222222222222", StatusActive, 1)
	codex.Tool = ToolCodex
	codex.AgentSessionID = "shared-id"

	snap := Snapshot{Missions: []Mission{claude, codex}}

	got, ok := snap.MissionBySession(ToolCodex, "shared-id")
	if !ok || got.ID != "ms_222222222222" {
		t.Errorf("MissionBySession returned %q, want the codex mission", got.ID)
	}

	// An empty session id must never match, or every codex mission that has not yet
	// reported its SessionStart would collide.
	if _, ok := snap.MissionBySession(ToolClaude, ""); ok {
		t.Error("empty session id must not match any mission")
	}
}

func TestMissionByDir(t *testing.T) {
	withDir := ms("ms_111111111111", StatusActive, 0)
	withDir.MissionDir = "/data/missions/operation--mission"

	snap := Snapshot{Missions: []Mission{withDir, ms("ms_222222222222", StatusBriefing, 1)}}

	if got, ok := snap.MissionByDir("/data/missions/operation--mission"); !ok || got.ID != "ms_111111111111" {
		t.Errorf("MissionByDir returned %q, %v", got.ID, ok)
	}

	// Draft missions have no directory yet; an empty query must not match them.
	if _, ok := snap.MissionByDir(""); ok {
		t.Error("empty dir must not match any mission")
	}
}

func TestMissionsInLaneOrdering(t *testing.T) {
	snap := Snapshot{Missions: []Mission{
		ms("ms_333333333333", StatusDebrief, 2),
		ms("ms_111111111111", StatusDebrief, 0),
		ms("ms_222222222222", StatusDebrief, 1),
		ms("ms_444444444444", StatusClosed, 0),
	}}

	got := snap.MissionsInLane(StatusDebrief)
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

	older := ms("ms_bbbbbbbbbbbb", StatusBriefing, 0)
	older.CreatedAt = base

	newer := ms("ms_aaaaaaaaaaaa", StatusBriefing, 0)
	newer.CreatedAt = base.Add(time.Hour)

	snap := Snapshot{Missions: []Mission{newer, older}}

	got := snap.MissionsInLane(StatusBriefing)
	if got[0].ID != "ms_bbbbbbbbbbbb" {
		t.Errorf("expected the older mission first, got %q", got[0].ID)
	}

	// Identical timestamps fall back to id order.
	sameA := ms("ms_aaaaaaaaaaaa", StatusDebrief, 0)
	sameB := ms("ms_bbbbbbbbbbbb", StatusDebrief, 0)
	snap = Snapshot{Missions: []Mission{sameB, sameA}}

	if got := snap.MissionsInLane(StatusDebrief); got[0].ID != "ms_aaaaaaaaaaaa" {
		t.Errorf("expected id tiebreak, got %q first", got[0].ID)
	}
}

func TestActiveMissionsForOperationExcludesDone(t *testing.T) {
	snap := Snapshot{Missions: []Mission{
		ms("ms_111111111111", StatusClosed, 0),
		ms("ms_222222222222", StatusDebrief, 1),
		ms("ms_333333333333", StatusBriefing, 2),
	}}

	got := snap.ActiveMissionsForOperation("op_aabbccddeeff")
	if len(got) != 2 {
		t.Fatalf("len = %d, want 2", len(got))
	}

	for _, ms := range got {
		if ms.Status == StatusClosed {
			t.Errorf("mission %q should have been excluded", ms.ID)
		}
	}
}

func TestNextOrderAppendsWithinLane(t *testing.T) {
	snap := Snapshot{Missions: []Mission{
		ms("ms_111111111111", StatusDebrief, 0),
		ms("ms_222222222222", StatusDebrief, 4),
		ms("ms_333333333333", StatusBriefing, 9),
	}}

	if got := snap.NextOrder(StatusDebrief); got != 5 {
		t.Errorf("NextOrder(debrief) = %d, want 5", got)
	}

	if got := snap.NextOrder(StatusClosed); got != 0 {
		t.Errorf("NextOrder(done) = %d, want 0 for an empty lane", got)
	}
}

// Colors are assigned as the lowest unused index rather than hashed, so adjacent
// operations never share a stripe.
func TestNextColorIdxAssignsLowestUnused(t *testing.T) {
	snap := Snapshot{Operations: []Operation{
		{ID: "op_111111111111", ColorIdx: 0},
		{ID: "op_222222222222", ColorIdx: 2},
	}}

	if got := snap.NextColorIdx(); got != 1 {
		t.Errorf("NextColorIdx() = %d, want the gap at 1", got)
	}
}

func TestNextColorIdxRecyclesFreedIndices(t *testing.T) {
	snap := Snapshot{Operations: []Operation{
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

	for i := range PaletteSize {
		snap.Operations = append(snap.Operations, Operation{
			ID:       OperationID("op_" + string(rune('a'+i)) + "00000000000"),
			ColorIdx: i,
		})
	}

	got := snap.NextColorIdx()
	if got < 0 || got >= PaletteSize {
		t.Errorf("NextColorIdx() = %d, want a value inside the palette", got)
	}
}

func TestPutReplacesRatherThanDuplicates(t *testing.T) {
	var snap Snapshot

	snap.PutOperation(Operation{ID: "op_aabbccddeeff", Name: "first"})
	snap.PutOperation(Operation{ID: "op_aabbccddeeff", Name: "second"})

	if len(snap.Operations) != 1 {
		t.Fatalf("len(Operations) = %d, want 1", len(snap.Operations))
	}

	if snap.Operations[0].Name != "second" {
		t.Errorf("Name = %q, want %q", snap.Operations[0].Name, "second")
	}

	snap.PutMission(ms("ms_111111111111", StatusBriefing, 0))
	snap.PutMission(ms("ms_111111111111", StatusDebrief, 0))

	if len(snap.Missions) != 1 {
		t.Fatalf("len(Missions) = %d, want 1", len(snap.Missions))
	}

	if snap.Missions[0].Status != StatusDebrief {
		t.Errorf("Status = %q, want %q", snap.Missions[0].Status, StatusDebrief)
	}
}

func TestDeleteReportsPresence(t *testing.T) {
	snap := Snapshot{
		Operations: []Operation{{ID: "op_aabbccddeeff"}},
		Missions:   []Mission{ms("ms_111111111111", StatusBriefing, 0)},
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
