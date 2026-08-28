package termopen

import (
	"errors"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/runner"
	"github.com/justinrush/q/internal/settings"
)

const osaScriptBin = "/usr/bin/osascript"

func newTestOpener() (*Opener, *runner.Fake) {
	fake := runner.NewFake()

	return New(Config{Mode: settings.TerminalGhostty, ScriptBin: osaScriptBin, Run: fake}), fake
}

func testSpec() Spec {
	return Spec{
		Dir:  "/missions/a mission",
		Argv: []string{"/opt/homebrew/bin/tmux", "attach-session", "-t", "=q-mission"},
	}
}

func TestOpenCreatesAWindowInTheExistingGhosttyInstance(t *testing.T) {
	opener, fake := newTestOpener()
	spec := testSpec()

	err := opener.Open(t.Context(), spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	call := fake.Calls()[0]
	if call.Name != osaScriptBin {
		t.Errorf("binary = %q, want %q", call.Name, osaScriptBin)
	}

	if !strings.Contains(call.Args[1], `tell application "Ghostty"`) {
		t.Errorf("script should target Ghostty:\n%s", call.Args[1])
	}

	if !strings.Contains(call.Args[1], "new window with configuration") {
		t.Errorf("script should create a window in the running application:\n%s", call.Args[1])
	}

	if strings.Contains(call.String(), "open -na") {
		t.Errorf("launch must not create a separate application instance: %q", call)
	}
}

func TestOpenPassesMissionValuesAsArguments(t *testing.T) {
	opener, fake := newTestOpener()
	spec := testSpec()

	err := opener.Open(t.Context(), spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	args := fake.Calls()[0].Args
	if args[0] != "-e" || args[2] != "--" {
		t.Fatalf("osascript arguments = %q", args)
	}

	if args[3] != spec.Dir {
		t.Errorf("working directory = %q, want %q", args[3], spec.Dir)
	}

	wantCommand := "/opt/homebrew/bin/tmux attach-session -t =q-mission"
	if args[4] != wantCommand {
		t.Errorf("command = %q, want %q", args[4], wantCommand)
	}

	if strings.Contains(args[1], spec.Dir) || strings.Contains(args[1], wantCommand) {
		t.Errorf("mission values must not be interpolated into AppleScript:\n%s", args[1])
	}
}

func TestOpenRejectsAnEmptyCommand(t *testing.T) {
	opener, _ := newTestOpener()
	spec := testSpec()
	spec.Argv = nil

	err := opener.Open(t.Context(), spec)
	if err == nil {
		t.Error("expected an error with no command to run")
	}
}

func TestRaiseActivatesGhosttyWithoutCreatingAWindow(t *testing.T) {
	opener, fake := newTestOpener()

	err := opener.Raise(t.Context())
	if err != nil {
		t.Fatalf("Raise: %v", err)
	}

	call := fake.Calls()[0]
	if !strings.Contains(call.String(), `tell application "Ghostty" to activate`) {
		t.Errorf("raise should activate Ghostty: %q", call)
	}

	if strings.Contains(call.String(), "new window") {
		t.Errorf("raise must not create a window: %q", call)
	}
}

func TestDescribeMatchesWhatOpenRuns(t *testing.T) {
	opener, fake := newTestOpener()
	spec := testSpec()

	err := opener.Open(t.Context(), spec)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	got := opener.Describe(spec)
	want := fake.Argv()[0]
	if got != want {
		t.Errorf("Describe() = %q, want %q", got, want)
	}
}

// A command-mode template is expanded without a shell, so a mission directory
// containing a space cannot change what runs.
func TestCommandModeExpandsTheTemplate(t *testing.T) {
	fake := runner.NewFake()
	opener := New(Config{
		Mode:    settings.TerminalCommand,
		Command: []string{"wezterm", "start", "--cwd", "{dir}", "--", "{argv}"},
		Run:     fake,
	})

	if err := opener.Open(t.Context(), testSpec()); err != nil {
		t.Fatalf("Open: %v", err)
	}

	call := fake.Calls()[0]
	if call.Name != "wezterm" {
		t.Errorf("binary = %q, want wezterm", call.Name)
	}

	want := []string{
		"start", "--cwd", "/missions/a mission", "--",
		"/opt/homebrew/bin/tmux", "attach-session", "-t", "=q-mission",
	}
	if len(call.Args) != len(want) {
		t.Fatalf("args = %v, want %v", call.Args, want)
	}

	for i := range want {
		if call.Args[i] != want[i] {
			t.Fatalf("args = %v, want %v", call.Args, want)
		}
	}
}

// A template that never says where the command goes would open a window running
// something else, which looks like success and is not.
func TestCommandModeRejectsATemplateWithNoCommandPlaceholder(t *testing.T) {
	opener := New(Config{
		Mode:    settings.TerminalCommand,
		Command: []string{"wezterm", "start", "--cwd", "{dir}"},
		Run:     runner.NewFake(),
	})

	if err := opener.Open(t.Context(), testSpec()); err == nil {
		t.Fatal("expected an error for a template with no {argv} or {cmd}")
	}
}

// The "none" mode opens nothing and says what to run instead.
func TestNoneModeReportsTheCommandToRunByHand(t *testing.T) {
	fake := runner.NewFake()
	opener := New(Config{Mode: settings.TerminalNone, Run: fake})

	err := opener.Open(t.Context(), testSpec())
	if !errors.Is(err, ErrNoTerminal) {
		t.Fatalf("Open error = %v, want ErrNoTerminal", err)
	}

	if !strings.Contains(err.Error(), "attach-session") {
		t.Errorf("error should name the command to run: %v", err)
	}

	if len(fake.Calls()) != 0 {
		t.Errorf("no process should have run, got %v", fake.Calls())
	}
}
