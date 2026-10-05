package usage

import (
	"math"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

func TestPricingPrice(t *testing.T) {
	pricing := Pricing{Models: map[string]ModelPrice{
		"claude-opus-5":    {Input: 5, Output: 25},
		"claude-haiku-4-5": {Input: 1, Output: 5},
		"claude-opus-5-1":  {Input: 7, Output: 35},
	}}

	cases := []struct {
		name  string
		model string
		want  float64
		found bool
	}{
		{name: "exact id", model: "claude-opus-5", want: 5, found: true},
		{
			name:  "dated snapshot falls back to the family",
			model: "claude-haiku-4-5-20251001",
			want:  1,
			found: true,
		},
		{
			name:  "longest matching prefix wins over a shorter one",
			model: "claude-opus-5-1-20260401",
			want:  7,
			found: true,
		},
		{name: "unknown model", model: "some-other-model", found: false},
		{name: "empty model", model: "", found: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			price, ok := pricing.Price(tc.model)
			if ok != tc.found {
				t.Fatalf("Price(%q) found = %v, want %v", tc.model, ok, tc.found)
			}

			if ok && price.Input != tc.want {
				t.Fatalf("Price(%q).Input = %v, want %v", tc.model, price.Input, tc.want)
			}
		})
	}
}

func TestPricingCost(t *testing.T) {
	pricing := Pricing{Models: map[string]ModelPrice{
		"priced":     {Input: 5, Output: 25},
		"cheap-read": {Input: 10, Output: 50, CacheRead: 0.25},
	}}

	cases := []struct {
		name   string
		model  string
		tokens mission.ModelTokens
		want   float64
		found  bool
	}{
		{
			name:   "input and output at their own rates",
			model:  "priced",
			tokens: mission.ModelTokens{Input: 1_000_000, Output: 1_000_000},
			want:   30,
			found:  true,
		},
		{
			name:   "a cache read is a tenth of input",
			model:  "priced",
			tokens: mission.ModelTokens{CacheRead: 1_000_000},
			want:   0.5,
			found:  true,
		},
		{
			name:   "the two cache write TTLs are priced apart",
			model:  "priced",
			tokens: mission.ModelTokens{CacheWrite5m: 1_000_000, CacheWrite1h: 1_000_000},
			want:   6.25 + 10,
			found:  true,
		},
		{
			name:   "an explicit cache read rate overrides the default fraction",
			model:  "cheap-read",
			tokens: mission.ModelTokens{CacheRead: 1_000_000},
			want:   0.25,
			found:  true,
		},
		{
			name:   "an unpriced model reports nothing rather than guessing",
			model:  "unlisted",
			tokens: mission.ModelTokens{Output: 1_000_000},
			found:  false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := pricing.Cost(tc.model, tc.tokens)
			if ok != tc.found {
				t.Fatalf("Cost() found = %v, want %v", ok, tc.found)
			}

			if ok && math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("Cost() = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPricingValue(t *testing.T) {
	pricing := Pricing{Models: map[string]ModelPrice{"priced": {Input: 5, Output: 25}}}

	cases := []struct {
		name         string
		perModel     map[string]mission.ModelTokens
		wantUSD      float64
		wantUnpriced bool
	}{
		{
			name:     "every model priced",
			perModel: map[string]mission.ModelTokens{"priced": {Output: 1_000_000}},
			wantUSD:  25,
		},
		{
			name: "one unpriced model makes the total a floor",
			perModel: map[string]mission.ModelTokens{
				"priced":   {Output: 1_000_000},
				"unlisted": {Output: 1_000_000},
			},
			wantUSD:      25,
			wantUnpriced: true,
		},
		{
			name:         "nothing but unpriced models",
			perModel:     map[string]mission.ModelTokens{"unlisted": {Output: 1_000_000}},
			wantUnpriced: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := pricing.Value(tc.perModel)

			if math.Abs(got.USD-tc.wantUSD) > 1e-9 {
				t.Fatalf("USD = %v, want %v", got.USD, tc.wantUSD)
			}

			if got.Unpriced != tc.wantUnpriced {
				t.Fatalf("Unpriced = %v, want %v", got.Unpriced, tc.wantUnpriced)
			}
		})
	}
}

func TestPricingUnpriced(t *testing.T) {
	pricing := Pricing{Models: map[string]ModelPrice{"priced": {Input: 5, Output: 25}}}

	got := pricing.Unpriced(map[string]mission.ModelTokens{
		"priced": {}, "zeta": {}, "alpha": {},
	})

	if len(got) != 2 || got[0] != "alpha" || got[1] != "zeta" {
		t.Fatalf("Unpriced() = %v, want [alpha zeta]", got)
	}
}

func TestDefaultPricingCoversTheModelsQRuns(t *testing.T) {
	pricing := DefaultPricing()

	// The models a mission actually runs under today. A release that drops one
	// of these would start reporting every card's cost as a floor.
	for _, model := range []string{
		"gpt-6.1-sol",
		"gpt-5.5",
		"gpt-5.6-sol",
		"gpt-5.6-luna",
		"gpt-6-astra",
		"gpt-6-sol",
		"gpt-6-luna",
		"gpt-5.6-terra",
		"claude-opus-5-5",
		"claude-sonnet-5-5",
		"claude-fable-5-1",
		"claude-mythos-5-1",
		"claude-opus-5",
		"claude-opus-4-8",
		"claude-sonnet-5",
		"claude-haiku-4-5-20251001",
		"claude-fable-5",
	} {
		if _, ok := pricing.Price(model); !ok {
			t.Errorf("DefaultPricing has no rate for %q", model)
		}
	}
}

func TestOpenAISnapshotDoesNotPriceUnknownVariants(t *testing.T) {
	pricing := DefaultPricing()
	for _, model := range []string{"gpt-5.4-pro", "gpt-5.4-mini", "gpt-5.6-sol-unknown", "gpt-5.40"} {
		if _, ok := pricing.Price(model); ok {
			t.Errorf("unexpected rate for %s", model)
		}
	}
	if price, ok := pricing.Price("gpt-5.6-sol-2026-09-01"); !ok || price.Input != 4 {
		t.Fatalf("snapshot price = %+v, %v", price, ok)
	}
}

// Keep the discounted reads distinct from the generic 10% rate: cached
// tokens dominate many coding sessions, so a wrong multiplier is material.
func TestCurrentModelCacheRates(t *testing.T) {
	pricing := DefaultPricing()
	for _, tc := range []struct {
		model string
		want  float64
	}{
		{"gpt-6.1-sol", 14.6},
		{"gpt-6.1-sol-2026-09-29", 14.6},
		{"gpt-5.5", 41.75},
		{"claude-opus-5-5", 29.2},
		{"claude-sonnet-5-5", 14.7},
		{"claude-fable-5-1", 72.75},
		{"claude-mythos-5-1", 72.75},
	} {
		t.Run(tc.model, func(t *testing.T) {
			got, ok := pricing.Cost(tc.model, mission.ModelTokens{
				Input: 1_000_000, Output: 1_000_000,
				CacheRead: 1_000_000, CacheWrite5m: 1_000_000,
			})
			if !ok || math.Abs(got-tc.want) > 1e-9 {
				t.Fatalf("Cost(%q) = %v, %v; want %v, true", tc.model, got, ok, tc.want)
			}
		})
	}
	for _, model := range []string{"gpt-reserve", "codex-auto-review", "gpt-6.1-sol-pro", "claude-opus-5-5-unknown"} {
		if _, ok := pricing.Price(model); ok {
			t.Errorf("unexpected rate for %q", model)
		}
	}
}
