package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/viewport"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/charmbracelet/x/ansi"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/tui/styles"
)

// missionLog keeps the entire launch error accessible even when it spans screens.
// The snapshot supplies diagnostics for local and paired-machine cards alike.
type missionLog struct {
	mission  mission.Mission
	status   string
	queuing  bool
	queued   bool
	title    string
	body     string
	viewport viewport.Model
}

func newMissionLog(ms mission.Mission) *missionLog {
	body := ms.LaunchError
	if body == "" {
		body = "No launch error recorded for this mission."
	}
	return &missionLog{mission: ms, title: "Launch log · " + string(ms.ID), body: body, viewport: viewport.New(1, 1)}
}

func (m *missionLog) Update(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch msg.String() {
	case "esc", "q", "l":
		return nil, emit(modalDismissed{})
	case "c":
		return m, emit(copyMissionLogMsg{Log: m})
	case "t":
		if !m.queuing && !m.queued {
			m.queuing = true
			m.status = "Queueing troubleshooting mission…"
			return m, emit(troubleshootMissionMsg{Log: m})
		}
	case "g":
		m.viewport.GotoTop()
	case "G":
		m.viewport.GotoBottom()
	default:
		var cmd tea.Cmd
		m.viewport, cmd = m.viewport.Update(msg)
		return m, cmd
	}
	return m, nil
}

func (m *missionLog) View(width, height int) string {
	inner := max(1, width-styles.Modal.GetHorizontalFrameSize()-2)
	footer := styles.Footer.Render(ansi.Hardwrap("↑/k ↓/j scroll · pgup/pgdown page · g/G top/end · c copy · t troubleshoot · esc close", inner, true))
	rows := max(1, height-styles.Modal.GetVerticalFrameSize()-4-lipgloss.Height(footer))
	if m.viewport.Width != inner || m.viewport.Height != rows {
		m.viewport.Width, m.viewport.Height = inner, rows
		// Hard wrapping also makes long command arguments and paths readable.
		m.viewport.SetContent(ansi.Hardwrap(strings.ReplaceAll(m.body, "\t", "    "), inner, true))
	}
	content := lipgloss.JoinVertical(lipgloss.Left,
		styles.ModalTitle.Render(styles.Truncate(m.title, inner)), "",
		m.viewport.View(), "",
		styles.Footer.Render(styles.Truncate(m.status, inner)),
		footer,
	)
	return center(styles.Modal.Render(content), width, height)
}

// Log actions retain the displayed snapshot, even if the board updates behind it.
type copyMissionLogMsg struct{ Log *missionLog }
type troubleshootMissionMsg struct{ Log *missionLog }
type missionLogResultMsg struct {
	Log          *missionLog
	Text         string
	Troubleshoot bool
	Err          bool
}
