package launch

import (
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// A plan-mode mission is invoked on its planning settings until its plan is
// accepted and on its implementing ones after, which is the whole of how a
// mission comes to plan on one model and implement on another.
func TestRelaunchFollowsThePlanPhase(t *testing.T) {
	cases := []struct {
		name   string
		mut    func(*mission.Mission)
		want   []string
		absent []string
	}{
		{
			name: "planning runs in plan mode on the planning pair",
			mut: func(ms *mission.Mission) {
				ms.PlanMode = true
				ms.PlanModel, ms.PlanEffort = "fable", "high"
			},
			want:   []string{"--permission-mode plan", "--model 'fable'", "--effort 'high'"},
			absent: []string{"'opus'", "'low'"},
		},
		{
			name: "planning with no planning model uses the implementing pair",
			mut:  func(ms *mission.Mission) { ms.PlanMode = true },
			want: []string{"--permission-mode plan", "--model 'opus'", "--effort 'low'"},
		},
		{
			name: "an approved plan is implemented out of plan mode on the implementing pair",
			mut: func(ms *mission.Mission) {
				ms.PlanMode, ms.PlanApproved = true, true
				ms.PlanModel, ms.PlanEffort = "fable", "high"
			},
			want:   []string{"--permission-mode auto", "--model 'opus'", "--effort 'low'"},
			absent: []string{"'fable'", "'high'", "--permission-mode plan"},
		},
		{
			name:   "an ordinary mission ignores planning settings",
			mut:    func(ms *mission.Mission) { ms.PlanModel = "fable" },
			want:   []string{"--permission-mode auto", "--model 'opus'"},
			absent: []string{"'fable'"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			launcher, fake, _ := newTestLauncher(t)

			ms := launchedMission(t, t.TempDir())
			ms.Model, ms.Effort = "opus", "low"
			tc.mut(&ms)

			fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "can't find session")

			got, err := launcher.Relaunch(t.Context(), testOperation("/dev/weave"), ms, "keep going")
			if err != nil {
				t.Fatalf("Relaunch: %v", err)
			}

			script := oneLine(readArtifact(t, got, "launch.sh"))

			for _, want := range tc.want {
				if !strings.Contains(script, want) {
					t.Errorf("script lacks %q:\n%s", want, script)
				}
			}

			for _, absent := range tc.absent {
				if strings.Contains(script, absent) {
					t.Errorf("script must not contain %q:\n%s", absent, script)
				}
			}
		})
	}
}

// oneLine undoes the launch script's one-argument-per-line layout, so a flag
// and its value can be matched together.
func oneLine(script string) string {
	return strings.Join(strings.Fields(strings.ReplaceAll(script, "\\\n", " ")), " ")
}

// codex is told to plan by a command typed into its UI, which must wait until
// it is listening: sent any sooner, the text answers whatever codex stopped to
// ask instead.
func TestPlanningCodexIsOpenedOnceReady(t *testing.T) {
	const (
		composer = "› Ask Codex to do anything\n  ? for shortcuts\n"
		asking   = "› 1. Review hooks\n  2. Trust all and continue\n  enter confirm · esc skip\n"
	)

	cases := []struct {
		name     string
		planMode bool
		pane     string
		wantSent bool
	}{
		{name: "sent once the composer is up", planMode: true, pane: composer, wantSent: true},
		{name: "held while codex is asking something", planMode: true, pane: asking},
		{name: "not sent to a session that is not planning", pane: composer},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			launcher, fake, _ := newTestLauncher(t)
			launcher.openingPoll = time.Millisecond

			ms := launchedMission(t, t.TempDir())
			ms.Tool = mission.ToolCodex
			ms.PlanMode = tc.planMode

			fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "can't find session")
			fake.Expect(tmuxBin+" capture-pane -p -t %13", tc.pane)
			fake.Expect(listPanesArgv, panesReply("%13", "codex", false))
			fake.Default = runner.Result{Stdout: []byte("%13")}

			got, err := launcher.Relaunch(t.Context(), testOperation("/dev/weave"), ms, "keep going")
			if err != nil {
				t.Fatalf("Relaunch: %v", err)
			}

			script := readArtifact(t, got, "launch.sh")
			if carried := strings.Contains(script, mission.PromptArg); carried == tc.planMode {
				t.Errorf("prompt on the command line = %v, want %v:\n%s", carried, !tc.planMode, script)
			}

			sent := func() bool { return strings.Contains(fake.Transcript(), "paste-buffer") }

			deadline := time.Now().Add(2 * time.Second)
			for tc.wantSent && !sent() && time.Now().Before(deadline) {
				time.Sleep(5 * time.Millisecond)
			}

			if !tc.wantSent {
				// Long enough for several polls to have come and gone.
				time.Sleep(50 * time.Millisecond)
			}

			if sent() != tc.wantSent {
				t.Errorf("opening sent = %v, want %v:\n%s", sent(), tc.wantSent, fake.Transcript())
			}
		})
	}
}
