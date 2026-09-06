// Reporting on the price table behind the cost figure on a card.

package main

import (
	"sort"
	"strings"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
)

// reportCost prints how metering is configured and, more usefully, whether the
// price table still covers the models the missions on this machine ran under.
//
// A model that has no rate does not break anything: its tokens are still
// counted and the card marks the total as a floor. But the symptom — every card
// quietly reporting less than the truth — is invisible from the board, so this
// is where a table that has fallen behind announces itself.
func reportCost(rep *report, dirs paths.Dirs) {
	rep.line("cost")

	if cfg.Cost.Disabled {
		rep.row("  metering\tdisabled by config")
		rep.line("")

		return
	}

	pricing := pricingFor(cfg)
	rep.row("  metering\tenabled")
	rep.row("  agy\tunavailable: transcripts do not expose token usage or quota resets")
	rep.row("  priced models\t%d", len(pricing.Models))

	unpriced, err := unpricedModels(dirs)
	if err != nil {
		rep.row("  coverage\tunknown\t%v", err)
		rep.line("")

		return
	}

	if len(unpriced) == 0 {
		rep.row("  coverage\tevery metered model has a rate")
		rep.line("")

		return
	}

	rep.row("  coverage\tNO RATE for %s", strings.Join(unpriced, ", "))
	rep.row("  \tthose missions report a floor; add rates under \"cost\".\"models\"")
	rep.line("")
}

// unpricedModels lists the models q has metered but cannot value.
func unpricedModels(dirs paths.Dirs) ([]string, error) {
	store, err := mission.Open(dirs)
	if err != nil {
		return nil, err
	}

	pricing := pricingFor(cfg)
	seen := map[string]bool{}

	for _, ms := range store.Snapshot().Missions {
		for _, model := range pricing.Unpriced(ms.Usage.PerModel) {
			seen[model] = true
		}
	}

	out := make([]string, 0, len(seen))
	for model := range seen {
		out = append(out, model)
	}

	sort.Strings(out)

	return out, nil
}
