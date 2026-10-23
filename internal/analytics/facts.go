// Package analytics projects q's retained usage records into flat facts that
// the browser pivots. Aggregation happens client-side so changing an axis,
// series or filter never needs a round-trip.
package analytics

import (
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// Configured is what the mission asked for, as opposed to what was observed.
type Configured struct {
	Model      string `json:"model"`
	Effort     string `json:"effort"`
	PlanModel  string `json:"planModel"`
	PlanEffort string `json:"planEffort"`
	PlanMode   bool   `json:"planMode"`
}

// Fact is one measured mission with its observed slices. Plan, Implement,
// Iterate and Workflow label what each phase actually ran, never the
// configured defaults.
type Fact struct {
	ID            mission.MissionID    `json:"id"`
	Name          string               `json:"name"`
	OperationID   mission.OperationID  `json:"operationId"`
	OperationName string               `json:"operationName"`
	Tool          mission.Tool         `json:"tool"`
	Status        mission.Status       `json:"status"`
	Deleted       bool                 `json:"deleted"`
	StartedAt     *time.Time           `json:"startedAt,omitempty"`
	FinishedAt    *time.Time           `json:"finishedAt,omitempty"`
	Configured    Configured           `json:"configured"`
	Plan          string               `json:"plan"`
	Implement     string               `json:"implement"`
	Iterate       string               `json:"iterate"`
	Workflow      string               `json:"workflow"`
	USD           float64              `json:"usd"`
	Unpriced      bool                 `json:"unpriced"`
	Slices        []mission.UsageSlice `json:"slices"`
}

type Facts struct {
	UpdatedAt time.Time `json:"updatedAt"`
	Missions  []Fact    `json:"missions"`
	// Unmeasured counts retained missions with no usage, which are left out.
	Unmeasured int `json:"unmeasured"`
}

const unknown = "unknown"

func dimension(s string) string {
	if s == "" {
		return unknown
	}
	return s
}

// phases labels each phase by the single model / effort it ran, "mixed" when
// it ran several, and "none" when it never ran. Unattributed usage makes every
// label unknown, since any phase could own it.
func phases(u mission.Usage) (plan, implement, iterate string) {
	seen := map[mission.Phase]map[string]bool{}
	for _, b := range u.Slices() {
		if b.Phase == "" {
			return unknown, unknown, unknown
		}
		if seen[b.Phase] == nil {
			seen[b.Phase] = map[string]bool{}
		}
		seen[b.Phase][dimension(b.Model)+" / "+dimension(b.Effort)] = true
	}
	name := func(p mission.Phase) string {
		switch m := seen[p]; len(m) {
		case 0:
			return "none"
		case 1:
			for s := range m {
				return s
			}
		}
		return "mixed"
	}
	return name(mission.PhasePlan), name(mission.PhaseImplement), name(mission.PhaseIterate)
}

// Project returns every measured mission. Its estimates are recorded dollar
// figures, not bills.
func Project(snapshot api.AnalyticsSnapshot) Facts {
	out := Facts{UpdatedAt: snapshot.UpdatedAt, Missions: []Fact{}}
	for _, r := range snapshot.Records {
		u := r.Usage()
		if u.Empty() {
			out.Unmeasured++
			continue
		}
		plan, implement, iterate := phases(u)
		slices := u.Slices()
		if slices == nil {
			slices = []mission.UsageSlice{}
		}
		out.Missions = append(out.Missions, Fact{
			ID: r.ID, Name: r.Name, OperationID: r.OperationID, OperationName: r.OperationName,
			Tool: r.Tool, Status: r.Status, Deleted: r.Deleted, StartedAt: r.StartedAt, FinishedAt: r.FinishedAt,
			Configured: Configured{Model: r.Model, Effort: r.Effort, PlanModel: r.PlanModel, PlanEffort: r.PlanEffort, PlanMode: r.PlanMode},
			Plan:       plan, Implement: implement, Iterate: iterate, Workflow: plan + " → " + implement,
			USD: u.USD, Unpriced: u.Unpriced, Slices: slices,
		})
	}
	return out
}
