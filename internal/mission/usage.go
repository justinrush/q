package mission

import (
	"slices"
	"time"
)

// ModelTokens is what one model consumed for one mission.
//
// The cache columns are kept apart rather than summed because they are priced
// differently from each other and from plain input: a read is a tenth of the
// input rate while a write is a premium on it, so collapsing them would make
// the dollar estimate wrong by an order of magnitude on a long session, where
// cache reads outnumber fresh input by a factor of thousands.
type ModelTokens struct {
	Input        int64 `json:"input,omitempty"`
	Output       int64 `json:"output,omitempty"`
	CacheRead    int64 `json:"cacheRead,omitempty"`
	CacheWrite5m int64 `json:"cacheWrite5m,omitempty"`
	CacheWrite1h int64 `json:"cacheWrite1h,omitempty"`
}

// Add returns the sum of two readings.
func (t ModelTokens) Add(o ModelTokens) ModelTokens {
	return ModelTokens{
		Input:        t.Input + o.Input,
		Output:       t.Output + o.Output,
		CacheRead:    t.CacheRead + o.CacheRead,
		CacheWrite5m: t.CacheWrite5m + o.CacheWrite5m,
		CacheWrite1h: t.CacheWrite1h + o.CacheWrite1h,
	}
}

// Total is every token the model processed, however it was billed.
func (t ModelTokens) Total() int64 {
	return t.Input + t.Output + t.CacheRead + t.CacheWrite5m + t.CacheWrite1h
}

// Zero reports whether nothing was consumed.
func (t ModelTokens) Zero() bool { return t.Total() == 0 }

// UsageSlice retains a measured attribution and its recorded estimate.
// Empty dimensions mean unknown, never the configured mission defaults.
type UsageSlice struct {
	Model    string      `json:"model"`
	Effort   string      `json:"effort"`
	Phase    Phase       `json:"phase"`
	Tokens   ModelTokens `json:"tokens"`
	USD      float64     `json:"usd"`
	Unpriced bool        `json:"unpriced"`
}

// Usage is what a mission has consumed so far.
//
// USD is an estimate of the equivalent API spend, derived from the token counts
// and a pricing table. It is not a bill: a session running under a subscription
// is not charged per token at all, and the table can lag a price change. The
// number is for comparing missions against each other.
type Usage struct {
	Breakdown []UsageSlice           `json:"breakdown,omitempty"`
	PerModel  map[string]ModelTokens `json:"perModel,omitempty"`
	USD       float64                `json:"usd,omitempty"`
	// Unpriced reports that some tokens came from a model the pricing table does
	// not know, so USD is a floor rather than a total.
	Unpriced bool `json:"unpriced,omitempty"`
	// At is when the measurement was taken.
	At time.Time `json:"at,omitzero"`
	// Phases splits the same consumption by the phase each request was made in.
	// It is nil unless every request could be placed from the agent's own record
	// of its mode, so a split that is shown is never an apportioned guess.
	Phases map[Phase]Usage `json:"phases,omitempty"`
}

// Phase is the part of a mission a request was made in.
type Phase string

// The phases a request can be attributed to. Implementing is the first turn
// made outside plan mode, which for a mission that never plans is the brief.
// Iterating is every turn prompted after that, in whatever mode or model, so
// feedback rounds do not blur what the initial implementation cost.
const (
	PhasePlan      Phase = "plan"
	PhaseImplement Phase = "implement"
	PhaseIterate   Phase = "iterate"
)

// Phases lists every phase in mission order.
var Phases = []Phase{PhasePlan, PhaseImplement, PhaseIterate}

// Tokens is every token the mission consumed, across all models.
func (u Usage) Tokens() int64 {
	var out int64

	for _, tokens := range u.PerModel {
		out += tokens.Total()
	}

	return out
}

// Empty reports whether nothing has been measured.
func (u Usage) Empty() bool {
	for _, t := range u.PerModel {
		if !t.Zero() {
			return false
		}
	}

	return true
}

// Clone returns a deep copy, so a mission handed outside the store cannot be
// mutated through its usage map.
func (u Usage) Clone() Usage {
	u.Breakdown = slices.Clone(u.Breakdown)
	if u.Phases != nil {
		phases := make(map[Phase]Usage, len(u.Phases))
		for phase, usage := range u.Phases {
			phases[phase] = usage.Clone()
		}

		u.Phases = phases
	}

	if u.PerModel == nil {
		return u
	}

	models := make(map[string]ModelTokens, len(u.PerModel))
	for model, tokens := range u.PerModel {
		models[model] = tokens
	}

	u.PerModel = models

	return u
}

// Plus returns the two usages combined, as one mission's consumption on two
// hosts. The measurement time is the later of the two.
func (u Usage) Plus(o Usage) Usage {
	out := Usage{USD: u.USD + o.USD, Unpriced: u.Unpriced || o.Unpriced, At: u.At}
	if o.At.After(out.At) {
		out.At = o.At
	}

	out.Phases = plusPhases(u, o)
	out.Breakdown = append(u.Slices(), o.Slices()...)

	if len(u.PerModel)+len(o.PerModel) == 0 {
		return out
	}

	out.PerModel = make(map[string]ModelTokens, len(u.PerModel)+len(o.PerModel))

	for model, tokens := range u.PerModel {
		out.PerModel[model] = tokens
	}

	for model, tokens := range o.PerModel {
		out.PerModel[model] = out.PerModel[model].Add(tokens)
	}

	return out
}

// plusPhases combines two splits. A reading with no split makes the combined
// one unknowable, unless that reading measured nothing.
func plusPhases(u, o Usage) map[Phase]Usage {
	switch {
	case u.Empty():
		return o.Clone().Phases
	case o.Empty():
		return u.Clone().Phases
	case u.Phases == nil || o.Phases == nil:
		return nil
	}

	out := make(map[Phase]Usage, len(u.Phases)+len(o.Phases))
	for _, phase := range Phases {
		a, inA := u.Phases[phase]
		b, inB := o.Phases[phase]

		if inA || inB {
			out[phase] = a.Plus(b)
		}
	}

	return out
}

// TotalUsage sums what a mission consumed across the hosts that ran it.
//
// The hosts are added in a fixed order. Floating-point addition is not
// associative, and a total that differed in its last bit depending on map order
// would read as a change on every measurement.
func TotalUsage(byHost map[HostID]Usage) Usage {
	hosts := make([]HostID, 0, len(byHost))
	for host := range byHost {
		hosts = append(hosts, host)
	}

	slices.Sort(hosts)

	var total Usage
	for _, host := range hosts {
		total = total.Plus(byHost[host])
	}

	return total
}

// Equal reports whether two usages are the same measurement, ignoring when it
// was taken. The daemon re-meters on a timer, so comparing At would publish a
// mission update on every tick of an idle session.
func (u Usage) Equal(o Usage) bool {
	if !slices.Equal(u.Breakdown, o.Breakdown) || u.USD != o.USD || u.Unpriced != o.Unpriced || len(u.PerModel) != len(o.PerModel) ||
		(u.Phases == nil) != (o.Phases == nil) || len(u.Phases) != len(o.Phases) {
		return false
	}

	for phase, usage := range u.Phases {
		if other, ok := o.Phases[phase]; !ok || !usage.Equal(other) {
			return false
		}
	}

	for model, tokens := range u.PerModel {
		if o.PerModel[model] != tokens {
			return false
		}
	}

	return true
}

// Limit records that an agent refused a request because a usage window was
// exhausted.
//
// It is deliberately retrospective. The agents publish nothing that says how
// much of a window is left, so the only honest signal available locally is a
// refusal that has already happened and the time it stops applying.
type Limit struct {
	Tool Tool `json:"tool"`
	// Kind is the window that was exhausted, e.g. "five_hour" or "weekly".
	Kind string `json:"kind"`
	// ResetsAt is when the window reopens.
	ResetsAt time.Time `json:"resetsAt"`
	// ObservedAt is when q read the refusal, used to keep the newer of two.
	ObservedAt time.Time `json:"observedAt,omitzero"`
}

// Live reports whether the limit still applies.
func (l Limit) Live(now time.Time) bool { return !l.ResetsAt.IsZero() && l.ResetsAt.After(now) }

// Describe renders the limit for a human, e.g. "5-hour limit hit · resets 4:20pm".
func (l Limit) Describe() string {
	return limitKindLabel(l.Kind) + " hit · resets " + l.ResetsAt.Local().Format("3:04pm")
}

// limitKindLabel renders an agent's window name in the terms a human uses.
func limitKindLabel(kind string) string {
	switch kind {
	case "five_hour":
		return "5-hour limit"
	case "weekly":
		return "weekly limit"
	case "":
		return "usage limit"
	default:
		return kind + " limit"
	}
}

// Slices preserves legacy dollars without inventing their model or effort.
func (u Usage) Slices() []UsageSlice {
	if len(u.Breakdown) > 0 {
		return slices.Clone(u.Breakdown)
	}
	if u.Empty() {
		return nil
	}
	if len(u.Phases) > 0 {
		var out []UsageSlice
		for _, phase := range Phases {
			for _, row := range u.Phases[phase].Slices() {
				row.Phase = phase
				out = append(out, row)
			}
		}
		return out
	}
	var tokens ModelTokens
	for _, t := range u.PerModel {
		tokens = tokens.Add(t)
	}
	model := ""
	if len(u.PerModel) == 1 {
		for m := range u.PerModel {
			model = m
		}
	}
	return []UsageSlice{{Model: model, Tokens: tokens, USD: u.USD, Unpriced: u.Unpriced}}
}
