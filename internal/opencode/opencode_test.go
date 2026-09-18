package opencode

import (
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

func TestOpencodeAgentMetadata(t *testing.T) {
	ag := New("/custom/bin/opencode", Options{Args: []string{"--mini"}})

	if ag.Tool() != mission.ToolOpencode {
		t.Errorf("Tool() = %q, want %q", ag.Tool(), mission.ToolOpencode)
	}

	if ag.Bin() != "/custom/bin/opencode" {
		t.Errorf("Bin() = %q, want %q", ag.Bin(), "/custom/bin/opencode")
	}

	if p := ag.Prologue(mission.Invocation{}); p != "" {
		t.Errorf("Prologue() = %q, want empty", p)
	}

	events := ag.HookEvents()
	for _, want := range []string{
		mission.EventSessionStart,
		mission.EventSessionEnd,
		mission.EventUserPromptSubmit,
		mission.EventPreToolUse,
		mission.EventPostToolUse,
		mission.EventPermissionRequest,
		mission.EventStop,
		mission.EventStopFailure,
	} {
		found := false
		for _, e := range events {
			if e == want {
				found = true
				break
			}
		}
		if !found {
			t.Errorf("HookEvents() missing %q", want)
		}
	}
}

func TestOpencodeArgs(t *testing.T) {
	ag := New("/bin/opencode", Options{Args: []string{"--pure"}})

	// 1. Plain invocation
	inv := mission.Invocation{
		MissionDir: "/missions/ms1",
	}
	args := ag.Args(inv)
	want := []string{"--auto", "'--pure'", "--prompt", mission.PromptArg}
	if strings.Join(args, " ") != strings.Join(want, " ") {
		t.Errorf("Args = %v, want %v", args, want)
	}

	// 2. With model, plan mode, resume
	inv = mission.Invocation{
		MissionDir: "/missions/ms1",
		Model:      "opencode/claude-sonnet-4-5",
		PlanMode:   true,
		Resume:     true,
		SessionID:  "ses-12345",
	}
	args = ag.Args(inv)
	joined := strings.Join(args, " ")
	for _, sub := range []string{
		"--session 'ses-12345'",
		"--agent plan",
		"--auto",
		"--model 'opencode/claude-sonnet-4-5'",
		"'--pure'",
		"--prompt " + mission.PromptArg,
	} {
		if !strings.Contains(joined, sub) {
			t.Errorf("Args %q does not contain %q", joined, sub)
		}
	}
}

func TestOpencodeArtifacts(t *testing.T) {
	ag := New("/bin/opencode", Options{})
	inv := mission.Invocation{
		MissionDir: "/missions/ms1",
		Model:      "opencode/gpt-5",
		Effort:     "high",
		QBin:       "/opt/q/bin/q",
	}

	artifacts, err := ag.Artifacts(inv)
	if err != nil {
		t.Fatalf("Artifacts() failed: %v", err)
	}

	if len(artifacts) != 2 {
		t.Fatalf("len(artifacts) = %d, want 2", len(artifacts))
	}

	cfgArt := artifacts[0]
	wantCfgPath := filepath.Join("/missions/ms1", ".opencode", "opencode.json")
	if cfgArt.Path != wantCfgPath {
		t.Errorf("cfg.Path = %q, want %q", cfgArt.Path, wantCfgPath)
	}

	var parsedCfg map[string]any
	if err := json.Unmarshal(cfgArt.Data, &parsedCfg); err != nil {
		t.Fatalf("invalid json in opencode.json: %v", err)
	}
	if parsedCfg["model"] != "opencode/gpt-5" {
		t.Errorf("model = %v, want opencode/gpt-5", parsedCfg["model"])
	}
	perm, _ := parsedCfg["permission"].(map[string]any)
	if perm["external_directory"] != "allow" {
		t.Errorf("permission.external_directory = %v, want allow", perm["external_directory"])
	}
	agent, _ := parsedCfg["agent"].(map[string]any)
	buildAgent, _ := agent["build"].(map[string]any)
	if buildAgent["variant"] != "high" {
		t.Errorf("agent.build.variant = %v, want high", buildAgent["variant"])
	}

	plugArt := artifacts[1]
	wantPlugPath := filepath.Join("/missions/ms1", ".opencode", "plugins", "q-status.js")
	if plugArt.Path != wantPlugPath {
		t.Errorf("plugin.Path = %q, want %q", plugArt.Path, wantPlugPath)
	}
	plugStr := string(plugArt.Data)
	if !strings.Contains(plugStr, "/opt/q/bin/q") {
		t.Errorf("plugin does not contain QBin path")
	}
	if !strings.Contains(plugStr, "session-start") || !strings.Contains(plugStr, "permission-request") {
		t.Errorf("plugin missing expected hook events")
	}
}

func TestMergeConfig(t *testing.T) {
	inv := mission.Invocation{
		Model:  "opencode/claude-sonnet-4-5",
		Effort: "max",
	}

	// 1. Empty existing
	gen, err := renderConfig(inv)
	if err != nil {
		t.Fatal(err)
	}
	res, changed := mergeConfig(inv, gen, nil)
	if !changed || string(res) != string(gen) {
		t.Errorf("empty existing did not return generated")
	}

	// 2. Existing config with custom fields
	existing := []byte(`{
  "username": "custom-user",
  "theme": "dark",
  "permission": {
    "read": "allow"
  }
}`)
	merged, changed := mergeConfig(inv, gen, existing)
	if !changed {
		t.Fatal("mergeConfig reported no change")
	}

	var root map[string]any
	if err := json.Unmarshal(merged, &root); err != nil {
		t.Fatalf("invalid json: %v", err)
	}

	if root["username"] != "custom-user" || root["theme"] != "dark" {
		t.Errorf("custom fields lost in merge: %+v", root)
	}
	perm, _ := root["permission"].(map[string]any)
	if perm["external_directory"] != "allow" || perm["read"] != "allow" {
		t.Errorf("permission not merged: %+v", perm)
	}
	if root["model"] != "opencode/claude-sonnet-4-5" {
		t.Errorf("model = %v, want opencode/claude-sonnet-4-5", root["model"])
	}
	agent, _ := root["agent"].(map[string]any)
	buildAgent, _ := agent["build"].(map[string]any)
	if buildAgent["variant"] != "max" {
		t.Errorf("agent.build.variant = %v, want max", buildAgent["variant"])
	}

	// 3. Corrupt existing preserved
	corrupt := []byte("{corrupt")
	preserved, changed := mergeConfig(inv, gen, corrupt)
	if changed || string(preserved) != string(corrupt) {
		t.Errorf("corrupt config was not preserved")
	}
}
