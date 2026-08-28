package tui

import (
	"strings"

	"github.com/charmbracelet/bubbles/textarea"
	"github.com/charmbracelet/bubbles/textinput"
	tea "github.com/charmbracelet/bubbletea"
)

// textAreaHeight is how many rows a multi-line field shows.
const textAreaHeight = 6

// textArea is a text field that is either a single line or a block.
//
// It wraps the two bubbles components behind one small interface so forms can hold a
// list of fields without caring which kind each one is.
type textArea struct {
	multiline bool
	input     textinput.Model
	area      textarea.Model
}

// newTextArea returns a field containing initial.
func newTextArea(initial string, multiline bool) *textArea {
	if multiline {
		area := textarea.New()
		area.SetValue(initial)
		area.SetHeight(textAreaHeight)
		area.ShowLineNumbers = false
		area.Focus()
		area.CursorEnd()

		return &textArea{multiline: true, area: area}
	}

	input := textinput.New()
	input.SetValue(initial)
	input.Focus()
	input.CursorEnd()

	return &textArea{input: input}
}

// Update feeds a keypress to the field.
func (t *textArea) Update(msg tea.KeyMsg) tea.Cmd {
	var cmd tea.Cmd

	if t.multiline {
		t.area, cmd = t.area.Update(msg)

		return cmd
	}

	t.input, cmd = t.input.Update(msg)

	return cmd
}

// Value returns the field's contents.
func (t *textArea) Value() string {
	if t.multiline {
		return t.area.Value()
	}

	return t.input.Value()
}

// SetValue replaces the field's contents.
func (t *textArea) SetValue(v string) {
	if t.multiline {
		t.area.SetValue(v)

		return
	}

	t.input.SetValue(v)
}

// Lines returns the field's contents split into logical lines.
func (t *textArea) Lines() []string {
	return strings.Split(t.Value(), "\n")
}

// Line reports which logical line the cursor is on. A single-line field has one.
func (t *textArea) Line() int {
	if t.multiline {
		return t.area.Line()
	}

	return 0
}

// SetLines replaces the contents and leaves the cursor at the end of line row.
//
// The textarea offers no way to edit one line, so the value is rebuilt whole.
// Placing the cursor afterwards means walking it back up from the end, where
// SetValue leaves it: the walk is by screen row rather than by logical line, so it
// is bounded by how many rows the content can occupy rather than by the line
// count, which a wrapped path would undercount.
func (t *textArea) SetLines(lines []string, row int) {
	t.SetValue(strings.Join(lines, "\n"))

	if !t.multiline {
		return
	}

	budget := len(lines)
	width := max(t.area.Width(), 1)

	for _, line := range lines {
		budget += len(line) / width
	}

	for range budget {
		if t.area.Line() <= row {
			break
		}

		t.area.CursorUp()
	}

	t.area.CursorEnd()
}

// Focus gives the field the cursor.
func (t *textArea) Focus() tea.Cmd {
	if t.multiline {
		return t.area.Focus()
	}

	return t.input.Focus()
}

// Blur takes the cursor away.
func (t *textArea) Blur() {
	if t.multiline {
		t.area.Blur()

		return
	}

	t.input.Blur()
}

// Focused reports whether the field holds the cursor.
func (t *textArea) Focused() bool {
	if t.multiline {
		return t.area.Focused()
	}

	return t.input.Focused()
}

// View renders the field at the given width.
func (t *textArea) View(width int) string {
	if width < 10 {
		width = 10
	}

	if t.multiline {
		t.area.SetWidth(width)

		return t.area.View()
	}

	t.input.Width = width

	return t.input.View()
}
