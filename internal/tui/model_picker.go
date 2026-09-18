package tui

import (
	"fmt"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/tui/styles"
)

// modelPicker chooses a model by typing.
//
// It is not a listModal because that one spends j and k on navigation, which
// would make half the alphabet unreachable in a box whose whole purpose is
// typing. Arrows move the cursor here; every printable key filters.
//
// The list is the whole catalog of the form's chosen agent, which is what
// makes the picker worth having: opencode offers hundreds of models, and
// stepping through them one spacebar at a time is no way to find one.
type modelPicker struct {
	tool mission.Tool
	// query is the typed fragment.
	query *textArea
	// options is everything the agent offers, unfiltered.
	options []mission.ModelOption
	// current is the form's existing choice, highlighted when the box opens.
	current string
	cursor  int
	parent  *missionForm
}

// maxModelChoices bounds how many rows the picker draws.
//
// Like the repo and branch pickers it does not scroll, so a catalog the size of
// opencode's has to be cut off somewhere; the dialog says how many were left
// out, and each keystroke narrows the list until the whole answer fits.
const maxModelChoices = 12

// newModelPicker builds the picker for one agent.
//
// The box opens empty with the form's existing choice merely highlighted,
// rather than opening with that choice as its contents: the query filters, so
// seeding it would hide every other model behind a fragment the user then has
// to delete before they can browse.
func newModelPicker(tool mission.Tool, current string, options []mission.ModelOption, parent *missionForm) *modelPicker {
	picker := &modelPicker{
		tool:    tool,
		query:   newTextArea("", false),
		options: options,
		current: current,
		parent:  parent,
	}

	picker.homeCursor()

	return picker
}

// homeCursor highlights the form's existing choice, while the query is untouched.
func (p *modelPicker) homeCursor() {
	if p.current == "" || p.query.Value() != "" {
		return
	}

	for i, opt := range p.matches() {
		if opt.Value == p.current {
			p.cursor = i

			return
		}
	}
}

// Update implements modal.
func (p *modelPicker) Update(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch msg.String() {
	case keyEsc:
		return p.parent, nil
	case keyUp:
		p.cursor = clamp(p.cursor-1, max(0, len(p.matches())-1))

		return p, nil
	case keyDown:
		p.cursor = clamp(p.cursor+1, max(0, len(p.matches())-1))

		return p, nil
	case keyEnter:
		p.accept()

		return p.parent, nil
	}

	cmd := p.query.Update(msg)
	// A filtered list is shorter than the one the cursor was placed in, so the
	// selection has to come back into range or enter would pick nothing.
	p.cursor = clamp(p.cursor, max(0, len(p.matches())-1))

	return p, cmd
}

// accept records the highlighted model, or the raw query when nothing matches.
//
// Taking the typed text verbatim mirrors the branch picker: a model's usefulness
// cannot depend on how current the catalog is, and the form already explains
// that a model q has never heard of is still launched as written.
func (p *modelPicker) accept() {
	matches := p.matches()

	switch {
	case len(matches) > 0:
		p.pick(matches[p.cursor].Value)
	case strings.TrimSpace(p.query.Value()) != "":
		p.pick(strings.TrimSpace(p.query.Value()))
	}
}

// pick sets the form's chosen model, dropping an effort the model does not take.
func (p *modelPicker) pick(value string) {
	p.parent.model = value
	p.parent.clampEffort()
}

// matches ranks the models the query could mean.
func (p *modelPicker) matches() []mission.ModelOption {
	return matchModelOptions(p.options, p.query.Value())
}

// matchModelOptions filters the catalog to what the fragment could mean.
//
// The match is a case-insensitive substring test on either the model's id or
// its short label, so "sonnet" finds opencode/claude-sonnet-4-5 and "opus"
// finds whichever variant carries that name. Models whose id or label begins
// with the fragment come first, the agent's catalog order preserved within
// each tier. An empty fragment returns the whole catalog, because a picker
// shows the full list before anything is typed rather than nothing at all.
func matchModelOptions(options []mission.ModelOption, fragment string) []mission.ModelOption {
	fragment = strings.ToLower(strings.TrimSpace(fragment))
	if fragment == "" {
		return append([]mission.ModelOption(nil), options...)
	}

	values := make([]string, 0, len(options))
	for _, opt := range options {
		values = append(values, strings.ToLower(opt.Value+" "+opt.Label))
	}

	prefixed := make([]bool, len(options))
	for i, opt := range options {
		prefixed[i] = strings.HasPrefix(strings.ToLower(opt.Value), fragment) ||
			strings.HasPrefix(strings.ToLower(opt.Label), fragment)
	}

	out := make([]mission.ModelOption, 0, len(options))
	for i, opt := range options {
		if prefixed[i] {
			out = append(out, opt)
		}
	}
	for i, opt := range options {
		if !prefixed[i] && strings.Contains(values[i], fragment) {
			out = append(out, opt)
		}
	}

	return out
}

// View implements modal.
func (p *modelPicker) View(width, height int) string {
	inner := min(max(width-20, 30), 90)

	rows := []string{
		styles.ModalTitle.Render("Model for " + p.tool.String()),
		styles.FieldLabel.Render("type to filter; any model containing it matches"),
		"",
		p.query.View(inner),
		"",
	}

	matches := p.matches()

	shown := matches
	if len(shown) > maxModelChoices {
		shown = shown[:maxModelChoices]
	}

	if len(matches) == 0 {
		rows = append(rows, styles.CardDetail.Render("no model matches; enter uses what you typed"))
	}

	for i, opt := range shown {
		rows = append(rows, p.renderRow(opt, i))
	}

	if len(matches) > len(shown) {
		rows = append(rows,
			styles.CardDetail.Render(fmt.Sprintf("… %d more, keep typing", len(matches)-len(shown))))
	}

	rows = append(rows, "", styles.Footer.Render("enter select   ↑↓ move   esc back"))

	return center(styles.Modal.Render(lipgloss.JoinVertical(lipgloss.Left, rows...)), width, height)
}

// renderRow draws one model, with its label and effort levels beside the id.
func (p *modelPicker) renderRow(opt mission.ModelOption, i int) string {
	marker := "  "
	if i == p.cursor {
		marker = "▸ "
	}

	line := marker + opt.Value

	var notes []string
	if opt.Label != "" && opt.Label != opt.Value {
		notes = append(notes, opt.Label)
	}
	if opt.Detail != "" {
		notes = append(notes, opt.Detail)
	}
	if len(opt.Efforts) > 0 {
		notes = append(notes, "effort "+strings.Join(opt.Efforts, "/"))
	}

	if len(notes) > 0 {
		line += styles.CardDetail.Render("  " + strings.Join(notes, " · "))
	}

	if i == p.cursor {
		return lipgloss.NewStyle().Foreground(styles.Accent).Bold(true).Render(marker) +
			strings.TrimPrefix(line, marker)
	}

	return line
}
