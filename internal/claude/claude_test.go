package claude

import (
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

// Workspace trust must be granted on every path into a session, not just the
// first one: a resume lands in the same never-before-seen mission directory, and
// a launch that stops on the trust dialog does so in a detached pane where nobody
// would see the question.
func TestPrologueGrantsWorkspaceTrust(t *testing.T) {
	cases := []struct {
		name string
		inv  mission.Invocation
	}{
		{name: "fresh", inv: mission.Invocation{SessionID: "s1"}},
		{name: "resume", inv: mission.Invocation{SessionID: "s1", Resume: true}},
		{name: "plan mode", inv: mission.Invocation{SessionID: "s1", PlanMode: true}},
	}

	agent := New("/usr/bin/claude", Options{})

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got := agent.Prologue(tc.inv)
			if !strings.Contains(got, "export "+EnvSandboxed+"=1") {
				t.Errorf("prologue does not grant workspace trust:\n%s", got)
			}
		})
	}
}

// The prologue is spliced in ahead of the exec, so it must end in a newline or it
// would run into the command it precedes.
func TestPrologueEndsInNewline(t *testing.T) {
	got := New("/usr/bin/claude", Options{}).Prologue(mission.Invocation{})
	if !strings.HasSuffix(got, "\n") {
		t.Errorf("prologue = %q, want a trailing newline", got)
	}
}

// TestArgsModelAndEffort covers the two flags a mission can now carry. The
// omitted case is the one that matters most: a mission created before model
// selection existed must invoke claude exactly as it always did.
func TestArgsModelAndEffort(t *testing.T) {
	cases := []struct {
		name   string
		model  string
		effort string
		// want are argv fragments the invocation must contain.
		want []string
		// absent are fragments it must not contain.
		absent []string
	}{
		{
			name:   "a mission naming neither invokes claude as it always did",
			absent: []string{"--model", "--effort"},
		},
		{
			name:   "a model is passed as a quoted flag",
			model:  "opus",
			want:   []string{"--model", "'opus'"},
			absent: []string{"--effort"},
		},
		{
			name:   "an effort accompanies its model",
			model:  "opus",
			effort: "high",
			want:   []string{"--model", "'opus'", "--effort", "'high'"},
		},
		{
			name:  "a model name needing quoting survives the shell",
			model: "claude-fable-5-1[1m]",
			want:  []string{"'claude-fable-5-1[1m]'"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			args := New("/bin/claude", Options{}).Args(mission.Invocation{
				Model:  tc.model,
				Effort: tc.effort,
			})

			line := strings.Join(args, " ")

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

			// The prompt is positional and must stay last, or the flags above would
			// swallow it.
			if args[len(args)-1] != mission.PromptArg {
				t.Errorf("argv ends with %q, want the prompt", args[len(args)-1])
			}
		})
	}
}

// TestArgsUserFlagsWinOverModel pins the ordering rule the package already
// documents: q's flags come first so a user's configured --model still wins,
// claude taking the last occurrence.
func TestArgsUserFlagsWinOverModel(t *testing.T) {
	args := New("/bin/claude", Options{Args: []string{"--model", "sonnet"}}).
		Args(mission.Invocation{Model: "opus"})

	line := strings.Join(args, " ")

	qAt := strings.Index(line, "'opus'")
	userAt := strings.Index(line, "'sonnet'")

	if qAt < 0 || userAt < 0 {
		t.Fatalf("argv = %q, want both models present", line)
	}

	if userAt < qAt {
		t.Errorf("argv = %q, want the user's model after q's so it wins", line)
	}
}
