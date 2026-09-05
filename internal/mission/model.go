package mission

import (
	"context"
	"slices"
	"strconv"
	"strings"
	"time"
)

// ModelOption is one model an agent offers.
type ModelOption struct {
	// Value is what goes on the agent's command line.
	Value string `json:"value"`
	// Label is the short name the board shows, e.g. "Opus".
	Label string `json:"label,omitempty"`
	// Detail is the agent's own one-line description, shown beside the label.
	Detail string `json:"detail,omitempty"`
	// Efforts are the effort levels this model accepts, in the order the agent
	// listed them. Empty means the model takes no effort setting, which is not
	// the same as taking one q happens not to know.
	Efforts []string `json:"efforts,omitempty"`
}

// ModelSet is what one agent offers, as of the last probe.
type ModelSet struct {
	// Default is the model a new mission is pre-filled with. Empty means q could
	// not establish one and should leave the choice to the agent by emitting no
	// model flag at all.
	Default string `json:"default,omitempty"`
	// DefaultEffort is the effort level a new mission is pre-filled with, when
	// the agent's configuration names one. Empty means emit no effort flag.
	DefaultEffort string `json:"defaultEffort,omitempty"`
	// Options are the models offered on the board, in the agent's own order.
	Options []ModelOption `json:"options,omitempty"`
	// ProbedAt is when the agent last answered. A zero time means it never has,
	// which the board reports rather than presenting an empty list as if the
	// agent had offered nothing.
	ProbedAt time.Time `json:"probedAt,omitzero"`
	// Err is why the last probe failed, when one did. A set that carries both
	// options and an error is a stale answer still worth showing.
	Err string `json:"err,omitempty"`
}

// Option returns the entry for a model value.
func (s ModelSet) Option(value string) (ModelOption, bool) {
	for _, opt := range s.Options {
		if opt.Value == value {
			return opt, true
		}
	}

	return ModelOption{}, false
}

// Efforts returns the effort levels a model accepts, or nil for one that accepts
// none or that this set has never heard of.
func (s ModelSet) Efforts(model string) []string {
	opt, ok := s.Option(model)
	if !ok {
		return nil
	}

	return opt.Efforts
}

// SupportsEffort reports whether a model accepts an effort level.
func (s ModelSet) SupportsEffort(model string) bool {
	return len(s.Efforts(model)) > 0
}

// NextModel returns the model after value, wrapping around, so the board's model
// toggle needs to know nothing about which agent it is cycling.
func (s ModelSet) NextModel(value string, delta int) string {
	if len(s.Options) == 0 {
		return value
	}

	idx := slices.IndexFunc(s.Options, func(opt ModelOption) bool { return opt.Value == value })
	if idx < 0 {
		// An unknown value is what a stale card or a hand-written --model looks
		// like. Cycling from it starts at the top rather than discarding it silently.
		return s.Options[0].Value
	}

	n := len(s.Options)

	return s.Options[((idx+delta)%n+n)%n].Value
}

// NextEffort returns the effort after value for a model, wrapping around. The
// empty string is part of the rotation, so a mission can always decline to set
// one and leave the agent to its own default.
func (s ModelSet) NextEffort(model, value string, delta int) string {
	efforts := s.Efforts(model)
	if len(efforts) == 0 {
		return ""
	}

	levels := append([]string{""}, efforts...)

	idx := slices.Index(levels, value)
	if idx < 0 {
		idx = 0
	}

	n := len(levels)

	return levels[((idx+delta)%n+n)%n]
}

// ValidEffort reports whether a model accepts the given effort. The empty effort
// is always valid: it means q emits no effort flag.
func (s ModelSet) ValidEffort(model, effort string) bool {
	return effort == "" || slices.Contains(s.Efforts(model), effort)
}

// ModelProber asks one agent which models and effort levels it offers.
//
// It is separate from [Agent] because answering means running the agent, and an
// Agent is deliberately a pure function from an [Invocation] to argv and bytes.
// It mirrors [Runtime] and [Healer]: optional, best-effort, and absent without
// harm — a board with no prober falls back to whatever was last cached, and
// failing that offers only the agent's own default.
type ModelProber interface {
	// Tool reports which agent this prober asks.
	Tool() Tool
	// Probe runs the agent and reports what it offers.
	Probe(ctx context.Context) (ModelSet, error)
}

// ValidateModelFlag checks a value q would put on an agent's command line.
//
// Membership in a [ModelSet] is deliberately not required. A probe can be stale,
// or absent entirely on a machine that has never reached the agent, and refusing
// a model the agent would have accepted is a worse failure than passing an
// unknown one through and letting the agent object.
func ValidateModelFlag(kind, value string) error {
	if value == "" {
		return nil
	}

	if strings.ContainsAny(value, " \t\r\n") {
		return &ModelFlagError{Kind: kind, Value: value, Reason: "must not contain whitespace"}
	}

	// A leading dash would be read as a flag by the agent's own parser, turning a
	// typo into an unrelated option rather than an error.
	if strings.HasPrefix(value, "-") {
		return &ModelFlagError{Kind: kind, Value: value, Reason: "must not start with a dash"}
	}

	return nil
}

// ModelFlagError reports a model or effort value q will not put on a command line.
type ModelFlagError struct {
	Kind   string
	Value  string
	Reason string
}

// Error implements error.
func (e *ModelFlagError) Error() string {
	return e.Kind + " " + strconv.Quote(e.Value) + " is not usable: " + e.Reason
}
