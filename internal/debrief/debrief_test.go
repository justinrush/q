package debrief

import (
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/gadgets"
	"github.com/justinrush/q/internal/gitx"
	"github.com/justinrush/q/internal/runner"
	"github.com/justinrush/q/internal/settings"
	"github.com/justinrush/q/internal/termopen"
	"github.com/justinrush/q/internal/tmuxc"
	"io"
)

const (
	gitBin       = "/usr/bin/git"
	tmuxBin      = "/usr/bin/tmux"
	osaScriptBin = "/usr/bin/osascript"
	// testEditor is the stub editor name, chosen so it cannot collide with a real
	// program on the test machine's PATH.
	testEditor = "q-test-editor"
	session    = "q-operation--mission-aabbccddeeff"
)

// newTestOpener returns an Opener whose external commands are faked.
func newTestOpener(t *testing.T) (*Opener, *runner.Fake) {
	t.Helper()

	// The editor is resolved through PATH, so a stub in a directory prepended to
	// PATH keeps the test off any real editor.
	dir := t.TempDir()

	stub := filepath.Join(dir, testEditor)
	if err := os.WriteFile(stub, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("writing stub: %v", err)
	}

	t.Setenv("PATH", dir+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("TMUX", "")

	fake := runner.NewFake()

	return New(Config{
		Git:  gitx.New(gitBin, fake),
		Tmux: tmuxc.New(tmuxBin, fake),
		Term: termopen.New(termopen.Config{
			Mode:      settings.TerminalGhostty,
			ScriptBin: osaScriptBin,
			Run:       fake,
		}),
		Bins:   gadgets.New(gadgets.Options{}),
		Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
		Editor: []string{testEditor, "+Neotree"},
	}), fake
}

// debriefMission returns a launched two-repo mission.
func debriefMission(t *testing.T) domain.Mission {
	t.Helper()

	root := t.TempDir()

	for _, repo := range []string{"weave", "azure-tf"} {
		if err := os.MkdirAll(filepath.Join(root, repo), 0o700); err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}
	}

	return domain.Mission{
		ID:          "ms_aabbccddeeff",
		Name:        "mission",
		Slug:        "mission",
		Tool:        domain.ToolClaude,
		Status:      domain.StatusDebrief,
		MissionDir:  root,
		TmuxSession: session,
		AgentPaneID: "%13",
		Work: map[string]domain.RepoWork{
			"weave": {
				RepoName: "weave", WorktreePath: filepath.Join(root, "weave"),
				Branch: "jarush/mission", BaseSHA: "base1", Created: true,
			},
			"azure-tf": {
				RepoName: "azure-tf", WorktreePath: filepath.Join(root, "azure-tf"),
				Branch: "jarush/mission", BaseSHA: "base2", Created: true,
			},
		},
	}
}

// expectTouched registers git answers describing what each worktree changed.
func expectTouched(fake *runner.Fake, mission domain.Mission, repo, status, count string) {
	path := mission.Work[repo].WorktreePath
	base := mission.Work[repo].BaseSHA

	fake.Expect(gitBin+" -C "+path+" status --porcelain=v1", status)
	fake.Expect(gitBin+" -C "+path+" rev-list --count "+base+"..HEAD", count)
	fake.Expect(gitBin+" -C "+path+" diff --shortstat "+base+"..HEAD", " 1 file changed")
}

// listPanesArgv is the argv used to inspect the session's panes.
var listPanesArgv = tmuxBin + " list-panes -t =" + session +
	" -F #{pane_id}\t#{pane_dead}\t#{pane_current_command}\t#{pane_current_path}"

// An untouched repo gets no pane. That is the entire point of the check: a debrief
// should show what changed, not every repo the operation happens to include.
func TestOpenAddsPanesOnlyForTouchedRepos(t *testing.T) {
	opener, fake := newTestOpener(t)
	mission := debriefMission(t)

	expectTouched(fake, mission, "weave", " M main.go", "0")
	expectTouched(fake, mission, "azure-tf", "", "0")

	fake.Expect(tmuxBin+" has-session -t ="+session, "")
	fake.Expect(listPanesArgv, "%13\t0\tclaude\t/missions/t")
	fake.Expect(tmuxBin+" list-clients -t ="+session+" -F #{client_tty}", "")
	fake.Default = runner.Result{Stdout: []byte("%21")}

	result, updated, err := opener.Open(t.Context(), mission, ModeAttach)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if len(result.Touched) != 1 || result.Touched[0].Repo != "weave" {
		t.Fatalf("Touched = %+v, want only weave", result.Touched)
	}

	if result.PanesAdded != 1 {
		t.Errorf("PanesAdded = %d, want 1", result.PanesAdded)
	}

	if updated.Work["weave"].DebriefPaneID != "%21" {
		t.Errorf("debrief pane not recorded: %+v", updated.Work["weave"])
	}

	if updated.Work["azure-tf"].DebriefPaneID != "" {
		t.Error("an untouched repo should get no pane")
	}
}

// The editor is invoked by absolute path with its configured flags, not through
// the user's shell alias, which is not an executable and would drag in a shell
// banner.
func TestOpenLaunchesTheEditorByAbsolutePath(t *testing.T) {
	opener, fake := newTestOpener(t)
	mission := debriefMission(t)

	expectTouched(fake, mission, "weave", " M main.go", "0")
	expectTouched(fake, mission, "azure-tf", "", "0")
	fake.Expect(tmuxBin+" has-session -t ="+session, "")
	fake.Expect(listPanesArgv, "%13\t0\tclaude\t/missions/t")
	fake.Expect(tmuxBin+" list-clients -t ="+session+" -F #{client_tty}", "")
	fake.Default = runner.Result{Stdout: []byte("%21")}

	if _, _, err := opener.Open(t.Context(), mission, ModeAttach); err != nil {
		t.Fatalf("Open: %v", err)
	}

	transcript := fake.Transcript()

	if !strings.Contains(transcript, "+Neotree") {
		t.Errorf("editor should open its file tree:\n%s", transcript)
	}

	if !strings.Contains(transcript, editorPath(t)) {
		t.Errorf("editor should be invoked by absolute path:\n%s", transcript)
	}

	if strings.Contains(transcript, "zsh -ic") {
		t.Errorf("editor must not go through a shell alias:\n%s", transcript)
	}
}

// editorPath resolves the stub editor the same way the opener does.
func editorPath(t *testing.T) string {
	t.Helper()

	path, err := exec.LookPath(testEditor)
	if err != nil {
		t.Fatalf("resolving the stub editor: %v", err)
	}

	abs, err := filepath.Abs(path)
	if err != nil {
		t.Fatalf("resolving the stub editor: %v", err)
	}

	return abs
}

// Re-opening must reuse existing panes rather than stacking another editor beside
// each one.
func TestOpenIsIdempotent(t *testing.T) {
	opener, fake := newTestOpener(t)
	mission := debriefMission(t)

	work := mission.Work["weave"]
	work.DebriefPaneID = "%21"
	mission.Work["weave"] = work

	expectTouched(fake, mission, "weave", " M main.go", "0")
	expectTouched(fake, mission, "azure-tf", "", "0")
	fake.Expect(tmuxBin+" has-session -t ="+session, "")
	// Both the agent and the existing debrief pane are alive.
	fake.Expect(listPanesArgv, "%13\t0\tclaude\t/missions/t\n%21\t0\tq-test-editor\t/missions/t/weave")
	fake.Expect(tmuxBin+" list-clients -t ="+session+" -F #{client_tty}", "")

	result, _, err := opener.Open(t.Context(), mission, ModeAttach)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if result.PanesAdded != 0 {
		t.Errorf("PanesAdded = %d, want 0", result.PanesAdded)
	}

	if strings.Contains(fake.Transcript(), "split-window") {
		t.Errorf("no pane should have been created:\n%s", fake.Transcript())
	}
}

// A pane the user closed should be recreated, which is why liveness is cross-checked
// rather than trusting the recorded id.
func TestOpenRecreatesAClosedPane(t *testing.T) {
	opener, fake := newTestOpener(t)
	mission := debriefMission(t)

	work := mission.Work["weave"]
	work.DebriefPaneID = "%21"
	mission.Work["weave"] = work

	expectTouched(fake, mission, "weave", " M main.go", "0")
	expectTouched(fake, mission, "azure-tf", "", "0")
	fake.Expect(tmuxBin+" has-session -t ="+session, "")
	// The recorded debrief pane is no longer listed.
	fake.Expect(listPanesArgv, "%13\t0\tclaude\t/missions/t")
	fake.Expect(tmuxBin+" list-clients -t ="+session+" -F #{client_tty}", "")
	fake.Default = runner.Result{Stdout: []byte("%30")}

	result, updated, err := opener.Open(t.Context(), mission, ModeAttach)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if result.PanesAdded != 1 {
		t.Errorf("PanesAdded = %d, want the pane recreated", result.PanesAdded)
	}

	if updated.Work["weave"].DebriefPaneID != "%30" {
		t.Errorf("pane id not updated: %+v", updated.Work["weave"])
	}
}

// Reviving an agent is a bigger step than opening a window, so the caller decides.
func TestOpenReportsAMissingSessionRatherThanRelaunching(t *testing.T) {
	opener, fake := newTestOpener(t)
	mission := debriefMission(t)

	expectTouched(fake, mission, "weave", " M main.go", "0")
	expectTouched(fake, mission, "azure-tf", "", "0")
	fake.ExpectExit(tmuxBin+" has-session -t ="+session, 1, "can't find session")

	result, _, err := opener.Open(t.Context(), mission, ModeAttach)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if !result.NeedsRelaunch {
		t.Error("NeedsRelaunch should be set")
	}

	// Changes are still reported, so the board can label the card.
	if len(result.Touched) != 1 {
		t.Errorf("Touched = %+v", result.Touched)
	}

	if strings.Contains(fake.Transcript(), "split-window") {
		t.Errorf("nothing should be arranged in a dead session:\n%s", fake.Transcript())
	}
}

// Opening a mission always gets its own terminal, even if another client is attached.
func TestOpenAddsAClientWhenAlreadyAttached(t *testing.T) {
	opener, fake := newTestOpener(t)
	mission := debriefMission(t)

	expectTouched(fake, mission, "weave", "", "0")
	expectTouched(fake, mission, "azure-tf", "", "0")
	fake.Expect(tmuxBin+" has-session -t ="+session, "")
	fake.Expect(listPanesArgv, "%13\t0\tclaude\t/missions/t")
	fake.Expect(tmuxBin+" list-clients -t ="+session+" -F #{client_tty}", "/dev/ttys004")

	result, _, err := opener.Open(t.Context(), mission, ModeAttach)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if len(result.AlreadyAttached) != 1 {
		t.Fatalf("AlreadyAttached = %+v", result.AlreadyAttached)
	}

	if !result.Attached {
		t.Error("should have opened a new client")
	}

	command := terminalCommand(t, fake)
	if !strings.Contains(command, "attach-session -t ="+session) {
		t.Errorf("new terminal should attach without detaching the existing client:\n%s", command)
	}

	if !strings.Contains(fake.Transcript(), osaScriptBin) {
		t.Errorf("a terminal should have been opened:\n%s", fake.Transcript())
	}
}

// Stealing is the explicit way to take over from a stale client elsewhere.
func TestOpenStealDetachesOtherClients(t *testing.T) {
	opener, fake := newTestOpener(t)
	mission := debriefMission(t)

	expectTouched(fake, mission, "weave", "", "0")
	expectTouched(fake, mission, "azure-tf", "", "0")
	fake.Expect(tmuxBin+" has-session -t ="+session, "")
	fake.Expect(listPanesArgv, "%13\t0\tclaude\t/missions/t")
	fake.Expect(tmuxBin+" list-clients -t ="+session+" -F #{client_tty}", "/dev/ttys004")

	result, _, err := opener.Open(t.Context(), mission, ModeSteal)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if !result.Attached {
		t.Error("steal should attach")
	}

	command := terminalCommand(t, fake)

	if !strings.Contains(command, "attach-session -d") {
		t.Errorf("steal should detach other clients:\n%s", command)
	}

	if !strings.Contains(command, "-t ="+session) {
		t.Errorf("session target lost its exact-match form:\n%s", command)
	}

	if !strings.Contains(fake.Transcript(), osaScriptBin) {
		t.Errorf("the launch should use Ghostty's AppleScript API:\n%s", fake.Transcript())
	}
}

func terminalCommand(t *testing.T, fake *runner.Fake) string {
	t.Helper()

	for _, call := range fake.Calls() {
		if call.Name == osaScriptBin && len(call.Args) == 5 {
			return call.Args[4]
		}
	}

	t.Fatal("terminal launch was not called")

	return ""
}

// Opening from inside tmux must not replace the board's current client.
func TestOpenCreatesANewTerminalWhenInsideTmux(t *testing.T) {
	opener, fake := newTestOpener(t)
	t.Setenv("TMUX", "/private/tmp/tmux-502/default,1234,0")

	mission := debriefMission(t)

	expectTouched(fake, mission, "weave", "", "0")
	expectTouched(fake, mission, "azure-tf", "", "0")
	fake.Expect(tmuxBin+" has-session -t ="+session, "")
	fake.Expect(listPanesArgv, "%13\t0\tclaude\t/missions/t")
	fake.Expect(tmuxBin+" list-clients -t ="+session+" -F #{client_tty}", "")

	result, _, err := opener.Open(t.Context(), mission, ModeAttach)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if !result.Attached {
		t.Error("should have opened a new client")
	}

	transcript := fake.Transcript()

	if strings.Contains(transcript, "switch-client") {
		t.Errorf("the current client must not be switched:\n%s", transcript)
	}

	if !strings.Contains(transcript, osaScriptBin) {
		t.Errorf("a new terminal should be opened from inside tmux:\n%s", transcript)
	}
}

// Preparing arranges panes without stealing focus, which is what a board refresh wants.
func TestOpenPrepareDoesNotAttach(t *testing.T) {
	opener, fake := newTestOpener(t)
	mission := debriefMission(t)

	expectTouched(fake, mission, "weave", " M x", "0")
	expectTouched(fake, mission, "azure-tf", "", "0")
	fake.Expect(tmuxBin+" has-session -t ="+session, "")
	fake.Expect(listPanesArgv, "%13\t0\tclaude\t/missions/t")
	fake.Expect(tmuxBin+" list-clients -t ="+session+" -F #{client_tty}", "")
	fake.Default = runner.Result{Stdout: []byte("%21")}

	result, _, err := opener.Open(t.Context(), mission, ModePrepare)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if result.Attached {
		t.Error("prepare must not attach")
	}

	if result.PanesAdded != 1 {
		t.Errorf("PanesAdded = %d, want the pane arranged", result.PanesAdded)
	}
}

// Touched is the read-only view; it must not drive tmux at all.
func TestTouchedDoesNotTouchTmux(t *testing.T) {
	opener, fake := newTestOpener(t)
	mission := debriefMission(t)

	expectTouched(fake, mission, "weave", "", "3")
	expectTouched(fake, mission, "azure-tf", "", "0")

	touched, err := opener.Touched(t.Context(), mission)
	if err != nil {
		t.Fatalf("Touched: %v", err)
	}

	if len(touched) != 1 || touched[0].Ahead != 3 {
		t.Fatalf("touched = %+v", touched)
	}

	if strings.Contains(fake.Transcript(), tmuxBin) {
		t.Errorf("Touched must not run tmux:\n%s", fake.Transcript())
	}
}

// A worktree the user deleted by hand must be skipped rather than failing the debrief.
func TestTouchedSkipsMissingWorktrees(t *testing.T) {
	opener, fake := newTestOpener(t)
	mission := debriefMission(t)

	work := mission.Work["weave"]
	work.WorktreePath = "/nonexistent/q-worktree"
	mission.Work["weave"] = work

	expectTouched(fake, mission, "azure-tf", " M x", "0")

	touched, err := opener.Touched(t.Context(), mission)
	if err != nil {
		t.Fatalf("Touched: %v", err)
	}

	if len(touched) != 1 || touched[0].Repo != "azure-tf" {
		t.Errorf("touched = %+v", touched)
	}
}

func TestOpenRejectsANeverLaunchedMission(t *testing.T) {
	opener, _ := newTestOpener(t)

	mission := debriefMission(t)
	mission.TmuxSession = ""

	if _, _, err := opener.Open(t.Context(), mission, ModeAttach); err == nil {
		t.Error("expected an error for a mission that was never launched")
	}
}

// The agent is what the human came to talk to, so focus lands there rather than in an
// editor pane.
func TestOpenFocusesTheAgentPane(t *testing.T) {
	opener, fake := newTestOpener(t)
	mission := debriefMission(t)

	expectTouched(fake, mission, "weave", " M x", "0")
	expectTouched(fake, mission, "azure-tf", "", "0")
	fake.Expect(tmuxBin+" has-session -t ="+session, "")
	fake.Expect(listPanesArgv, "%13\t0\tclaude\t/missions/t")
	fake.Expect(tmuxBin+" list-clients -t ="+session+" -F #{client_tty}", "")
	fake.Default = runner.Result{Stdout: []byte("%21")}

	if _, _, err := opener.Open(t.Context(), mission, ModePrepare); err != nil {
		t.Fatalf("Open: %v", err)
	}

	transcript := fake.Transcript()

	if !strings.Contains(transcript, "select-pane -t %13") {
		t.Errorf("focus should land on the agent pane:\n%s", transcript)
	}

	if !strings.Contains(transcript, "select-layout -t ="+session+":agent "+mainVertical) {
		t.Errorf("expected the main-vertical layout:\n%s", transcript)
	}
}

// Reflowing only after every split makes tmux repeatedly halve the active pane.
// A mission touching enough repos then runs out of room before it can be opened, even
// though main-vertical has room for all of the editor panes.
func TestOpenReflowsAfterEachAddedPane(t *testing.T) {
	opener, fake := newTestOpener(t)
	mission := debriefMission(t)
	mission.Work = make(map[string]domain.RepoWork)

	const repoCount = 6

	for i := range repoCount {
		name := fmt.Sprintf("repo-%d", i)
		path := filepath.Join(mission.MissionDir, name)
		err := os.MkdirAll(path, 0o700)
		if err != nil {
			t.Fatalf("MkdirAll: %v", err)
		}

		mission.Work[name] = domain.RepoWork{
			RepoName: name, WorktreePath: path, Branch: "jarush/mission",
			BaseSHA: fmt.Sprintf("base%d", i), Created: true,
		}
		expectTouched(fake, mission, name, " M main.go", "0")
	}

	fake.Expect(tmuxBin+" has-session -t ="+session, "")
	fake.Expect(listPanesArgv, "%13\t0\tclaude\t/missions/t")
	fake.Expect(tmuxBin+" list-clients -t ="+session+" -F #{client_tty}", "")
	fake.Default = runner.Result{Stdout: []byte("%21")}

	result, _, err := opener.Open(t.Context(), mission, ModePrepare)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if result.PanesAdded != repoCount {
		t.Fatalf("PanesAdded = %d, want %d", result.PanesAdded, repoCount)
	}

	argv := fake.Argv()
	for i, command := range argv {
		if !strings.Contains(command, " split-window ") {
			continue
		}

		if i+1 >= len(argv) || !strings.Contains(argv[i+1], " select-layout ") {
			t.Errorf("split was not immediately reflowed:\n%s", fake.Transcript())
		}
	}
}

// Pane creation is not atomic. If a later split fails, the successfully created
// pane ids must still reach the caller so a retry can reuse them.
func TestOpenReturnsPanesCreatedBeforeSplitFailure(t *testing.T) {
	opener, fake := newTestOpener(t)
	mission := debriefMission(t)

	expectTouched(fake, mission, "weave", " M main.go", "0")
	expectTouched(fake, mission, "azure-tf", " M main.go", "0")
	fake.Expect(tmuxBin+" has-session -t ="+session, "")
	fake.Expect(listPanesArgv, "%13\t0\tclaude\t/missions/t")
	fake.Default = runner.Result{Stdout: []byte("%21")}

	editor := editorPath(t)
	failedSplit := tmuxBin + " split-window -d -t =" + session + ":agent -c " +
		mission.Work["weave"].WorktreePath + " -P -F #{pane_id} " + editor + " +Neotree"
	fake.ExpectError(failedSplit, errors.New("no space for new pane"))

	result, updated, err := opener.Open(t.Context(), mission, ModePrepare)
	if err == nil {
		t.Fatal("Open succeeded after a split failure")
	}

	if result.PanesAdded != 1 {
		t.Errorf("PanesAdded = %d, want 1", result.PanesAdded)
	}

	if updated.Work["azure-tf"].DebriefPaneID != "%21" {
		t.Errorf("first pane was not returned: %+v", updated.Work["azure-tf"])
	}

	if updated.Work["weave"].DebriefPaneID != "" {
		t.Errorf("failed pane was recorded: %+v", updated.Work["weave"])
	}
}
