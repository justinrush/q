package tui

import (
	"github.com/justinrush/q/internal/mission"
	"strings"
	"testing"
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
