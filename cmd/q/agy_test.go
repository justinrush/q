package main

import (
	"bytes"
	"encoding/json"
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
