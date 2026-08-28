package tui

import "github.com/justinrush/q/internal/domain"

// Layout thresholds.
const (
	// collapsedDoneWidth is the width of the done column when collapsed. Done cards
	// are history, so they get a count and little else.
	collapsedDoneWidth = 14
	// focusModeBelow is the terminal width under which the board shows one lane at a
	// time.
	//
	// Five columns of readable cards need roughly this much room. Below it, squeezing
	// them all in produces five unreadable slivers, so showing one lane properly is
	// the better trade.
	focusModeBelow = 110
	// laneGap is the space between columns.
	laneGap = 1
)

// Layout is the computed geometry of one board render.
//
// It is produced by a single pure function so that View is pure assembly and every
// width decision is testable without a terminal.
type Layout struct {
	// Focus reports that only one lane is shown at a time.
	Focus bool
	// Widths is the rendered width of each lane, indexed as domain.Lanes.
	Widths []int
	// CardHeight is the rendered height of one card, including its stripe.
	CardHeight int
	// VisibleCards is how many cards fit in a column.
	VisibleCards int
}

// computeLayout decides the board's geometry for a given terminal size.
//
// doneExpanded and focusedLane matter because the two ways of buying space are
// collapsing the done column and, when that is not enough, showing a single lane.
func computeLayout(width, height int, doneExpanded bool, focusedLane int) Layout {
	layout := Layout{
		Widths:     make([]int, len(domain.Lanes)),
		CardHeight: cardHeight(),
	}

	// Two header rows, a footer, and a little breathing room.
	usable := height - 5
	if usable < layout.CardHeight {
		usable = layout.CardHeight
	}

	layout.VisibleCards = usable / layout.CardHeight
	if layout.VisibleCards < 1 {
		layout.VisibleCards = 1
	}

	if width < focusModeBelow {
		layout.Focus = true

		lane := clamp(focusedLane, len(domain.Lanes)-1)
		layout.Widths[lane] = max(width, MinCardWidth)

		return layout
	}

	layout.Widths = shareWidth(width, doneExpanded)

	return layout
}

// shareWidth divides the terminal between the lanes.
func shareWidth(width int, doneExpanded bool) []int {
	widths := make([]int, len(domain.Lanes))
	doneIdx := len(domain.Lanes) - 1

	gaps := (len(domain.Lanes) - 1) * laneGap
	available := width - gaps

	if !doneExpanded {
		// The done column is collapsed to a count, freeing its share for the lanes
		// that describe work still in flight.
		widths[doneIdx] = collapsedDoneWidth
		available -= collapsedDoneWidth
	}

	share := len(domain.Lanes)
	if !doneExpanded {
		share--
	}

	if share <= 0 {
		return widths
	}

	each := available / share
	if each < MinCardWidth {
		each = MinCardWidth
	}

	remainder := available - each*share

	for i := range widths {
		if !doneExpanded && i == doneIdx {
			continue
		}

		widths[i] = each

		// Spread the rounding remainder across the leftmost lanes rather than
		// leaving a ragged gap on the right.
		if remainder > 0 {
			widths[i]++
			remainder--
		}
	}

	return widths
}

// cardHeight is the total rendered height of a card: three body lines, two border
// lines, and the operation stripe.
func cardHeight() int { return 3 + 2 + 1 }

// clamp bounds v to the inclusive range [0, hi].
//
// Every caller is indexing into a lane or a list, so the lower bound is always zero
// and passing it would be noise.
func clamp(v, hi int) int {
	if v < 0 {
		return 0
	}

	if v > hi {
		return hi
	}

	return v
}
