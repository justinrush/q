package launch

import (
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// launchedMission returns a mission that has already been started.
func launchedMission(t *testing.T, missionDir string) mission.Mission {
	t.Helper()

	started := time.Now()

	return mission.Mission{
		ID:             "ms_aabbccddeeff",
		Name:           "add endpoint",
		Slug:           "add-endpoint",
		Tool:           mission.ToolClaude,
		Prompt:         "do the thing",
		Status:         mission.StatusDebrief,
		MissionDir:     missionDir,
		TmuxSession:    testSession,
		AgentPaneID:    "%13",
		AgentSessionID: "sess-1",
		HookEpoch:      1,
		StartedAt:      &started,
		Work: map[string]mission.RepoWork{
			"weave": {
				RepoName:     "weave",
				WorktreePath: missionDir + "/weave",
				Branch:       "jarush/add-endpoint",
				BaseSHA:      "deadbeef",
				Created:      true,
			},
		},
	}
}

// panesReply renders a list-panes answer.
func panesReply(paneID, command string, dead bool) string {
	flag := "0"
	if dead {
		flag = "1"
	}

	return strings.Join([]string{paneID, flag, command, "/missions/t"}, "\t")
}

// listPanesArgv is the argv the launcher uses to inspect a session's panes.
var listPanesArgv = tmuxBin + " list-panes -t =" + testSession +
	" -F #{pane_id}\t#{pane_dead}\t#{pane_current_command}\t#{pane_current_path}"

// This is the most dangerous path in q. If the agent has exited and its pane
// fell back to a shell, pasting a message would type it into that shell and run it.
func TestSendMessageRefusesAPaneRunningAShell(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	fake.Expect(listPanesArgv, panesReply("%13", "zsh", false))

	err := launcher.SendMessage(t.Context(), launchedMission(t, t.TempDir()), "rm -rf something")
	if err == nil {
		t.Fatal("expected the send to be refused")
	}

	if !strings.Contains(err.Error(), "executed as a command") {
		t.Errorf("error should explain the danger, got %v", err)
	}

	for _, line := range fake.Argv() {
		if strings.Contains(line, "paste-buffer") || strings.Contains(line, "send-keys") {
			t.Errorf("nothing should have been sent: %q", line)
		}
	}
}

func TestSendMessageRefusesADeadPane(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	fake.Expect(listPanesArgv, panesReply("%13", "claude", true))

	err := launcher.SendMessage(t.Context(), launchedMission(t, t.TempDir()), "carry on")
	if err == nil {
		t.Fatal("expected the send to be refused")
	}

	if !strings.Contains(err.Error(), "relaunch") {
		t.Errorf("error should suggest relaunching, got %v", err)
	}
}

func TestSendMessageRefusesAMissingPane(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	fake.Expect(listPanesArgv, panesReply("%99", "claude", false))

	if err := launcher.SendMessage(t.Context(), launchedMission(t, t.TempDir()), "carry on"); err == nil {
		t.Fatal("expected the send to be refused when the recorded pane is gone")
	}
}

// A live agent pane is the only case a message is delivered, and it goes via a buffer
// rather than as keystrokes so multi-line text survives.
func TestSendMessageDeliversToALiveAgentPane(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	fake.Expect(listPanesArgv, panesReply("%13", "claude", false))

	msg := "look again at weave\nit's still wrong"

	if err := launcher.SendMessage(t.Context(), launchedMission(t, t.TempDir()), msg); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	transcript := fake.Transcript()

	for _, want := range []string{"load-buffer", "paste-buffer -d -p", "send-keys -t %13 Enter"} {
		if !strings.Contains(transcript, want) {
			t.Errorf("missing %q in:\n%s", want, transcript)
		}
	}

	// The message must never appear in argv, or it would be visible in ps output and
	// subject to shell interpretation.
	if strings.Contains(transcript, "still wrong") {
		t.Errorf("message text leaked into argv:\n%s", transcript)
	}
}

// codex ships as a node wrapper, so its pane command is not always "codex".
func TestSendMessageAcceptsNodeAsAnAgent(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	fake.Expect(listPanesArgv, panesReply("%13", "node", false))

	if err := launcher.SendMessage(t.Context(), launchedMission(t, t.TempDir()), "carry on"); err != nil {
		t.Errorf("SendMessage: %v", err)
	}
}

func TestSendMessageIgnoresEmptyText(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)

	if err := launcher.SendMessage(t.Context(), launchedMission(t, t.TempDir()), ""); err != nil {
		t.Fatalf("SendMessage: %v", err)
	}

	if len(fake.Argv()) != 0 {
		t.Errorf("nothing should have run: %q", fake.Argv())
	}
}

func TestSendMessageRequiresALiveSession(t *testing.T) {
	launcher, _, _ := newTestLauncher(t)

	ms := launchedMission(t, t.TempDir())
	ms.TmuxSession = ""

	if err := launcher.SendMessage(t.Context(), ms, "hello"); err == nil {
		t.Error("expected an error for a mission with no session")
	}
}

// Relaunch revives an agent against surviving worktrees, which is what recovers a
// mission after the tmux server or the machine restarts.
func TestRelaunchResumesTheSession(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)

	dir := t.TempDir()
	ms := launchedMission(t, dir)

	// The old session is gone, so nothing needs killing before the new one starts.
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "can't find session")
	fake.Default = runner.Result{Stdout: []byte("%77")}

	got, err := launcher.Relaunch(t.Context(), testOperation("/dev/weave"), ms, "keep going")
	if err != nil {
		t.Fatalf("Relaunch: %v", err)
	}

	// The epoch increments so a late hook from the previous incarnation is dropped
	// rather than moving the revived card.
	if got.HookEpoch != 2 {
		t.Errorf("HookEpoch = %d, want 2", got.HookEpoch)
	}

	if got.AgentPaneID != "%77" {
		t.Errorf("AgentPaneID = %q, want the new pane", got.AgentPaneID)
	}

	script := readArtifact(t, got, "launch.sh")
	if !strings.Contains(script, "--resume") {
		t.Errorf("relaunch should resume the session:\n%s", script)
	}

	if strings.Contains(script, "--session-id") {
		t.Errorf("--session-id cannot be combined with --resume:\n%s", script)
	}

	if prompt := readArtifact(t, got, "prompt.md"); prompt != "keep going" {
		t.Errorf("prompt = %q, want the follow-up message", prompt)
	}
}

// A session left behind with a dead pane would block creating the replacement.
func TestRelaunchKillsAStaleSession(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	fake.Default = runner.Result{Stdout: []byte("%77")}
	fake.Expect(tmuxBin+" has-session -t ="+testSession, "")

	if _, err := launcher.Relaunch(t.Context(), testOperation("/dev/weave"), launchedMission(t, t.TempDir()), ""); err != nil {
		t.Fatalf("Relaunch: %v", err)
	}

	if !strings.Contains(fake.Transcript(), "kill-session") {
		t.Errorf("a stale session should be killed first:\n%s", fake.Transcript())
	}
}

// Both agents take their prompt positionally, so a revived session must be told
// something rather than left at an empty input box.
func TestRelaunchWithoutAMessageStillSendsAPrompt(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	fake.Default = runner.Result{Stdout: []byte("%77")}
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "")

	got, err := launcher.Relaunch(t.Context(), testOperation("/dev/weave"), launchedMission(t, t.TempDir()), "")
	if err != nil {
		t.Fatalf("Relaunch: %v", err)
	}

	prompt := readArtifact(t, got, "prompt.md")
	if prompt == "" {
		t.Fatal("a revived session must be given a prompt")
	}

	if !strings.Contains(prompt, "restarted by q") {
		t.Errorf("the default prompt should explain what happened: %q", prompt)
	}
}

// codex has no way to be told a session id, so with none learned there is nothing to
// resume and it must start fresh rather than silently adopting another session.
func TestRelaunchWithoutASessionIDStartsFresh(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	fake.Default = runner.Result{Stdout: []byte("%77")}
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "")

	ms := launchedMission(t, t.TempDir())
	ms.Tool = mission.ToolCodex
	ms.AgentSessionID = ""

	got, err := launcher.Relaunch(t.Context(), testOperation("/dev/weave"), ms, "")
	if err != nil {
		t.Fatalf("Relaunch: %v", err)
	}

	if script := readArtifact(t, got, "launch.sh"); strings.Contains(script, "resume") {
		t.Errorf("nothing to resume, so it should start fresh:\n%s", script)
	}
}

func TestRelaunchRejectsAMissingMissionDir(t *testing.T) {
	launcher, _, _ := newTestLauncher(t)

	ms := launchedMission(t, "/nonexistent/q-mission-dir")

	if _, err := launcher.Relaunch(t.Context(), testOperation("/dev/weave"), ms, ""); err == nil {
		t.Error("expected an error when the mission directory is gone")
	}
}

func TestRelaunchRejectsANeverLaunchedMission(t *testing.T) {
	launcher, _, _ := newTestLauncher(t)

	ms := launchedMission(t, t.TempDir())
	ms.MissionDir = ""

	if _, err := launcher.Relaunch(t.Context(), testOperation("/dev/weave"), ms, ""); err == nil {
		t.Error("expected an error for a mission that was never launched")
	}
}

// The bug this guards against filed cards for sessions that were still working.
// claude's native install symlinks the launcher at a versioned binary, so the pane
// of a perfectly healthy claude reports "2.1.251", and an allowlist of agent names
// read that as an agent that had died.
func TestPaneRunsAgent(t *testing.T) {
	cases := []struct {
		name    string
		command string
		want    bool
	}{
		{name: "claude on PATH", command: "claude", want: true},
		{name: "claude's versioned binary", command: "2.1.251", want: true},
		{name: "a later claude release", command: "2.2.7", want: true},
		{name: "codex's node wrapper", command: "node", want: true},
		{name: "codex", command: "codex", want: true},
		{name: "a pane that fell back to zsh", command: "zsh", want: false},
		{name: "a pane that fell back to bash", command: "bash", want: false},
		{name: "a pane running sh", command: "sh", want: false},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := PaneRunsAgent(tc.command); got != tc.want {
				t.Errorf("PaneRunsAgent(%q) = %v, want %v", tc.command, got, tc.want)
			}
		})
	}
}
