package tui

import (
	tea "github.com/charmbracelet/bubbletea"
	"github.com/justinrush/q/internal/mission"
)

// The board emits intents rather than calling the daemon itself.
//
// Keeping the view free of I/O is what lets every keypress handler stay a few lines and
// be tested by asserting the message it produces, with no server involved.
type (
	// openDebriefMsg asks to open a mission's debrief session.
	openDebriefMsg struct{ Mission mission.Mission }
	// messagePromptMsg asks for the dialog that sends text to a live agent.
	messagePromptMsg struct{ Mission mission.Mission }
	// newMissionMsg asks for the new-mission form.
	newMissionMsg struct{}
	// editMissionMsg asks for the edit form for a mission.
	editMissionMsg struct{ Mission mission.Mission }
	// deleteMissionMsg asks to confirm deleting a mission.
	deleteMissionMsg struct{ Mission mission.Mission }
	// togglePlanMsg asks to flip an unlaunched mission's plan-mode flag.
	togglePlanMsg struct{ Mission mission.Mission }
	// filterPromptMsg asks for the operation filter picker.
	filterPromptMsg struct{}
	// statusMenuMsg asks for the lane picker for a mission.
	statusMenuMsg struct{ Mission mission.Mission }
	// setFilterMsg applies an operation filter to the board.
	setFilterMsg struct{ OperationID mission.OperationID }
	// moveMissionMsg asks to move a mission to another lane.
	moveMissionMsg struct {
		Mission mission.Mission
		To      mission.Status
	}
	// resumePromptMsg asks for the follow-up message dialog shown when a mission is
	// moved back into progress.
	resumePromptMsg struct{ Mission mission.Mission }
	// reorderMsg asks to change a mission's position within its lane.
	reorderMsg struct {
		Mission mission.Mission
		Delta   int
	}
)

// emit wraps a message as a command.
func emit(msg tea.Msg) tea.Cmd {
	return func() tea.Msg { return msg }
}

// withSelected runs fn against the focused mission, doing nothing when the lane is empty.
func (b *Board) withSelected(fn func(mission.Mission) tea.Cmd) tea.Cmd {
	ms, ok := b.Selected()
	if !ok {
		return nil
	}

	return fn(ms)
}

// openDebrief opens the focused mission's debrief session.
func (b *Board) openDebrief() tea.Cmd {
	return b.withSelected(func(ms mission.Mission) tea.Cmd {
		// A mission in briefing has no session to open; its editor is the useful thing instead.
		if !ms.Launched() {
			return emit(editMissionMsg{Mission: ms})
		}

		return emit(openDebriefMsg{Mission: ms})
	})
}

// messageAgent sends text to the focused mission's live agent.
func (b *Board) messageAgent() tea.Cmd {
	return b.withSelected(func(ms mission.Mission) tea.Cmd {
		if !ms.Launched() {
			return nil
		}

		return emit(messagePromptMsg{Mission: ms})
	})
}

// newMission opens the new-mission form.
func (b *Board) newMission() tea.Cmd { return emit(newMissionMsg{}) }

// editMission opens the focused mission's editor.
func (b *Board) editMission() tea.Cmd {
	return b.withSelected(func(ms mission.Mission) tea.Cmd {
		return emit(editMissionMsg{Mission: ms})
	})
}

// deleteMission asks to delete the focused mission.
func (b *Board) deleteMission() tea.Cmd {
	return b.withSelected(func(ms mission.Mission) tea.Cmd {
		return emit(deleteMissionMsg{Mission: ms})
	})
}

// togglePlan flips plan mode on the focused mission.
func (b *Board) togglePlan() tea.Cmd {
	return b.withSelected(func(ms mission.Mission) tea.Cmd {
		return emit(togglePlanMsg{Mission: ms})
	})
}

// filterByOperation opens the operation filter picker.
func (b *Board) filterByOperation() tea.Cmd { return emit(filterPromptMsg{}) }

// SetFilter limits the board to one operation.
func (b *Board) SetFilter(id mission.OperationID) {
	b.filter = id
	b.clampSelection()
}

// moveCardLeft moves the focused mission one lane left.
func (b *Board) moveCardLeft() tea.Cmd { return b.moveCard(-1) }

// moveCardRight moves the focused mission one lane right.
func (b *Board) moveCardRight() tea.Cmd { return b.moveCard(1) }

// moveCard moves the focused mission by the given number of lanes.
//
// Moving into the active lane launches or resumes an agent, while moving to closed reclaims
// it. The distinction is made here so the confirmation a human sees matches what will
// actually happen.
func (b *Board) moveCard(delta int) tea.Cmd {
	return b.withSelected(func(ms mission.Mission) tea.Cmd {
		target := clamp(b.lane+delta, len(mission.Lanes)-1)
		to := mission.Lanes[target]

		if to == ms.Status {
			return nil
		}

		if to == mission.StatusActive && ms.Launched() {
			return emit(resumePromptMsg{Mission: ms})
		}

		// Follow the card rather than the lane, so pressing this again moves the same
		// mission instead of whatever lands under the cursor.
		b.follow = ms.ID

		return emit(moveMissionMsg{Mission: ms, To: to})
	})
}

// statusMenu offers every lane, so a card can be moved straight to one rather than
// stepped through the lanes between.
func (b *Board) statusMenu() tea.Cmd {
	return b.withSelected(func(ms mission.Mission) tea.Cmd {
		return emit(statusMenuMsg{Mission: ms})
	})
}

// reorderUp moves the focused mission earlier in its lane.
func (b *Board) reorderUp() tea.Cmd { return b.reorder(-1) }

// reorderDown moves the focused mission later in its lane.
func (b *Board) reorder(delta int) tea.Cmd {
	return b.withSelected(func(ms mission.Mission) tea.Cmd {
		missions := b.missionsIn(b.lane)
		idx := clamp(b.cursor[b.lane], len(missions)-1)

		target := idx + delta
		if target < 0 || target >= len(missions) {
			return nil
		}

		b.cursor[b.lane] = target
		b.ensureVisible()
		b.follow = ms.ID

		return emit(reorderMsg{Mission: ms, Delta: delta})
	})
}

// reorderDown moves the focused mission later in its lane.
func (b *Board) reorderDown() tea.Cmd { return b.reorder(1) }
