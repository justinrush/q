package tui

import (
	"context"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// fetchSnapshot loads the full state.
func (a *App) fetchSnapshot() tea.Cmd {
	return func() tea.Msg {
		snap, err := a.client.State(a.ctx())
		if err != nil {
			return toastMsg{text: "could not load state: " + err.Error(), err: true}
		}

		return snapshotMsg{Snapshot: snap}
	}
}

// startStream opens the event stream in a goroutine that forwards frames onto the
// model's channel.
//
// The goroutine owns the HTTP response body; the model only ever receives from the
// channel, which is what keeps bubbletea's single-operationed update loop intact.
//
// Each stream is numbered and the previous one is canceled, because the reader this
// replaces may be blocked on a half-open socket that will never error. Without
// both, a reconnect would leave a goroutine behind every time and the dead
// connection would still be able to report itself down.
func (a *App) startStream() tea.Cmd {
	if a.stopStream != nil {
		a.stopStream()
	}

	ctx, cancel := context.WithCancel(a.ctx())
	a.stopStream = cancel
	a.stream++
	stream := a.stream

	return func() tea.Msg {
		go func() {
			events := make(chan api.Event, 32)
			done := make(chan error, 1)

			go func() { done <- a.client.Stream(ctx, events) }()

			for {
				select {
				case event := <-events:
					a.events <- streamEventMsg{Stream: stream, Event: event}
				case err := <-done:
					a.events <- streamDownMsg{Stream: stream, Err: err}

					return
				}
			}
		}()

		return nil
	}
}

// listen waits for the next frame from the stream goroutine.
func (a *App) listen() tea.Cmd {
	return func() tea.Msg { return <-a.events }
}

// applyEvent folds one daemon event into the model.
//
// A snapshot frame replaces everything, which is what makes reconnecting safe: the
// daemon may have dropped this client while it was slow, and the first frame of a new
// connection brings it fully back in sync.
func (a *App) applyEvent(event api.Event) tea.Cmd {
	switch event.Name {
	case api.EventPing:
		return nil
	case api.EventSnapshot:
		var snap struct {
			Operations []mission.Operation `json:"operations"`
			Missions   []mission.Mission   `json:"missions"`
		}

		if err := event.Decode(&snap); err != nil {
			return nil
		}

		a.snapshot.Operations = snap.Operations
		a.snapshot.Missions = snap.Missions
	case api.EventMission:
		var ms mission.Mission
		if err := event.Decode(&ms); err != nil {
			return nil
		}

		a.snapshot.PutMission(ms)
	case api.EventOperation:
		var operation mission.Operation
		if err := event.Decode(&operation); err != nil {
			return nil
		}

		a.snapshot.PutOperation(operation)
	case api.EventDeleted:
		var deleted api.Deleted
		if err := event.Decode(&deleted); err != nil {
			return nil
		}

		if deleted.Kind == api.KindMission {
			a.snapshot.DeleteMission(mission.MissionID(deleted.ID))
		} else {
			a.snapshot.DeleteOperation(mission.OperationID(deleted.ID))
		}
	default:
		return nil
	}

	return a.applySnapshot(a.snapshot)
}

// setStatus moves a mission to another lane.
func (a *App) setStatus(id mission.MissionID, to mission.Status, message string) tea.Cmd {
	return a.setStatusForce(id, to, message, false)
}

// setStatusForce moves a mission and permits an explicitly confirmed dirty finish.
func (a *App) setStatusForce(id mission.MissionID, to mission.Status, message string, force bool) tea.Cmd {
	return func() tea.Msg {
		ms, err := a.client.SetStatus(a.ctx(), id, api.SetStatusRequest{
			To:      to,
			Message: message,
			Force:   force,
		})
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		if ms.Status == mission.StatusClosed {
			return toastMsg{text: "finished " + ms.Name + "; resources reclaimed"}
		}

		return toastMsg{text: ms.Name + " → " + ms.Status.Label()}
	}
}

// openDebriefCmd opens a mission's debrief session.
func (a *App) openDebriefCmd(ms mission.Mission, mode string) tea.Cmd {
	return func() tea.Msg {
		result, err := a.client.OpenDebrief(a.ctx(), ms.ID, mode)
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return debriefOpenedMsg{Mission: ms, Result: result}
	}
}

// debriefOpenedMsg carries the outcome of opening a debrief.
type debriefOpenedMsg struct {
	Mission mission.Mission
	Result  api.Result
}

// messageAgentCmd sends text to a live agent, reviving the session if needed.
func (a *App) messageAgentCmd(id mission.MissionID, text string) tea.Cmd {
	return func() tea.Msg {
		ms, err := a.client.Message(a.ctx(), id, text)
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return toastMsg{text: "sent to " + ms.Name}
	}
}

// createMission adds a mission, optionally launching it straight away.
func (a *App) createMission(msg submitMissionMsg) tea.Cmd {
	return func() tea.Msg {
		ms, err := a.client.CreateMission(a.ctx(), api.CreateMissionRequest{
			OperationID: msg.OperationID,
			Name:        msg.Name,
			Prompt:      msg.Prompt,
			Tool:        msg.Tool,
			PlanMode:    msg.PlanMode,
			ExtraRepos:  msg.ExtraRepos,
		})
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		if !msg.Launch {
			return toastMsg{text: "created " + ms.Name}
		}

		if _, err := a.client.SetStatus(a.ctx(), ms.ID,
			api.SetStatusRequest{To: mission.StatusActive}); err != nil {
			return toastMsg{text: "created, but launching failed: " + err.Error(), err: true}
		}

		return toastMsg{text: "launched " + ms.Name}
	}
}

// updateMission patches a mission.
func (a *App) updateMission(msg submitMissionMsg) tea.Cmd {
	return func() tea.Msg {
		req := api.UpdateMissionRequest{
			Name:        &msg.Name,
			Prompt:      &msg.Prompt,
			OperationID: &msg.OperationID,
		}

		// Tool and plan mode are immutable after launch, so they are only sent for a
		// mission that has not started.
		if ms, ok := a.snapshot.Mission(msg.ID); ok && !ms.Launched() {
			req.Tool = &msg.Tool
			req.PlanMode = &msg.PlanMode
		}

		if ms, ok := a.snapshot.Mission(msg.ID); ok && ms.Status == mission.StatusBriefing {
			req.ExtraRepos = &msg.ExtraRepos
		}

		ms, err := a.client.UpdateMission(a.ctx(), msg.ID, req)
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		if !msg.Launch {
			return toastMsg{text: "saved " + ms.Name}
		}

		if _, err := a.client.SetStatus(a.ctx(), ms.ID,
			api.SetStatusRequest{To: mission.StatusActive}); err != nil {
			return toastMsg{text: "saved, but launching failed: " + err.Error(), err: true}
		}

		return toastMsg{text: "launched " + ms.Name}
	}
}

// deleteMissionCmd removes a mission and reclaims what it provisioned.
//
// The outcome is reported rather than assumed: a branch kept because it holds commits is
// something the human needs to know about, since nothing else will mention it again.
func (a *App) deleteMissionCmd(ms mission.Mission, force bool) tea.Cmd {
	return func() tea.Msg {
		report, err := a.client.DeleteMission(a.ctx(), ms.ID, force)
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return toastMsg{text: describeReport(ms, report), err: len(report.Failures) > 0}
	}
}

// fetchDeletePlan asks what deleting a mission would discard.
func (a *App) fetchDeletePlan(ms mission.Mission) tea.Cmd {
	return func() tea.Msg {
		plan, err := a.client.DeletePlan(a.ctx(), ms.ID)
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return deletePlanMsg{Mission: ms, Plan: plan}
	}
}

// fetchFinishPlan asks what filing a mission would reclaim.
func (a *App) fetchFinishPlan(ms mission.Mission) tea.Cmd {
	return func() tea.Msg {
		plan, err := a.client.DeletePlan(a.ctx(), ms.ID)
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return finishPlanMsg{Mission: ms, Plan: plan}
	}
}

// deletePlanMsg carries what deleting a mission would discard.
type deletePlanMsg struct {
	Mission mission.Mission
	Plan    mission.Plan
}

// finishPlanMsg carries what moving a mission to closed would reclaim.
type finishPlanMsg struct {
	Mission mission.Mission
	Plan    mission.Plan
}

// describeReport summarizes what a delete actually did.
func describeReport(ms mission.Mission, report mission.Report) string {
	if len(report.Failures) > 0 {
		return "deleted " + ms.Name + ", but: " + strings.Join(report.Failures, "; ")
	}

	if len(report.KeptBranches) > 0 {
		return "deleted " + ms.Name + ", kept branch " + strings.Join(report.KeptBranches, ", ")
	}

	return "deleted " + ms.Name
}

// setPlanMode flips an unlaunched mission's plan-mode flag.
func (a *App) setPlanMode(ms mission.Mission, planMode bool) tea.Cmd {
	return func() tea.Msg {
		updated, err := a.client.UpdateMission(a.ctx(), ms.ID, api.UpdateMissionRequest{PlanMode: &planMode})
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		if updated.PlanMode {
			return toastMsg{text: updated.Name + ": plan mode on"}
		}

		return toastMsg{text: updated.Name + ": plan mode off"}
	}
}

// reorderMission changes a mission's position within its lane.
func (a *App) reorderMission(ms mission.Mission, order int) tea.Cmd {
	return func() tea.Msg {
		if _, err := a.client.UpdateMission(a.ctx(), ms.ID, api.UpdateMissionRequest{Order: &order}); err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return refreshMsg{}
	}
}

// createOperation adds an operation.
func (a *App) createOperation(msg submitOperationMsg) tea.Cmd {
	return func() tea.Msg {
		operation, err := a.client.CreateOperation(a.ctx(), msg.toCreateOperationRequest())
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return toastMsg{text: "created " + operation.Name}
	}
}

// updateOperation patches an operation.
func (a *App) updateOperation(msg submitOperationMsg) tea.Cmd {
	return func() tea.Msg {
		operation, err := a.client.UpdateOperation(a.ctx(), msg.ID, msg.toUpdateOperationRequest())
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return toastMsg{text: "saved " + operation.Name}
	}
}

// deleteOperationCmd removes an operation and its missions.
func (a *App) deleteOperationCmd(operation mission.Operation, force bool) tea.Cmd {
	return func() tea.Msg {
		if err := a.client.DeleteOperation(a.ctx(), operation.ID, force); err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return toastMsg{text: "deleted " + operation.Name}
	}
}
