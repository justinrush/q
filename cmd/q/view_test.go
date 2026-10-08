package main

import (
	"bytes"
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/api"
)

// viewScript drives runView with canned answers and records what it attached to.
type viewScript struct {
	commands []api.AttachCommand
	failures []error
	attached [][]string
	asked    int
}

func (s *viewScript) command(context.Context) (api.AttachCommand, error) {
	i := s.asked
	s.asked++

	if i < len(s.failures) && s.failures[i] != nil {
		return api.AttachCommand{}, s.failures[i]
	}

	return s.commands[min(i, len(s.commands)-1)], nil
}

func (s *viewScript) attach(_ context.Context, argv []string, _ viewIO) error {
	s.attached = append(s.attached, argv)

	return errors.New("exit status 255")
}

// A dropped connection must leave the pane open, saying what happened, and
// reconnect when asked.
func TestViewSurvivesADroppedConnectionAndReconnects(t *testing.T) {
	script := &viewScript{commands: []api.AttachCommand{{Argv: []string{"ssh", "mini", "q", "attach", "ms_1"}, Host: "mini"}}}

	var out bytes.Buffer

	// Enter once to reconnect, then q to close.
	err := runView(t.Context(), viewIO{in: strings.NewReader("\nq\n"), out: &out, err: &out},
		"ms_1", script.command, script.attach)
	if err != nil {
		t.Fatalf("runView: %v", err)
	}

	if len(script.attached) != 2 {
		t.Errorf("attached %d times, want twice: once, and once more after enter", len(script.attached))
	}

	for _, want := range []string{"the connection to mini ended", "press enter to reconnect"} {
		if !strings.Contains(out.String(), want) {
			t.Errorf("output lacks %q:\n%s", want, out.String())
		}
	}
}

// The command is asked for on every attempt, so a mission that has moved since
// the pane opened is reported rather than attached to where it no longer is.
func TestViewAsksAgainEachTimeAndReportsARefusal(t *testing.T) {
	script := &viewScript{
		commands: []api.AttachCommand{{Argv: []string{"ssh", "mini"}, Host: "mini"}},
		failures: []error{nil, errors.New("build it now runs on this machine; close this and open the mission again")},
	}

	var out bytes.Buffer

	if err := runView(t.Context(), viewIO{in: strings.NewReader("\nq\n"), out: &out, err: &out},
		"ms_1", script.command, script.attach); err != nil {
		t.Fatalf("runView: %v", err)
	}

	if script.asked != 2 || len(script.attached) != 1 {
		t.Errorf("asked %d times and attached %d, want 2 and 1", script.asked, len(script.attached))
	}

	if !strings.Contains(out.String(), "now runs on this machine") {
		t.Errorf("the refusal was not shown:\n%s", out.String())
	}
}

// With nothing left to read — the pane's terminal has gone — the view ends
// rather than spinning.
func TestViewEndsWhenItsTerminalCloses(t *testing.T) {
	script := &viewScript{commands: []api.AttachCommand{{Argv: []string{"ssh", "mini"}, Host: "mini"}}}

	var out bytes.Buffer

	if err := runView(t.Context(), viewIO{in: strings.NewReader(""), out: &out, err: &out},
		"ms_1", script.command, script.attach); err != nil {
		t.Fatalf("runView: %v", err)
	}

	if len(script.attached) != 1 {
		t.Errorf("attached %d times with no terminal, want once", len(script.attached))
	}
}
