package daemon

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/terminal"
)

// This is the pane codex showed for 25 minutes in a detached session with nothing on
// the board but a badge. Recognizing it is what turns that into a card that says why.
const codexTrustPane = `> You are in /Users/jarush/.local/share/q/missions/pipeline-images--inv-acr-scoped-tokens

  Do you trust the contents of this directory? Working with untrusted contents comes with
  higher risk of prompt injection. Trusting the directory allows project-local config,
  hooks, and exec policies to load.

› 1. Yes, continue
  2. No, quit

  Press enter to continue
`

// The second gate, which appears once the first is answered.
const codexHookReviewPane = `  Hooks need review
  1 hook is new or changed.
  Hooks can run outside the sandbox after you trust them.

› 1. Review hooks
  2. Trust all and continue
  3. Continue without trusting (hooks won't run)

  Press enter to confirm or esc to go back
`

func TestFindPromptRecognizesCodexTrustGate(t *testing.T) {
	got, ok := findPrompt(codexTrustPane)
	if !ok {
		t.Fatal("the directory trust prompt should be recognized")
	}

	if !strings.Contains(got, "waiting at prompt") {
		t.Errorf("hint = %q, want it to say it is waiting", got)
	}

	// The heading reads better on a card than the numbered options below it.
	if !strings.Contains(got, "You are in") {
		t.Errorf("hint = %q, want the first line of the prompt", got)
	}
}

func TestFindPromptRecognizesHookReviewGate(t *testing.T) {
	got, ok := findPrompt(codexHookReviewPane)
	if !ok {
		t.Fatal("the hook review prompt should be recognized")
	}

	if !strings.Contains(got, "Hooks need review") {
		t.Errorf("hint = %q, want it to name the hook review", got)
	}
}

// A working agent's screen must never be mistaken for a question, or a busy mission would
// be dragged into the waiting lane by whatever it happened to be printing.
func TestFindPromptIgnoresOrdinaryOutput(t *testing.T) {
	for _, pane := range []string{
		"",
		"   \n\n  \n",
		"╭───────────────╮\n│ OpenAI Codex  │\n╰───────────────╯\n\n  gpt-5.6-sol default",
		"• Ran git status\n  On branch jarush/mission\n  nothing to commit",
		"I'll start by reading the terraform files to understand the current setup.",
	} {
		if got, ok := findPrompt(pane); ok {
			t.Errorf("pane %q was read as a prompt: %q", pane, got)
		}
	}
}

// Screen text has no length limit, and a card has very little room.
func TestFindPromptBoundsItsLength(t *testing.T) {
	pane := strings.Repeat("a very long heading line ", 20) + "\nDo you trust this?\n"

	got, ok := findPrompt(pane)
	if !ok {
		t.Fatal("expected a prompt")
	}

	if len(got) > maxPromptLength+len("waiting at prompt: ")+len("…") {
		t.Errorf("hint is %d characters: %q", len(got), got)
	}
}

// The marker set is what makes detection work, so an empty one would silently disable it.
func TestPromptMarkersAreLowercase(t *testing.T) {
	if len(promptMarkers) == 0 {
		t.Fatal("no prompt markers defined")
	}

	for _, marker := range promptMarkers {
		if marker != strings.ToLower(marker) {
			t.Errorf("marker %q must be lowercase; matching is done on a lowered line", marker)
		}
	}
}

// stubProbe answers with one pane, which is all the session checks read.
type stubProbe struct {
	pane terminal.PaneInfo
}

func (p stubProbe) HasSession(context.Context, string) bool { return true }

func (p stubProbe) ListPanes(context.Context, terminal.Target) ([]terminal.PaneInfo, error) {
	return []terminal.PaneInfo{p.pane}, nil
}

func (p stubProbe) CapturePane(context.Context, terminal.Target, int) (string, error) {
	return "", nil
}

// Reconciliation may only file a card on evidence that the agent is gone. It once
// took an unfamiliar process name as that evidence, and claude's native install
// shows one: the launcher is a symlink to a versioned binary, so a live session's
// pane reports "2.1.251". Cards went to debrief seconds after going active, while
// the agent kept working and kept sending hooks.
func TestReconcileLeavesActiveMissionsRunningAnAgent(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    mission.Status
	}{
		{name: "claude on PATH", command: "claude", want: mission.StatusActive},
		{name: "claude's versioned binary", command: "2.1.251", want: mission.StatusActive},
		{name: "codex's node wrapper", command: "node", want: mission.StatusActive},
		{name: "a pane that fell back to a shell", command: "zsh", want: mission.StatusDebrief},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			ms := launchedServiceMission(t, svc)

			// Past the launch grace, so the pane's command is what decides.
			started := time.Now().Add(-time.Hour)
			ms.StartedAt = &started
			ms.TmuxSession = "q-mission"
			ms.AgentPaneID = "%1"
			ms.Status = mission.StatusActive
			ms.AgentState = mission.AgentBusy
			ms.LastEventAt = time.Now()

			if err := svcStore(svc).Mutate("test.active", func(snap *mission.Snapshot) error {
				snap.PutMission(ms)

				return nil
			}); err != nil {
				t.Fatalf("storing active mission: %v", err)
			}

			svc.apply(WithProbe(stubProbe{pane: terminal.PaneInfo{ID: "%1", Command: tc.command}}))
			svc.Reconcile(t.Context())

			stored, ok := svc.Snapshot().Mission(ms.ID)
			if !ok {
				t.Fatal("mission disappeared")
			}

			if stored.Status != tc.want {
				t.Errorf("status = %q, want %q (pane running %q)", stored.Status, tc.want, tc.command)
			}
		})
	}
}
