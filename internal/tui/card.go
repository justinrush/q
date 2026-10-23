package tui

import (
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/tui/styles"
)

// Card geometry.
const (
	// MinCardWidth is the narrowest a card may be rendered.
	MinCardWidth = 20
	// cardBorderWidth is the horizontal space the left and right border take.
	cardBorderWidth = 2
	// cardPaddingWidth is the horizontal padding inside the border, so text does not
	// sit against it.
	cardPaddingWidth = 2
)

// agentGlyphs mark a card with what its agent is doing, which is distinct from the
// lane it sits in.
var agentGlyphs = map[mission.AgentState]string{
	mission.AgentUnknown: "·",
	mission.AgentBusy:    "◐",
	mission.AgentWaiting: "⏸",
	mission.AgentIdle:    "○",
	mission.AgentDead:    "✕",
}

// renderCard draws one mission card.
//
// It is a pure function of the mission, its operation, and the available width, so the whole
// visual result is assertable in a test without a terminal.
//
// The operation stripe is rendered as its own full-width line joined beneath the bordered
// body rather than inside the border, because a bordered style clips its background at
// the edges and would break the solid bar.
//
// host names the other machine for a mission that runs, or must run, there, and
// is empty otherwise. It is on the card because everything else about such a
// mission looks the same as a local one, and the difference decides what
// opening it does.
func renderCard(ms mission.Mission, operation mission.Operation, host string, width int, selected bool) string {
	width = max(width, MinCardWidth)

	// lipgloss sizes a block by its content plus padding, with the border outside
	// that, so the frame is asked for two fewer columns than the card occupies and
	// the text is truncated to two fewer again.
	frame := width - cardBorderWidth
	content := frame - cardPaddingWidth

	body := strings.Join([]string{
		styles.CardTitle.Render(styles.Truncate(agentGlyph(ms)+" "+ms.Name, content)),
		detailLine(ms, content),
		metaLine(ms, host, content),
		modelLine(ms, mission.PhasePlan, content),
		modelLine(ms, mission.PhaseImplement, content),
		modelLine(ms, mission.PhaseIterate, content),
	}, "\n")

	style := styles.Card
	if selected {
		style = styles.CardSelected
	}

	framed := style.Width(frame).Render(body)

	return lipgloss.JoinVertical(lipgloss.Left, framed, styles.Stripe(operation.ColorIdx, width, operationLabel(operation)))
}

// agentGlyph returns the marker for a mission's observed agent state.
func agentGlyph(ms mission.Mission) string {
	if glyph, ok := agentGlyphs[ms.AgentState]; ok {
		return glyph
	}

	return "·"
}

// operationLabel is the text on the stripe.
func operationLabel(operation mission.Operation) string {
	if operation.Name == "" {
		return "(no operation)"
	}

	return operation.Name
}

// detailLine is the card's second line: whatever most needs saying about this mission
// right now.
//
// The order is deliberate. A failed launch and a blocked agent are the two things that
// need a human, so they outrank the agent's closing message, which is only context.
func detailLine(ms mission.Mission, width int) string {
	if ms.PlanFailed() {
		return styles.CardError.Render(styles.Truncate("Codex handoff failed: "+ms.PlanTransition.Error, width))
	}
	if ms.LaunchError != "" {
		return styles.CardError.Render(styles.Truncate("launch failed: "+firstLine(ms.LaunchError), width))
	}

	if ms.WaitingFor != "" {
		return styles.CardWaiting.Render(styles.Truncate("⏸ "+ms.WaitingFor, width))
	}

	if ms.LastMessage != "" {
		return styles.CardDetail.Render(styles.Truncate(ms.LastMessage, width))
	}

	return styles.CardDetail.Render(styles.Truncate(firstLine(ms.Prompt), width))
}

// metaLine shows the tool, host, cost and run metadata. Models have their own lines
// so long model names cannot crowd the cost or one another off the card.
func metaLine(ms mission.Mission, host string, width int) string {
	parts := []string{ms.Tool.Glyph() + " " + ms.Tool.String()}

	// The host comes straight after the agent, ahead of everything that can be
	// truncated away, for the same reason cost does: it exists only here.
	if host != "" {
		parts = append(parts, "@"+host)
	}

	if cost := missionCost(ms); cost != "" {
		parts = append(parts, cost)
	}

	// A review can be renamed, so the name alone cannot be relied on to say
	// that this card's agent is reading another mission's work, not doing its own.
	if ms.IsReview() {
		parts = append(parts, "review")
	}

	// Queued is a flag rather than a stored badge, since the brief owns it and
	// the badges belong to the run.
	if ms.Queued {
		parts = append(parts, mission.BadgeQueued)
	}

	if n := countCreated(ms); n > 0 {
		parts = append(parts, fmt.Sprintf("%dr", n))
	}

	if age := missionAge(ms); age != "" {
		parts = append(parts, age)
	}

	for _, badge := range ms.AllBadges() {
		parts = append(parts, renderBadge(badge))
	}

	return styles.CardDetail.Render(styles.Truncate(strings.Join(parts, " · "), width))
}

// missionCost renders what a mission has consumed, or "" when nothing has been
// measured — an unlaunched mission, or an agent q cannot meter.
//
// The trailing "+" on a partial total is not decoration. It appears when a
// model in the breakdown has no published rate, which makes the figure a floor
// rather than a sum, and a floor that does not say so is a wrong number.
//
// When nothing in the breakdown has a rate — an agent running models q ships no
// prices for — the count itself is shown instead. "$0.00+" would be true and
// useless; the token count is the honest thing left to say, and adding the
// rates under "cost" in the config turns it back into money.
func missionCost(ms mission.Mission) string { return usageCost(ms.Usage) }

// usageCost renders one measurement the way [missionCost] describes.
func usageCost(usage mission.Usage) string {
	if usage.Empty() {
		return ""
	}

	if usage.Unpriced && usage.USD == 0 {
		return formatTokens(usage.Tokens())
	}

	out := formatUSD(usage.USD)
	if usage.Unpriced {
		out += "+"
	}

	return out
}

// phaseCost renders what a mission spent in one phase, or "" when the meter
// could not place every request in a phase or the phase has cost nothing yet.
// A mission that neither plans nor iterates spends everything implementing,
// which the total already says.
func phaseCost(ms mission.Mission, phase mission.Phase) string {
	if !ms.PlanMode && ms.Usage.Phases[mission.PhaseIterate].Empty() {
		return ""
	}

	return usageCost(ms.Usage.Phases[phase])
}

// iterateModel names the model that did most of a mission's iterating, and how
// many others shared it, or "" before any iteration. It is what was observed
// rather than configured, because iterating is where a model gets switched.
func iterateModel(ms mission.Mission) string {
	spend := map[string]float64{}
	for _, slice := range ms.Usage.Slices() {
		if slice.Phase != mission.PhaseIterate {
			continue
		}
		name := slice.Model
		if name == "" {
			name = "unknown"
		}
		if slice.Effort != "" {
			name += "/" + slice.Effort
		}
		spend[name] += slice.USD
	}
	if len(spend) == 0 {
		return ""
	}
	names := slices.Sorted(maps.Keys(spend))
	top := names[0]
	for _, name := range names[1:] {
		if spend[name] > spend[top] {
			top = name
		}
	}
	if len(names) > 1 {
		top += fmt.Sprintf(" +%d", len(names)-1)
	}
	return top
}

// formatTokens renders a token count in at most five columns.
func formatTokens(tokens int64) string {
	switch {
	case tokens >= 10_000_000:
		return fmt.Sprintf("%dM", tokens/1_000_000)
	case tokens >= 1_000_000:
		return fmt.Sprintf("%.1fM", float64(tokens)/1_000_000)
	case tokens >= 10_000:
		return fmt.Sprintf("%dk", tokens/1_000)
	case tokens >= 1_000:
		return fmt.Sprintf("%.1fk", float64(tokens)/1_000)
	default:
		return fmt.Sprintf("%d", tokens)
	}
}

// formatUSD renders a dollar figure in at most five columns, spending its
// precision where the reader can use it: cents matter at a dollar and are noise
// at a hundred.
func formatUSD(usd float64) string {
	switch {
	case usd < 0.01:
		return "<$0.01"
	case usd < 10:
		return fmt.Sprintf("$%.2f", usd)
	case usd < 100:
		return fmt.Sprintf("$%.1f", usd)
	default:
		return fmt.Sprintf("$%.0f", usd)
	}
}

// renderBadge formats one badge compactly.
func renderBadge(badge mission.Badge) string {
	if badge.Detail == "" {
		return badge.Kind
	}

	return badge.Kind + ":" + badge.Detail
}

// countCreated counts the mission's successfully provisioned worktrees.
func countCreated(ms mission.Mission) int {
	var n int

	for _, work := range ms.Work {
		if work.Created {
			n++
		}
	}

	return n
}

// missionAge renders how long the mission has been in flight, or how long ago it finished.
func missionAge(ms mission.Mission) string {
	switch {
	case ms.FinishedAt != nil:
		return shortDuration(time.Since(*ms.FinishedAt)) + " ago"
	case ms.StartedAt != nil:
		return shortDuration(time.Since(*ms.StartedAt))
	default:
		return ""
	}
}

// shortDuration renders a duration in the least space that still reads clearly.
func shortDuration(d time.Duration) string {
	switch {
	case d < time.Minute:
		return fmt.Sprintf("%ds", int(d.Seconds()))
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// firstLine returns the first line of s.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if head, _, found := strings.Cut(s, "\n"); found {
		return strings.TrimSpace(head)
	}

	return s
}

// modelLine gives each phase its own line, including when it shares a model.
//
// The phase's cost ends the line and is kept when the model name is truncated,
// because it is the one thing on the line that is not also in the brief.
func modelLine(ms mission.Mission, phase mission.Phase, width int) string {
	model, effort := ms.Model, ms.Effort
	switch phase {
	case mission.PhasePlan:
		if !ms.PlanMode {
			return styles.CardDetail.Render(styles.Truncate("plan: off", width))
		}
		if ms.PlanModel != "" {
			model, effort = ms.PlanModel, ms.PlanEffort
		}
	case mission.PhaseIterate:
		model, effort = iterateModel(ms), ""
		if model == "" {
			return styles.CardDetail.Render(styles.Truncate("iterate: —", width))
		}
	}
	if model == "" {
		model = "default"
	}
	if effort != "" {
		model += "/" + effort
	}
	line := string(phase) + ": " + model
	if cost := phaseCost(ms, phase); cost != "" {
		suffix := " · " + cost
		line = styles.Truncate(line, width-lipgloss.Width(suffix)) + suffix
	}
	return styles.CardDetail.Render(styles.Truncate(line, width))
}
