// Package usage measures what an agent session has consumed and estimates what
// it would have cost.
//
// The measurement comes from what the agent already writes down — for claude,
// the JSONL transcript q is handed the path to on every hook — so metering
// needs no credentials, makes no network calls, and adds nothing to the bill it
// is reporting on.
//
// The dollar figure is an estimate of equivalent API spend, not a bill. A
// session running under a subscription is not charged per token at all, and a
// published price can change under a table compiled into a binary. It exists so
// two missions can be compared against each other, and it says so when a model
// it does not recognize means the number is a floor.
//
// The package implements [mission.Meter]; nothing else depends on it, and the
// cmd package wires it in.
package usage

import (
	"slices"
	"strings"
	"time"

	"github.com/justinrush/q/internal/mission"
)

// Cache multipliers, applied to a model's input rate.
//
// A cache read is a fraction of fresh input; a write is a premium on it, and
// the premium depends on how long the entry is kept alive.
const (
	cacheReadMultiplier    = 0.1
	cacheWrite5mMultiplier = 1.25
	cacheWrite1hMultiplier = 2.0
)

// perMillion converts a $/MTok rate to a $/token one.
const perMillion = 1_000_000.0

// ModelPrice is one model's published rates, in dollars per million tokens.
type ModelPrice struct {
	Input  float64
	Output float64
	// CacheRead overrides the usual fraction-of-input read rate for a model
	// priced differently, and is zero when the default applies.
	CacheRead float64
}

// Pricing values a token count.
//
// It is a plain Go struct with no marshaling concerns of its own: the
// JSON-tagged configuration that can extend it lives in the cmd package, which
// builds one of these.
type Pricing struct {
	// Models maps a model id to its rates.
	Models map[string]ModelPrice
}

// DefaultPricing returns the built-in table.
//
// Only models with a published rate are listed. A model that is missing is
// reported as unpriced rather than guessed at, because a plausible-looking
// wrong number is worse than an obviously incomplete one — and the user can
// add it to the table without waiting for a release.
func DefaultPricing() Pricing {
	return Pricing{Models: map[string]ModelPrice{
		// OpenAI standard, short-context API rates, verified 2026-09-09.
		// https://developers.openai.com/api/docs/pricing
		"gpt-5-codex":       {Input: 1.25, Output: 10, CacheRead: 0.125},
		"gpt-5.1-codex":     {Input: 1.25, Output: 10, CacheRead: 0.125},
		"gpt-5.3-codex":     {Input: 1.75, Output: 14, CacheRead: 0.175},
		"gpt-5.4":           {Input: 2.5, Output: 15, CacheRead: 0.25},
		"gpt-6-astra":       {Input: 10, Output: 50, CacheRead: 1},
		"gpt-5.6-sol":       {Input: 4, Output: 20, CacheRead: 0.4},
		"gpt-5.6-terra":     {Input: 2, Output: 12, CacheRead: 0.2},
		"gpt-5.6-luna":      {Input: 0.2, Output: 1.2, CacheRead: 0.02},
		"claude-fable-5-1":  {Input: 10, Output: 50, CacheRead: 0.25},
		"claude-mythos-5-1": {Input: 10, Output: 50},
		"claude-fable-5":    {Input: 10, Output: 50},
		"claude-mythos-5":   {Input: 10, Output: 50},
		"claude-opus-5":     {Input: 5, Output: 25},
		"claude-opus-4-8":   {Input: 5, Output: 25},
		"claude-opus-4-7":   {Input: 5, Output: 25},
		"claude-opus-4-6":   {Input: 5, Output: 25},
		"claude-sonnet-5":   {Input: 2, Output: 10},
		"claude-sonnet-4-6": {Input: 3, Output: 15},
		"claude-haiku-4-5":  {Input: 1, Output: 5},
	}}
}

// Price returns a model's rates.
//
// A model id may carry a dated snapshot suffix that the table does not list —
// "claude-haiku-4-5-20251001" for "claude-haiku-4-5" — so an exact miss falls
// back to a listed id followed by a dated snapshot suffix. Longest wins so that
// adding a "claude-opus-5-1" entry later cannot be shadowed by "claude-opus-5".
func (p Pricing) Price(model string) (ModelPrice, bool) {
	if price, ok := p.Models[model]; ok {
		return price, true
	}

	var (
		best  ModelPrice
		match string
	)

	for id, price := range p.Models {
		if len(id) > len(match) && strings.HasPrefix(model, id+"-") && datedSnapshot(strings.TrimPrefix(model, id+"-")) {
			best, match = price, id
		}
	}

	return best, match != ""
}

// datedSnapshot permits snapshot aliases without assigning a base model's price
// to a differently priced variant such as a mini or pro model.
func datedSnapshot(suffix string) bool {
	for _, layout := range []string{"20060102", "2006-01-02"} {
		if _, err := time.Parse(layout, suffix); err == nil {
			return true
		}
	}
	return false
}

// Cost estimates what one model's tokens would have cost, reporting false when
// the model has no published rate.
func (p Pricing) Cost(model string, tokens mission.ModelTokens) (float64, bool) {
	price, ok := p.Price(model)
	if !ok {
		return 0, false
	}

	cacheRead := price.CacheRead
	if cacheRead == 0 {
		cacheRead = price.Input * cacheReadMultiplier
	}

	dollars := float64(tokens.Input)*price.Input +
		float64(tokens.Output)*price.Output +
		float64(tokens.CacheRead)*cacheRead +
		float64(tokens.CacheWrite5m)*price.Input*cacheWrite5mMultiplier +
		float64(tokens.CacheWrite1h)*price.Input*cacheWrite1hMultiplier

	return dollars / perMillion, true
}

// Value prices a whole per-model breakdown, setting Unpriced when any model in
// it is missing from the table so the caller can render the total as a floor.
func (p Pricing) Value(perModel map[string]mission.ModelTokens) mission.Usage {
	out := mission.Usage{PerModel: perModel}

	for model, tokens := range perModel {
		dollars, ok := p.Cost(model, tokens)
		if !ok {
			out.Unpriced = true

			continue
		}

		out.USD += dollars
	}

	return out
}

// Unpriced lists the models in a breakdown that the table cannot value, sorted,
// so q doctor can report a table that has fallen behind rather than letting it
// silently under-report.
func (p Pricing) Unpriced(perModel map[string]mission.ModelTokens) []string {
	var out []string

	for model := range perModel {
		if _, ok := p.Price(model); !ok {
			out = append(out, model)
		}
	}

	slices.Sort(out)

	return out
}
