package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/client"
	"github.com/justinrush/q/internal/daemon"
	"github.com/justinrush/q/internal/debrief"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/launch"
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
func (a *App) startStream() tea.Cmd {
	return func() tea.Msg {
		go func() {
			events := make(chan client.Event, 32)
			done := make(chan error, 1)

			go func() { done <- a.client.Stream(a.ctx(), events) }()

			for {
				select {
				case event := <-events:
					a.events <- streamEventMsg{Event: event}
				case err := <-done:
					a.events <- streamDownMsg{Err: err}

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
func (a *App) applyEvent(event client.Event) tea.Cmd {
	switch event.Name {
	case daemon.EventPing:
		return nil
	case daemon.EventSnapshot:
		var snap struct {
			Operations []domain.Operation `json:"operations"`
			Missions   []domain.Mission   `json:"missions"`
		}

		if err := event.Decode(&snap); err != nil {
			return nil
		}

		a.snapshot.Operations = snap.Operations
		a.snapshot.Missions = snap.Missions
	case daemon.EventMission:
		var mission domain.Mission
		if err := event.Decode(&mission); err != nil {
			return nil
		}

		a.snapshot.PutMission(mission)
	case daemon.EventOperation:
		var operation domain.Operation
		if err := event.Decode(&operation); err != nil {
			return nil
		}

		a.snapshot.PutOperation(operation)
	case daemon.EventDeleted:
		var deleted api.Deleted
		if err := event.Decode(&deleted); err != nil {
			return nil
		}

		if deleted.Kind == api.KindMission {
			a.snapshot.DeleteMission(domain.MissionID(deleted.ID))
		} else {
			a.snapshot.DeleteOperation(domain.OperationID(deleted.ID))
		}
	default:
		return nil
	}

	return a.applySnapshot(a.snapshot)
}

// setStatus moves a mission to another lane.
func (a *App) setStatus(id domain.MissionID, to domain.Status, message string) tea.Cmd {
	return a.setStatusForce(id, to, message, false)
}

// setStatusForce moves a mission and permits an explicitly confirmed dirty finish.
func (a *App) setStatusForce(id domain.MissionID, to domain.Status, message string, force bool) tea.Cmd {
	return func() tea.Msg {
		mission, err := a.client.SetStatus(a.ctx(), id, api.SetStatusRequest{
			To:      to,
			Message: message,
			Force:   force,
		})
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		if mission.Status == domain.StatusClosed {
			return toastMsg{text: "finished " + mission.Name + "; resources reclaimed"}
		}

		return toastMsg{text: mission.Name + " → " + mission.Status.Label()}
	}
}

// openDebriefCmd opens a mission's debrief session.
func (a *App) openDebriefCmd(mission domain.Mission, mode string) tea.Cmd {
	return func() tea.Msg {
		result, err := a.client.OpenDebrief(a.ctx(), mission.ID, mode)
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return debriefOpenedMsg{Mission: mission, Result: result}
	}
}

// debriefOpenedMsg carries the outcome of opening a debrief.
type debriefOpenedMsg struct {
	Mission domain.Mission
	Result  debrief.Result
}

// messageAgentCmd sends text to a live agent, reviving the session if needed.
func (a *App) messageAgentCmd(id domain.MissionID, text string) tea.Cmd {
	return func() tea.Msg {
		mission, err := a.client.Message(a.ctx(), id, text)
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return toastMsg{text: "sent to " + mission.Name}
	}
}

// createMission adds a mission, optionally launching it straight away.
func (a *App) createMission(msg submitMissionMsg) tea.Cmd {
	return func() tea.Msg {
		mission, err := a.client.CreateMission(a.ctx(), api.CreateMissionRequest{
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
			return toastMsg{text: "created " + mission.Name}
		}

		if _, err := a.client.SetStatus(a.ctx(), mission.ID,
			api.SetStatusRequest{To: domain.StatusActive}); err != nil {
			return toastMsg{text: "created, but launching failed: " + err.Error(), err: true}
		}

		return toastMsg{text: "launched " + mission.Name}
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
		if mission, ok := a.snapshot.Mission(msg.ID); ok && !mission.Launched() {
			req.Tool = &msg.Tool
			req.PlanMode = &msg.PlanMode
		}

		if mission, ok := a.snapshot.Mission(msg.ID); ok && mission.Status == domain.StatusBriefing {
			req.ExtraRepos = &msg.ExtraRepos
		}

		mission, err := a.client.UpdateMission(a.ctx(), msg.ID, req)
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		if !msg.Launch {
			return toastMsg{text: "saved " + mission.Name}
		}

		if _, err := a.client.SetStatus(a.ctx(), mission.ID,
			api.SetStatusRequest{To: domain.StatusActive}); err != nil {
			return toastMsg{text: "saved, but launching failed: " + err.Error(), err: true}
		}

		return toastMsg{text: "launched " + mission.Name}
	}
}

// deleteMissionCmd removes a mission and reclaims what it provisioned.
//
// The outcome is reported rather than assumed: a branch kept because it holds commits is
// something the human needs to know about, since nothing else will mention it again.
func (a *App) deleteMissionCmd(mission domain.Mission, force bool) tea.Cmd {
	return func() tea.Msg {
		report, err := a.client.DeleteMission(a.ctx(), mission.ID, force)
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return toastMsg{text: describeReport(mission, report), err: len(report.Failures) > 0}
	}
}

// fetchDeletePlan asks what deleting a mission would discard.
func (a *App) fetchDeletePlan(mission domain.Mission) tea.Cmd {
	return func() tea.Msg {
		plan, err := a.client.DeletePlan(a.ctx(), mission.ID)
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return deletePlanMsg{Mission: mission, Plan: plan}
	}
}

// fetchFinishPlan asks what filing a mission would reclaim.
func (a *App) fetchFinishPlan(mission domain.Mission) tea.Cmd {
	return func() tea.Msg {
		plan, err := a.client.DeletePlan(a.ctx(), mission.ID)
		if err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return finishPlanMsg{Mission: mission, Plan: plan}
	}
}

// deletePlanMsg carries what deleting a mission would discard.
type deletePlanMsg struct {
	Mission domain.Mission
	Plan    launch.Plan
}

// finishPlanMsg carries what moving a mission to closed would reclaim.
type finishPlanMsg struct {
	Mission domain.Mission
	Plan    launch.Plan
}

// describeReport summarizes what a delete actually did.
func describeReport(mission domain.Mission, report launch.Report) string {
	if len(report.Failures) > 0 {
		return "deleted " + mission.Name + ", but: " + strings.Join(report.Failures, "; ")
	}

	if len(report.KeptBranches) > 0 {
		return "deleted " + mission.Name + ", kept branch " + strings.Join(report.KeptBranches, ", ")
	}

	return "deleted " + mission.Name
}

// setPlanMode flips an unlaunched mission's plan-mode flag.
func (a *App) setPlanMode(mission domain.Mission, planMode bool) tea.Cmd {
	return func() tea.Msg {
		updated, err := a.client.UpdateMission(a.ctx(), mission.ID, api.UpdateMissionRequest{PlanMode: &planMode})
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
func (a *App) reorderMission(mission domain.Mission, order int) tea.Cmd {
	return func() tea.Msg {
		if _, err := a.client.UpdateMission(a.ctx(), mission.ID, api.UpdateMissionRequest{Order: &order}); err != nil {
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
func (a *App) deleteOperationCmd(operation domain.Operation, force bool) tea.Cmd {
	return func() tea.Msg {
		if err := a.client.DeleteOperation(a.ctx(), operation.ID, force); err != nil {
			return toastMsg{text: err.Error(), err: true}
		}

		return toastMsg{text: "deleted " + operation.Name}
	}
}
