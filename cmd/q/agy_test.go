package main

import (
	"bytes"
	"context"
	"encoding/json"
	"github.com/justinrush/q/internal/agy"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/runner"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

func TestAgyConfigurationAndDiscovery(t *testing.T) {
	bin := writeExecutable(t, "agy")
	var file fileConfig
	data, err := json.Marshal(map[string]any{"agents": map[string]any{"default": "agy", "agy": map[string]any{"bin": bin, "args": []string{"--effort", "high"}}}})
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(data, &file); err != nil {
		t.Fatal(err)
	}
	s := defaultSettings()
	applyFile(&s, file)
	if s.Agents.Default != "agy" || len(s.Agents.Agy.Args) != 2 {
		t.Fatalf("%+v", s.Agents)
	}
	t.Setenv("Q_AGY_BIN", "")
	if got, err := resolveTool(s, toolAgy); err != nil || got != bin {
		t.Fatalf("%s, %v", got, err)
	}
	found := false
	for _, agent := range agentsFor(s) {
		if agent.Tool() == mission.ToolAgy {
			found = true
			if agent.Bin() != bin {
				t.Fatal(agent.Bin())
			}
		}
	}
	if !found {
		t.Fatal("agy was not registered")
	}
	var out bytes.Buffer
	if err := writeSampleConfig(&out, s); err != nil {
		t.Fatal(err)
	}
	var roundtrip fileConfig
	if err := json.Unmarshal(out.Bytes(), &roundtrip); err != nil {
		t.Fatal(err)
	}
	if roundtrip.Agents.Agy == nil || roundtrip.Agents.Agy.Bin != bin {
		t.Fatal(out.String())
	}
	override := writeExecutable(t, "agy-override")
	t.Setenv("Q_AGY_BIN", override)
	if got, err := resolveTool(s, toolAgy); err != nil || got != override {
		t.Fatalf("%s, %v", got, err)
	}
	if tool, err := mission.ParseTool("agy"); err != nil || tool != mission.ToolAgy || tool.Next() != mission.ToolClaude || mission.ToolCodex.Next() != tool {
		t.Fatalf("tool rotation: %s %v", tool, err)
	}
}

func TestAgyModelConfiguration(t *testing.T) {
	s := defaultSettings()
	applyAgents(&s, &agentsConfig{Agy: &agentConfig{Model: "custom", Effort: "high", Models: []string{"custom"}}})
	if s.Agents.Agy.Model != "custom" || s.Agents.Agy.Effort != "high" || len(s.Agents.Agy.Models) != 1 {
		t.Fatal(s.Agents.Agy)
	}
	var out bytes.Buffer
	if err := writeSampleConfig(&out, s); err != nil {
		t.Fatal(err)
	}
	var cfg fileConfig
	if err := json.Unmarshal(out.Bytes(), &cfg); err != nil {
		t.Fatal(err)
	}
	if cfg.Agents.Agy.Model != "custom" || cfg.Agents.Agy.Effort != "high" || len(cfg.Agents.Agy.Models) != 1 {
		t.Fatal(out.String())
	}
	run := runner.NewFake().Expect("/bin/agy models", "custom\tCustom Model\n")
	set, err := withOverrides(agy.NewProber("/bin/agy", run, nil), s.Agents.Agy).Probe(context.Background())
	if err != nil || set.Default != "custom" || set.DefaultEffort != "high" || !set.ValidEffort(set.Default, set.DefaultEffort) {
		t.Fatalf("%+v %v", set, err)
	}
	t.Setenv(EnvAgyModel, "environment-model")
	applyEnv(&s)
	if s.Agents.Agy.Model != "environment-model" {
		t.Fatal(s.Agents.Agy)
	}
}

func TestAgyMeteringUnavailable(t *testing.T) {
	s := defaultSettings()
	for _, meter := range metersFor(s) {
		if meter.Tool() == mission.ToolAgy {
			t.Fatal("no verified agy usage format is available")
		}
	}
	old := cfg
	t.Cleanup(func() { cfg = old })
	cfg = s
	rep := newReport()
	reportCost(rep, paths.Dirs{Data: t.TempDir(), State: t.TempDir()})
	if !strings.Contains(rep.String(), "agy") || !strings.Contains(rep.String(), "unavailable") {
		t.Fatal(rep.String())
	}
}
