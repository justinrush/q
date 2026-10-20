package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/mission"
)

// rowsBoard returns a narrow board with lanes drawn as rows.
//
// At 60x40 the rows stack as: briefing (one card) on lines 0-8, active (two
// cards) on 9-17, awaiting (empty) on 18-19, debrief (one card) on 20-28, and the
// collapsed done summary on 29-30.
func rowsBoard(t *testing.T) *Board {
	t.Helper()

	ops := []mission.Operation{testOperation("op_1", "API", 0)}
	board := boardWith(ops, []mission.Mission{
		testMission("ms_0_0", "briefing 1", "op_1", mission.StatusBriefing),
		testMission("ms_1_0", "active 1", "op_1", mission.StatusActive),
		testMission("ms_1_1", "active 2", "op_1", mission.StatusActive),
		testMission("ms_3_0", "debrief 1", "op_1", mission.StatusDebrief),
		testMission("ms_4_0", "closed 1", "op_1", mission.StatusClosed),
	})
	board.SetSize(60, 40)
	board.SetHorizontal(true)

	return board
}

// The point of rows is a narrow window, so it must never fall back to showing a
// single lane the way columns do.
func TestHorizontalLayoutNeverFocusesOneLane(t *testing.T) {
	for _, width := range []int{0, 20, 60, focusModeBelow - 1, 200} {
		layout := computeLayout(width, 40, false, 2, true)
		if layout.Focus {
			t.Errorf("width %d: horizontal layout chose focus mode", width)
		}

		if layout.CardsPerRow < 1 || layout.CardWidth < MinCardWidth {
			t.Errorf("width %d: %d cards of %d wide", width, layout.CardsPerRow, layout.CardWidth)
		}

		if width >= MinCardWidth {
			used := layout.CardsPerRow*layout.CardWidth + (layout.CardsPerRow-1)*laneGap
			if used > width {
				t.Errorf("width %d: a row of cards takes %d", width, used)
			}
		}
	}
}

func TestHorizontalViewShowsEveryLaneInANarrowWindow(t *testing.T) {
	view := rowsBoard(t).View()

	for _, lane := range mission.Lanes {
		if label := strings.ToUpper(lane.Label()); !strings.Contains(view, label) {
			t.Errorf("view is missing the %s lane:\n%s", label, view)
		}
	}

	for _, line := range strings.Split(view, "\n") {
		if w := lipgloss.Width(line); w > 60 {
			t.Errorf("line is %d wide, over the 60 available: %q", w, line)
		}
	}

	if got := strings.Count(view, "\n") + 1; got > 39 {
		t.Errorf("view is %d lines, over the 39 available", got)
	}
}

// When the rows do not all fit, the focused one must still be on screen.
func TestHorizontalViewScrollsToTheFocusedLane(t *testing.T) {
	board := rowsBoard(t)
	board.SetSize(60, 12)

	board.Update(keyMsg("4"))

	view := board.View()
	if !strings.Contains(view, "DEBRIEF") || !strings.Contains(view, "debrief 1") {
		t.Errorf("focused debrief lane is not visible:\n%s", view)
	}

	if strings.Contains(view, "BRIEFING 1") {
		t.Errorf("the first lane should have scrolled away:\n%s", view)
	}
}

func TestHorizontalKeysFollowTheScreen(t *testing.T) {
	board := rowsBoard(t)

	board.Update(keyMsg("j"))
	if board.lane != 1 {
		t.Fatalf("j: lane = %d, want 1", board.lane)
	}

	board.Update(tea.KeyMsg{Type: tea.KeyRight})
	if board.cursor[1] != 1 {
		t.Errorf("→: cursor = %d, want 1", board.cursor[1])
	}

	board.Update(keyMsg("h"))
	if board.cursor[1] != 0 {
		t.Errorf("h: cursor = %d, want 0", board.cursor[1])
	}

	board.Update(keyMsg("k"))
	if board.lane != 0 {
		t.Errorf("k: lane = %d, want 0", board.lane)
	}

	// K/J carry the card between lanes; H/L reorder it.
	if cmd := board.Update(keyMsg("J")); cmd == nil {
		t.Errorf("J did not move the card")
	}
}

func TestOrientationKeyToggles(t *testing.T) {
	board := boardWith(nil, nil)

	board.Update(keyMsg("v"))
	if !board.Horizontal() {
		t.Fatalf("v did not switch to rows")
	}

	if got := board.Help()[0].Help().Desc; got != "card" {
		t.Errorf("rows help for ←/h = %q, want card", got)
	}

	board.Update(keyMsg("v"))
	if board.Horizontal() {
		t.Errorf("v did not switch back to columns")
	}

	if got := board.Help()[0].Help().Desc; got != "lane" {
		t.Errorf("columns help for ←/h = %q, want lane", got)
	}
}

func TestHorizontalClickSelectsTheCardUnderThePointer(t *testing.T) {
	board := rowsBoard(t)

	// The active row's cards sit on lines 10-17; the second starts at x=30.
	board.HandleMouse(mouseClickMsg(35, 12), 35, 12)
	if board.lane != 1 || board.cursor[1] != 1 {
		t.Errorf("selected lane %d card %d, want lane 1 card 1", board.lane, board.cursor[1])
	}

	// The awaiting header.
	board.HandleMouse(mouseClickMsg(5, 18), 5, 18)
	if board.lane != 2 {
		t.Errorf("header click: lane = %d, want 2", board.lane)
	}

	// The collapsed done summary expands.
	board.HandleMouse(mouseClickMsg(5, 30), 5, 30)
	if board.lane != 4 || !board.doneExpanded {
		t.Errorf("done click: lane %d expanded %v, want lane 4 expanded", board.lane, board.doneExpanded)
	}
}
