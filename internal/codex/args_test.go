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
