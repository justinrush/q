package mission

import (
	"slices"
	"strings"
	"time"
)

// StampTurn records now as the end of the agent's last turn when this change is
// the one that ended it. A mission already between turns keeps its stamp, so a
// poll that keeps reading an idle agent does not keep moving its card.
func (m *Mission) StampTurn(before Mission, now time.Time) {
	if m.TurnEnded && !before.TurnEnded {
		m.LastTurnAt = now
	}
}

// TurnRecency is the time the board orders cards by: the end of the last turn,
// or for a mission that has not finished one yet, when it started or was created.
func (m Mission) TurnRecency() time.Time {
	switch {
	case !m.LastTurnAt.IsZero():
		return m.LastTurnAt
	case m.StartedAt != nil:
		return *m.StartedAt
	default:
		return m.CreatedAt
	}
}

// SortByTurnRecency orders missions newest turn first, falling back to id so the
// order is total and two processes draw the same board.
func SortByTurnRecency(missions []Mission) {
	slices.SortStableFunc(missions, func(a, b Mission) int {
		if c := b.TurnRecency().Compare(a.TurnRecency()); c != 0 {
			return c
		}
		return strings.Compare(string(a.ID), string(b.ID))
	})
}
