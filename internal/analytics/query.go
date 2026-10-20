// Package analytics provides read-only queries over q's retained usage records.
package analytics

import (
	"fmt"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

type Filter struct {
	From, To                                               time.Time
	Operation, Tool, Status, Deleted, Model, Effort, Phase string
	GroupBy                                                string
	Undated                                                bool
	Page                                                   int
}

func ParseFilter(v url.Values) (Filter, error) {
	f := Filter{Page: 1}
	for k, values := range v {
		if len(values) != 1 {
			return f, fmt.Errorf("repeated filter %s", k)
		}
		value := values[0]
		switch k {
		case "from", "to":
			t, err := time.Parse(time.RFC3339, value)
			if err != nil {
				return f, fmt.Errorf("%s must be an RFC3339 instant", k)
			}
			if k == "from" {
				f.From = t
			} else {
				f.To = t
			}
		case "groupBy":
			f.GroupBy = value
		case "operation":
			f.Operation = value
		case "tool":
			f.Tool = value
		case "status":
			f.Status = value
		case "deleted":
			f.Deleted = value
		case "model":
			f.Model = value
		case "effort":
			f.Effort = value
		case "phase":
			f.Phase = value
		case "undated":
			if value != "true" && value != "false" {
				return f, fmt.Errorf("invalid undated")
			}
			f.Undated = value == "true"
		case "page":
			n, err := strconv.Atoi(value)
			if err != nil || n < 1 || n > 1000000 {
				return f, fmt.Errorf("invalid page")
			}
			f.Page = n
		default:
			return f, fmt.Errorf("unsupported filter %s", k)
		}
	}
	if !f.From.IsZero() && !f.To.IsZero() && !f.From.Before(f.To) {
		return f, fmt.Errorf("from must precede to")
	}
	if f.Tool != "" && !slices.Contains(mission.Tools, mission.Tool(f.Tool)) {
		return f, fmt.Errorf("invalid tool")
	}
	if f.GroupBy != "" && !slices.Contains([]string{"model", "effort", "phase", "model-effort"}, f.GroupBy) {
		return f, fmt.Errorf("invalid groupBy")
	}
	if f.Status != "" && f.Status != "ongoing" && !mission.Status(f.Status).Valid() {
		return f, fmt.Errorf("invalid status")
	}
	if f.Deleted != "" && f.Deleted != "true" && f.Deleted != "false" {
		return f, fmt.Errorf("invalid deleted")
	}
	if f.Phase != "" && f.Phase != "plan" && f.Phase != "implement" && f.Phase != "unknown" {
		return f, fmt.Errorf("invalid phase")
	}
	return f, nil
}

func dimension(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}
func (f Filter) contribution(r mission.UsageSlice) bool {
	return (f.Model == "" || dimension(r.Model) == f.Model) && (f.Effort == "" || dimension(r.Effort) == f.Effort) && (f.Phase == "" || dimension(string(r.Phase)) == f.Phase)
}
func (f Filter) record(r mission.AnalyticsRecord) bool {
	if r.StartedAt == nil {
		if !f.Undated {
			return false
		}
	} else if (!f.From.IsZero() && r.StartedAt.Before(f.From)) || (!f.To.IsZero() && !r.StartedAt.Before(f.To)) {
		return false
	}
	if f.Operation != "" && string(r.OperationID) != f.Operation {
		return false
	}
	if f.Tool != "" && string(r.Tool) != f.Tool {
		return false
	}
	if f.Status == "ongoing" {
		if r.Status == mission.StatusClosed {
			return false
		}
	} else if f.Status != "" && string(r.Status) != f.Status {
		return false
	}
	if f.Deleted != "" && r.Deleted != (f.Deleted == "true") {
		return false
	}
	return true
}

type Group struct {
	Label         string              `json:"label"`
	Missions      int                 `json:"missions"`
	USD           float64             `json:"usd"`
	Mean          float64             `json:"mean"`
	Median        float64             `json:"median"`
	PlanUSD       float64             `json:"planUSD"`
	ImplementUSD  float64             `json:"implementUSD"`
	UnknownUSD    float64             `json:"unknownUSD"`
	PlanningShare *float64            `json:"planningShare"`
	Incomplete    int                 `json:"incomplete"`
	Tokens        mission.ModelTokens `json:"tokens"`
}
type Detail struct {
	Record    mission.AnalyticsRecord `json:"record"`
	Usage     mission.Usage           `json:"usage"`
	Breakdown []mission.UsageSlice    `json:"breakdown"`
	Workflow  string                  `json:"workflow"`
}
type Result struct {
	UpdatedAt     time.Time           `json:"updatedAt"`
	Matched       int                 `json:"matched"`
	Measured      int                 `json:"measured"`
	UnknownEffort int                 `json:"unknownEffort"`
	Unpriced      int                 `json:"unpriced"`
	TotalUSD      float64             `json:"totalUSD"`
	Workflows     []Group             `json:"workflows"`
	Breakdown     []Group             `json:"breakdown"`
	Missions      []Detail            `json:"missions"`
	Page          int                 `json:"page"`
	Pages         int                 `json:"pages"`
	Facets        map[string][]string `json:"facets"`
	Operations    map[string]string   `json:"operations"`
}

func workflow(u mission.Usage) string {
	phases := map[mission.Phase]map[string]bool{}
	for _, b := range u.Slices() {
		if b.Phase == "" {
			return "unknown"
		}
		if phases[b.Phase] == nil {
			phases[b.Phase] = map[string]bool{}
		}
		phases[b.Phase][dimension(b.Model)+" / "+dimension(b.Effort)] = true
	}
	name := func(p mission.Phase) string {
		m := phases[p]
		if len(m) == 0 {
			return "unknown"
		}
		if len(m) > 1 {
			return "mixed"
		}
		for s := range m {
			return s
		}
		return "unknown"
	}
	plan := name(mission.PhasePlan)
	if len(phases[mission.PhasePlan]) == 0 && !u.Empty() {
		plan = "no planning"
	}
	return plan + " → " + name(mission.PhaseImplement)
}

// Query computes workflow totals over whole qualifying missions; breakdown rows
// include only matching contributions. Both are recorded dollar estimates.
func Query(snapshot api.AnalyticsSnapshot, f Filter) Result {
	if f.Page < 1 {
		f.Page = 1
	}
	out := Result{UpdatedAt: snapshot.UpdatedAt, Page: f.Page, Workflows: []Group{}, Breakdown: []Group{}, Missions: []Detail{}, Facets: map[string][]string{}, Operations: map[string]string{}}
	facets := map[string]map[string]bool{"model": {}, "effort": {}, "tool": {}}
	workflows := map[string]*Group{}
	breakdowns := map[string]*Group{}
	samples := map[string][]float64{}
	var details []Detail
	for _, r := range snapshot.Records {
		out.Operations[string(r.OperationID)] = r.OperationName
		facets["tool"][string(r.Tool)] = true
		u := r.Usage()
		rows := u.Slices()
		for _, b := range rows {
			facets["model"][dimension(b.Model)] = true
			facets["effort"][dimension(b.Effort)] = true
		}
		if !f.record(r) {
			continue
		}
		matching := false
		for _, b := range rows {
			if f.contribution(b) {
				matching = true
			}
		}
		if !matching && (f.Model != "" || f.Effort != "" || f.Phase != "") {
			continue
		}
		out.Matched++
		unknown := false
		for _, b := range rows {
			if b.Effort == "" {
				unknown = true
			}
		}
		if unknown {
			out.UnknownEffort++
		}
		if u.Unpriced {
			out.Unpriced++
		}
		label := workflow(u)
		if r.Status != mission.StatusClosed {
			label = "Ongoing · " + label
		}
		details = append(details, Detail{Record: r, Usage: u, Breakdown: rows, Workflow: label})
		if u.Empty() {
			continue
		}
		out.Measured++
		out.TotalUSD += u.USD
		g := workflows[label]
		if g == nil {
			g = &Group{Label: label}
			workflows[label] = g
		}
		g.Missions++
		g.USD += u.USD
		samples[label] = append(samples[label], u.USD)
		incomplete := u.Unpriced
		seen := map[string]bool{}
		seenIncomplete := map[string]bool{}
		for _, b := range rows {
			switch b.Phase {
			case mission.PhasePlan:
				g.PlanUSD += b.USD
			case mission.PhaseImplement:
				g.ImplementUSD += b.USD
			default:
				g.UnknownUSD += b.USD
				incomplete = true
			}
			if !f.contribution(b) {
				continue
			}
			key := dimension(b.Model) + " / " + dimension(b.Effort) + " / " + dimension(string(b.Phase))
			switch f.GroupBy {
			case "model":
				key = dimension(b.Model)
			case "effort":
				key = dimension(b.Effort)
			case "phase":
				key = dimension(string(b.Phase))
			case "model-effort":
				key = dimension(b.Model) + " / " + dimension(b.Effort)
			}
			bg := breakdowns[key]
			if bg == nil {
				bg = &Group{Label: key}
				breakdowns[key] = bg
			}
			if !seen[key] {
				bg.Missions++
				seen[key] = true
			}
			bg.USD += b.USD
			bg.Tokens = bg.Tokens.Add(b.Tokens)
			switch b.Phase {
			case mission.PhasePlan:
				bg.PlanUSD += b.USD
			case mission.PhaseImplement:
				bg.ImplementUSD += b.USD
			default:
				bg.UnknownUSD += b.USD
			}
			if b.Unpriced && !seenIncomplete[key] {
				seenIncomplete[key] = true
				bg.Incomplete++
			}
		}
		if incomplete {
			g.Incomplete++
		}
	}
	for label, g := range workflows {
		a := samples[label]
		slices.Sort(a)
		n := len(a)
		g.Mean = g.USD / float64(n)
		g.Median = a[n/2]
		if n%2 == 0 {
			g.Median = (a[n/2-1] + a[n/2]) / 2
		}
		if g.USD > 0 && g.Incomplete == 0 {
			share := g.PlanUSD / g.USD
			g.PlanningShare = &share
		}
		out.Workflows = append(out.Workflows, *g)
	}
	for _, g := range breakdowns {
		out.Breakdown = append(out.Breakdown, *g)
	}
	sortGroups := func(a, b Group) int {
		if a.USD > b.USD {
			return -1
		}
		if a.USD < b.USD {
			return 1
		}
		return strings.Compare(a.Label, b.Label)
	}
	slices.SortFunc(out.Workflows, sortGroups)
	slices.SortFunc(out.Breakdown, sortGroups)
	slices.SortFunc(details, func(a, b Detail) int {
		if a.Usage.USD > b.Usage.USD {
			return -1
		}
		if a.Usage.USD < b.Usage.USD {
			return 1
		}
		return strings.Compare(string(a.Record.ID), string(b.Record.ID))
	})
	const pageSize = 25
	out.Pages = max(1, (len(details)+pageSize-1)/pageSize)
	start := min((f.Page-1)*pageSize, len(details))
	out.Missions = append(out.Missions, details[start:min(start+pageSize, len(details))]...)
	for key, values := range facets {
		out.Facets[key] = []string{}
		for value := range values {
			out.Facets[key] = append(out.Facets[key], value)
		}
		slices.Sort(out.Facets[key])
	}
	return out
}
