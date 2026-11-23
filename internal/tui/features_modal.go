package tui

import (
	"context"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/tui/styles"
)

// Features are the opt-in feature flags the F modal toggles.
type Features struct {
	Memory         bool
	NestedMissions bool
	// DebriefPanes splits an editor pane per changed repo when a debrief opens.
	DebriefPanes bool
}

type featuresSavedMsg struct {
	features Features
	err      error
}

// featureRow is one toggle in the modal.
type featureRow struct {
	label string
	flag  func(*Features) *bool
}

var featureRows = []featureRow{
	{"Memory", func(f *Features) *bool { return &f.Memory }},
	{"Nested missions", func(f *Features) *bool { return &f.NestedMissions }},
	{"Debrief editor panes", func(f *Features) *bool { return &f.DebriefPanes }},
}

type featuresModal struct {
	features, initial Features
	row               int
	save              func(context.Context, Features) error
	ctx               context.Context
}

func newFeaturesModal(features Features, save func(context.Context, Features) error, ctx context.Context) *featuresModal {
	return &featuresModal{features: features, initial: features, save: save, ctx: ctx}
}

func (m *featuresModal) Update(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return nil, emit(modalDismissed{})
	case "up", "k":
		m.row = (m.row + len(featureRows) - 1) % len(featureRows)
	case "down", "j":
		m.row = (m.row + 1) % len(featureRows)
	case " ", "space":
		flag := featureRows[m.row].flag(&m.features)
		*flag = !*flag
	case "enter":
		if m.features == m.initial {
			return nil, emit(modalDismissed{})
		}
		if m.save == nil {
			return nil, emit(featuresSavedMsg{err: context.Canceled})
		}
		features := m.features
		return nil, func() tea.Msg {
			return featuresSavedMsg{features: features, err: m.save(m.ctx, features)}
		}
	}
	return m, nil
}

func (m *featuresModal) View(width, height int) string {
	lines := []string{styles.ModalTitle.Render("Features"), ""}
	for i, row := range featureRows {
		mark := "[ ]"
		if *row.flag(&m.features) {
			mark = "[x]"
		}
		cursor := " "
		if i == m.row {
			cursor = ">"
		}
		lines = append(lines, cursor+" "+mark+" "+row.label)
	}
	lines = append(lines, "", styles.Footer.Render("↑/↓ select · space toggle · enter save · esc cancel"))
	return center(styles.Modal.Render(lipgloss.JoinVertical(lipgloss.Left, lines...)), width, height)
}
