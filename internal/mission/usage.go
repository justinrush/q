package mission

import "time"

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

// Usage is what a mission has consumed so far.
//
// USD is an estimate of the equivalent API spend, derived from the token counts
// and a pricing table. It is not a bill: a session running under a subscription
// is not charged per token at all, and the table can lag a price change. The
// number is for comparing missions against each other.
type Usage struct {
	PerModel map[string]ModelTokens `json:"perModel,omitempty"`
	USD      float64                `json:"usd,omitempty"`
	// Unpriced reports that some tokens came from a model the pricing table does
	// not know, so USD is a floor rather than a total.
	Unpriced bool `json:"unpriced,omitempty"`
	// At is when the measurement was taken.
	At time.Time `json:"at,omitzero"`
}

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

// Equal reports whether two usages are the same measurement, ignoring when it
// was taken. The daemon re-meters on a timer, so comparing At would publish a
// mission update on every tick of an idle session.
func (u Usage) Equal(o Usage) bool {
	if u.USD != o.USD || u.Unpriced != o.Unpriced || len(u.PerModel) != len(o.PerModel) {
		return false
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
