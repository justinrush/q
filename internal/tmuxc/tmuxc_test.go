package tmuxc

import (
	"strings"
	"testing"

	"github.com/justinrush/q/internal/runner"
)

const tmuxBin = "/opt/homebrew/bin/tmux"

func newTestTmux() (*Tmux, *runner.Fake) {
	fake := runner.NewFake()

	return New(tmuxBin, fake), fake
}

// tmux matches session names by prefix and fnmatch unless the target is written
// "=name". q generates session names from operation and mission slugs, which share
// prefixes constantly, so a missing "=" would silently address the wrong session.
func TestSessionTargetsAreExactMatch(t *testing.T) {
	if got := Session("q-foo").String(); got != "=q-foo" {
		t.Errorf("Session().String() = %q, want %q", got, "=q-foo")
	}

	if got := Window("q-foo", "agent").String(); got != "=q-foo:agent" {
		t.Errorf("Window().String() = %q, want %q", got, "=q-foo:agent")
	}
}

// Pane ids are already unambiguous and must not be prefixed, or tmux rejects them.
func TestPaneTargetsAreNotPrefixed(t *testing.T) {
	if got := Pane("%13").String(); got != "%13" {
		t.Errorf("Pane().String() = %q, want %q", got, "%13")
	}
}

// Every command that takes a target must emit the exact-match form. This is the
// regression test for the whole class of bug.
func TestEveryTargetedCommandUsesExactMatch(t *testing.T) {
	tmux, fake := newTestTmux()
	ctx := t.Context()

	_ = tmux.HasSession(ctx, "q-foo")
	_, _ = tmux.ListPanes(ctx, Session("q-foo"))
	_, _ = tmux.ListWindowNames(ctx, "q-foo")
	_, _ = tmux.HasClients(ctx, "q-foo")
	_ = tmux.SetWindowOption(ctx, Window("q-foo", "agent"), "automatic-rename", "off")
	_ = tmux.SelectLayout(ctx, Window("q-foo", "agent"), "main-vertical")
	_ = tmux.SelectWindow(ctx, Window("q-foo", "agent"))
	_ = tmux.KillSession(ctx, "q-foo")

	for _, line := range fake.Argv() {
		if !strings.Contains(line, "q-foo") {
			continue
		}

		if !strings.Contains(line, "=q-foo") {
			t.Errorf("target not exact-matched: %q", line)
		}
	}
}

// A detached new-session defaults to 80x24, which would leave an agent TUI
// rendering at 80 columns for as long as it ran unattended.
func TestNewSessionRequestsRealGeometry(t *testing.T) {
	tmux, fake := newTestTmux()
	fake.Default = runner.Result{Stdout: []byte("%13")}

	paneID, err := tmux.NewSession(t.Context(), NewSessionOptions{
		Name:    "q-mission",
		Window:  "agent",
		Dir:     "/missions/t",
		Command: []string{"/missions/t/.q/launch.sh"},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if paneID != "%13" {
		t.Errorf("pane id = %q, want %q", paneID, "%13")
	}

	argv := fake.Argv()[0]

	for _, want := range []string{
		"new-session -d",
		"-s q-mission",
		"-x 240",
		"-y 60",
		"-n agent",
		"-c /missions/t",
		"-P -F #{pane_id}",
		"/missions/t/.q/launch.sh",
	} {
		if !strings.Contains(argv, want) {
			t.Errorf("argv %q is missing %q", argv, want)
		}
	}
}

// The user's config may flip pane-base-index to 1, so a computed "session:0.1" can
// address the wrong pane. Every creation call must ask for the id back instead.
func TestPaneCreatingCommandsRequestThePaneID(t *testing.T) {
	tmux, fake := newTestTmux()
	fake.Default = runner.Result{Stdout: []byte("%21")}
	ctx := t.Context()

	_, _ = tmux.NewSession(ctx, NewSessionOptions{Name: "q-mission"})
	_, _ = tmux.SplitWindow(ctx, SplitOptions{Target: Window("q-mission", "agent")})

	for _, line := range fake.Argv() {
		if !strings.Contains(line, "-P -F #{pane_id}") {
			t.Errorf("pane-creating command does not capture the pane id: %q", line)
		}
	}
}

func TestNewSessionEnvIsSortedAndUsesDashE(t *testing.T) {
	tmux, fake := newTestTmux()
	fake.Default = runner.Result{Stdout: []byte("%13")}

	_, err := tmux.NewSession(t.Context(), NewSessionOptions{
		Name: "q-mission",
		Env: map[string]string{
			"Q_MISSION_ID":  "ms_abc",
			"Q_HOOK_EPOCH":  "1",
			"Q_DAEMON_FILE": "/state/daemon.json",
		},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	argv := fake.Argv()[0]

	// Sorted order makes the argv deterministic and therefore assertable.
	wantOrder := []string{
		"-e Q_DAEMON_FILE=/state/daemon.json",
		"-e Q_HOOK_EPOCH=1",
		"-e Q_MISSION_ID=ms_abc",
	}

	var last int

	for _, want := range wantOrder {
		idx := strings.Index(argv, want)
		if idx < 0 {
			t.Fatalf("argv %q is missing %q", argv, want)
		}

		if idx < last {
			t.Errorf("env args out of order in %q", argv)
		}

		last = idx
	}
}

// The daemon token must never reach a session environment: `tmux show-environment`
// prints it in plaintext.
func TestNewSessionEnvCarriesNoToken(t *testing.T) {
	tmux, fake := newTestTmux()
	fake.Default = runner.Result{Stdout: []byte("%13")}

	_, err := tmux.NewSession(t.Context(), NewSessionOptions{
		Name: "q-mission",
		Env:  map[string]string{"Q_DAEMON_FILE": "/state/daemon.json"},
	})
	if err != nil {
		t.Fatalf("NewSession: %v", err)
	}

	if strings.Contains(strings.ToLower(fake.Argv()[0]), "token") {
		t.Errorf("session environment must not mention a token: %q", fake.Argv()[0])
	}
}

// tmux 3.x deprecates -p for split sizing in favour of -l with a percentage.
func TestSplitWindowUsesModernSizeFlag(t *testing.T) {
	tmux, fake := newTestTmux()
	fake.Default = runner.Result{Stdout: []byte("%21")}

	_, err := tmux.SplitWindow(t.Context(), SplitOptions{
		Target:      Window("q-mission", "agent"),
		Dir:         "/wt",
		Horizontal:  true,
		SizePercent: 30,
		Command:     []string{"/opt/homebrew/bin/nvim", "+Neotree"},
	})
	if err != nil {
		t.Fatalf("SplitWindow: %v", err)
	}

	argv := fake.Argv()[0]

	if !strings.Contains(argv, "-l 30%") {
		t.Errorf("argv %q should size with -l 30%%", argv)
	}

	if strings.Contains(argv, "-p 30") {
		t.Errorf("argv %q uses the deprecated -p form", argv)
	}

	if !strings.Contains(argv, "-h") {
		t.Errorf("argv %q should split horizontally", argv)
	}
}

func TestListPanesParsesFields(t *testing.T) {
	tmux, fake := newTestTmux()
	fake.Expect(
		tmuxBin+" list-panes -t =q-mission -F #{pane_id}\t#{pane_dead}\t#{pane_current_command}\t#{pane_current_path}",
		"%13\t0\tclaude\t/missions/t\n%14\t1\tzsh\t/missions/t/weave",
	)

	panes, err := tmux.ListPanes(t.Context(), Session("q-mission"))
	if err != nil {
		t.Fatalf("ListPanes: %v", err)
	}

	if len(panes) != 2 {
		t.Fatalf("len = %d, want 2", len(panes))
	}

	if panes[0].ID != "%13" || panes[0].Dead || panes[0].Command != "claude" {
		t.Errorf("first pane = %+v", panes[0])
	}

	// A dead pane is how remain-on-exit reports that the agent finished, and the
	// message-sending guard depends on noticing it.
	if !panes[1].Dead {
		t.Errorf("second pane should be dead: %+v", panes[1])
	}
}

func TestHasSession(t *testing.T) {
	tmux, fake := newTestTmux()
	fake.Expect(tmuxBin+" has-session -t =q-live", "")
	fake.ExpectExit(tmuxBin+" has-session -t =q-gone", 1, "can't find session")

	if !tmux.HasSession(t.Context(), "q-live") {
		t.Error("HasSession should be true for a live session")
	}

	if tmux.HasSession(t.Context(), "q-gone") {
		t.Error("HasSession should be false for a missing session")
	}
}

// No tmux server running is not an error; there are simply no sessions.
func TestListSessionNamesToleratesNoServer(t *testing.T) {
	tmux, fake := newTestTmux()
	fake.ExpectExit(tmuxBin+" list-sessions -F #{session_name}", 1, "no server running")

	got, err := tmux.ListSessionNames(t.Context())
	if err != nil {
		t.Fatalf("ListSessionNames: %v", err)
	}

	if len(got) != 0 {
		t.Errorf("got %q, want none", got)
	}
}

func TestKillSessionToleratesMissingSession(t *testing.T) {
	tmux, fake := newTestTmux()
	fake.ExpectExit(tmuxBin+" kill-session -t =q-gone", 1, "can't find session")

	if err := tmux.KillSession(t.Context(), "q-gone"); err != nil {
		t.Errorf("KillSession: %v", err)
	}
}

func TestAttachArgs(t *testing.T) {
	tmux, _ := newTestTmux()

	got := strings.Join(tmux.AttachArgs("q-mission", false), " ")
	if got != tmuxBin+" attach-session -t =q-mission" {
		t.Errorf("AttachArgs = %q", got)
	}

	got = strings.Join(tmux.AttachArgs("q-mission", true), " ")
	if got != tmuxBin+" attach-session -d -t =q-mission" {
		t.Errorf("AttachArgs with detach = %q", got)
	}
}

// Bracketed paste makes a multi-line message arrive as one paste rather than as a
// series of submissions, one per newline.
func TestPasteBufferUsesBracketedPaste(t *testing.T) {
	tmux, fake := newTestTmux()

	if err := tmux.PasteBuffer(t.Context(), Pane("%13"), "q-msg"); err != nil {
		t.Fatalf("PasteBuffer: %v", err)
	}

	argv := fake.Argv()[0]

	if !strings.Contains(argv, "-p") {
		t.Errorf("argv %q should use bracketed paste", argv)
	}

	if !strings.Contains(argv, "-d") {
		t.Errorf("argv %q should delete the buffer after pasting", argv)
	}
}

func TestSetWindowAndPaneOptionScopes(t *testing.T) {
	tmux, fake := newTestTmux()
	ctx := t.Context()

	if err := tmux.SetWindowOption(ctx, Window("q-mission", "agent"), "automatic-rename", "off"); err != nil {
		t.Fatalf("SetWindowOption: %v", err)
	}

	if err := tmux.SetPaneOption(ctx, Pane("%13"), "remain-on-exit", "on"); err != nil {
		t.Fatalf("SetPaneOption: %v", err)
	}

	argv := fake.Argv()

	if !strings.Contains(argv[0], "set-option -w") {
		t.Errorf("window option argv = %q, want -w scope", argv[0])
	}

	if !strings.Contains(argv[1], "set-option -p") {
		t.Errorf("pane option argv = %q, want -p scope", argv[1])
	}
}

func TestCapturePaneLineLimit(t *testing.T) {
	tmux, fake := newTestTmux()
	fake.Default = runner.Result{Stdout: []byte("output")}

	if _, err := tmux.CapturePane(t.Context(), Pane("%13"), 40); err != nil {
		t.Fatalf("CapturePane: %v", err)
	}

	if got := fake.Argv()[0]; !strings.Contains(got, "-S -40") {
		t.Errorf("argv = %q, want a scrollback limit", got)
	}
}

func TestTargetEmpty(t *testing.T) {
	var zero Target

	if !zero.Empty() {
		t.Error("the zero Target should report Empty")
	}

	if Session("x").Empty() {
		t.Error("a named session target is not empty")
	}
}
