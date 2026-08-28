// Package keys holds the TUI's keybindings.
//
// One constraint shapes the whole map: ctrl+h, ctrl+j, ctrl+k, and ctrl+l are bound in
// the user's tmux root table to vim-tmux-navigator, whose passthrough check matches on
// process name and does not include q. Those keys move tmux panes and never reach
// this application, so binding them would produce a control that silently does nothing.
// A test asserts no binding uses them.
//
// ctrl+a is likewise unavailable, being the tmux prefix.
package keys

import "github.com/charmbracelet/bubbles/key"

// Global bindings, available in every view.
type Global struct {
	NextTab    key.Binding
	PrevTab    key.Binding
	Board      key.Binding
	Operations key.Binding
	Refresh    key.Binding
	Help       key.Binding
	Quit       key.Binding
}

// NewGlobal returns the global bindings.
func NewGlobal() Global {
	return Global{
		NextTab:    key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "switch view")),
		PrevTab:    key.NewBinding(key.WithKeys("shift+tab")),
		Board:      key.NewBinding(key.WithKeys("B"), key.WithHelp("B", "board")),
		Operations: key.NewBinding(key.WithKeys("T"), key.WithHelp("T", "operations")),
		Refresh:    key.NewBinding(key.WithKeys("R"), key.WithHelp("R", "refresh")),
		Help:       key.NewBinding(key.WithKeys("?"), key.WithHelp("?", "help")),
		Quit:       key.NewBinding(key.WithKeys("q", "ctrl+c"), key.WithHelp("q", "quit")),
	}
}

// ShortHelp implements help.KeyMap.
func (g Global) ShortHelp() []key.Binding {
	return []key.Binding{g.NextTab, g.Help, g.Quit}
}

// FullHelp implements help.KeyMap.
func (g Global) FullHelp() [][]key.Binding {
	return [][]key.Binding{{g.NextTab, g.Board, g.Operations}, {g.Refresh, g.Help, g.Quit}}
}

// Board bindings, available on the kanban view.
type Board struct {
	Left       key.Binding
	Right      key.Binding
	Up         key.Binding
	Down       key.Binding
	MoveLeft   key.Binding
	MoveRight  key.Binding
	ReorderUp  key.Binding
	ReorderDn  key.Binding
	Lane       key.Binding
	First      key.Binding
	Last       key.Binding
	Open       key.Binding
	Message    key.Binding
	New        key.Binding
	Edit       key.Binding
	Delete     key.Binding
	TogglePlan key.Binding
	ToggleDone key.Binding
	Status     key.Binding
	Filter     key.Binding
	Clear      key.Binding
}

// NewBoard returns the board bindings.
func NewBoard() Board {
	return Board{
		Left:       key.NewBinding(key.WithKeys("left", "h"), key.WithHelp("←/h", "lane")),
		Right:      key.NewBinding(key.WithKeys("right", "l"), key.WithHelp("→/l", "lane")),
		Up:         key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "card")),
		Down:       key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "card")),
		MoveLeft:   key.NewBinding(key.WithKeys("H"), key.WithHelp("H/L", "move card")),
		MoveRight:  key.NewBinding(key.WithKeys("L")),
		ReorderUp:  key.NewBinding(key.WithKeys("K"), key.WithHelp("K/J", "reorder")),
		ReorderDn:  key.NewBinding(key.WithKeys("J")),
		Lane:       key.NewBinding(key.WithKeys("1", "2", "3", "4", "5"), key.WithHelp("1-5", "jump to lane")),
		First:      key.NewBinding(key.WithKeys("g"), key.WithHelp("g/G", "first/last")),
		Last:       key.NewBinding(key.WithKeys("G")),
		Open:       key.NewBinding(key.WithKeys("enter", "ctrl+o"), key.WithHelp("enter", "open debrief")),
		Message:    key.NewBinding(key.WithKeys("m"), key.WithHelp("m", "message agent")),
		New:        key.NewBinding(key.WithKeys("n"), key.WithHelp("n", "new mission")),
		Edit:       key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit")),
		Delete:     key.NewBinding(key.WithKeys("d", "x"), key.WithHelp("d", "delete")),
		TogglePlan: key.NewBinding(key.WithKeys("p"), key.WithHelp("p", "toggle plan mode")),
		ToggleDone: key.NewBinding(key.WithKeys("z"), key.WithHelp("z", "expand closed")),
		Status:     key.NewBinding(key.WithKeys("s"), key.WithHelp("s", "move to lane…")),
		Filter:     key.NewBinding(key.WithKeys("/"), key.WithHelp("/", "filter by operation")),
		Clear:      key.NewBinding(key.WithKeys("esc")),
	}
}

// ShortHelp implements help.KeyMap.
func (b Board) ShortHelp() []key.Binding {
	return []key.Binding{b.Left, b.Down, b.MoveLeft, b.Status, b.Open, b.New}
}

// FullHelp implements help.KeyMap.
func (b Board) FullHelp() [][]key.Binding {
	return [][]key.Binding{
		{b.Left, b.Right, b.Up, b.Down, b.Lane, b.First},
		{b.MoveLeft, b.ReorderUp, b.Open, b.Message},
		{b.New, b.Edit, b.Delete, b.TogglePlan},
		{b.Status, b.ToggleDone, b.Filter},
	}
}

// Operations bindings, available on the operation view.
type Operations struct {
	Up     key.Binding
	Down   key.Binding
	New    key.Binding
	Edit   key.Binding
	Delete key.Binding
	Detail key.Binding
}

// NewOperations returns the operation-view bindings.
func NewOperations() Operations {
	return Operations{
		Up:     key.NewBinding(key.WithKeys("up", "k"), key.WithHelp("↑/k", "up")),
		Down:   key.NewBinding(key.WithKeys("down", "j"), key.WithHelp("↓/j", "down")),
		New:    key.NewBinding(key.WithKeys("a", "n"), key.WithHelp("a", "add operation")),
		Edit:   key.NewBinding(key.WithKeys("e"), key.WithHelp("e", "edit")),
		Delete: key.NewBinding(key.WithKeys("d"), key.WithHelp("d", "delete")),
		Detail: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "detail")),
	}
}

// ShortHelp implements help.KeyMap.
func (t Operations) ShortHelp() []key.Binding {
	return []key.Binding{t.Down, t.New, t.Edit, t.Delete}
}

// FullHelp implements help.KeyMap.
func (t Operations) FullHelp() [][]key.Binding {
	return [][]key.Binding{{t.Up, t.Down, t.Detail}, {t.New, t.Edit, t.Delete}}
}

// Form bindings, available inside a dialog.
type Form struct {
	Next    key.Binding
	Prev    key.Binding
	Submit  key.Binding
	Cancel  key.Binding
	Toggle  key.Binding
	Newline key.Binding
}

// NewForm returns the form bindings.
func NewForm() Form {
	return Form{
		Next:    key.NewBinding(key.WithKeys("tab"), key.WithHelp("tab", "next field")),
		Prev:    key.NewBinding(key.WithKeys("shift+tab"), key.WithHelp("shift+tab", "previous")),
		Submit:  key.NewBinding(key.WithKeys("ctrl+s"), key.WithHelp("ctrl+s", "save")),
		Cancel:  key.NewBinding(key.WithKeys("esc"), key.WithHelp("esc", "cancel")),
		Toggle:  key.NewBinding(key.WithKeys(" "), key.WithHelp("space", "toggle")),
		Newline: key.NewBinding(key.WithKeys("enter"), key.WithHelp("enter", "newline")),
	}
}

// ShortHelp implements help.KeyMap.
func (f Form) ShortHelp() []key.Binding {
	return []key.Binding{f.Next, f.Submit, f.Cancel}
}

// FullHelp implements help.KeyMap.
func (f Form) FullHelp() [][]key.Binding {
	return [][]key.Binding{{f.Next, f.Prev, f.Toggle}, {f.Submit, f.Cancel}}
}

// Reasons a key is off limits. q runs inside tmux, which sees every keystroke
// first, so a binding tmux intercepts is one the board can never receive.
const (
	// reasonPaneNav covers the directional keys that vim-tmux-navigator and
	// similar tmux setups bind to move between panes. They are extremely common,
	// and a board that appeared to bind them would look broken on those machines.
	reasonPaneNav = "commonly bound to pane navigation in tmux; may never reach q"
	// reasonPrefix covers the tmux prefix itself.
	reasonPrefix = "a common tmux prefix, so it is unsafe to bind"
)

// Forbidden lists keys that must never be bound, with the reason.
//
// It exists so the constraint is testable rather than a comment someone has to
// remember.
var Forbidden = map[string]string{
	"ctrl+h": reasonPaneNav,
	"ctrl+j": reasonPaneNav,
	"ctrl+k": reasonPaneNav,
	"ctrl+l": reasonPaneNav,
	"ctrl+a": reasonPrefix,
	"ctrl+b": reasonPrefix,
}

// All returns every binding the TUI defines, for validation and help.
func All() []key.Binding {
	global := NewGlobal()
	board := NewBoard()
	operations := NewOperations()
	form := NewForm()

	var out []key.Binding

	for _, group := range [][][]key.Binding{global.FullHelp(), board.FullHelp(), operations.FullHelp(), form.FullHelp()} {
		for _, row := range group {
			out = append(out, row...)
		}
	}

	// Bindings absent from the help groups still count as bindings.
	out = append(out, global.PrevTab, board.MoveRight, board.ReorderDn, board.Last, board.Clear, form.Newline)

	return out
}
