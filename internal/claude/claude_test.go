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
