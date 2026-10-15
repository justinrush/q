package codex

import (
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

// TestArgsModelAndEffort covers both invocation paths, because codex reaches a
// resumed session through a subcommand whose flags follow it. A model that
// applied only to a fresh launch would silently change on the first resume.
func TestArgsModelAndEffort(t *testing.T) {
	cases := []struct {
		name   string
		inv    mission.Invocation
		want   []string
		absent []string
	}{
		{
			name:   "a mission naming neither invokes codex as it always did",
			inv:    mission.Invocation{MissionDir: "/m"},
			absent: []string{"--model", "model_reasoning_effort"},
		},
		{
			name: "a model is passed as a quoted flag",
			inv:  mission.Invocation{MissionDir: "/m", Model: "gpt-5.1-codex"},
			want: []string{"--model", "'gpt-5.1-codex'"},
		},
		{
			name: "effort travels as a -c override, since codex has no flag for it",
			inv:  mission.Invocation{MissionDir: "/m", Model: "gpt-5.1-codex", Effort: "high"},
			want: []string{"-c", "'model_reasoning_effort=high'"},
		},
		{
			name: "a resumed session carries the same model",
			inv: mission.Invocation{
				MissionDir: "/m", SessionID: "abc", Resume: true,
				Model: "gpt-5.1-codex", Effort: "low",
			},
			want: []string{"resume", "--model", "'gpt-5.1-codex'", "'model_reasoning_effort=low'"},
		},
		{
			name:   "a resumed session with neither is unchanged",
			inv:    mission.Invocation{MissionDir: "/m", SessionID: "abc", Resume: true},
			want:   []string{"resume"},
			absent: []string{"--model", "model_reasoning_effort"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			line := strings.Join(New("/bin/codex", Options{}).Args(tc.inv), " ")

			for _, want := range tc.want {
				if !strings.Contains(line, want) {
					t.Errorf("argv = %q, want it to contain %q", line, want)
				}
			}

			for _, absent := range tc.absent {
				if strings.Contains(line, absent) {
					t.Errorf("argv = %q, want it not to contain %q", line, absent)
				}
			}
		})
	}
}

// TestRemoteArgsKeepModel guards the app-server path, which splices --remote
// into the argv by index. Adding flags ahead of it must not displace the
// subcommand a resume depends on.
func TestRemoteArgsKeepModel(t *testing.T) {
	agent := New("/bin/codex", Options{})

	cases := []struct {
		name string
		inv  mission.Invocation
		// wantFirst is the argument the spliced argv must begin with.
		wantFirst string
	}{
		{
			name:      "a fresh launch leads with the remote transport",
			inv:       mission.Invocation{MissionDir: "/m", Model: "gpt-5.1-codex", Effort: "high"},
			wantFirst: "--remote",
		},
		{
			name: "a resume keeps its subcommand first",
			inv: mission.Invocation{
				MissionDir: "/m", SessionID: "abc", Resume: true, Model: "gpt-5.1-codex",
			},
			wantFirst: "resume",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := agent.remoteArgs(tc.inv)

			if args[0] != tc.wantFirst {
				t.Errorf("argv[0] = %q, want %q (full: %v)", args[0], tc.wantFirst, args)
			}

			line := strings.Join(args, " ")

			if !strings.Contains(line, "--remote unix://") {
				t.Errorf("argv = %q, want the remote transport intact", line)
			}

			if !strings.Contains(line, "'gpt-5.1-codex'") {
				t.Errorf("argv = %q, want the model to survive the splice", line)
			}
		})
	}
}

// codex enters plan mode only through its /plan command, and a prompt on the
// command line is submitted as written, so a planning session is started with
// no prompt and told what to do once it is listening.
func TestPlanModeLaunch(t *testing.T) {
	cases := []struct {
		name        string
		inv         mission.Invocation
		wantPrompt  bool
		wantOpening bool
	}{
		{
			name:       "an ordinary launch carries its prompt",
			inv:        mission.Invocation{MissionDir: "/m"},
			wantPrompt: true,
		},
		{
			name:        "a planning launch is opened instead",
			inv:         mission.Invocation{MissionDir: "/m", PlanMode: true},
			wantOpening: true,
		},
		{
			name:       "an ordinary resume carries its prompt",
			inv:        mission.Invocation{MissionDir: "/m", SessionID: "abc", Resume: true},
			wantPrompt: true,
		},
		{
			name:        "a planning resume is opened instead",
			inv:         mission.Invocation{MissionDir: "/m", SessionID: "abc", Resume: true, PlanMode: true},
			wantOpening: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			agent := New("/bin/codex", Options{})
			line := strings.Join(agent.Args(tc.inv), " ")

			if got := strings.Contains(line, mission.PromptArg); got != tc.wantPrompt {
				t.Errorf("argv = %q, prompt present = %v, want %v", line, got, tc.wantPrompt)
			}

			opening, ok := agent.Opening(tc.inv)
			if ok != tc.wantOpening {
				t.Fatalf("Opening() ok = %v, want %v", ok, tc.wantOpening)
			}

			if !tc.wantOpening {
				return
			}

			if !strings.HasPrefix(opening, "/plan ") {
				t.Errorf("Opening() = %q, want a /plan command", opening)
			}

			if !strings.Contains(opening, ".q/prompt.md") {
				t.Errorf("Opening() = %q, want it to name the brief", opening)
			}

			// codex folds a long or multi-line paste into a placeholder, and a
			// line that starts with one is no longer read as a command.
			if strings.Contains(opening, "\n") || len(opening) > 200 {
				t.Errorf("Opening() = %q, want one short line", opening)
			}
		})
	}
}

// Text sent before codex shows its composer answers whatever it was asking
// instead. These panes are what codex 0.162.1 draws.
func TestReady(t *testing.T) {
	cases := []struct {
		name string
		pane string
		want bool
	}{
		{name: "nothing drawn yet", pane: "\n\n"},
		{
			name: "asking about hooks",
			pane: "  Hooks need review\n  4 hooks are new or changed.\n\n" +
				"› 1. Review hooks\n  2. Trust all and continue\n\n  enter confirm · esc skip\n",
		},
		{
			name: "asking whether to implement a plan",
			pane: "  Implement this plan?\n› 1. Yes, implement this plan\n  2. Yes, clear context and implement\n" +
				"  enter select · esc back\n",
		},
		{
			name: "asking whether to trust the folder",
			pane: "  Trust this folder?\n\n  1. Trust and continue\n  2. Quit\n\n  enter continue · esc quit\n" +
				"› stale text\n",
		},
		{
			name: "a menu with no footer in view",
			pane: "› 2. Trust all and continue\n",
		},
		{
			name: "the composer",
			pane: "  >_ OpenAI Codex (v0.162.1)\n\n› Ask Codex to do anything\n\n" +
				"  GPT-6.1-Sol default · /m\n  ? for shortcuts\n",
			want: true,
		},
		{
			name: "the composer with text in it",
			pane: "› 12 angry men is a film\n  ? for shortcuts\n",
			want: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := New("/bin/codex", Options{}).Ready(tc.pane); got != tc.want {
				t.Errorf("Ready() = %v, want %v", got, tc.want)
			}
		})
	}
}

// TestPrologueStartsDaemonWithoutProfile guards the managed path. codex rejects
// --profile on app-server subcommands, so a prologue that passed it would always
// fail and fall back to a session q cannot observe — one whose questions and
// approvals never reach the board.
func TestPrologueStartsDaemonWithoutProfile(t *testing.T) {
	prologue := New("/bin/codex", Options{}).Prologue(mission.Invocation{MissionDir: "/m"})

	start, _, ok := strings.Cut(prologue, "; then")
	if !ok {
		t.Fatalf("prologue = %q, want an if ...; then guard", prologue)
	}

	if !strings.Contains(start, "app-server daemon start") {
		t.Errorf("guard = %q, want it to start the app-server daemon", start)
	}

	if strings.Contains(start, "--profile") {
		t.Errorf("guard = %q, want no --profile on the daemon command", start)
	}

	if !strings.Contains(prologue, "--remote") || !strings.Contains(prologue, "--profile") {
		t.Errorf("prologue = %q, want the remote terminal UI to keep its profile", prologue)
	}
}
