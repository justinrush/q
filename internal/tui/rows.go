package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/tui/styles"
)

// Horizontal mode draws each lane as a row: a header line, then its cards side by
// side. It exists for narrow windows on a board with few missions, where five
// columns would squeeze into slivers but five short rows fit comfortably.

// rowHeight is the rendered height of one lane row.
//
// A lane with nothing to show, and the collapsed done lane, take a single body
// line rather than a card's height, which is what lets a quiet board fit in a
// short window.
func (b *Board) rowHeight(lane int) int {
	if b.collapsedDone(lane) || len(b.missionsIn(lane)) == 0 {
		return 2
	}

	return 1 + cardHeight()
}

// collapsedDone reports whether lane is the done lane shown as a summary.
func (b *Board) collapsedDone(lane int) bool {
	return !b.doneExpanded && lane == len(mission.Lanes)-1
}

// rowSpace is how many lines the rows may take.
func (b *Board) rowSpace() int { return max(b.height-1, 1) }

// firstRow is the first lane row drawn, chosen so the focused lane is on screen.
//
// It starts from the remembered scroll position so the view does not jump about
// as focus moves, and pulls back up when that would leave space unused below.
func (b *Board) firstRow() int {
	last := len(mission.Lanes) - 1
	space := b.rowSpace()
	lane := clamp(b.lane, last)

	start := min(clamp(b.laneScroll, last), lane)

	for start < lane && b.rowsHeight(start, lane) > space {
		start++
	}

	for start > 0 && b.rowsHeight(start-1, last) <= space {
		start--
	}

	return start
}

// rowsHeight is the combined height of lanes from through to, inclusive.
func (b *Board) rowsHeight(from, to int) int {
	total := 0

	for lane := from; lane <= to; lane++ {
		total += b.rowHeight(lane)
	}

	return total
}

// renderRows renders the board with lanes as rows.
func (b *Board) renderRows(layout Layout) string {
	space := b.rowSpace()
	used := 0

	rows := make([]string, 0, len(mission.Lanes))

	for lane := b.firstRow(); lane < len(mission.Lanes); lane++ {
		height := b.rowHeight(lane)
		if len(rows) > 0 && used+height > space {
			break
		}

		rows = append(rows, b.renderLaneRow(lane, layout))
		used += height
	}

	return strings.Join(rows, "\n")
}

// renderLaneRow renders one lane as a header line over its visible cards.
func (b *Board) renderLaneRow(lane int, layout Layout) string {
	missions := b.missionsIn(lane)
	width := layout.Widths[lane]

	style := styles.LaneHeader
	if lane == b.lane {
		style = styles.LaneHeaderFocused
	}

	start := clamp(b.scroll[lane], max(0, len(missions)-1))
	end := min(len(missions), start+layout.CardsPerRow)

	label := fmt.Sprintf("%s %d", strings.ToUpper(mission.Lanes[lane].Label()), len(missions))
	if !b.collapsedDone(lane) {
		// Cards scrolled off either end are counted in the header, which is the one
		// line of the row that is always as wide as the window.
		if start > 0 {
			label = fmt.Sprintf("‹%d  %s", start, label)
		}

		if end < len(missions) {
			label = fmt.Sprintf("%s  +%d ›", label, len(missions)-end)
		}
	}

	header := style.Render(styles.Truncate(label, width))

	var body string

	switch {
	case b.collapsedDone(lane):
		body = b.renderCollapsedDoneRow(missions, width)
	case len(missions) == 0:
		body = styles.CardDetail.Render(styles.Truncate(" —", width))
	default:
		cards := make([]string, 0, end-start)

		for i := start; i < end; i++ {
			selected := lane == b.lane && i == b.cursor[lane]
			cards = append(cards, renderCard(missions[i], b.operation(missions[i]), b.hostOf(missions[i]), layout.CardWidth, selected))
		}

		body = lipgloss.JoinHorizontal(lipgloss.Top, spaceColumns(cards)...)
	}

	return lipgloss.JoinVertical(lipgloss.Left, header, body)
}

// renderCollapsedDoneRow summarizes the done lane on one line.
func (b *Board) renderCollapsedDoneRow(missions []mission.Mission, width int) string {
	if len(missions) == 0 {
		return styles.CardDetail.Render(styles.Truncate("  nothing", width))
	}

	names := make([]string, 0, len(missions))
	for _, ms := range missions {
		names = append(names, ms.Name)
	}

	hint := "  z to expand"
	text := styles.Truncate("  "+strings.Join(names, ", "), max(width-len(hint), 1))

	return styles.CardDetail.Render(text) + styles.Footer.Render(hint)
}

// laneRowAt finds the lane row under y, and the line within it, or -1 when y is
// below the last row drawn.
func (b *Board) laneRowAt(y int) (lane, line int) {
	space := b.rowSpace()
	top := 0

	for lane := b.firstRow(); lane < len(mission.Lanes); lane++ {
		height := b.rowHeight(lane)
		if top > 0 && top+height > space {
			break
		}

		if y >= top && y < top+height {
			return lane, y - top
		}

		top += height
	}

	return -1, 0
}

// handleMouseRows processes the mouse when lanes are rows.
func (b *Board) handleMouseRows(msg tea.MouseMsg, layout Layout, x, y int) tea.Cmd {
	lane, line := b.laneRowAt(y)
	if lane < 0 {
		return nil
	}

	switch msg.Button {
	case tea.MouseButtonWheelUp, tea.MouseButtonWheelLeft:
		b.lane = lane

		return b.selectPrev()
	case tea.MouseButtonWheelDown, tea.MouseButtonWheelRight:
		b.lane = lane

		return b.selectNext()
	}

	if msg.Action != tea.MouseActionPress || msg.Button != tea.MouseButtonLeft {
		return nil
	}

	b.lane = lane

	// Clicking the done summary expands it, as it does in column mode.
	collapsed := b.collapsedDone(lane)
	if collapsed && line > 0 {
		b.doneExpanded = true
	}

	missions := b.missionsIn(lane)
	if line == 0 || collapsed || len(missions) == 0 {
		b.clampSelection()
		b.ensureVisible()

		return nil
	}

	slot := x / (layout.CardWidth + laneGap)
	idx := b.scroll[lane] + slot

	if x < 0 || slot >= layout.CardsPerRow || idx >= len(missions) {
		b.clampSelection()

		return nil
	}

	b.cursor[lane] = idx
	b.ensureVisible()

	return b.registerClick(lane, idx)
}
