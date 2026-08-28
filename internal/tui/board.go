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

// Board is the kanban view.
type Board struct {
	keys keys.Board

	snapshot state.Snapshot
	width    int
	height   int

	// lane is the focused column, indexed as domain.Lanes.
	lane int
	// cursor is the selected card within each lane, so moving between columns
	// remembers where you were in each.
	cursor []int
	// scroll is the first visible card in each lane.
	scroll []int
	// doneExpanded shows the done column at full width.
	doneExpanded bool
	// filter limits the board to one operation, empty for all.
	filter domain.OperationID
	// follow is a mission the selection should jump to once the next snapshot arrives.
	//
	// A moved card lands at the end of its new lane, which is rarely the index the
	// cursor already held. Without this, stepping a card across two lanes would move
	// it once and then move whatever else happened to be under the cursor.
	follow domain.MissionID
}

// NewBoard returns an empty board.
func NewBoard() *Board {
	return &Board{
		keys:   keys.NewBoard(),
		cursor: make([]int, len(domain.Lanes)),
		scroll: make([]int, len(domain.Lanes)),
	}
}

// boardAction is a keypress handler. Returning a command lets an action talk to the
// daemon without the board knowing how.
type boardAction func(*Board) tea.Cmd

// boardActions maps keys to handlers.
//
// A dispatch table rather than a switch, so each handler stays a few lines and the
// Update method stays a router. It also makes "every bound key has a handler"
// something a test can check.
var boardActions = map[string]boardAction{
	"left":   (*Board).focusPrevLane,
	"h":      (*Board).focusPrevLane,
	"right":  (*Board).focusNextLane,
	"l":      (*Board).focusNextLane,
	"up":     (*Board).selectPrev,
	"k":      (*Board).selectPrev,
	"down":   (*Board).selectNext,
	"j":      (*Board).selectNext,
	"g":      (*Board).selectFirst,
	"G":      (*Board).selectLast,
	"H":      (*Board).moveCardLeft,
	"L":      (*Board).moveCardRight,
	"K":      (*Board).reorderUp,
	"J":      (*Board).reorderDown,
	"z":      (*Board).toggleDone,
	"esc":    (*Board).clearFilter,
	"enter":  (*Board).openDebrief,
	"ctrl+o": (*Board).openDebrief,
	"m":      (*Board).messageAgent,
	"n":      (*Board).newMission,
	"e":      (*Board).editMission,
	"d":      (*Board).deleteMission,
	"x":      (*Board).deleteMission,
	"p":      (*Board).togglePlan,
	"/":      (*Board).filterByOperation,
	"s":      (*Board).statusMenu,
}

// SetSnapshot replaces the board's data and re-bounds the selection.
func (b *Board) SetSnapshot(snap state.Snapshot) {
	b.snapshot = snap
	b.clampSelection()
	b.applyFollow()
}

// applyFollow moves the selection onto a mission that was just acted on.
//
// This is what makes a run of moves act on the same card: the daemon decides where a
// moved mission lands, so the cursor can only catch up once the new state arrives.
func (b *Board) applyFollow() {
	if b.follow == "" {
		return
	}

	for lane := range domain.Lanes {
		for i, mission := range b.missionsIn(lane) {
			if mission.ID != b.follow {
				continue
			}

			b.lane = lane
			b.cursor[lane] = i
			b.follow = ""
			b.ensureVisible()

			return
		}
	}

	// The mission is gone, or filtered out of view; stop looking for it.
	b.follow = ""
}

// Follow asks the selection to jump to a mission when its next state arrives.
func (b *Board) Follow(id domain.MissionID) { b.follow = id }

// SetSize records the terminal size.
func (b *Board) SetSize(width, height int) {
	b.width, b.height = width, height
}

// Update handles a keypress.
func (b *Board) Update(msg tea.KeyMsg) tea.Cmd {
	if handler, ok := boardActions[msg.String()]; ok {
		return handler(b)
	}

	// The digit keys jump directly to a lane.
	if key.Matches(msg, b.keys.Lane) {
		if n := int(msg.String()[0] - '1'); n >= 0 && n < len(domain.Lanes) {
			b.lane = n
			b.clampSelection()
		}
	}

	return nil
}

// Help returns the board's bindings.
func (b *Board) Help() []key.Binding { return b.keys.ShortHelp() }

// FullHelp returns the board's grouped bindings.
func (b *Board) FullHelp() [][]key.Binding { return b.keys.FullHelp() }

// Title names the view.
func (b *Board) Title() string { return "Board" }

// missionsIn returns the visible missions of a lane, honoring the operation filter.
func (b *Board) missionsIn(lane int) []domain.Mission {
	if lane < 0 || lane >= len(domain.Lanes) {
		return nil
	}

	missions := b.snapshot.MissionsInLane(domain.Lanes[lane])

	if b.filter == "" {
		return missions
	}

	filtered := make([]domain.Mission, 0, len(missions))

	for _, mission := range missions {
		if mission.OperationID == b.filter {
			filtered = append(filtered, mission)
		}
	}

	return filtered
}

// Selected returns the focused mission.
func (b *Board) Selected() (domain.Mission, bool) {
	missions := b.missionsIn(b.lane)
	if len(missions) == 0 {
		return domain.Mission{}, false
	}

	idx := clamp(b.cursor[b.lane], len(missions)-1)

	return missions[idx], true
}

// operation returns a mission's operation.
func (b *Board) operation(mission domain.Mission) domain.Operation {
	operation, _ := b.snapshot.Operation(mission.OperationID)

	return operation
}

// clampSelection keeps cursors and scroll offsets inside their lanes after the data
// changes underneath them.
func (b *Board) clampSelection() {
	b.lane = clamp(b.lane, len(domain.Lanes)-1)

	for lane := range domain.Lanes {
		count := len(b.missionsIn(lane))
		if count == 0 {
			b.cursor[lane], b.scroll[lane] = 0, 0

			continue
		}

		b.cursor[lane] = clamp(b.cursor[lane], count-1)
		b.scroll[lane] = clamp(b.scroll[lane], max(0, count-1))
	}
}

func (b *Board) focusPrevLane() tea.Cmd {
	b.lane = clamp(b.lane-1, len(domain.Lanes)-1)
	b.clampSelection()

	return nil
}

func (b *Board) focusNextLane() tea.Cmd {
	b.lane = clamp(b.lane+1, len(domain.Lanes)-1)
	b.clampSelection()

	return nil
}

func (b *Board) selectPrev() tea.Cmd {
	b.cursor[b.lane]--
	b.clampSelection()
	b.ensureVisible()

	return nil
}

func (b *Board) selectNext() tea.Cmd {
	b.cursor[b.lane]++
	b.clampSelection()
	b.ensureVisible()

	return nil
}

func (b *Board) selectFirst() tea.Cmd {
	b.cursor[b.lane] = 0
	b.scroll[b.lane] = 0

	return nil
}

func (b *Board) selectLast() tea.Cmd {
	b.cursor[b.lane] = max(0, len(b.missionsIn(b.lane))-1)
	b.ensureVisible()

	return nil
}

// ensureVisible scrolls the focused lane so the cursor stays on screen.
func (b *Board) ensureVisible() {
	layout := computeLayout(b.width, b.height, b.doneExpanded, b.lane)
	visible := layout.VisibleCards
	cursor := b.cursor[b.lane]

	if cursor < b.scroll[b.lane] {
		b.scroll[b.lane] = cursor
	}

	if cursor >= b.scroll[b.lane]+visible {
		b.scroll[b.lane] = cursor - visible + 1
	}

	if b.scroll[b.lane] < 0 {
		b.scroll[b.lane] = 0
	}
}

func (b *Board) toggleDone() tea.Cmd {
	b.doneExpanded = !b.doneExpanded

	return nil
}

func (b *Board) clearFilter() tea.Cmd {
	b.filter = ""
	b.clampSelection()

	return nil
}

// View renders the board.
func (b *Board) View() string {
	layout := computeLayout(b.width, b.height, b.doneExpanded, b.lane)

	if layout.Focus {
		return b.renderFocusMode(layout)
	}

	columns := make([]string, 0, len(domain.Lanes))

	for lane := range domain.Lanes {
		columns = append(columns, b.renderLane(lane, layout.Widths[lane], layout))
	}

	return lipgloss.JoinHorizontal(lipgloss.Top, spaceColumns(columns)...)
}

// spaceColumns inserts a gap between columns.
func spaceColumns(columns []string) []string {
	out := make([]string, 0, len(columns)*2)

	for i, col := range columns {
		if i > 0 {
			out = append(out, strings.Repeat(" ", laneGap))
		}

		out = append(out, col)
	}

	return out
}

// renderFocusMode renders a single lane, for terminals too narrow for five columns.
func (b *Board) renderFocusMode(layout Layout) string {
	header := fmt.Sprintf("‹ %d/%d %s ›", b.lane+1, len(domain.Lanes), domain.Lanes[b.lane].Label())

	return lipgloss.JoinVertical(lipgloss.Left,
		styles.LaneHeaderFocused.Render(header),
		b.renderCards(b.lane, layout.Widths[b.lane], layout),
	)
}

// renderLane renders one column.
func (b *Board) renderLane(lane, width int, layout Layout) string {
	missions := b.missionsIn(lane)
	focused := lane == b.lane

	style := styles.LaneHeader
	if focused {
		style = styles.LaneHeaderFocused
	}

	label := fmt.Sprintf("%s %d", strings.ToUpper(domain.Lanes[lane].Label()), len(missions))
	header := style.Render(styles.Truncate(label, width))

	var body string

	if collapsed := !b.doneExpanded && lane == len(domain.Lanes)-1; collapsed {
		body = b.renderCollapsedDone(missions, width)
	} else {
		body = b.renderCards(lane, width, layout)
	}

	// The column is padded to its allotted width. Without this an empty lane would
	// shrink to the width of its own text and the whole grid would shift left.
	return lipgloss.NewStyle().Width(width).Render(
		lipgloss.JoinVertical(lipgloss.Left, header, body))
}

// renderCollapsedDone summarizes the done lane in a narrow column.
func (b *Board) renderCollapsedDone(missions []domain.Mission, width int) string {
	if len(missions) == 0 {
		return styles.CardDetail.Render(styles.Truncate("  nothing", width))
	}

	lines := make([]string, 0, 4)

	for i, mission := range missions {
		if i == 3 {
			lines = append(lines, styles.CardDetail.Render(
				styles.Truncate(fmt.Sprintf("  +%d more", len(missions)-3), width)))

			break
		}

		lines = append(lines, styles.CardDetail.Render(styles.Truncate("  "+mission.Name, width)))
	}

	lines = append(lines, styles.Footer.Render(styles.Truncate("  z to expand", width)))

	return strings.Join(lines, "\n")
}

// renderCards renders a lane's visible cards.
func (b *Board) renderCards(lane, width int, layout Layout) string {
	missions := b.missionsIn(lane)
	if len(missions) == 0 {
		return styles.CardDetail.Render(styles.Truncate(" —", width))
	}

	start := clamp(b.scroll[lane], max(0, len(missions)-1))
	end := min(len(missions), start+layout.VisibleCards)

	cards := make([]string, 0, end-start)

	for i := start; i < end; i++ {
		selected := lane == b.lane && i == b.cursor[lane]
		cards = append(cards, renderCard(missions[i], b.operation(missions[i]), width, selected))
	}

	if end < len(missions) {
		cards = append(cards, styles.Footer.Render(
			styles.Truncate(fmt.Sprintf(" +%d below", len(missions)-end), width)))
	}

	return strings.Join(cards, "\n")
}

// FilterLabel describes the active operation filter, empty when none is set.
func (b *Board) FilterLabel() string {
	if b.filter == "" {
		return ""
	}

	operation, ok := b.snapshot.Operation(b.filter)
	if !ok {
		return ""
	}

	return "filter: " + operation.Name
}
