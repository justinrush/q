package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// handleIntent turns a view's intent into a dialog or a daemon call.
//
// Views emit intents and this decides what they mean, which keeps every keypress
// handler short and puts all the confirmation policy in one place.
func (a *App) handleIntent(msg tea.Msg) tea.Cmd {
	if cmd, handled := a.handleMissionIntent(msg); handled {
		return cmd
	}

	return a.handleOperationIntent(msg)
}

// handleMissionIntent handles the intents concerning missions and the board.
func (a *App) handleMissionIntent(msg tea.Msg) (tea.Cmd, bool) {
	switch m := msg.(type) {
	case newMissionMsg:
		a.showMissionForm(mission.Mission{})

		return nil, true
	case editMissionMsg:
		a.showMissionForm(m.Mission)

		return nil, true
	case deleteMissionMsg:
		return a.confirmDeleteMission(m.Mission), true
	case deletePlanMsg:
		a.showDeleteConfirm(m)

		return nil, true
	case finishPlanMsg:
		return a.handleFinishPlan(m), true
	case togglePlanMsg:
		return a.handleTogglePlan(m.Mission), true
	case moveMissionMsg:
		return a.moveToLane(m.Mission, m.To), true
	case resumePromptMsg:
		a.showResumePrompt(m.Mission)

		return nil, true
	case reorderMsg:
		return a.handleReorder(m), true
	case submitMissionMsg:
		return a.handleMissionSubmit(m), true
	}

	return nil, false
}

// handleOperationIntent handles the intents concerning operations, debriefs, and filtering.
func (a *App) handleOperationIntent(msg tea.Msg) tea.Cmd {
	switch m := msg.(type) {
	case openDebriefMsg:
		return a.openDebriefCmd(m.Mission, api.DebriefAttach)
	case debriefOpenedMsg:
		return a.handleDebriefOpened(m)
	case messagePromptMsg:
		return a.showMessagePrompt(m.Mission)
	case filterPromptMsg:
		return a.showOperationFilter()
	case statusMenuMsg:
		return a.showStatusMenu(m.Mission)
	case setFilterMsg:
		a.board.SetFilter(m.OperationID)
	case newOperationMsg:
		return a.showOperationForm(mission.Operation{})
	case editOperationMsg:
		return a.showOperationForm(m.Operation)
	case deleteOperationMsg:
		return a.confirmDeleteOperation(m.Operation)
	case submitOperationMsg:
		return a.handleOperationSubmit(m)
	case focusOperationMsg:
		a.board.SetFilter(m.Operation.ID)
		a.active = tabBoard
	}

	return nil
}

// showMissionForm opens the mission editor.
func (a *App) showMissionForm(ms mission.Mission) {
	a.modal = newMissionForm(ms, a.snapshot.Operations, a.currentOperationID(), a.opts)
}

// showOperationForm opens the operation editor.
func (a *App) showOperationForm(operation mission.Operation) tea.Cmd {
	a.modal = newOperationForm(operation, a.opts)

	return nil
}

// handleMissionSubmit saves a created or edited mission.
func (a *App) handleMissionSubmit(msg submitMissionMsg) tea.Cmd {
	if msg.ID == "" {
		return a.createMission(msg)
	}

	return a.updateMission(msg)
}

// handleOperationSubmit saves a created or edited operation.
func (a *App) handleOperationSubmit(msg submitOperationMsg) tea.Cmd {
	if msg.ID == "" {
		return a.createOperation(msg)
	}

	return a.updateOperation(msg)
}

// confirmDeleteMission asks the daemon what deleting would discard, then confirms.
//
// The question is asked first rather than after, because deleting a mission removes real
// worktrees and can throw away an agent's uncommitted work. A dialog that only says "are
// you sure" gives the human nothing to be sure about.
func (a *App) confirmDeleteMission(ms mission.Mission) tea.Cmd {
	if !ms.Launched() {
		// A mission still in briefing provisioned nothing, so there is nothing to enumerate.
		a.modal = newConfirm("Delete mission", fmt.Sprintf("Delete %q?\n\nIt was never launched.", ms.Name),
			"delete", true, a.deleteMissionCmd(ms, false))

		return nil
	}

	return a.fetchDeletePlan(ms)
}

// showDeleteConfirm presents what deleting a mission would discard.
func (a *App) showDeleteConfirm(msg deletePlanMsg) {
	lines := []string{fmt.Sprintf("Delete %q?", msg.Mission.Name)}

	if msg.Plan.SessionAlive {
		lines = append(lines, "", "Its agent is still running and will be stopped.")
	}

	if len(msg.Plan.Repos) > 0 {
		lines = append(lines, "", "Worktrees:")
		for _, repo := range msg.Plan.Repos {
			lines = append(lines, "  "+describeDisposition(repo))
		}
	}

	if len(msg.Plan.KeptBranches) > 0 {
		lines = append(lines, "", "These branches are kept: "+strings.Join(msg.Plan.KeptBranches, ", "))
	}

	confirm := "delete"

	if msg.Plan.NeedsForce {
		// Naming the loss explicitly, because this is the one case that is not
		// recoverable and the one most likely to be regretted.
		lines = append(lines, "",
			"Uncommitted changes will be lost. There is no way to get them back.")

		confirm = "discard and delete"
	}

	a.modal = newConfirm("Delete mission", strings.Join(lines, "\n"), confirm, true,
		a.deleteMissionCmd(msg.Mission, msg.Plan.NeedsForce))
}

// handleFinishPlan files a clean mission immediately and asks before either discarding
// uncommitted changes or leaving an unpushed branch behind.
func (a *App) handleFinishPlan(msg finishPlanMsg) tea.Cmd {
	if !msg.Plan.NeedsForce && len(msg.Plan.KeptBranches) == 0 {
		return a.setStatus(msg.Mission.ID, mission.StatusClosed, "")
	}

	lines := []string{fmt.Sprintf("Finish %q?", msg.Mission.Name)}

	if len(msg.Plan.KeptBranches) > 0 {
		lines = append(lines, "", "These branches will be kept: "+
			strings.Join(msg.Plan.KeptBranches, ", "))
	}

	confirm := "finish"
	if msg.Plan.NeedsForce {
		lines = append(lines, "",
			"Uncommitted changes will be lost. There is no way to get them back.")
		confirm = "discard and finish"
	}

	a.modal = newConfirm("Finish mission", strings.Join(lines, "\n"), confirm, true,
		a.setStatusForce(msg.Mission.ID, mission.StatusClosed, "", msg.Plan.NeedsForce))

	return nil
}

// describeDisposition renders one repo's fate in the delete dialog.
func describeDisposition(repo mission.RepoDisposition) string {
	switch repo.Action {
	case mission.ActionNeedsForce:
		return fmt.Sprintf("%s  uncommitted changes will be discarded", repo.Repo)
	case mission.ActionKeepBranch:
		return fmt.Sprintf("%s  %d commit(s), branch %s kept", repo.Repo, repo.Ahead, repo.Branch)
	case mission.ActionUnavailable:
		return fmt.Sprintf("%s  cannot be inspected: %s", repo.Repo, repo.Reason)
	default:
		return fmt.Sprintf("%s  nothing to lose", repo.Repo)
	}
}

// confirmDeleteOperation asks before deleting an operation and its missions.
func (a *App) confirmDeleteOperation(operation mission.Operation) tea.Cmd {
	active := a.snapshot.ActiveMissionsForOperation(operation.ID)
	total := a.snapshot.MissionsForOperation(operation.ID)

	lines := []string{fmt.Sprintf("Delete %q?", operation.Name)}

	if len(total) > 0 {
		lines = append(lines, "", fmt.Sprintf("This also deletes %d mission(s), %d of them unfinished.",
			len(total), len(active)))
		lines = append(lines, "Their worktrees and tmux sessions are left in place.")
	}

	// Force is needed only when unfinished missions would be swept up with it.
	force := len(active) > 0

	a.modal = newConfirm("Delete operation", strings.Join(lines, "\n"), "delete", true,
		a.deleteOperationCmd(operation, force))

	return nil
}

// handleTogglePlan flips plan mode, explaining when it cannot be flipped.
func (a *App) handleTogglePlan(ms mission.Mission) tea.Cmd {
	if ms.Launched() {
		return emit(toastMsg{
			text: "plan mode is fixed once a mission has launched",
			err:  true,
		})
	}

	if !ms.Tool.SupportsPlanMode() {
		return emit(toastMsg{
			text: ms.Tool.String() + " has no plan mode",
			err:  true,
		})
	}

	return a.setPlanMode(ms, !ms.PlanMode)
}

// handleReorder computes the mission's new position and sends it.
func (a *App) handleReorder(msg reorderMsg) tea.Cmd {
	missions := a.snapshot.MissionsInLane(msg.Mission.Status)

	for i, ms := range missions {
		if ms.ID != msg.Mission.ID {
			continue
		}

		target := i + msg.Delta
		if target < 0 || target >= len(missions) {
			return nil
		}

		// Adopt the neighbour's position, which is what moving past it means.
		return a.reorderMission(msg.Mission, missions[target].Order)
	}

	return nil
}

// showResumePrompt asks what to tell an agent before putting it back to work.
//
// An empty answer is allowed and means "just move the card", which is why the prompt
// says so: sometimes the lane is wrong and the agent needs nothing.
func (a *App) showResumePrompt(ms mission.Mission) {
	hint := "sent to the live session; leave empty to only move the card"
	if ms.AgentState == mission.AgentDead {
		hint = "the session has ended, so this restarts the agent and resumes the conversation"
	}

	a.modal = newPrompt("Back to work: "+ms.Name, hint, "", true, true, func(text string) tea.Cmd {
		if strings.TrimSpace(text) == "" {
			return a.setStatus(ms.ID, mission.StatusActive, "")
		}

		return a.messageAgentCmd(ms.ID, text)
	})
}

// showMessagePrompt asks for text to send to a live agent.
func (a *App) showMessagePrompt(ms mission.Mission) tea.Cmd {
	a.modal = newPrompt("Message "+ms.Name, "delivered to the agent's live session", "", true, false,
		func(text string) tea.Cmd {
			return a.messageAgentCmd(ms.ID, text)
		})

	return nil
}

// showStatusMenu offers every lane, so a card can be moved straight to one.
//
// Stepping a card lane by lane with H and L is fine for one hop and tedious for two, and
// this is also the only way to reach a non-adjacent lane in a single decision.
func (a *App) showStatusMenu(ms mission.Mission) tea.Cmd {
	items := make([]listItem, 0, len(mission.Lanes))

	for _, lane := range mission.Lanes {
		item := listItem{Key: string(lane), Label: lane.Label()}

		switch {
		case lane == ms.Status:
			item.Detail = "current"
		case lane == mission.StatusActive && ms.Launched():
			item.Detail = "resumes the agent"
		case lane == mission.StatusActive:
			item.Detail = "launches the agent"
		case lane == mission.StatusClosed && ms.Launched():
			item.Detail = "stops agent and reclaims worktrees"
		}

		items = append(items, item)
	}

	a.modal = newList("Move "+ms.Name, "pick a lane", items, func(picked string) tea.Cmd {
		return a.moveToLane(ms, mission.Status(picked))
	})

	return nil
}

// moveToLane moves a mission to a chosen lane, taking the same route a lane-by-lane move
// would so the two cannot behave differently.
func (a *App) moveToLane(ms mission.Mission, to mission.Status) tea.Cmd {
	if to == ms.Status {
		return nil
	}

	// Follow the card, so the selection lands on it wherever it went.
	a.board.Follow(ms.ID)

	// Putting a launched mission back to work resumes an agent, which needs a message.
	if to == mission.StatusActive && ms.Launched() {
		return emit(resumePromptMsg{Mission: ms})
	}

	if to == mission.StatusClosed && ms.Launched() {
		return a.fetchFinishPlan(ms)
	}

	return a.setStatus(ms.ID, to, "")
}

// showOperationFilter offers the board's operation filter.
func (a *App) showOperationFilter() tea.Cmd {
	items := make([]listItem, 0, len(a.snapshot.Operations)+1)

	items = append(items, listItem{Key: "", Label: "all operations"})

	for _, operation := range a.snapshot.Operations {
		items = append(items, listItem{
			Key:      string(operation.ID),
			Label:    operation.Name,
			Detail:   fmt.Sprintf("%d mission(s)", len(a.snapshot.MissionsForOperation(operation.ID))),
			ColorIdx: operation.ColorIdx,
			Colored:  true,
		})
	}

	a.modal = newList("Filter board", "show only one operation", items, func(picked string) tea.Cmd {
		return emit(setFilterMsg{OperationID: mission.OperationID(picked)})
	})

	return nil
}

// handleDebriefOpened reports the outcome of opening a debrief, offering the next step
// when there is one.
func (a *App) handleDebriefOpened(msg debriefOpenedMsg) tea.Cmd {
	if msg.Result.NeedsRelaunch {
		a.modal = newConfirm("Session has ended",
			fmt.Sprintf("%q has no live session.\n\nRestart the agent and resume the conversation?\n"+
				"Its worktrees are untouched.", msg.Mission.Name),
			"restart", false,
			a.messageAgentCmd(msg.Mission.ID, ""))

		return nil
	}

	if msg.Result.AttachCommand != "" {
		return emit(toastMsg{text: "run: " + msg.Result.AttachCommand})
	}

	if len(msg.Result.Touched) == 0 {
		return emit(toastMsg{text: msg.Mission.Name + ": nothing changed yet"})
	}

	repos := make([]string, 0, len(msg.Result.Touched))
	for _, item := range msg.Result.Touched {
		repos = append(repos, item.Repo)
	}

	return emit(toastMsg{text: "opened " + msg.Mission.Name + " (" + strings.Join(repos, ", ") + ")"})
}
