// Package styles holds the TUI's colors and reusable lipgloss styles.
//
// The operation palette is the one part of this that carries meaning rather than
// decoration: a card's colored stripe is how a glance at the board tells you which
// investigation it belongs to.
package styles

import "github.com/charmbracelet/lipgloss"

// Palette is the set of operation colors, indexed by an operation's ColorIdx.
//
// Every entry has a dark and light variant and is chosen to be legible as a
// background behind black or white text, because that is how it is used. Colors are
// assigned as the lowest unused index rather than hashed from an operation id, so two
// operations created together never share a stripe.
var Palette = []lipgloss.AdaptiveColor{
	{Dark: "#7AA2F7", Light: "#3B5FBF"}, // blue
	{Dark: "#9ECE6A", Light: "#4F7A2A"}, // green
	{Dark: "#E0AF68", Light: "#8A5F13"}, // amber
	{Dark: "#BB9AF7", Light: "#5B3FA8"}, // violet
	{Dark: "#F7768E", Light: "#B03050"}, // rose
	{Dark: "#7DCFFF", Light: "#1F6F99"}, // cyan
	{Dark: "#E6A0C4", Light: "#93436C"}, // pink
	{Dark: "#B8CC52", Light: "#5F6E17"}, // lime
	{Dark: "#FF9E64", Light: "#A2521C"}, // orange
	{Dark: "#73DACA", Light: "#1E7A6C"}, // teal
	{Dark: "#C0CAF5", Light: "#454B6B"}, // periwinkle
	{Dark: "#D4A574", Light: "#7A5220"}, // sand
}

// OperationColor returns the palette entry for an index, wrapping if it is out of range.
func OperationColor(idx int) lipgloss.AdaptiveColor {
	if len(Palette) == 0 {
		return lipgloss.AdaptiveColor{}
	}

	return Palette[((idx%len(Palette))+len(Palette))%len(Palette)]
}

// Interface colors.
var (
	// Text is ordinary foreground text.
	Text = lipgloss.AdaptiveColor{Dark: "#C0CAF5", Light: "#1F2335"}
	// Muted is secondary text: counts, timestamps, hints.
	Muted = lipgloss.AdaptiveColor{Dark: "#787C99", Light: "#6B7089"}
	// Faint is text that should recede almost entirely.
	Faint = lipgloss.AdaptiveColor{Dark: "#565A75", Light: "#8A8FA3"}
	// Border is an unfocused border.
	Border = lipgloss.AdaptiveColor{Dark: "#3B4261", Light: "#B4B8C5"}
	// Accent marks the focused element.
	Accent = lipgloss.AdaptiveColor{Dark: "#7DCFFF", Light: "#1F6F99"}
	// Warn marks something needing attention.
	Warn = lipgloss.AdaptiveColor{Dark: "#E0AF68", Light: "#8A5F13"}
	// Danger marks a failure or a destructive action.
	Danger = lipgloss.AdaptiveColor{Dark: "#F7768E", Light: "#B03050"}
	// Good marks a healthy or completed state.
	Good = lipgloss.AdaptiveColor{Dark: "#9ECE6A", Light: "#4F7A2A"}
	// OnColor is text drawn on top of a palette color.
	OnColor = lipgloss.AdaptiveColor{Dark: "#16161E", Light: "#FFFFFF"}
)

// Shared styles.
var (
	// App wraps the whole screen.
	App = lipgloss.NewStyle().Padding(0, 1)
	// Title is the application title in the tab bar.
	Title = lipgloss.NewStyle().Foreground(Accent).Bold(true)
	// TabActive is the selected tab.
	TabActive = lipgloss.NewStyle().Foreground(OnColor).Background(Accent).Bold(true).Padding(0, 1)
	// TabInactive is an unselected tab.
	TabInactive = lipgloss.NewStyle().Foreground(Muted).Padding(0, 1)
	// LaneHeader labels a board column.
	LaneHeader = lipgloss.NewStyle().Foreground(Muted).Bold(true).Padding(0, 1)
	// LaneHeaderFocused labels the focused board column.
	LaneHeaderFocused = lipgloss.NewStyle().Foreground(Accent).Bold(true).Padding(0, 1)
	// Card is an unselected card.
	Card = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(Border).Padding(0, 1)
	// CardSelected is the selected card.
	CardSelected = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(Accent).Padding(0, 1)
	// CardTitle is a card's name.
	CardTitle = lipgloss.NewStyle().Foreground(Text).Bold(true)
	// CardDetail is a card's secondary line.
	CardDetail = lipgloss.NewStyle().Foreground(Muted)
	// CardWaiting is the detail line of a card blocked on the human.
	CardWaiting = lipgloss.NewStyle().Foreground(Warn)
	// CardError is the detail line of a failed card.
	CardError = lipgloss.NewStyle().Foreground(Danger)
	// Footer is the help line.
	Footer = lipgloss.NewStyle().Foreground(Faint)
	// Toast is a transient message.
	Toast = lipgloss.NewStyle().Foreground(OnColor).Background(Accent).Padding(0, 1)
	// ToastError is a transient failure message.
	ToastError = lipgloss.NewStyle().Foreground(OnColor).Background(Danger).Padding(0, 1)
	// Modal frames a dialog.
	Modal = lipgloss.NewStyle().Border(lipgloss.RoundedBorder()).BorderForeground(Accent).Padding(1, 2)
	// ModalTitle labels a dialog.
	ModalTitle = lipgloss.NewStyle().Foreground(Accent).Bold(true)
	// FieldLabel labels a form field.
	FieldLabel = lipgloss.NewStyle().Foreground(Muted)
	// FieldLabelFocused labels the focused form field.
	FieldLabelFocused = lipgloss.NewStyle().Foreground(Accent).Bold(true)
	// Disabled marks an option that cannot be chosen.
	Disabled = lipgloss.NewStyle().Foreground(Faint).Strikethrough(true)
)

// Stripe renders a card's operation stripe: a solid bar of the operation's color carrying
// its name.
//
// It is rendered separately from the card border rather than as part of it, because a
// bordered style would clip the background at the edges and break the solid bar.
func Stripe(colorIdx, width int, label string) string {
	if width <= 0 {
		return ""
	}

	return lipgloss.NewStyle().
		Background(OperationColor(colorIdx)).
		Foreground(OnColor).
		Width(width).
		Render(Truncate(" "+label, width))
}

// Truncate shortens s to width, marking where it was cut.
func Truncate(s string, width int) string {
	if width <= 0 {
		return ""
	}

	if lipgloss.Width(s) <= width {
		return s
	}

	if width == 1 {
		return "…"
	}

	runes := []rune(s)
	for len(runes) > 0 && lipgloss.Width(string(runes))+1 > width {
		runes = runes[:len(runes)-1]
	}

	return string(runes) + "…"
}
