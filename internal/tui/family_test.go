package tui

import (
	"github.com/justinrush/q/internal/mission"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFamilyProjectionFollowsToggleAndParentLane(t *testing.T) {
	parent := testMission("ms_parent", "parent", "op_1", mission.StatusActive)
	child := testMission("ms_child", "child", "op_1", mission.StatusDebrief)
	child.ParentMissionID = parent.ID
	board := boardWith([]mission.Operation{testOperation("op_1", "API", 0)}, []mission.Mission{child, parent})
	board.SetSize(200, 40)
	board.lane = 1
	active, debrief := 1, 3
	if len(board.missionsIn(active)) != 1 || len(board.missionsIn(debrief)) != 1 {
		t.Fatal("disabled view should remain flat")
	}
	if strings.Contains(board.View(), "└─") {
		t.Fatal("disabled view exposed nesting")
	}
	board.nestedMissions = true
	got := board.missionsIn(active)
	if len(got) != 2 || got[0].ID != parent.ID || got[1].ID != child.ID || len(board.missionsIn(debrief)) != 0 {
		t.Fatalf("family projection: %+v", got)
	}
	if !strings.Contains(board.View(), "└─") || !strings.Contains(board.View(), "debrief") {
		t.Fatal("enabled view omitted child connector or status")
	}
	board.nestedMissions = false
	for _, group := range board.FullHelp() {
		for _, key := range group {
			if key.Help().Key == "N" || key.Help().Key == "I" {
				t.Fatal("disabled help exposed child control")
			}
		}
	}
}

func TestChildReorderTargetsSiblingAcrossStatuses(t *testing.T) {
	parent := testMission("ms_parent", "parent", "op_1", mission.StatusActive)
	first := testMission("ms_first", "first", "op_1", mission.StatusDebrief)
	first.ParentMissionID = parent.ID
	first.SiblingOrder = 0
	second := testMission("ms_second", "second", "op_1", mission.StatusAwaiting)
	second.ParentMissionID = parent.ID
	second.SiblingOrder = 1
	board := boardWith([]mission.Operation{testOperation("op_1", "API", 0)}, []mission.Mission{parent, first, second})
	board.nestedMissions = true
	board.lane = 1
	board.cursor[1] = 1
	cmd := board.reorder(1)
	if cmd == nil {
		t.Fatal("reorder produced no command")
	}
	msg, ok := cmd().(reorderMsg)
	if !ok || msg.Mission.ID != first.ID || msg.Target.ID != second.ID {
		t.Fatalf("reorder: %#v", cmd())
	}
}

func TestFoldHidesChildrenUnderParent(t *testing.T) {
	parent := testMission("ms_parent", "parent", "op_1", mission.StatusActive)
	first := testMission("ms_first", "first", "op_1", mission.StatusDebrief)
	first.ParentMissionID = parent.ID
	second := testMission("ms_second", "second", "op_1", mission.StatusAwaiting)
	second.ParentMissionID = parent.ID
	second.SiblingOrder = 1
	board := boardWith([]mission.Operation{testOperation("op_1", "API", 0)}, []mission.Mission{parent, first, second})
	board.SetSize(200, 40)
	board.nestedMissions = true
	board.lane = 1
	board.cursor[1] = 2 // on the second child

	board.toggleFold()
	got := board.missionsIn(1)
	if len(got) != 1 || got[0].ID != parent.ID {
		t.Fatalf("folded lane: %+v", got)
	}
	if sel, _ := board.Selected(); sel.ID != parent.ID {
		t.Fatalf("selection after fold: %s", sel.ID)
	}
	if view := board.View(); !strings.Contains(view, "▸2") || strings.Contains(view, "└─") {
		t.Fatal("folded parent should show its child count and hide the children")
	}

	// Folding survives a fresh snapshot.
	board.SetSnapshot(board.snapshot)
	if len(board.missionsIn(1)) != 1 {
		t.Fatal("fold lost on snapshot")
	}

	board.toggleFold()
	if len(board.missionsIn(1)) != 3 {
		t.Fatal("unfold did not restore children")
	}
}

func TestFoldIgnoresChildlessMission(t *testing.T) {
	solo := testMission("ms_solo", "solo", "op_1", mission.StatusActive)
	board := boardWith([]mission.Operation{testOperation("op_1", "API", 0)}, []mission.Mission{solo})
	board.nestedMissions = true
	board.lane = 1
	board.toggleFold()
	if len(board.folded) != 0 {
		t.Fatal("folded a mission with no children")
	}
}

// Lanes an agent has run in show the card whose turn ended most recently first,
// whatever order they were filed in. Briefing keeps its hand-set order, since
// that is the order the scheduler launches in.
func TestRunLanesSortByLastTurnNewestFirst(t *testing.T) {
	base := time.Date(2026, 11, 20, 9, 0, 0, 0, time.UTC)
	older := testMission("ms_older", "older", "op_1", mission.StatusDebrief)
	older.Order, older.LastTurnAt = 0, base
	newer := testMission("ms_newer", "newer", "op_1", mission.StatusDebrief)
	newer.Order, newer.LastTurnAt = 1, base.Add(time.Hour)
	first := testMission("ms_first", "first", "op_1", mission.StatusBriefing)
	first.Order, first.CreatedAt = 0, base
	second := testMission("ms_second", "second", "op_1", mission.StatusBriefing)
	second.Order, second.CreatedAt = 1, base.Add(time.Hour)
	board := boardWith([]mission.Operation{testOperation("op_1", "API", 0)}, []mission.Mission{older, newer, first, second})

	debrief := board.missionsIn(slices.Index(mission.Lanes, mission.StatusDebrief))
	if len(debrief) != 2 || debrief[0].ID != newer.ID || debrief[1].ID != older.ID {
		t.Errorf("debrief lane = %v, want newer then older", debrief)
	}
	briefing := board.missionsIn(slices.Index(mission.Lanes, mission.StatusBriefing))
	if len(briefing) != 2 || briefing[0].ID != first.ID || briefing[1].ID != second.ID {
		t.Errorf("briefing lane = %v, want first then second", briefing)
	}

	board.lane = slices.Index(mission.Lanes, mission.StatusDebrief)
	if msg, ok := board.reorder(1)().(toastMsg); !ok || !msg.err {
		t.Errorf("reorder in a sorted lane = %#v, want an error toast", msg)
	}
}
