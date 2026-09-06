package agy

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

func TestLaunchAndResume(t *testing.T) {
	a := New("/bin/agy", Options{Args: []string{"--model", "model name"}})
	inv := mission.Invocation{MissionDir: "/missions/one", Worktrees: []string{"/missions/one/repo's files"}, SessionID: "conversation-one"}
	fresh := strings.Join(a.Args(inv), " ")
	for _, want := range []string{"--add-dir '/missions/one/repo'\\''s files'", "'--model' 'model name'", "--prompt-interactive " + mission.PromptArg} {
		if !strings.Contains(fresh, want) {
			t.Errorf("argv %s missing %s", fresh, want)
		}
	}
	if strings.Contains(fresh, "--conversation") {
		t.Fatal("fresh launch resumes a conversation")
	}
	inv.Resume = true
	if got := strings.Join(a.Args(inv), " "); !strings.Contains(got, "--conversation 'conversation-one'") {
		t.Fatal(got)
	}
	inv.SessionID = ""
	if got := strings.Join(a.Args(inv), " "); strings.Contains(got, "--conversation") || strings.Contains(got, "--continue") {
		t.Fatal("must not resume another mission: " + got)
	}
}

func TestWorkspaceHooks(t *testing.T) {
	inv := mission.Invocation{MissionDir: "/missions/one", QBin: "/my tools/q", MissionID: "ms_one", HookEpoch: 3, DaemonFile: "/state/daemon"}
	artifacts, err := New("agy", Options{}).Artifacts(inv)
	if err != nil {
		t.Fatal(err)
	}
	if len(artifacts) != 1 || artifacts[0].Path != filepath.Join(inv.MissionDir, ".agents", "hooks.json") {
		t.Fatalf("%+v", artifacts)
	}
	var doc map[string]map[string]json.RawMessage
	if err := json.Unmarshal(artifacts[0].Data, &doc); err != nil {
		t.Fatal(err)
	}
	var direct []handler
	if err := json.Unmarshal(doc["q-mission"]["PreInvocation"], &direct); err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{"'/my tools/q' hook agy session-start", "Q_MISSION_ID='ms_one'", "Q_HOOK_EPOCH='3'"} {
		if len(direct) != 1 || !strings.Contains(direct[0].Command, want) {
			t.Fatalf("missing %s: %+v", want, direct)
		}
	}
	var grouped []struct {
		Matcher string
		Hooks   []handler
	}
	if err := json.Unmarshal(doc["q-mission"]["PreToolUse"], &grouped); err != nil {
		t.Fatal(err)
	}
	if len(grouped) != 1 || grouped[0].Matcher != "*" || len(grouped[0].Hooks) != 1 {
		t.Fatalf("%+v", grouped)
	}
	merged, _ := artifacts[0].Merge(artifacts[0].Data, []byte(`{"custom":{"Stop":[]},"q-mission":{}}`))
	if !strings.Contains(string(merged), `"custom"`) || !strings.Contains(string(merged), "session-start") {
		t.Fatal(string(merged))
	}
	invalid := []byte(`{invalid`)
	if got, changed := artifacts[0].Merge(artifacts[0].Data, invalid); changed || string(got) != string(invalid) {
		t.Fatal("invalid user hooks overwritten")
	}
}

func TestModelEffortOnLaunchAndResume(t *testing.T) {
	a := New("agy", Options{})
	if got := strings.Join(a.Args(mission.Invocation{}), " "); strings.Contains(got, "--model") || strings.Contains(got, "--effort") {
		t.Fatal(got)
	}
	for _, resume := range []bool{false, true} {
		inv := mission.Invocation{Model: "gemini-3.8-flash-high", Effort: "high", Resume: resume, SessionID: "conv-one"}
		args := strings.Join(a.Args(inv), " ")
		for _, want := range []string{"--model 'gemini-3.8-flash-high'", "--effort 'high'"} {
			if !strings.Contains(args, want) {
				t.Fatalf("resume=%v: %s", resume, args)
			}
		}
	}
}
