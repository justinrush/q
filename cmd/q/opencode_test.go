package main

import (
	"bytes"
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/opencode"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/runner"
)

func TestOpencodeConfigurationAndDiscovery(t *testing.T) {
	bin := writeExecutable(t, "opencode")
	var file fileConfig
	data, err := json.Marshal(map[string]any{
		"agents": map[string]any{
			"default": "opencode",
			"opencode": map[string]any{
				"bin":  bin,
				"args": []string{"--pure"},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	s := defaultSettings()
	applyFile(&s, file)
	if s.Agents.Default != "opencode" || len(s.Agents.Opencode.Args) != 1 {
		t.Fatalf("%+v", s.Agents)
	}
	t.Setenv("Q_OPENCODE_BIN", "")
	if got, err := resolveTool(s, toolOpencode); err != nil || got != bin {
		t.Fatalf("%s, %v", got, err)
	}
	found := false
	for _, agent := range agentsFor(s) {
		if agent.Tool() == mission.ToolOpencode {
			found = true
			if agent.Bin() != bin {
				t.Fatal(agent.Bin())
			}
		}
	}
	if !found {
		t.Fatal("opencode was not registered in agentsFor")
	}

	var out bytes.Buffer
	if err := writeSampleConfig(&out, s); err != nil {
		t.Fatal(err)
	}
	var roundtrip fileConfig
	if err := json.Unmarshal(out.Bytes(), &roundtrip); err != nil {
		t.Fatal(err)
	}
	if roundtrip.Agents.Opencode == nil || roundtrip.Agents.Opencode.Bin != bin {
		t.Fatal(out.String())
	}

	override := writeExecutable(t, "opencode-override")
	t.Setenv("Q_OPENCODE_BIN", override)
	if got, err := resolveTool(s, toolOpencode); err != nil || got != override {
		t.Fatalf("%s, %v", got, err)
	}
	if tool, err := mission.ParseTool("opencode"); err != nil || tool != mission.ToolOpencode || tool.Next() != mission.ToolClaude || mission.ToolAgy.Next() != tool {
		t.Fatalf("tool rotation: %s %v", tool, err)
	}
}

func TestOpencodeModelConfiguration(t *testing.T) {
	s := defaultSettings()
	applyAgents(&s, &agentsConfig{
		Opencode: &agentConfig{
			Model:  "opencode/claude-sonnet-4-5",
			Effort: "high",
			Models: []string{"opencode/claude-sonnet-4-5"},
		},
	})
	if s.Agents.Opencode.Model != "opencode/claude-sonnet-4-5" || s.Agents.Opencode.Effort != "high" || len(s.Agents.Opencode.Models) != 1 {
		t.Fatal(s.Agents.Opencode)
	}

	var out bytes.Buffer
	if err := writeSampleConfig(&out, s); err != nil {
		t.Fatal(err)
	}
	var cfg fileConfig
	if err := json.Unmarshal(out.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Agents.Opencode.Model != "opencode/claude-sonnet-4-5" || cfg.Agents.Opencode.Effort != "high" || len(cfg.Agents.Opencode.Models) != 1 {
		t.Fatal(out.String())
	}

	run := runner.NewFake()
	run.Expect("/bin/opencode models", "opencode/claude-sonnet-4-5\n")
	set, err := withOverrides(opencode.NewProber("/bin/opencode", run, nil), s.Agents.Opencode).Probe(context.Background())
	if err != nil || set.Default != "opencode/claude-sonnet-4-5" || set.DefaultEffort != "high" {
		t.Fatalf("%+v %v", set, err)
	}

	t.Setenv(EnvOpencodeModel, "opencode/gpt-5")
	applyEnv(&s)
	if s.Agents.Opencode.Model != "opencode/gpt-5" {
		t.Fatal(s.Agents.Opencode)
	}
}

func TestOpencodeMeteringReport(t *testing.T) {
	s := defaultSettings()
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = s
	rep := newReport()
	reportCost(rep, paths.Dirs{Data: t.TempDir(), State: t.TempDir()})
	if !strings.Contains(rep.String(), "opencode") || !strings.Contains(rep.String(), "unavailable") {
		t.Fatal(rep.String())
	}
}
