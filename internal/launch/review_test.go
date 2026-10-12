package launch

import (
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/claude"
	"github.com/justinrush/q/internal/mission"
)

const reviewedID mission.MissionID = "ms_112233445566"

// reviewSession is the tmux session a launch of reviewMission produces.
var reviewSession = mission.TmuxSessionName("discussions-api", "review-add-endpoint", "ms_aabbccddeeff")

func reviewMission() mission.Mission {
	ms := testMission()
	ms.Name = "Review add endpoint"
	ms.Slug = "review-add-endpoint"
	ms.Prompt = "Look hard at the error handling."
	ms.ReviewOf = reviewedID

	return ms
}

// reviewedMission is a launched mission with one worktree and, when transcript
// is not empty, a conversation log holding it.
func reviewedMission(t *testing.T, transcript string) mission.Mission {
	t.Helper()

	source := mission.Mission{
		ID:     reviewedID,
		Name:   "add endpoint",
		Tool:   mission.ToolCodex,
		Prompt: "Add the endpoint.",
		Work: map[string]mission.RepoWork{
			"weave": {
				RepoName: "weave", WorktreePath: "/missions/source/weave", Branch: "jarush/add-endpoint",
				BaseRef: "refs/remotes/origin/main", BaseSHA: "deadbeefcafebabe", Created: true,
			},
		},
	}

	if transcript != "" {
		source.TranscriptPath = filepath.Join(t.TempDir(), "rollout.jsonl")
		if err := os.WriteFile(source.TranscriptPath, []byte(transcript), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	return source
}

func withMissions(l *Launcher, missions ...mission.Mission) {
	WithHostSnapshot(func() mission.Snapshot {
		var snap mission.Snapshot
		for _, ms := range missions {
			snap.PutMission(ms)
		}

		return snap
	})(l)
}

func TestLaunchReview(t *testing.T) {
	cases := []struct {
		name           string
		transcript     string
		wantTranscript bool
	}{
		{name: "with the reviewed agent's transcript", transcript: `{"type":"message"}` + "\n", wantTranscript: true},
		{name: "without a transcript"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			launcher, fake, _ := newTestLauncher(t)
			fake.ExpectExit(tmuxBin+" has-session -t ="+reviewSession, 1, "can't find session")

			source := reviewedMission(t, tc.transcript)
			withMissions(launcher, source)

			// The operation's repository must not be provisioned: the fake has no
			// answers for it, so touching it would fail the launch.
			got, err := launcher.Launch(t.Context(), testOperation("/dev/weave"), reviewMission())
			if err != nil {
				t.Fatalf("Launch: %v", err)
			}

			if len(got.Work) != 0 {
				t.Errorf("a review was given worktrees of its own: %+v", got.Work)
			}

			for _, call := range fake.Argv() {
				if strings.Contains(call, "worktree add") {
					t.Errorf("a review provisioned a worktree: %s", call)
				}
			}

			prompt := readArtifact(t, got, mission.PromptFile)
			for _, want := range []string{
				"# Review of mission: add endpoint",
				"/missions/source/weave",
				"Look hard at the error handling.",
			} {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt missing %q:\n%s", want, prompt)
				}
			}

			if settings := readArtifact(t, got, claude.SettingsFile); !strings.Contains(settings, "/missions/source/weave") {
				t.Errorf("settings should grant the reviewed worktree:\n%s", settings)
			}

			copied := filepath.Join(got.MissionDir, mission.ArtifactDir, mission.ReviewTranscriptFile)
			data, err := os.ReadFile(copied)

			if tc.wantTranscript {
				if err != nil || string(data) != tc.transcript {
					t.Errorf("transcript copy = %q, %v", data, err)
				}

				if !strings.Contains(prompt, copied) {
					t.Errorf("prompt does not name the transcript copy:\n%s", prompt)
				}

				return
			}

			if !errors.Is(err, os.ErrNotExist) {
				t.Errorf("a transcript was written with none to copy: %v", err)
			}

			if !strings.Contains(prompt, "No conversation log") {
				t.Errorf("prompt does not say the transcript is missing:\n%s", prompt)
			}
		})
	}
}

func TestLaunchReviewWithoutItsSubject(t *testing.T) {
	cases := []struct {
		name     string
		missions []mission.Mission
		snapshot bool
	}{
		{name: "the reviewed mission was deleted", snapshot: true},
		{name: "the launcher has no view of other missions"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			launcher, fake, dirs := newTestLauncher(t)
			if tc.snapshot {
				withMissions(launcher, tc.missions...)
			}

			_, err := launcher.Launch(t.Context(), testOperation("/dev/weave"), reviewMission())
			if !errors.Is(err, errNothingToReview) {
				t.Fatalf("Launch error = %v, want errNothingToReview", err)
			}

			if slices.ContainsFunc(fake.Argv(), func(call string) bool { return strings.Contains(call, "new-session") }) {
				t.Error("a session was started for a review with no subject")
			}

			if entries, _ := os.ReadDir(dirs.MissionsDir()); len(entries) != 0 {
				t.Errorf("a mission directory was left behind: %v", entries)
			}
		})
	}
}

func TestRelaunchReviewKeepsItsAccess(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	fake.ExpectExit(tmuxBin+" has-session -t ="+reviewSession, 1, "can't find session")

	withMissions(launcher, reviewedMission(t, ""))

	launched, err := launcher.Launch(t.Context(), testOperation("/dev/weave"), reviewMission())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	cases := []struct {
		name       string
		sessionID  string
		wantPrompt string
	}{
		{name: "resumed", sessionID: launched.AgentSessionID, wantPrompt: "carry on"},
		{name: "started fresh", wantPrompt: "# Review of mission: add endpoint"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := launched
			ms.AgentSessionID = tc.sessionID

			got, err := launcher.Relaunch(t.Context(), testOperation("/dev/weave"), ms, "carry on")
			if err != nil {
				t.Fatalf("Relaunch: %v", err)
			}

			if prompt := readArtifact(t, got, mission.PromptFile); !strings.Contains(prompt, tc.wantPrompt) {
				t.Errorf("prompt missing %q:\n%s", tc.wantPrompt, prompt)
			}

			if settings := readArtifact(t, got, claude.SettingsFile); !strings.Contains(settings, "/missions/source/weave") {
				t.Errorf("settings lost the reviewed worktree:\n%s", settings)
			}
		})
	}
}
