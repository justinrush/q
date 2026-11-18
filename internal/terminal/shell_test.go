package terminal

import (
	"os/exec"
	"slices"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/runner"
)

func TestShellJoinLeavesPlainArgumentsBare(t *testing.T) {
	got := ShellJoin([]string{"ssh", "-o", "ControlPath=/tmp/ssh-%C", "-t", "p3x984", "attach", "ms_01"})
	want := "ssh -o ControlPath=/tmp/ssh-%C -t p3x984 attach ms_01"

	if got != want {
		t.Errorf("ShellJoin() = %q, want %q", got, want)
	}
}

// The peer's q is named relative to the peer's home. A local shell must hand
// the ~ to ssh untouched, or the far end is told to run this machine's path.
func TestShellJoinKeepsTildeForTheFarShell(t *testing.T) {
	got := ShellJoin([]string{"ssh", "-t", "peer", "~/.local/bin/q", "attach", "ms_01"})
	want := "ssh -t peer '~/.local/bin/q' attach ms_01"

	if got != want {
		t.Errorf("ShellJoin() = %q, want %q", got, want)
	}
}

// Whatever the arguments, sh splits the joined line back into them.
func TestShellJoinRoundTripsThroughSh(t *testing.T) {
	argv := []string{"~/.local/bin/q", "it's", "a b", "$HOME", "`x`", "", `back\slash`, "*", "ok"}

	script := "for a in " + ShellJoin(argv) + `; do printf '%s\n' "$a"; done`

	out, err := exec.CommandContext(t.Context(), "sh", "-c", script).Output()
	if err != nil {
		t.Fatalf("sh: %v", err)
	}

	got := strings.Split(strings.TrimSuffix(string(out), "\n"), "\n")
	if !slices.Equal(got, argv) {
		t.Errorf("sh split %q into %q", ShellJoin(argv), got)
	}
}

func TestGhosttyQuotesTheCommand(t *testing.T) {
	fake := runner.NewFake()
	opener := NewGhostty("osascript", fake)

	err := opener.Open(t.Context(), Spec{Dir: "/tmp", Argv: []string{"ssh", "-t", "peer", "~/.local/bin/q", "attach", "ms_01"}})
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	args := fake.Calls()[0].Args
	if got := args[len(args)-1]; got != "ssh -t peer '~/.local/bin/q' attach ms_01" {
		t.Errorf("Ghostty command = %q", got)
	}
}
