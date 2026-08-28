package tui

import (
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/domain"
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
var agentGlyphs = map[domain.AgentState]string{
	domain.AgentUnknown: "·",
	domain.AgentBusy:    "◐",
	domain.AgentWaiting: "⏸",
	domain.AgentIdle:    "○",
	domain.AgentDead:    "✕",
}

// renderCard draws one mission card.
//
// It is a pure function of the mission, its operation, and the available width, so the whole
// visual result is assertable in a test without a terminal.
//
// The operation stripe is rendered as its own full-width line joined beneath the bordered
// body rather than inside the border, because a bordered style clips its background at
// the edges and would break the solid bar.
func renderCard(mission domain.Mission, operation domain.Operation, width int, selected bool) string {
	width = max(width, MinCardWidth)

	// lipgloss sizes a block by its content plus padding, with the border outside
	// that, so the frame is asked for two fewer columns than the card occupies and
	// the text is truncated to two fewer again.
	frame := width - cardBorderWidth
	content := frame - cardPaddingWidth

	body := strings.Join([]string{
		styles.CardTitle.Render(styles.Truncate(agentGlyph(mission)+" "+mission.Name, content)),
		detailLine(mission, content),
		metaLine(mission, content),
	}, "\n")

	style := styles.Card
	if selected {
		style = styles.CardSelected
	}

	framed := style.Width(frame).Render(body)

	return lipgloss.JoinVertical(lipgloss.Left, framed, styles.Stripe(operation.ColorIdx, width, operationLabel(operation)))
}

// agentGlyph returns the marker for a mission's observed agent state.
func agentGlyph(mission domain.Mission) string {
	if glyph, ok := agentGlyphs[mission.AgentState]; ok {
		return glyph
	}

	return "·"
}

// operationLabel is the text on the stripe.
func operationLabel(operation domain.Operation) string {
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
func detailLine(mission domain.Mission, width int) string {
	if mission.LaunchError != "" {
		return styles.CardError.Render(styles.Truncate("launch failed: "+firstLine(mission.LaunchError), width))
	}

	if mission.WaitingFor != "" {
		return styles.CardWaiting.Render(styles.Truncate("⏸ "+mission.WaitingFor, width))
	}

	if mission.LastMessage != "" {
		return styles.CardDetail.Render(styles.Truncate(mission.LastMessage, width))
	}

	return styles.CardDetail.Render(styles.Truncate(firstLine(mission.Prompt), width))
}

// metaLine is the card's third line: tool, mode, repo count, age, and badges.
func metaLine(mission domain.Mission, width int) string {
	parts := []string{mission.Tool.Glyph() + " " + mission.Tool.String()}

	if mission.PlanMode {
		parts = append(parts, "plan")
	}

	if n := countCreated(mission); n > 0 {
		parts = append(parts, fmt.Sprintf("%dr", n))
	}

	if age := missionAge(mission); age != "" {
		parts = append(parts, age)
	}

	for _, badge := range mission.Badges {
		parts = append(parts, renderBadge(badge))
	}

	return styles.CardDetail.Render(styles.Truncate(strings.Join(parts, " · "), width))
}

// renderBadge formats one badge compactly.
func renderBadge(badge domain.Badge) string {
	if badge.Detail == "" {
		return badge.Kind
	}

	return badge.Kind + ":" + badge.Detail
}

// countCreated counts the mission's successfully provisioned worktrees.
func countCreated(mission domain.Mission) int {
	var n int

	for _, work := range mission.Work {
		if work.Created {
			n++
		}
	}

	return n
}

// missionAge renders how long the mission has been in flight, or how long ago it finished.
func missionAge(mission domain.Mission) string {
	switch {
	case mission.FinishedAt != nil:
		return shortDuration(time.Since(*mission.FinishedAt)) + " ago"
	case mission.StartedAt != nil:
		return shortDuration(time.Since(*mission.StartedAt))
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
