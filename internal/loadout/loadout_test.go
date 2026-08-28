package loadout

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/domain"
)

const qBin = "/Users/jarush/.local/bin/q"

// testMissionDirs are the mission directories a profile pre-trusts.
var testMissionDirs = []string{"/data/missions/b--two", "/data/missions/a--one"}

// sampleOperation and sampleMission describe a two-repo claude mission.
func sampleOperation() domain.Operation {
	return domain.Operation{
		Name:    "Discussions API",
		Slug:    "discussions-api",
		Summary: "Wire discussions into the change-management flow.",
	}
}

func sampleMission() domain.Mission {
	return domain.Mission{
		ID:         "ms_aabbccddeeff",
		Name:       "add endpoint",
		Slug:       "add-endpoint",
		Tool:       domain.ToolClaude,
		Prompt:     "Add the discussions endpoint and the matching terraform.",
		MissionDir: "/data/missions/discussions-api--add-endpoint",
		Status:     domain.StatusBriefing,
		Work: map[string]domain.RepoWork{
			"weave": {
				RepoName:     "weave",
				WorktreePath: "/data/missions/discussions-api--add-endpoint/weave",
				Branch:       "jarush/add-endpoint",
				BaseRef:      "refs/remotes/origin/main",
				BaseSHA:      "deadbeefcafebabe",
				Created:      true,
			},
			"azure-tf": {
				RepoName:     "azure-tf",
				WorktreePath: "/data/missions/discussions-api--add-endpoint/azure-tf",
				Branch:       "jarush/add-endpoint",
				BaseRef:      "refs/remotes/origin/main",
				BaseSHA:      "deadbeefcafebabe",
				Created:      true,
			},
		},
	}
}

func TestHookSlug(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"SessionStart", "session-start"},
		{"PermissionRequest", "permission-request"},
		{"Stop", "stop"},
		{"StopFailure", "stop-failure"},
		{"UserPromptSubmit", "user-prompt-submit"},
	} {
		if got := hookSlug(tc.in); got != tc.want {
			t.Errorf("hookSlug(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

func TestComposePromptGolden(t *testing.T) {
	got, err := ComposePrompt(sampleOperation(), sampleMission())
	if err != nil {
		t.Fatalf("ComposePrompt: %v", err)
	}

	want := `# Operation: Discussions API

Wire discussions into the change-management flow.

## Repositories

Your working directory is the mission root. Each repository below is a git worktree
of the user's own checkout, on a branch created for this mission. They contain
tracked files only, so run any install or initialization step yourself if you
need one.

- azure-tf: ./azure-tf (branch jarush/add-endpoint, from origin/main at deadbee)
- weave: ./weave (branch jarush/add-endpoint, from origin/main at deadbee)

## Mission: add endpoint

Add the discussions endpoint and the matching terraform.
`

	if got != want {
		t.Errorf("prompt mismatch:\n--- got ---\n%s\n--- want ---\n%s", got, want)
	}
}

// Repos appear in a stable order so the prompt does not change between runs, which
// would silently alter every mission's behavior.
func TestComposePromptOrdersReposDeterministically(t *testing.T) {
	for range 5 {
		got, err := ComposePrompt(sampleOperation(), sampleMission())
		if err != nil {
			t.Fatalf("ComposePrompt: %v", err)
		}

		azure := strings.Index(got, "azure-tf:")
		weave := strings.Index(got, "weave:")

		if azure < 0 || weave < 0 || azure > weave {
			t.Fatalf("repos not in sorted order:\n%s", got)
		}
	}
}

// A worktree that failed to provision must not be described to the agent as
// available.
func TestComposePromptSkipsUncreatedWorktrees(t *testing.T) {
	mission := sampleMission()
	work := mission.Work["weave"]
	work.Created = false
	work.Error = "fetch failed"
	mission.Work["weave"] = work

	got, err := ComposePrompt(sampleOperation(), mission)
	if err != nil {
		t.Fatalf("ComposePrompt: %v", err)
	}

	if strings.Contains(got, "weave:") {
		t.Errorf("prompt should omit the failed worktree:\n%s", got)
	}

	if !strings.Contains(got, "azure-tf:") {
		t.Errorf("prompt should still list the successful worktree:\n%s", got)
	}
}

func TestComposePromptWithoutReposOrSummary(t *testing.T) {
	operation := domain.Operation{Name: "Bare"}
	mission := domain.Mission{Name: "solo", Prompt: "just do it"}

	got, err := ComposePrompt(operation, mission)
	if err != nil {
		t.Fatalf("ComposePrompt: %v", err)
	}

	if strings.Contains(got, "## Repositories") {
		t.Errorf("no repo section expected:\n%s", got)
	}

	for _, want := range []string{"# Operation: Bare", "## Mission: solo", "just do it"} {
		if !strings.Contains(got, want) {
			t.Errorf("prompt missing %q:\n%s", want, got)
		}
	}
}

// Worktree access is granted through the settings file because claude's --add-dir
// is variadic and would swallow the positional prompt that follows it.
func TestClaudeSettingsGrantsWorktreesViaPermissions(t *testing.T) {
	worktrees := []string{"/missions/t/weave", "/missions/t/azure-tf"}

	data, err := ClaudeSettings(qBin, worktrees)
	if err != nil {
		t.Fatalf("ClaudeSettings: %v", err)
	}

	var settings struct {
		Permissions struct {
			AdditionalDirectories []string `json:"additionalDirectories"`
		} `json:"permissions"`
		Hooks map[string][]struct {
			Matcher string `json:"matcher"`
			Hooks   []struct {
				Type    string `json:"type"`
				Command string `json:"command"`
				Timeout int    `json:"timeout"`
			} `json:"hooks"`
		} `json:"hooks"`
	}

	if err := json.Unmarshal(data, &settings); err != nil {
		t.Fatalf("decoding settings: %v", err)
	}

	if len(settings.Permissions.AdditionalDirectories) != 2 {
		t.Errorf("AdditionalDirectories = %q", settings.Permissions.AdditionalDirectories)
	}

	// The events q depends on to keep the board honest.
	for _, event := range []string{
		"SessionStart", "PermissionRequest", "PostToolUse", "Stop", "StopFailure", "SessionEnd",
	} {
		group, ok := settings.Hooks[event]
		if !ok || len(group) == 0 || len(group[0].Hooks) == 0 {
			t.Fatalf("no hook configured for %s", event)
		}

		entry := group[0].Hooks[0]

		// Only command handlers run in either agent.
		if entry.Type != "command" {
			t.Errorf("%s handler type = %q, want command", event, entry.Type)
		}

		if !strings.HasPrefix(entry.Command, qBin+" hook claude ") {
			t.Errorf("%s command = %q", event, entry.Command)
		}

		if entry.Timeout == 0 {
			t.Errorf("%s has no timeout; a hook must never make the agent wait forever", event)
		}
	}

	// Matching on notification_type matters because Notification is really eleven
	// different events sharing one name.
	notification := settings.Hooks["Notification"]
	if len(notification) == 0 || !strings.Contains(notification[0].Matcher, "permission_prompt") {
		t.Errorf("Notification matcher = %+v", notification)
	}
}

func TestClaudeSettingsIsValidJSON(t *testing.T) {
	data, err := ClaudeSettings(qBin, nil)
	if err != nil {
		t.Fatalf("ClaudeSettings: %v", err)
	}

	if !json.Valid(data) {
		t.Fatalf("settings are not valid JSON:\n%s", data)
	}

	if !strings.HasSuffix(string(data), "\n") {
		t.Error("settings should end with a newline")
	}
}

// codex keys hook trust on a hash of each handler, so this file must be
// byte-stable: any drift silently requires the user to re-approve hooks.
func TestCodexProfileIsByteStable(t *testing.T) {
	first := string(CodexProfile(qBin, testMissionDirs))

	for range 5 {
		if got := string(CodexProfile(qBin, testMissionDirs)); got != first {
			t.Fatal("CodexProfile output is not deterministic")
		}
	}

	for _, want := range []string{
		"[hooks]",
		`SessionStart = [{ hooks = [{ type = "command", command = "` + qBin + ` hook codex session-start", timeout = 5 }] }]`,
	} {
		if !strings.Contains(first, want) {
			t.Errorf("profile missing %q:\n%s", want, first)
		}
	}
}

// codex refuses to start in an untrusted directory and asks interactively, which in a
// detached session blocks forever with nothing on screen. Trust cannot be passed with
// -c, so the profile is the only place it can go.
func TestCodexProfilePreTrustsMissionDirectories(t *testing.T) {
	profile := string(CodexProfile(qBin, testMissionDirs))

	for _, dir := range testMissionDirs {
		want := "[projects." + `"` + dir + `"` + "]"
		if !strings.Contains(profile, want) {
			t.Errorf("profile missing trust for %q:\n%s", dir, profile)
		}
	}

	if strings.Count(profile, `trust_level = "trusted"`) != len(testMissionDirs) {
		t.Errorf("expected one trust entry per directory:\n%s", profile)
	}
}

// Trust is matched on the exact path with no inheritance, so entries must be written
// verbatim rather than collapsed to a common parent.
func TestCodexProfileWritesExactPaths(t *testing.T) {
	dirs := []string{"/data/missions/a--one", "/data/missions/b--two"}
	profile := string(CodexProfile(qBin, dirs))

	if strings.Contains(profile, `[projects."/data/missions"]`) {
		t.Errorf("a parent entry would not cover the mission directories:\n%s", profile)
	}
}

// The hooks section must stay put as directories come and go above it, because hook
// trust is approved once and keyed on the handler.
func TestCodexProfileHooksSectionIsStableAcrossMissionDirChanges(t *testing.T) {
	// Anchored to a line of its own, so the mention of [hooks] in the header comment
	// is not mistaken for the table itself.
	hooksOf := func(profile string) string {
		idx := strings.Index(profile, "\n[hooks]\n")
		if idx < 0 {
			t.Fatalf("no hooks section in:\n%s", profile)
		}

		return profile[idx:]
	}

	one := hooksOf(string(CodexProfile(qBin, []string{"/data/missions/a--one"})))
	many := hooksOf(string(CodexProfile(qBin, []string{
		"/data/missions/a--one", "/data/missions/b--two", "/data/missions/c--three",
	})))

	if one != many {
		t.Errorf("hooks section changed with the directory list:\n--- one ---\n%s\n--- many ---\n%s", one, many)
	}

	// The section has to come last for that to hold, since TOML keys belong to the
	// table most recently opened above them.
	profile := string(CodexProfile(qBin, testMissionDirs))
	if strings.Contains(hooksOf(profile), "[projects.") {
		t.Errorf("a projects table after [hooks] would capture the hook keys:\n%s", profile)
	}
}

// The same set of directories must produce the same bytes however they were ordered, or
// an unchanged configuration would look changed.
func TestCodexProfileIsOrderIndependent(t *testing.T) {
	forward := CodexProfile(qBin, []string{"/data/missions/a", "/data/missions/b", "/data/missions/a"})
	backward := CodexProfile(qBin, []string{"/data/missions/b", "/data/missions/a"})

	if string(forward) != string(backward) {
		t.Errorf("profile depends on input order:\n--- a ---\n%s\n--- b ---\n%s", forward, backward)
	}
}

// codex 0.147.0 has no Notification, PermissionDenied, or StopFailure event, so
// configuring them would look wired up and do nothing.
func TestCodexProfileOmitsUnsupportedEvents(t *testing.T) {
	profile := string(CodexProfile(qBin, testMissionDirs))

	for _, unsupported := range []string{"Notification", "PermissionDenied", "StopFailure"} {
		if strings.Contains(profile, unsupported) {
			t.Errorf("profile configures %q, which codex does not support:\n%s", unsupported, profile)
		}
	}

	// SessionStart is the only way to learn a codex session id, so it is required.
	if !strings.Contains(profile, "SessionStart") {
		t.Error("codex profile must subscribe to SessionStart to learn the session id")
	}
}

// Only command handlers actually run; prompt, agent, and async handlers are parsed
// and then silently skipped.
func TestCodexProfileEmitsOnlyCommandHandlers(t *testing.T) {
	profile := string(CodexProfile(qBin, testMissionDirs))

	for _, unsupported := range []string{`type = "prompt"`, `type = "agent"`, "async = true"} {
		if strings.Contains(profile, unsupported) {
			t.Errorf("profile contains a handler codex silently skips: %q", unsupported)
		}
	}
}

func TestShellQuote(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"plain", "'plain'"},
		{"with space", "'with space'"},
		{"it's", `'it'\''s'`},
		{"$(rm -rf /)", "'$(rm -rf /)'"},
		{"back`tick`", "'back`tick`'"},
		{"", "''"},
	} {
		if got := ShellQuote(tc.in); got != tc.want {
			t.Errorf("ShellQuote(%q) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// baseSpec is a claude launch for script tests.
func baseSpec() LaunchSpec {
	return LaunchSpec{
		Tool:        domain.ToolClaude,
		Bin:         "/Users/jarush/.local/bin/claude",
		MissionDir:  "/data/missions/discussions-api--add-endpoint",
		MissionID:   "ms_aabbccddeeff",
		HookEpoch:   1,
		DaemonFile:  "/state/q/daemon.json",
		SessionID:   "3f2a1b4c-5d6e-4f70-8a9b-0c1d2e3f4a5b",
		DisplayName: "q: add endpoint",
		Worktrees:   []string{"/data/missions/discussions-api--add-endpoint/weave"},
		PathEnv:     "/opt/homebrew/bin:/usr/bin:/bin",
	}
}

// The prompt is read from a file rather than passed as an argument, because tmux
// re-parses the trailing arguments of new-session through sh when they contain
// metacharacters, and a composed prompt always does.
func TestLaunchScriptReadsPromptFromFile(t *testing.T) {
	script, err := RenderLaunchScript(baseSpec())
	if err != nil {
		t.Fatalf("RenderLaunchScript: %v", err)
	}

	if !strings.Contains(script, `"$(cat "$d/prompt.md")"`) {
		t.Errorf("script should read the prompt from a file:\n%s", script)
	}
}

// exec makes the process replace the shell, which is what makes tmux report
// pane_current_command as "claude" and makes pane death mean agent death.
func TestLaunchScriptExecsTheAgent(t *testing.T) {
	script, err := RenderLaunchScript(baseSpec())
	if err != nil {
		t.Fatalf("RenderLaunchScript: %v", err)
	}

	if !strings.Contains(script, "\nexec '/Users/jarush/.local/bin/claude'") {
		t.Errorf("script should exec the agent:\n%s", script)
	}

	if !strings.HasPrefix(script, "#!/bin/sh\n") {
		t.Errorf("script needs a shebang:\n%s", script)
	}

	if !strings.Contains(script, "set -eu") {
		t.Errorf("script should fail fast:\n%s", script)
	}
}

func TestCodexLaunchScriptUsesAppServerWithFallback(t *testing.T) {
	tests := map[string]struct {
		resume bool
	}{
		"fresh": {},
		"resume": {
			resume: true,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			spec := baseSpec()
			spec.Tool = domain.ToolCodex
			spec.Resume = test.resume
			if !test.resume {
				spec.SessionID = ""
			}

			script, err := RenderLaunchScript(spec)
			if err != nil {
				t.Fatalf("RenderLaunchScript: %v", err)
			}

			if !strings.Contains(script, "--profile 'q' app-server daemon start") {
				t.Errorf("script does not start the managed app-server:\n%s", script)
			}

			if strings.Count(script, "\n  exec ") != 1 || strings.Count(script, "\nexec ") != 1 {
				t.Errorf("script should have remote and fallback execs:\n%s", script)
			}

			remote, fallback, found := strings.Cut(script, "\nfi\n")
			if !found {
				t.Fatalf("script has no app-server fallback boundary:\n%s", script)
			}

			if !strings.Contains(remote, "--remote") || !strings.Contains(remote, "unix://") {
				t.Errorf("remote invocation does not select the Unix socket:\n%s", remote)
			}

			if strings.Contains(fallback, "--remote") {
				t.Errorf("compatibility fallback unexpectedly uses app-server:\n%s", fallback)
			}
		})
	}
}

func TestLaunchScriptExportsHookIdentity(t *testing.T) {
	script, err := RenderLaunchScript(baseSpec())
	if err != nil {
		t.Fatalf("RenderLaunchScript: %v", err)
	}

	for _, want := range []string{
		"export Q_MISSION_ID='ms_aabbccddeeff'",
		"export Q_HOOK_EPOCH='1'",
		"export Q_DAEMON_FILE='/state/q/daemon.json'",
	} {
		if !strings.Contains(script, want) {
			t.Errorf("script missing %q:\n%s", want, script)
		}
	}
}

// TZ is the POSIX timezone variable, so a near-miss prefix would corrupt time
// handling in every process the agent spawns, git included.
func TestLaunchScriptNeverExportsBareTZVariables(t *testing.T) {
	script, err := RenderLaunchScript(baseSpec())
	if err != nil {
		t.Fatalf("RenderLaunchScript: %v", err)
	}

	for line := range strings.SplitSeq(script, "\n") {
		if strings.HasPrefix(line, "export TZ") && !strings.HasPrefix(line, "export Q_") {
			t.Errorf("script exports a TZ-prefixed variable: %q", line)
		}
	}
}

// claude's --add-dir is variadic and would consume the positional prompt.
func TestClaudeArgsNeverUseAddDir(t *testing.T) {
	args, err := AgentArgs(baseSpec())
	if err != nil {
		t.Fatalf("AgentArgs: %v", err)
	}

	joined := strings.Join(args, " ")

	if strings.Contains(joined, "--add-dir") {
		t.Errorf("claude args must not use the variadic --add-dir: %q", joined)
	}

	if strings.Contains(joined, "--allowedTools") {
		t.Errorf("claude args must not use the variadic --allowedTools: %q", joined)
	}

	// The prompt must be the final argument, after every flag.
	if args[len(args)-1] != promptArg {
		t.Errorf("prompt is not last: %q", joined)
	}
}

func TestClaudeArgsSessionAndMode(t *testing.T) {
	spec := baseSpec()

	args, err := AgentArgs(spec)
	if err != nil {
		t.Fatalf("AgentArgs: %v", err)
	}

	joined := strings.Join(args, " ")

	// A session id chosen in advance makes the session resumable immediately.
	if !strings.Contains(joined, "--session-id '"+spec.SessionID+"'") {
		t.Errorf("args should preset the session id: %q", joined)
	}

	if !strings.Contains(joined, "--permission-mode auto") {
		t.Errorf("args should use auto permission mode: %q", joined)
	}

	if !strings.Contains(joined, `--settings "$d/claude-settings.json"`) {
		t.Errorf("args should point at the per-mission settings file: %q", joined)
	}
}

func TestClaudeArgsPlanMode(t *testing.T) {
	spec := baseSpec()
	spec.PlanMode = true

	args, err := AgentArgs(spec)
	if err != nil {
		t.Fatalf("AgentArgs: %v", err)
	}

	if joined := strings.Join(args, " "); !strings.Contains(joined, "--permission-mode plan") {
		t.Errorf("plan mode not requested: %q", joined)
	}
}

// --session-id cannot be combined with --resume, so a resume must switch flags.
func TestClaudeArgsResumeUsesResumeFlag(t *testing.T) {
	spec := baseSpec()
	spec.Resume = true

	args, err := AgentArgs(spec)
	if err != nil {
		t.Fatalf("AgentArgs: %v", err)
	}

	joined := strings.Join(args, " ")

	if !strings.Contains(joined, "--resume '"+spec.SessionID+"'") {
		t.Errorf("args should resume: %q", joined)
	}

	if strings.Contains(joined, "--session-id") {
		t.Errorf("--session-id cannot be combined with --resume: %q", joined)
	}
}

// The user's approval policy and sandbox mode are left alone; the only override is
// trust for the mission directory, which lives outside the paths they have trusted.
func TestCodexArgsLeaveApprovalPolicyAlone(t *testing.T) {
	spec := baseSpec()
	spec.Tool = domain.ToolCodex
	spec.Bin = "/Users/jarush/.nvm/versions/node/v26.5.0/bin/codex"
	spec.SessionID = ""

	args, err := AgentArgs(spec)
	if err != nil {
		t.Fatalf("AgentArgs: %v", err)
	}

	joined := strings.Join(args, " ")

	for _, forbidden := range []string{
		"--sandbox", "-s ", "--ask-for-approval", "-a ",
		"--dangerously-bypass-approvals-and-sandbox",
	} {
		if strings.Contains(joined, forbidden) {
			t.Errorf("codex args should not override %q: %q", forbidden, joined)
		}
	}

	if !strings.Contains(joined, "--profile '"+DefaultCodexProfile+"'") {
		t.Errorf("codex args should select the q profile: %q", joined)
	}

	if !strings.Contains(joined, "--remote unix://") {
		t.Errorf("codex args should connect to the managed app-server: %q", joined)
	}

	// Trust is deliberately absent from argv: codex ignores it as a -c override and
	// reads project trust from a config file only, so passing it here would look like
	// a guarantee while doing nothing.
	if strings.Contains(joined, "trust_level") {
		t.Errorf("trust belongs in the profile, not argv: %q", joined)
	}

	// The trust check is what a bypass flag would disable, and q does not use
	// one: hook trust is approved once by the human instead.
	if strings.Contains(joined, "bypass") {
		t.Errorf("codex args must not bypass any trust check: %q", joined)
	}

	// codex's --add-dir takes a single value and is safe to repeat.
	if !strings.Contains(joined, "--add-dir") {
		t.Errorf("codex args should grant worktree access: %q", joined)
	}
}

// The mission directory is written in full because codex matches project trust on the
// path it is given, and a path containing ".." would not match the trust entry.
func TestCodexArgsUseAbsoluteMissionDir(t *testing.T) {
	spec := baseSpec()
	spec.Tool = domain.ToolCodex
	spec.SessionID = ""

	args, err := AgentArgs(spec)
	if err != nil {
		t.Fatalf("AgentArgs: %v", err)
	}

	joined := strings.Join(args, " ")

	if strings.Contains(joined, "..") {
		t.Errorf("codex args should not contain a relative path: %q", joined)
	}

	if !strings.Contains(joined, "--cd '"+spec.MissionDir+"'") {
		t.Errorf("codex args should cd to the mission dir: %q", joined)
	}
}

// codex takes the subcommand first, then its flags.
func TestCodexResumePutsSubcommandFirst(t *testing.T) {
	spec := baseSpec()
	spec.Tool = domain.ToolCodex
	spec.Resume = true
	spec.SessionID = "019fe1d9-8e2b-7592-85d3-6ae4fdc97f93"

	args, err := AgentArgs(spec)
	if err != nil {
		t.Fatalf("AgentArgs: %v", err)
	}

	if args[0] != "resume" {
		t.Errorf("args[0] = %q, want %q", args[0], "resume")
	}

	if joined := strings.Join(args, " "); !strings.Contains(joined, "'"+spec.SessionID+"'") {
		t.Errorf("resume should name the session: %q", joined)
	}
}

func TestAgentArgsRejectsUnknownTool(t *testing.T) {
	spec := baseSpec()
	spec.Tool = "cursor"

	if _, err := AgentArgs(spec); err == nil {
		t.Error("expected an error for an unknown tool")
	}
}

// A prompt full of shell metacharacters must not be able to escape its quoting.
func TestLaunchScriptQuotesHostilePrompt(t *testing.T) {
	spec := baseSpec()
	spec.DisplayName = "q: it's `evil` $(whoami)"

	script, err := RenderLaunchScript(spec)
	if err != nil {
		t.Fatalf("RenderLaunchScript: %v", err)
	}

	if strings.Contains(script, "--name 'q: it's ") {
		t.Errorf("single quote not escaped:\n%s", script)
	}

	if !strings.Contains(script, `'\''`) {
		t.Errorf("expected escaped single quote:\n%s", script)
	}
}

func TestLaunchSpecArtifactPath(t *testing.T) {
	spec := baseSpec()

	want := "/data/missions/discussions-api--add-endpoint/.q/prompt.md"
	if got := spec.Artifact(PromptFile); got != want {
		t.Errorf("Artifact = %q, want %q", got, want)
	}
}

func TestHookEventListsAreNotEmpty(t *testing.T) {
	if len(ClaudeHookEvents()) == 0 {
		t.Error("claude hook events should not be empty")
	}

	if len(CodexHookEvents()) == 0 {
		t.Error("codex hook events should not be empty")
	}
}
