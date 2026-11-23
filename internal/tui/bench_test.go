package tui

import (
	"encoding/json"
	"strconv"
	"strings"
	"testing"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/mission/missiontest"
	"github.com/justinrush/q/internal/tui/styles"
	"github.com/muesli/termenv"
)

var tuiBoards = []struct {
	name  string
	shape missiontest.Shape
}{
	{"small", missiontest.Small},
	{"work", missiontest.Work},
}

// trueColor renders in full color for the benchmark, as a real terminal does:
// the tests render plain text, which skips most of lipgloss's work.
func trueColor(b *testing.B) {
	lipgloss.SetColorProfile(termenv.TrueColor)
	b.Cleanup(func() { lipgloss.SetColorProfile(termenv.Ascii) })
}

// wire is a board as the daemon streams it.
func wire(snap mission.Snapshot) mission.Snapshot {
	snap.Missions = append([]mission.Mission(nil), snap.Missions...)
	for i := range snap.Missions {
		snap.Missions[i].ToolHandoff.Plan = ""
		snap.Missions[i].ToolHandoff.Conversation = ""
		snap.Missions[i].CodexUsage = nil
	}

	return snap
}

func benchApp(b *testing.B, snap mission.Snapshot, nested bool) *App {
	b.Helper()

	app := New(nil, Options{Features: Features{NestedMissions: nested}})
	app.Update(tea.WindowSizeMsg{Width: 200, Height: 50})
	app.applySnapshot(wire(snap))

	return app
}

func eachBoard(b *testing.B, run func(b *testing.B, snap mission.Snapshot)) {
	for _, board := range tuiBoards {
		b.Run(board.name, func(b *testing.B) { run(b, missiontest.Board(board.shape)) })
	}
}

// BenchmarkApplyEvent is the board taking one frame from the daemon's stream:
// decoding it and rebuilding its lanes. A snapshot frame arrives whenever the
// board connects or reconnects; a mission frame each time any mission changes.
func BenchmarkApplyEvent(b *testing.B) {
	b.Run("snapshot", func(b *testing.B) {
		eachBoard(b, func(b *testing.B, snap mission.Snapshot) {
			data, err := json.Marshal(wire(snap))
			if err != nil {
				b.Fatal(err)
			}

			app := benchApp(b, snap, false)
			event := api.Event{Name: api.EventSnapshot, Data: data}

			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				app.applyEvent(event)
			}

			b.ReportMetric(float64(len(data)), "bytes/frame")
		})
	})

	b.Run("mission", func(b *testing.B) {
		eachBoard(b, func(b *testing.B, snap mission.Snapshot) {
			var changed mission.Mission

			for _, m := range snap.Missions {
				if !m.Status.Terminal() && (changed.ID == "" || len(m.CodexUsage) > len(changed.CodexUsage)) {
					changed = m
				}
			}

			data, err := json.Marshal(wire(mission.Snapshot{Missions: []mission.Mission{changed}}).Missions[0])
			if err != nil {
				b.Fatal(err)
			}

			app := benchApp(b, snap, false)
			event := api.Event{Name: api.EventMission, Data: data}

			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				app.applyEvent(event)
			}

			b.ReportMetric(float64(len(data)), "bytes/frame")
		})
	})
}

// BenchmarkView is one redraw, which bubbletea does after every message:
// each key, each stream frame, each ping and each tick.
func BenchmarkView(b *testing.B) {
	views := []struct {
		name       string
		horizontal bool
		nested     bool
		operations bool
	}{
		{name: "lanes"},
		{name: "rows", horizontal: true},
		{name: "nested", nested: true},
		{name: "operations", operations: true},
	}

	for _, view := range views {
		b.Run(view.name, func(b *testing.B) {
			eachBoard(b, func(b *testing.B, snap mission.Snapshot) {
				trueColor(b)

				app := benchApp(b, snap, view.nested)
				app.board.SetHorizontal(view.horizontal)

				if view.operations {
					app.active = tabOperations
				}

				b.ReportAllocs()
				b.ResetTimer()

				for range b.N {
					_ = app.View()
				}
			})
		})
	}
}

// BenchmarkTruncate is the cut every card makes to fit each of its lines.
func BenchmarkTruncate(b *testing.B) {
	for _, n := range []int{80, 400, 4000} {
		b.Run(strconv.Itoa(n), func(b *testing.B) {
			s := strings.Repeat("the agent said something long ", n/30+1)[:n]

			b.ReportAllocs()

			for range b.N {
				_ = styles.Truncate(s, 40)
			}
		})
	}
}
