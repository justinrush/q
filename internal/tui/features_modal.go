package tui

import (
	"context"

	"github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/tui/styles"
)

type featuresSavedMsg struct {
	memory, nested bool
	err            error
}
type featuresModal struct {
	memory, nested               bool
	initialMemory, initialNested bool
	row                          int
	save                         func(context.Context, bool, bool) error
	ctx                          context.Context
}

func newFeaturesModal(memory, nested bool, save func(context.Context, bool, bool) error, ctx context.Context) *featuresModal {
	return &featuresModal{memory: memory, nested: nested, initialMemory: memory, initialNested: nested, save: save, ctx: ctx}
}
func (m *featuresModal) Update(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch msg.String() {
	case "esc":
		return nil, emit(modalDismissed{})
	case "up", "k", "down", "j":
		m.row = 1 - m.row
	case " ", "space":
		if m.row == 0 {
			m.memory = !m.memory
		} else {
			m.nested = !m.nested
		}
	case "enter":
		if m.memory == m.initialMemory && m.nested == m.initialNested {
			return nil, emit(modalDismissed{})
		}
		if m.save == nil {
			return nil, emit(featuresSavedMsg{err: context.Canceled})
		}
		return nil, func() tea.Msg {
			err := m.save(m.ctx, m.memory, m.nested)
			return featuresSavedMsg{memory: m.memory, nested: m.nested, err: err}
		}
	}
	return m, nil
}
func (m *featuresModal) View(width, height int) string {
	mark := func(v bool) string {
		if v {
			return "[x]"
		}
		return "[ ]"
	}
	memory, nested := "  "+mark(m.memory)+" Memory", "  "+mark(m.nested)+" Nested missions"
	if m.row == 0 {
		memory = ">" + memory[1:]
	} else {
		nested = ">" + nested[1:]
	}
	body := lipgloss.JoinVertical(lipgloss.Left, styles.ModalTitle.Render("Features"), "", memory, nested, "", styles.Footer.Render("↑/↓ select · space toggle · enter save · esc cancel"))
	return center(styles.Modal.Render(body), width, height)
}
