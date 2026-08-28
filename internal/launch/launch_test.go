package launch

import (
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/gadgets"
	"github.com/justinrush/q/internal/gitx"
	"github.com/justinrush/q/internal/loadout"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/runner"
	"github.com/justinrush/q/internal/tmuxc"
	"io"
)

const (
	gitBin    = "/usr/bin/git"
	tmuxBin   = "/usr/bin/tmux"
	claudeBin = "/usr/bin/claude"
)

// stubBin writes an executable stub and returns its path.
func stubBin(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("writing stub: %v", err)
	}

	return path
}

// newTestLauncher returns a Launcher whose external commands are faked.
func newTestLauncher(t *testing.T) (*Launcher, *runner.Fake, paths.Dirs) {
	t.Helper()

	// Point tool resolution at stubs so no real binary is required.
	t.Setenv(gadgets.Claude.EnvVar(), stubBin(t, "claude"))
	t.Setenv(gadgets.Codex.EnvVar(), stubBin(t, "codex"))

	root := t.TempDir()
	dirs := paths.Dirs{Data: filepath.Join(root, "data"), State: filepath.Join(root, "state")}

	if err := dirs.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	fake := runner.NewFake()
	fake.Default = runner.Result{Stdout: []byte("%13")}

	launcher := New(Config{
		Dirs:         dirs,
		Git:          gitx.New(gitBin, fake),
		Tmux:         tmuxc.New(tmuxBin, fake),
		Bins:         gadgets.New(gadgets.Options{}),
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
		BranchPrefix: "jarush",
	})

	return launcher, fake, dirs
}

// testSession is the tmux session name a launch of testMission under testOperation
// produces.
var testSession = domain.TmuxSessionName("discussions-api", "add-endpoint", "ms_aabbccddeeff")

// seedGitResponses registers the answers a successful launch needs.
func seedGitResponses(fake *runner.Fake, repoPath, commonDir string) {
	seedRepoGitResponses(fake, repoPath, commonDir)

	// The target session must not already exist, or the launcher refuses rather
	// than adopting someone else's session.
	fake.ExpectExit(tmuxBin+" has-session -t ="+testSession, 1, "can't find session")
}

func seedRepoGitResponses(fake *runner.Fake, repoPath, commonDir string) {
	fake.Expect(gitBin+" -C "+repoPath+" rev-parse --git-common-dir", commonDir)
	fake.Expect(gitBin+" -C "+repoPath+" symbolic-ref --short refs/remotes/origin/HEAD", "origin/main")
	fake.Expect(gitBin+" -C "+commonDir+" rev-parse refs/remotes/origin/main", "deadbeefcafebabe")
	fake.ExpectExit(gitBin+" -C "+commonDir+" show-ref --verify --quiet refs/heads/jarush/add-endpoint", 1, "")
}

func testOperation(repoPath string) domain.Operation {
	return domain.Operation{
		ID:      "op_aabbccddeeff",
		Name:    "Discussions API",
		Slug:    "discussions-api",
		Summary: "wire it up",
		Repos:   []domain.Repo{{Name: "weave", Path: repoPath}},
	}
}

func testMission() domain.Mission {
	return domain.Mission{
		ID:     "ms_aabbccddeeff",
		Name:   "add endpoint",
		Slug:   "add-endpoint",
		Tool:   domain.ToolClaude,
		Prompt: "do the thing",
		Status: domain.StatusBriefing,
	}
}

func TestLaunchProvisionsAndStarts(t *testing.T) {
	launcher, fake, dirs := newTestLauncher(t)
	seedGitResponses(fake, "/dev/weave", "/dev/weave/.git")

	got, err := launcher.Launch(t.Context(), testOperation("/dev/weave"), testMission())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got.Status != domain.StatusActive {
		t.Errorf("Status = %q, want active", got.Status)
	}

	if got.StartedAt == nil {
		t.Error("StartedAt should be set")
	}

	// The epoch increments on every launch, so hooks from an abandoned session can
	// be told apart from the live one.
	if got.HookEpoch != 1 {
		t.Errorf("HookEpoch = %d, want 1", got.HookEpoch)
	}

	if got.AgentPaneID != "%13" {
		t.Errorf("AgentPaneID = %q, want the id tmux reported", got.AgentPaneID)
	}

	wantDir := dirs.MissionDir("discussions-api--add-endpoint")
	if got.MissionDir != wantDir {
		t.Errorf("MissionDir = %q, want %q", got.MissionDir, wantDir)
	}

	work, ok := got.Work["weave"]
	if !ok || !work.Created {
		t.Fatalf("worktree not recorded: %+v", got.Work)
	}

	if work.Branch != "jarush/add-endpoint" {
		t.Errorf("Branch = %q", work.Branch)
	}

	// The branch point is pinned so later diffs cannot drift when origin/main moves.
	if work.BaseSHA != "deadbeefcafebabe" {
		t.Errorf("BaseSHA = %q", work.BaseSHA)
	}
}

func TestLaunchCombinesAndFreezesMissionRepos(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	seedGitResponses(fake, "/dev/weave", "/dev/weave/.git")
	seedRepoGitResponses(fake, "/dev/mac", "/dev/mac/.git")

	mission := testMission()
	mission.ExtraRepos = []domain.Repo{{Name: "mac", Path: "/dev/mac"}}

	got, err := launcher.Launch(t.Context(), testOperation("/dev/weave"), mission)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if !got.LaunchReposFrozen {
		t.Error("launch repositories were not frozen")
	}

	if len(got.LaunchRepos) != 2 {
		t.Fatalf("LaunchRepos = %+v, want inherited and additional repos", got.LaunchRepos)
	}

	if len(got.Work) != 2 || !got.Work["weave"].Created || !got.Work["mac"].Created {
		t.Fatalf("Work = %+v, want both worktrees", got.Work)
	}
}

func TestLaunchSupportsAdditionalRepoOnRepoLessOperation(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	seedGitResponses(fake, "/dev/mac", "/dev/mac/.git")

	operation := testOperation("")
	operation.Repos = nil
	mission := testMission()
	mission.ExtraRepos = []domain.Repo{{Name: "mac", Path: "/dev/mac"}}

	got, err := launcher.Launch(t.Context(), operation, mission)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if len(got.Work) != 1 || !got.Work["mac"].Created {
		t.Fatalf("Work = %+v, want mission-specific mac worktree", got.Work)
	}
}

// A directory left by an older q version has no mission ID proving who owns
// it. Launching alongside it preserves the unknown work and uses a stable,
// mission-specific directory instead.
func TestLaunchDoesNotAdoptUnownedMissionDirectory(t *testing.T) {
	launcher, fake, dirs := newTestLauncher(t)
	seedGitResponses(fake, "/dev/weave", "/dev/weave/.git")

	legacyDir := dirs.MissionDir("discussions-api--add-endpoint")
	err := os.MkdirAll(filepath.Join(legacyDir, "weave"), 0o700)
	if err != nil {
		t.Fatalf("creating legacy worktree path: %v", err)
	}

	got, err := launcher.Launch(t.Context(), testOperation("/dev/weave"), testMission())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	wantDir := dirs.MissionDir("discussions-api--add-endpoint--aabbccddeeff")
	if got.MissionDir != wantDir {
		t.Errorf("MissionDir = %q, want %q", got.MissionDir, wantDir)
	}

	if _, err = os.Stat(filepath.Join(legacyDir, "weave")); err != nil {
		t.Errorf("legacy worktree was disturbed: %v", err)
	}
}

// If the daemon exits after git creates a worktree but before it journals that
// repo, the ownership marker makes the existing checkout safe to recover.
func TestLaunchRecoversUnjournaledOwnedWorktree(t *testing.T) {
	launcher, fake, dirs := newTestLauncher(t)
	mission := testMission()
	operation := testOperation("/dev/weave")

	state, err := launcher.prepareMissionDir(operation, &mission)
	if err != nil {
		t.Fatalf("prepareMissionDir: %v", err)
	}

	worktreePath := filepath.Join(mission.MissionDir, "weave")
	err = os.MkdirAll(worktreePath, 0o700)
	if err != nil {
		t.Fatalf("creating interrupted worktree: %v", err)
	}

	seedGitResponses(fake, "/dev/weave", "/dev/weave/.git")
	fake.Expect(gitBin+" -C /dev/weave/.git worktree list --porcelain", strings.Join([]string{
		"worktree " + worktreePath,
		"HEAD originalbase",
		"branch refs/heads/jarush/add-endpoint",
		"",
	}, "\n"))

	if state.MissionID != mission.ID {
		t.Fatalf("provision owner = %q, want %q", state.MissionID, mission.ID)
	}

	got, err := launcher.Launch(t.Context(), operation, mission)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	work := got.Work["weave"]
	if work.BaseSHA != "originalbase" || work.Branch != "jarush/add-endpoint" || !work.Created {
		t.Errorf("recovered work = %+v", work)
	}

	for _, line := range fake.Argv() {
		if strings.Contains(line, "worktree add") {
			t.Errorf("recovery tried to recreate the worktree: %q", line)
		}
	}

	data, err := os.ReadFile(filepath.Join(dirs.MissionDir("discussions-api--add-endpoint"),
		loadout.ArtifactDir, provisionStateFile))
	if err != nil {
		t.Fatalf("reading provision journal: %v", err)
	}

	if !strings.Contains(string(data), "originalbase") {
		t.Errorf("provision journal did not record recovered base:\n%s", data)
	}
}

// Two missions may have the same name in one operation. The ownership marker keeps
// the second launch out of the first mission's directory.
func TestLaunchUsesUniqueDirectoryForOwnershipConflict(t *testing.T) {
	launcher, fake, dirs := newTestLauncher(t)
	owner := testMission()
	owner.ID = "ms_111111111111"
	operation := testOperation("/dev/weave")

	_, err := launcher.prepareMissionDir(operation, &owner)
	if err != nil {
		t.Fatalf("claiming first mission directory: %v", err)
	}

	seedGitResponses(fake, "/dev/weave", "/dev/weave/.git")
	got, err := launcher.Launch(t.Context(), operation, testMission())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	wantDir := dirs.MissionDir("discussions-api--add-endpoint--aabbccddeeff")
	if got.MissionDir != wantDir {
		t.Errorf("MissionDir = %q, want %q", got.MissionDir, wantDir)
	}
}

// claude accepts a session id chosen in advance, which is what makes a session
// resumable from the instant it starts rather than only after a hook reports back.
func TestLaunchPresetsClaudeSessionID(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	seedGitResponses(fake, "/dev/weave", "/dev/weave/.git")

	got, err := launcher.Launch(t.Context(), testOperation("/dev/weave"), testMission())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got.AgentSessionID == "" {
		t.Fatal("claude launches must preset a session id")
	}

	// Arguments are emitted one per line for readability, so check the flag and its
	// value separately rather than as one contiguous string.
	script := readArtifact(t, got, loadout.LaunchScript)

	if !strings.Contains(script, "--session-id") {
		t.Errorf("script does not pass --session-id:\n%s", script)
	}

	if !strings.Contains(script, "'"+got.AgentSessionID+"'") {
		t.Errorf("script does not carry the recorded session id:\n%s", script)
	}
}

// codex has no --session-id, so its id must be learned from its SessionStart hook
// and stays empty until then.
func TestLaunchDoesNotPresetCodexSessionID(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	seedGitResponses(fake, "/dev/weave", "/dev/weave/.git")

	mission := testMission()
	mission.Tool = domain.ToolCodex

	got, err := launcher.Launch(t.Context(), testOperation("/dev/weave"), mission)
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if got.AgentSessionID != "" {
		t.Errorf("AgentSessionID = %q, want empty for codex", got.AgentSessionID)
	}
}

func TestLaunchWritesArtifacts(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	seedGitResponses(fake, "/dev/weave", "/dev/weave/.git")

	got, err := launcher.Launch(t.Context(), testOperation("/dev/weave"), testMission())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	prompt := readArtifact(t, got, loadout.PromptFile)
	if !strings.Contains(prompt, "do the thing") || !strings.Contains(prompt, "weave") {
		t.Errorf("prompt looks wrong:\n%s", prompt)
	}

	settings := readArtifact(t, got, loadout.ClaudeSettingsFile)
	if !strings.Contains(settings, "additionalDirectories") {
		t.Errorf("settings should grant worktree access:\n%s", settings)
	}

	// The script must be executable, since tmux runs it directly.
	scriptPath := filepath.Join(got.MissionDir, loadout.ArtifactDir, loadout.LaunchScript)

	info, err := os.Stat(scriptPath)
	if err != nil {
		t.Fatalf("stat script: %v", err)
	}

	if info.Mode().Perm()&0o100 == 0 {
		t.Errorf("launch script mode = %v, want an execute bit", info.Mode().Perm())
	}

	// The metadata file records what produced this session, for debugging.
	if meta := readArtifact(t, got, loadout.MetaFile); !strings.Contains(meta, "ms_aabbccddeeff") {
		t.Errorf("metadata looks wrong:\n%s", meta)
	}
}

// readArtifact reads a generated file from a mission's .q directory.
func readArtifact(t *testing.T, mission domain.Mission, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(mission.MissionDir, loadout.ArtifactDir, name))
	if err != nil {
		t.Fatalf("reading %s: %v", name, err)
	}

	return string(data)
}

// A mission whose worktrees exist for some repos but not others is worse than one that
// failed outright, because the agent would start and improvise around the gap.
func TestLaunchRollsBackPartialProvisioning(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)

	seedGitResponses(fake, "/dev/weave", "/dev/weave/.git")

	// The second repo cannot resolve its git directory.
	fake.ExpectExit(gitBin+" -C /dev/broken rev-parse --git-common-dir", 128, "not a git repository")

	operation := testOperation("/dev/weave")
	operation.Repos = append(operation.Repos, domain.Repo{Name: "broken", Path: "/dev/broken"})

	_, err := launcher.Launch(t.Context(), operation, testMission())
	if err == nil {
		t.Fatal("expected the launch to fail")
	}

	// The successfully created worktree and its branch must both be undone.
	var sawRemove, sawBranchDelete bool

	for _, line := range fake.Argv() {
		if strings.Contains(line, "worktree remove --force") {
			sawRemove = true
		}

		if strings.Contains(line, "branch -D jarush/add-endpoint") {
			sawBranchDelete = true
		}
	}

	if !sawRemove {
		t.Errorf("rollback did not remove the created worktree:\n%s", fake.Transcript())
	}

	if !sawBranchDelete {
		t.Errorf("rollback did not delete the created branch:\n%s", fake.Transcript())
	}
}

// A failed launch must not leave a tmux session behind.
func TestLaunchDoesNotStartSessionWhenProvisioningFails(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	fake.ExpectExit(gitBin+" -C /dev/weave rev-parse --git-common-dir", 128, "not a git repository")

	if _, err := launcher.Launch(t.Context(), testOperation("/dev/weave"), testMission()); err == nil {
		t.Fatal("expected the launch to fail")
	}

	for _, line := range fake.Argv() {
		if strings.Contains(line, "new-session") {
			t.Errorf("a failed launch started a tmux session: %q", line)
		}
	}
}

// Every option the user's tmux config would otherwise override must be pinned, and
// remain-on-exit is a safety requirement: without it a finished agent's pane falls
// back to a shell, which would execute any follow-up message typed into it.
func TestLaunchPinsSessionOptions(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	seedGitResponses(fake, "/dev/weave", "/dev/weave/.git")

	if _, err := launcher.Launch(t.Context(), testOperation("/dev/weave"), testMission()); err != nil {
		t.Fatalf("Launch: %v", err)
	}

	transcript := fake.Transcript()

	for _, want := range []string{
		"automatic-rename off",
		"allow-rename off",
		"window-size latest",
		"remain-on-exit on",
	} {
		if !strings.Contains(transcript, want) {
			t.Errorf("missing tmux option %q:\n%s", want, transcript)
		}
	}
}

// A colliding branch name would otherwise attach the mission to leftover work from a
// previous attempt.
func TestLaunchSuffixesCollidingBranchName(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	seedGitResponses(fake, "/dev/weave", "/dev/weave/.git")

	// The unsuffixed name is taken; the first suffix is free.
	fake.Expect(gitBin+" -C /dev/weave/.git show-ref --verify --quiet refs/heads/jarush/add-endpoint", "")
	fake.ExpectExit(gitBin+" -C /dev/weave/.git show-ref --verify --quiet refs/heads/jarush/add-endpoint-2", 1, "")

	got, err := launcher.Launch(t.Context(), testOperation("/dev/weave"), testMission())
	if err != nil {
		t.Fatalf("Launch: %v", err)
	}

	if branch := got.Work["weave"].Branch; branch != "jarush/add-endpoint-2" {
		t.Errorf("Branch = %q, want the suffixed name", branch)
	}
}

func TestLaunchFailsForUnknownTool(t *testing.T) {
	launcher, fake, _ := newTestLauncher(t)
	seedGitResponses(fake, "/dev/weave", "/dev/weave/.git")

	mission := testMission()
	mission.Tool = "cursor"

	if _, err := launcher.Launch(t.Context(), testOperation("/dev/weave"), mission); err == nil {
		t.Error("expected an error for an unknown tool")
	}
}

func TestDefaultBranchPrefixFallsBackToUser(t *testing.T) {
	t.Setenv("USER", "someone")
	t.Setenv("LOGNAME", "")

	if got := defaultBranchPrefix(); got != "someone" {
		t.Errorf("defaultBranchPrefix() = %q, want %q", got, "someone")
	}

	t.Setenv("USER", "")

	if got := defaultBranchPrefix(); got != "q" {
		t.Errorf("defaultBranchPrefix() = %q, want the q fallback", got)
	}
}
