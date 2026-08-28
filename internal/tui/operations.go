package tui

import (
	"fmt"
	"strings"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/state"
	"github.com/justinrush/q/internal/tui/keys"
	"github.com/justinrush/q/internal/tui/styles"
)

// listPaneWidth is how much of the operation view the list occupies before the detail
// pane takes the rest.
const listPaneRatio = 40

// Operations is the operation-management view: a list on the left, the selected operation's
// detail on the right.
type Operations struct {
	keys keys.Operations

	snapshot state.Snapshot
	width    int
	height   int
	cursor   int
}

// The messages the operation view emits.
type (
	// newOperationMsg asks for the new-operation form.
	newOperationMsg struct{}
	// editOperationMsg asks for an operation's editor.
	editOperationMsg struct{ Operation domain.Operation }
	// deleteOperationMsg asks to confirm deleting an operation.
	deleteOperationMsg struct{ Operation domain.Operation }
	// focusOperationMsg asks the board to filter to an operation and show it.
	focusOperationMsg struct{ Operation domain.Operation }
)

// NewOperations returns an empty operation view.
func NewOperations() *Operations {
	return &Operations{keys: keys.NewOperations()}
}

// operationAction is a keypress handler.
type operationAction func(*Operations) tea.Cmd

// operationActions maps keys to handlers.
var operationActions = map[string]operationAction{
	"up":    (*Operations).selectPrev,
	"k":     (*Operations).selectPrev,
	"down":  (*Operations).selectNext,
	"j":     (*Operations).selectNext,
	"a":     (*Operations).newOperation,
	"n":     (*Operations).newOperation,
	"e":     (*Operations).editOperation,
	"d":     (*Operations).deleteOperation,
	"enter": (*Operations).focusOperation,
}

// SetSnapshot replaces the view's data.
func (t *Operations) SetSnapshot(snap state.Snapshot) {
	t.snapshot = snap
	t.cursor = clamp(t.cursor, max(0, len(snap.Operations)-1))
}

// SetSize records the terminal size.
func (t *Operations) SetSize(width, height int) {
	t.width, t.height = width, height
}

// Update handles a keypress.
func (t *Operations) Update(msg tea.KeyMsg) tea.Cmd {
	if handler, ok := operationActions[msg.String()]; ok {
		return handler(t)
	}

	return nil
}

// Help returns the view's bindings.
func (t *Operations) Help() []key.Binding { return t.keys.ShortHelp() }

// FullHelp returns the view's grouped bindings.
func (t *Operations) FullHelp() [][]key.Binding { return t.keys.FullHelp() }

// Title names the view.
func (t *Operations) Title() string { return "Operations" }

// Selected returns the focused operation.
func (t *Operations) Selected() (domain.Operation, bool) {
	if len(t.snapshot.Operations) == 0 {
		return domain.Operation{}, false
	}

	return t.snapshot.Operations[clamp(t.cursor, len(t.snapshot.Operations)-1)], true
}

func (t *Operations) selectPrev() tea.Cmd {
	t.cursor = clamp(t.cursor-1, max(0, len(t.snapshot.Operations)-1))

	return nil
}

func (t *Operations) selectNext() tea.Cmd {
	t.cursor = clamp(t.cursor+1, max(0, len(t.snapshot.Operations)-1))

	return nil
}

func (t *Operations) newOperation() tea.Cmd { return emit(newOperationMsg{}) }

func (t *Operations) editOperation() tea.Cmd {
	operation, ok := t.Selected()
	if !ok {
		return nil
	}

	return emit(editOperationMsg{Operation: operation})
}

func (t *Operations) deleteOperation() tea.Cmd {
	operation, ok := t.Selected()
	if !ok {
		return nil
	}

	return emit(deleteOperationMsg{Operation: operation})
}

func (t *Operations) focusOperation() tea.Cmd {
	operation, ok := t.Selected()
	if !ok {
		return nil
	}

	return emit(focusOperationMsg{Operation: operation})
}

// View renders the operation view.
func (t *Operations) View() string {
	if len(t.snapshot.Operations) == 0 {
		return styles.CardDetail.Render("No operations yet. Press a to create one.\n\n" +
			"An operation is an area of investigation: a summary plus the git repos it spans.\n" +
			"Its summary and repo list are handed to every agent you start on it.")
	}

	listWidth := t.width * listPaneRatio / 100
	if listWidth < 24 {
		listWidth = 24
	}

	detailWidth := t.width - listWidth - 2
	if detailWidth < 20 {
		// Too narrow for two panes; show the list alone.
		return t.renderList(t.width)
	}

	return lipgloss.JoinHorizontal(lipgloss.Top,
		t.renderList(listWidth),
		"  ",
		t.renderDetail(detailWidth),
	)
}

// renderList draws the operation list.
func (t *Operations) renderList(width int) string {
	rows := make([]string, 0, len(t.snapshot.Operations)+1)

	rows = append(rows, styles.LaneHeaderFocused.Render("THREADS"))

	for i, operation := range t.snapshot.Operations {
		rows = append(rows, t.renderRow(i, operation, width))
	}

	return strings.Join(rows, "\n")
}

// renderRow draws one operation row, tinted with its stripe color.
func (t *Operations) renderRow(i int, operation domain.Operation, width int) string {
	swatch := lipgloss.NewStyle().Foreground(styles.OperationColor(operation.ColorIdx)).Render("█")

	active := len(t.snapshot.ActiveMissionsForOperation(operation.ID))
	total := len(t.snapshot.MissionsForOperation(operation.ID))

	counts := styles.CardDetail.Render(fmt.Sprintf(" %d/%d", active, total))

	name := styles.Truncate(operation.Name, max(1, width-10))
	if i == t.cursor {
		return "▸ " + swatch + " " + lipgloss.NewStyle().Foreground(styles.Accent).Bold(true).Render(name) + counts
	}

	return "  " + swatch + " " + name + counts
}

// renderDetail draws the selected operation's summary, repos, and missions.
func (t *Operations) renderDetail(width int) string {
	operation, ok := t.Selected()
	if !ok {
		return ""
	}

	rows := []string{
		lipgloss.NewStyle().Foreground(styles.OperationColor(operation.ColorIdx)).Bold(true).Render(operation.Name),
		styles.CardDetail.Render(string(operation.ID)),
		"",
	}

	rows = append(rows, t.renderSummary(operation, width)...)
	rows = append(rows, t.renderRepos(operation, width)...)
	rows = append(rows, t.renderMissionCounts(operation, width)...)

	return strings.Join(rows, "\n")
}

// renderSummary draws the operation's summary block.
func (t *Operations) renderSummary(operation domain.Operation, width int) []string {
	if strings.TrimSpace(operation.Summary) == "" {
		return []string{styles.CardDetail.Render("(no summary yet; press e to add one)"), ""}
	}

	wrapped := lipgloss.NewStyle().Width(width).Render(operation.Summary)

	return []string{wrapped, ""}
}

// renderRepos lists the operation's repos.
func (t *Operations) renderRepos(operation domain.Operation, width int) []string {
	if len(operation.Repos) == 0 {
		return []string{styles.CardDetail.Render("no repos yet; press e to add some"), ""}
	}

	rows := []string{styles.FieldLabel.Render("REPOS")}

	for _, repo := range operation.Repos {
		branch := repo.DefaultBranch
		if branch == "" {
			branch = "?"
		}

		rows = append(rows, styles.Truncate(
			fmt.Sprintf("  %s  %s", repo.Name, styles.CardDetail.Render(branch+"  "+repo.Path)), width))
	}

	return append(rows, "")
}

// renderMissionCounts summarizes the operation's missions by lane.
func (t *Operations) renderMissionCounts(operation domain.Operation, width int) []string {
	missions := t.snapshot.MissionsForOperation(operation.ID)
	if len(missions) == 0 {
		return []string{styles.CardDetail.Render("no missions yet")}
	}

	counts := map[domain.Status]int{}
	for _, mission := range missions {
		counts[mission.Status]++
	}

	parts := make([]string, 0, len(domain.Lanes))

	for _, lane := range domain.Lanes {
		if counts[lane] > 0 {
			parts = append(parts, fmt.Sprintf("%d %s", counts[lane], lane.Label()))
		}
	}

	return []string{
		styles.FieldLabel.Render("TASKS"),
		styles.Truncate("  "+strings.Join(parts, " · "), width),
		"",
		styles.Footer.Render("  enter to show these on the board"),
	}
}
