package gemini

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

const geminiBin = "/usr/bin/gemini"

func testInvocation(t *testing.T) mission.Invocation {
	t.Helper()

	dir := t.TempDir()

	return mission.Invocation{
		MissionID:   "ms_aabbccddeeff",
		HookEpoch:   1,
		MissionDir:  dir,
		MissionDirs: []string{dir},
		DisplayName: "q: add endpoint",
		Worktrees:   []string{filepath.Join(dir, "weave"), filepath.Join(dir, "azure-tf")},
		QBin:        "/usr/local/bin/q",
	}
}

func TestArgs(t *testing.T) {
	cases := []struct {
		name     string
		opts     Options
		mutate   func(*mission.Invocation)
		want     []string
		wantLast string
	}{{
		name: "fresh launch auto-approves edits",
		want: []string{"--approval-mode", approvalAutoEdit},
	}, {
		name:   "plan mode starts read-only",
		mutate: func(inv *mission.Invocation) { inv.PlanMode = true },
		want:   []string{"--approval-mode", approvalPlan},
	}, {
		name: "resume names the learned session",
		mutate: func(inv *mission.Invocation) {
			inv.Resume = true
			inv.SessionID = "7f3d0c1e-0000-4000-8000-000000000001"
		},
		want: []string{"--resume", "'7f3d0c1e-0000-4000-8000-000000000001'"},
	}, {
		// gemini has no --session-id, so a resume with nothing learned yet must
		// start fresh rather than guessing at "latest".
		name:   "resume without an id starts fresh",
		mutate: func(inv *mission.Invocation) { inv.Resume = true },
		want:   []string{"--approval-mode", approvalAutoEdit},
	}, {
		name: "user args land after q's own",
		opts: Options{Args: []string{"--model", "gemini-3-pro"}},
		want: []string{"--approval-mode", approvalAutoEdit, "'--model'", "'gemini-3-pro'"},
	}}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			inv := testInvocation(t)
			if tc.mutate != nil {
				tc.mutate(&inv)
			}

			got := New(geminiBin, tc.opts).Args(inv)

			joined := strings.Join(got, " ")
			if !strings.Contains(joined, strings.Join(tc.want, " ")) {
				t.Errorf("Args() = %v, want it to contain %v", got, tc.want)
			}

			if last := got[len(got)-1]; last != mission.PromptArg {
				t.Errorf("last arg = %q, want the prompt %q", last, mission.PromptArg)
			}
		})
	}
}

// gemini's --include-directories is a greedy list flag, so emitting it would let
// it swallow the positional prompt that follows. Worktrees travel in the
// settings file for exactly this reason.
func TestArgsEmitNoVariadicFlag(t *testing.T) {
	inv := testInvocation(t)

	for _, arg := range New(geminiBin, Options{}).Args(inv) {
		if strings.HasPrefix(arg, "--include-directories") {
			t.Fatalf("Args() emitted a variadic flag: %v", arg)
		}
	}
}

// An untrusted mission directory both blocks on an invisible dialog and makes
// gemini discard the settings file carrying q's hooks, so the launch script must
// trust it before the agent starts.
func TestPrologueTrustsTheWorkspace(t *testing.T) {
	got := New(geminiBin, Options{}).Prologue(testInvocation(t))

	if !strings.Contains(got, "export "+TrustEnvVar+"=true") {
		t.Errorf("Prologue() = %q, want it to export %s", got, TrustEnvVar)
	}
}

// decodeSettings renders the agent's settings artifact and decodes it.
func decodeSettings(t *testing.T, inv mission.Invocation) (map[string]any, mission.Artifact) {
	t.Helper()

	artifacts, err := New(geminiBin, Options{}).Artifacts(inv)
	if err != nil {
		t.Fatalf("Artifacts: %v", err)
	}

	if len(artifacts) != 1 {
		t.Fatalf("Artifacts returned %d files, want 1", len(artifacts))
	}

	var doc map[string]any
	if err := json.Unmarshal(artifacts[0].Data, &doc); err != nil {
		t.Fatalf("decoding settings: %v", err)
	}

	return doc, artifacts[0]
}

// The file has to land where gemini looks for workspace settings, which is the
// mission directory's .gemini, not q's own .q artifact directory.
func TestArtifactPathIsTheWorkspaceSettingsFile(t *testing.T) {
	inv := testInvocation(t)
	_, artifact := decodeSettings(t, inv)

	want := filepath.Join(inv.MissionDir, SettingsDir, SettingsFile)
	if artifact.Path != want {
		t.Errorf("Path = %q, want %q", artifact.Path, want)
	}

	if got := inv.ArtifactPath(artifact); got != want {
		t.Errorf("ArtifactPath = %q, want it left absolute at %q", got, want)
	}
}

func TestArtifactGrantsWorktreeAccess(t *testing.T) {
	inv := testInvocation(t)
	doc, _ := decodeSettings(t, inv)

	ctx, ok := doc["context"].(map[string]any)
	if !ok {
		t.Fatalf("settings have no context section: %v", doc)
	}

	dirs, ok := ctx["includeDirectories"].([]any)
	if !ok || len(dirs) != len(inv.Worktrees) {
		t.Fatalf("includeDirectories = %v, want %v", ctx["includeDirectories"], inv.Worktrees)
	}

	for i, want := range inv.Worktrees {
		if dirs[i] != want {
			t.Errorf("includeDirectories[%d] = %v, want %q", i, dirs[i], want)
		}
	}
}

// Every subscription must be keyed by gemini's own event name and must report
// q's canonical name, because the settings file and the hook command are read by
// different programs.
func TestArtifactTranslatesEventNames(t *testing.T) {
	cases := []struct {
		name      string
		event     string
		canonical string
	}{
		{"a turn ending", "AfterAgent", mission.EventStop},
		{"a prompt", "BeforeAgent", mission.EventUserPromptSubmit},
		{"a tool starting", "BeforeTool", mission.EventPreToolUse},
		{"a tool finishing", "AfterTool", mission.EventPostToolUse},
		{"a permission prompt", "Notification", mission.EventPermissionRequest},
		{"the session starting", "SessionStart", mission.EventSessionStart},
		{"the session ending", "SessionEnd", mission.EventSessionEnd},
	}

	inv := testInvocation(t)
	doc, _ := decodeSettings(t, inv)

	hooksDoc, ok := doc["hooks"].(map[string]any)
	if !ok {
		t.Fatalf("settings have no hooks section: %v", doc)
	}

	if len(hooksDoc) != len(cases) {
		t.Errorf("hooks section has %d events, want %d: %v", len(hooksDoc), len(cases), hooksDoc)
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			groups, ok := hooksDoc[tc.event].([]any)
			if !ok || len(groups) != 1 {
				t.Fatalf("hooks[%q] = %v, want one matcher group", tc.event, hooksDoc[tc.event])
			}

			entries, ok := groups[0].(map[string]any)["hooks"].([]any)
			if !ok || len(entries) != 1 {
				t.Fatalf("hooks[%q] has no single command: %v", tc.event, groups[0])
			}

			entry, ok := entries[0].(map[string]any)
			if !ok {
				t.Fatalf("hooks[%q] entry is not an object: %v", tc.event, entries[0])
			}

			want := mission.HookCommand(inv.QBin, mission.ToolGemini, tc.canonical)
			if entry["command"] != want {
				t.Errorf("command = %v, want %q", entry["command"], want)
			}

			// gemini reads this as milliseconds. Sending seconds would give every
			// hook a five-millisecond budget and fail all of them.
			if entry["timeout"] != float64(hookTimeoutMillis) {
				t.Errorf("timeout = %v, want %d milliseconds", entry["timeout"], hookTimeoutMillis)
			}
		})
	}
}

// The tool events need a catch-all, and gemini matches the field as a regular
// expression rather than as a glob.
func TestArtifactUsesARegexpMatcher(t *testing.T) {
	doc, _ := decodeSettings(t, testInvocation(t))
	hooksDoc, _ := doc["hooks"].(map[string]any)

	for _, event := range []string{"BeforeTool", "AfterTool"} {
		t.Run(event, func(t *testing.T) {
			groups, _ := hooksDoc[event].([]any)
			group, _ := groups[0].(map[string]any)

			if group["matcher"] != ".*" {
				t.Errorf("matcher = %v, want %q", group["matcher"], ".*")
			}
		})
	}
}

func TestAgentIdentity(t *testing.T) {
	agent := New(geminiBin, Options{})

	if agent.Tool() != mission.ToolGemini {
		t.Errorf("Tool() = %q, want gemini", agent.Tool())
	}

	if agent.Bin() != geminiBin {
		t.Errorf("Bin() = %q, want %q", agent.Bin(), geminiBin)
	}

	// The reported events are q's names, not gemini's, so a caller comparing
	// agents is comparing like with like.
	for _, event := range agent.HookEvents() {
		if _, err := mission.CanonicalHookEvent(mission.HookEventSlug(event)); err != nil {
			t.Errorf("HookEvents() reported %q, which is not a canonical event: %v", event, err)
		}
	}
}
