package tui

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/git"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/tui/keys"
	"github.com/justinrush/q/internal/tui/styles"
)

// Timings.
const (
	// tickInterval is how often the model checks its own liveness and re-renders
	// relative timestamps.
	tickInterval = 5 * time.Second
	// streamSilentAfter is how long without an event before the header says the
	// connection looks dead.
	//
	// The daemon sends a heartbeat every fifteen seconds, so silence past this means
	// the socket died without telling us.
	streamSilentAfter = 25 * time.Second
	// toastLife is how long a transient message stays on screen.
	toastLife = 4 * time.Second
	// helpDescWidth is the width of a description cell in the help overlay.
	helpDescWidth = 17
)

// tab identifies a view.
type tab int

// The views.
const (
	tabBoard tab = iota
	tabOperations
	tabCount
)

// Options are the parts of q's configuration the board needs.
//
// The board is a client of the daemon and does not read the config file itself;
// cmd/q resolves these and hands them over, which is what keeps the TUI
// testable without a home directory.
type Options struct {
	// Repos bounds the checkout search behind the repo picker.
	Repos git.ScanOptions
	// DefaultTool is the agent a new mission starts with.
	DefaultTool mission.Tool
}

// withDefaults fills in anything cmd/q left unset.
func (o Options) withDefaults() Options {
	if o.DefaultTool == "" {
		o.DefaultTool = mission.DefaultTool
	}

	return o
}

// App is the root model.
type App struct {
	client *api.Client
	keys   keys.Global
	opts   Options

	board      *Board
	operations *Operations
	active     tab

	modal modal
	toast toast

	snapshot mission.Snapshot
	width    int
	height   int

	// events carries frames from the SSE reader goroutine.
	events chan tea.Msg
	// lastEvent is when the stream last said anything, heartbeats included.
	lastEvent time.Time
	// streamDown reports that the event stream is not currently connected.
	streamDown bool
	// streamErr is the last reason the stream ended, so a repeated failure is not
	// reported repeatedly.
	streamErr string
	// reconnectIn backs off repeated reconnection attempts.
	reconnectIn time.Duration
}

// toast is a transient message shown in the header.
type toast struct {
	text  string
	err   bool
	until time.Time
}

// New returns the root model.
func New(c *api.Client, opts Options) *App {
	return &App{
		client:      c,
		keys:        keys.NewGlobal(),
		opts:        opts.withDefaults(),
		board:       NewBoard(),
		operations:  NewOperations(),
		events:      make(chan tea.Msg, 64),
		lastEvent:   time.Now(),
		reconnectIn: 500 * time.Millisecond,
	}
}

// Init implements tea.Model.
func (a *App) Init() tea.Cmd {
	return tea.Batch(
		a.fetchSnapshot(),
		a.startStream(),
		a.listen(),
		tea.Tick(tickInterval, func(time.Time) tea.Msg { return tickMsg{} }),
	)
}

// Internal messages.
type (
	// tickMsg drives liveness checks and relative-time redraws.
	tickMsg struct{}
	// snapshotMsg carries a full state fetch.
	snapshotMsg struct{ Snapshot mission.Snapshot }
	// streamEventMsg carries one decoded event from the daemon.
	streamEventMsg struct{ Event api.Event }
	// streamDownMsg reports that the event stream ended.
	streamDownMsg struct{ Err error }
	// toastMsg shows a transient message.
	toastMsg struct {
		text string
		err  bool
	}
	// refreshMsg asks for a fresh snapshot.
	refreshMsg struct{}
)

// Update implements tea.Model.
//
// It is deliberately a short router. Every branch delegates, which is what keeps this
// method readable as the number of message types grows.
func (a *App) Update(msg tea.Msg) (tea.Model, tea.Cmd) {
	switch m := msg.(type) {
	case tea.WindowSizeMsg:
		a.resize(m)

		return a, nil
	case tea.KeyMsg:
		return a, a.handleKey(m)
	case snapshotMsg:
		return a, a.applySnapshot(m.Snapshot)
	case streamEventMsg:
		return a, a.handleStreamEvent(m)
	case streamDownMsg:
		return a, a.handleStreamDown(m)
	case tickMsg:
		return a, a.handleTick()
	case toastMsg:
		a.toast = toast{text: m.text, err: m.err, until: time.Now().Add(toastLife)}

		return a, nil
	case refreshMsg:
		return a, a.fetchSnapshot()
	case modalDismissed:
		return a, nil
	}

	return a, a.handleIntent(msg)
}

// resize records the new terminal size.
func (a *App) resize(msg tea.WindowSizeMsg) {
	a.width, a.height = msg.Width, msg.Height

	// Reserve the header and footer rows.
	inner := msg.Height - 3

	a.board.SetSize(msg.Width, inner)
	a.operations.SetSize(msg.Width, inner)
}

// handleKey routes a keypress: modals first, then globals, then the active view.
func (a *App) handleKey(msg tea.KeyMsg) tea.Cmd {
	// A modal owns the keyboard while it is up, so a stray key cannot act on the
	// board behind it.
	if a.modal != nil {
		next, cmd := a.modal.Update(msg)
		a.modal = next

		return cmd
	}

	if cmd, handled := a.handleGlobalKey(msg); handled {
		return cmd
	}

	if a.active == tabBoard {
		return a.board.Update(msg)
	}

	return a.operations.Update(msg)
}

// handleGlobalKey handles the bindings that work everywhere.
func (a *App) handleGlobalKey(msg tea.KeyMsg) (tea.Cmd, bool) {
	switch {
	case key.Matches(msg, a.keys.Quit):
		return tea.Quit, true
	case key.Matches(msg, a.keys.Help):
		a.modal = newHelp(a.helpBody())

		return nil, true
	case key.Matches(msg, a.keys.NextTab):
		a.active = (a.active + 1) % tabCount

		return nil, true
	case key.Matches(msg, a.keys.PrevTab):
		a.active = (a.active - 1 + tabCount) % tabCount

		return nil, true
	case key.Matches(msg, a.keys.Board):
		a.active = tabBoard

		return nil, true
	case key.Matches(msg, a.keys.Operations):
		a.active = tabOperations

		return nil, true
	case key.Matches(msg, a.keys.Refresh):
		return a.fetchSnapshot(), true
	}

	return nil, false
}

// applySnapshot stores a fresh snapshot and hands it to the views.
func (a *App) applySnapshot(snap mission.Snapshot) tea.Cmd {
	a.snapshot = snap
	a.board.SetSnapshot(snap)
	a.operations.SetSnapshot(snap)

	return nil
}

// handleStreamEvent applies one event and re-arms the listener.
//
// Re-arming on every event is the standard way to model a channel as a bubbletea
// subscription: each received message schedules the next receive.
func (a *App) handleStreamEvent(msg streamEventMsg) tea.Cmd {
	a.lastEvent = time.Now()
	a.streamDown = false
	a.reconnectIn = 500 * time.Millisecond

	cmds := []tea.Cmd{a.listen()}

	if cmd := a.applyEvent(msg.Event); cmd != nil {
		cmds = append(cmds, cmd)
	}

	return tea.Batch(cmds...)
}

// handleStreamDown records a dropped stream and schedules a reconnect with backoff.
//
// The daemon drops a subscriber that falls behind, so a disconnect is expected rather
// than exceptional; it is only worth telling the user about if it keeps happening,
// which the backoff makes visible in the header.
func (a *App) handleStreamDown(msg streamDownMsg) tea.Cmd {
	a.streamDown = true

	if msg.Err != nil {
		log := a.streamErr
		a.streamErr = msg.Err.Error()

		// Only surface a changed reason, so a flapping connection does not bury the
		// board in identical toasts.
		if log != a.streamErr {
			a.toast = toast{text: "event stream: " + a.streamErr, err: true, until: time.Now().Add(toastLife)}
		}
	}

	delay := a.reconnectIn
	if a.reconnectIn < 8*time.Second {
		a.reconnectIn *= 2
	}

	return tea.Tick(delay, func(time.Time) tea.Msg { return reconnectMsg{} })
}

// reconnectMsg asks for another attempt at the event stream.
type reconnectMsg struct{}

// handleTick checks stream liveness and keeps relative timestamps honest.
func (a *App) handleTick() tea.Cmd {
	cmds := []tea.Cmd{tea.Tick(tickInterval, func(time.Time) tea.Msg { return tickMsg{} })}

	if a.toast.text != "" && time.Now().After(a.toast.until) {
		a.toast = toast{}
	}

	// The daemon heartbeats, so prolonged silence means the socket died quietly.
	if !a.streamDown && time.Since(a.lastEvent) > streamSilentAfter {
		a.streamDown = true
		cmds = append(cmds, a.startStream(), a.fetchSnapshot())
	}

	return tea.Batch(cmds...)
}

// View implements tea.Model.
func (a *App) View() string {
	if a.width == 0 {
		return "starting…"
	}

	body := a.board.View()
	if a.active == tabOperations {
		body = a.operations.View()
	}

	screen := lipgloss.JoinVertical(lipgloss.Left, a.header(), body)

	// A modal replaces the screen rather than overlaying it, because lipgloss has no
	// true compositing and a half-overlaid board is harder to read than a clean dialog.
	if a.modal != nil {
		return a.modal.View(a.width, a.height)
	}

	return lipgloss.JoinVertical(lipgloss.Left, screen, a.footer())
}

// header renders the tab bar and status.
func (a *App) header() string {
	tabs := make([]string, 0, int(tabCount))

	for i, name := range []string{a.board.Title(), a.operations.Title()} {
		style := styles.TabInactive
		if tab(i) == a.active {
			style = styles.TabActive
		}

		tabs = append(tabs, style.Render(name))
	}

	left := styles.Title.Render("q ") + strings.Join(tabs, " ")

	return lipgloss.JoinVertical(lipgloss.Left, left+"  "+a.status(), "")
}

// status renders the right-hand side of the header.
func (a *App) status() string {
	var parts []string

	if a.streamDown {
		parts = append(parts, styles.CardError.Render("● reconnecting"))
	}

	if label := a.board.FilterLabel(); label != "" && a.active == tabBoard {
		parts = append(parts, styles.CardWaiting.Render(label+" (esc clears)"))
	}

	if a.toast.text != "" {
		style := styles.Toast
		if a.toast.err {
			style = styles.ToastError
		}

		parts = append(parts, style.Render(a.toast.text))
	}

	return strings.Join(parts, "  ")
}

// footer renders the contextual help line.
func (a *App) footer() string {
	var bindings []key.Binding

	if a.active == tabBoard {
		bindings = a.board.Help()
	} else {
		bindings = a.operations.Help()
	}

	bindings = append(bindings, a.keys.NextTab, a.keys.Help, a.keys.Quit)

	parts := make([]string, 0, len(bindings))

	for _, binding := range bindings {
		help := binding.Help()
		if help.Key == "" {
			continue
		}

		parts = append(parts, help.Key+" "+help.Desc)
	}

	return styles.Footer.Render(styles.Truncate(strings.Join(parts, "   "), a.width))
}

// helpBody renders the full keymap for the help overlay.
func (a *App) helpBody() string {
	sections := []struct {
		name   string
		groups [][]key.Binding
	}{
		{"global", keys.NewGlobal().FullHelp()},
		{"board", keys.NewBoard().FullHelp()},
		{"operations", keys.NewOperations().FullHelp()},
		{"forms", keys.NewForm().FullHelp()},
	}

	var rows []string

	for _, section := range sections {
		rows = append(rows, styles.FieldLabelFocused.Render(section.name))

		for _, group := range section.groups {
			var line []string

			for _, binding := range group {
				help := binding.Help()
				if help.Key == "" {
					continue
				}

				// A fixed-width cell per binding, so the columns line up down the
				// overlay rather than drifting with each description's length.
				line = append(line, fmt.Sprintf("%-11s %-*s", help.Key, helpDescWidth, help.Desc))
			}

			if len(line) > 0 {
				rows = append(rows, "  "+strings.TrimRight(strings.Join(line, " "), " "))
			}
		}

		rows = append(rows, "")
	}

	rows = append(rows,
		styles.CardDetail.Render("ctrl+h/j/k/l are deliberately unbound: your tmux config"),
		styles.CardDetail.Render("routes them to vim-tmux-navigator, so they never arrive."),
	)

	return strings.Join(rows, "\n")
}

// currentOperationID returns the operation to preselect in a new-mission form.
func (a *App) currentOperationID() mission.OperationID {
	if a.active == tabOperations {
		if operation, ok := a.operations.Selected(); ok {
			return operation.ID
		}
	}

	if ms, ok := a.board.Selected(); ok {
		return ms.OperationID
	}

	if len(a.snapshot.Operations) > 0 {
		return a.snapshot.Operations[0].ID
	}

	return ""
}

// ctx is the context used for daemon calls.
//
// Requests are short-lived and the client applies its own timeout, so a background
// context is the right scope here.
func (a *App) ctx() context.Context { return context.Background() }
