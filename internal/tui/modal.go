package tui

import (
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/tui/styles"
)

// modal is a dialog laid over the board.
//
// Modals are their own small models rather than branches inside a view, which is what
// keeps every Update method short and means a dialog cannot accidentally leave a view
// in a half-edited state.
//
// Update returns the modal to display next; returning nil dismisses it.
type modal interface {
	Update(tea.KeyMsg) (modal, tea.Cmd)
	View(width, height int) string
}

// modalDismissed is emitted when a modal closes without doing anything, so the root
// model can restore focus.
type modalDismissed struct{}

// confirmModal asks a yes-or-no question.
//
// Destructive answers are never the default. Deleting a mission can discard hours of
// uncommitted agent work, so the dialog opens on "no" and says what will be lost.
type confirmModal struct {
	title   string
	body    string
	confirm string
	// onConfirm is the command to run when accepted.
	onConfirm tea.Cmd
	// danger renders the dialog as destructive.
	danger bool
}

// newConfirm builds a confirmation dialog.
func newConfirm(title, body, confirmLabel string, danger bool, onConfirm tea.Cmd) *confirmModal {
	return &confirmModal{
		title:     title,
		body:      body,
		confirm:   confirmLabel,
		onConfirm: onConfirm,
		danger:    danger,
	}
}

// Update implements modal.
func (m *confirmModal) Update(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch msg.String() {
	case "y", "Y", keyEnter:
		return nil, m.onConfirm
	case "n", "N", keyEsc, keyQuit:
		return nil, emit(modalDismissed{})
	}

	return m, nil
}

// View implements modal.
func (m *confirmModal) View(width, height int) string {
	style := styles.Modal
	if m.danger {
		style = style.BorderForeground(styles.Danger)
	}

	label := m.confirm
	if label == "" {
		label = "confirm"
	}

	content := lipgloss.JoinVertical(lipgloss.Left,
		styles.ModalTitle.Render(m.title),
		"",
		m.body,
		"",
		styles.Footer.Render("y "+label+"   n cancel"),
	)

	return center(style.Render(content), width, height)
}

// promptModal collects a single line or block of text.
type promptModal struct {
	title string
	hint  string
	input *textArea
	// onSubmit receives the entered text.
	onSubmit func(string) tea.Cmd
	// allowEmpty lets the dialog be submitted with no text, which the resume dialog
	// wants: an empty follow-up means "just relabel it".
	allowEmpty bool
}

// newPrompt builds a text dialog.
func newPrompt(title, hint, initial string, multiline, allowEmpty bool, onSubmit func(string) tea.Cmd) *promptModal {
	return &promptModal{
		title:      title,
		hint:       hint,
		input:      newTextArea(initial, multiline),
		onSubmit:   onSubmit,
		allowEmpty: allowEmpty,
	}
}

// Update implements modal.
func (m *promptModal) Update(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch msg.String() {
	case keyEsc:
		return nil, emit(modalDismissed{})
	case keySave:
		return m.submit()
	case keyEnter:
		if !m.input.multiline {
			return m.submit()
		}
	}

	m.input.Update(msg)

	return m, nil
}

// submit closes the dialog and runs its action.
func (m *promptModal) submit() (modal, tea.Cmd) {
	text := strings.TrimRight(m.input.Value(), "\n")

	if text == "" && !m.allowEmpty {
		return m, nil
	}

	return nil, m.onSubmit(text)
}

// View implements modal.
func (m *promptModal) View(width, height int) string {
	inner := min(max(width-20, 30), 90)

	submit := "enter submit"
	if m.input.multiline {
		submit = "ctrl+s submit   enter newline"
	}

	content := lipgloss.JoinVertical(lipgloss.Left,
		styles.ModalTitle.Render(m.title),
		styles.FieldLabel.Render(m.hint),
		"",
		m.input.View(inner),
		"",
		styles.Footer.Render(submit+"   esc cancel"),
	)

	return center(styles.Modal.Render(content), width, height)
}

// listModal picks one item from a list.
type listModal struct {
	title  string
	hint   string
	items  []listItem
	cursor int
	// onPick receives the chosen item's key.
	onPick func(string) tea.Cmd
	// parent is the modal to restore when this one closes, for a picker opened from
	// inside a form. Without it, canceling the picker would throw away everything
	// already typed into the form behind it.
	parent modal
}

// listItem is one selectable row.
type listItem struct {
	// Key identifies the item to the caller.
	Key string
	// Label is the displayed text.
	Label string
	// Detail is optional secondary text.
	Detail string
	// ColorIdx tints the row, used to show an operation's stripe color in the picker.
	ColorIdx int
	// Colored enables the tint.
	Colored bool
}

// newList builds a picker.
func newList(title, hint string, items []listItem, onPick func(string) tea.Cmd) *listModal {
	return &listModal{title: title, hint: hint, items: items, onPick: onPick}
}

// under makes the picker return to parent when it closes, instead of dismissing.
func (m *listModal) under(parent modal) *listModal {
	m.parent = parent

	return m
}

// Update implements modal.
func (m *listModal) Update(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch msg.String() {
	case keyEsc, keyQuit:
		return m.close()
	case keyUp, keyVimUp:
		m.cursor = clamp(m.cursor-1, max(0, len(m.items)-1))
	case keyDown, keyVimDown:
		m.cursor = clamp(m.cursor+1, max(0, len(m.items)-1))
	case keyEnter:
		if len(m.items) == 0 {
			return m.close()
		}

		return m.parent, m.onPick(m.items[m.cursor].Key)
	}

	return m, nil
}

// close leaves the picker without choosing anything.
func (m *listModal) close() (modal, tea.Cmd) {
	if m.parent != nil {
		return m.parent, nil
	}

	return nil, emit(modalDismissed{})
}

// View implements modal.
func (m *listModal) View(width, height int) string {
	rows := make([]string, 0, len(m.items)+4)

	rows = append(rows, styles.ModalTitle.Render(m.title))

	if m.hint != "" {
		rows = append(rows, styles.FieldLabel.Render(m.hint))
	}

	rows = append(rows, "")

	if len(m.items) == 0 {
		rows = append(rows, styles.CardDetail.Render("nothing to choose from"))
	}

	for i, item := range m.items {
		rows = append(rows, m.renderItem(i, item))
	}

	cancel := "esc cancel"
	if m.parent != nil {
		cancel = "esc back"
	}

	rows = append(rows, "", styles.Footer.Render("enter select   "+cancel))

	return center(styles.Modal.Render(lipgloss.JoinVertical(lipgloss.Left, rows...)), width, height)
}

// renderItem draws one row.
func (m *listModal) renderItem(i int, item listItem) string {
	marker := "  "
	if i == m.cursor {
		marker = "▸ "
	}

	label := item.Label
	if item.Colored {
		label = lipgloss.NewStyle().Foreground(styles.OperationColor(item.ColorIdx)).Render("█ ") + label
	}

	line := marker + label

	if item.Detail != "" {
		line += styles.CardDetail.Render("  " + item.Detail)
	}

	if i == m.cursor {
		return lipgloss.NewStyle().Foreground(styles.Accent).Bold(true).Render(marker) +
			strings.TrimPrefix(line, marker)
	}

	return line
}

// helpModal shows every binding.
type helpModal struct {
	body string
}

// newHelp builds the help overlay.
func newHelp(body string) *helpModal { return &helpModal{body: body} }

// Update implements modal.
func (m *helpModal) Update(tea.KeyMsg) (modal, tea.Cmd) {
	return nil, emit(modalDismissed{})
}

// View implements modal.
func (m *helpModal) View(width, height int) string {
	content := lipgloss.JoinVertical(lipgloss.Left,
		styles.ModalTitle.Render("q keys"),
		"",
		m.body,
		"",
		styles.Footer.Render("any key to close"),
	)

	return center(styles.Modal.Render(content), width, height)
}

// center places content in the middle of the screen.
func center(content string, width, height int) string {
	if width <= 0 || height <= 0 {
		return content
	}

	return lipgloss.Place(width, height, lipgloss.Center, lipgloss.Center, content)
}
