package tui

import (
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/justinrush/q/internal/mission"
)

func TestMissionLogKey(t *testing.T) {
	b := NewBoard()
	if cmd := b.Update(keyMsg("l")); cmd != nil {
		t.Fatal("empty lane opened log")
	}
	ms := mission.Mission{ID: "ms_log", Status: mission.Lanes[0], LaunchError: "fetch failed\nclock skew"}
	b.SetSnapshot(mission.Snapshot{Missions: []mission.Mission{ms}})
	cmd := b.Update(keyMsg("l"))
	if cmd == nil {
		t.Fatal("l did not open log")
	}
	msg, ok := cmd().(missionLogMsg)
	if !ok || msg.Mission.LaunchError != ms.LaunchError {
		t.Fatal("lost launch diagnostics")
	}
	app := &App{}
	app.handleIntent(msg)
	if _, ok := app.modal.(*missionLog); !ok {
		t.Fatal("log modal not opened")
	}
	b.Update(tea.KeyMsg{Type: tea.KeyRight})
	if b.lane != 1 {
		t.Fatal("right arrow no longer navigates")
	}
}

func TestMissionLogScrollAndResize(t *testing.T) {
	body := "command " + strings.Repeat("x", 200) + "\n" + strings.Repeat("output\n", 100) + "ticketbooth clock skew"
	m := newMissionLog(mission.Mission{ID: "ms_log", LaunchError: body})
	m.View(60, 20)
	if m.viewport.TotalLineCount() < 105 {
		t.Fatal("long lines were not wrapped")
	}
	m.Update(keyMsg("G"))
	if !strings.Contains(m.View(60, 20), "ticketbooth clock skew") {
		t.Fatal("cannot reach end of error")
	}
	m.View(40, 15)
	m.Update(keyMsg("G"))
	if !strings.Contains(m.View(40, 15), "clock skew") {
		t.Fatal("resize lost diagnostics")
	}
	if next, _ := m.Update(tea.KeyMsg{Type: tea.KeyEsc}); next != nil {
		t.Fatal("escape did not dismiss")
	}
	empty := newMissionLog(mission.Mission{})
	if !strings.Contains(empty.View(80, 24), "No launch error recorded") {
		t.Fatal("missing empty state")
	}
}
