package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/memory"
	"github.com/spf13/cobra"
)

func TestMemoryCaptureConfiguration(t *testing.T) {
	s := defaultSettings()
	if s.Features.Memory || s.Features.NestedMissions {
		t.Fatal("features should default off")
	}
	var file fileConfig
	if err := json.Unmarshal([]byte(`{"featureFlags":{"memory":true,"nestedMissions":true}}`), &file); err != nil {
		t.Fatal(err)
	}
	applyFile(&s, file)
	if !s.Features.Memory || !s.Features.NestedMissions {
		t.Fatal("feature flags ignored")
	}
	var buf bytes.Buffer
	if err := writeSampleConfig(&buf, defaultSettings()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"featureFlags"`) || strings.Contains(buf.String(), `"captureOnFinish"`) {
		t.Fatal("feature flags omitted or legacy memory settings exported")
	}
}

func TestMemorySaveInputAndSourceRequired(t *testing.T) {
	t.Setenv("Q_MISSION_ID", "")
	cmd := buildMemorySubcommand()
	cmd.SetIn(strings.NewReader(`{"records":[{"kind":"lesson","title":"Discovery","body":"Evidence"}]}`))
	cmd.SetArgs([]string{"save", "--file", "-"})
	err := cmd.Execute()
	if err == nil || !strings.Contains(err.Error(), "--mission or Q_MISSION_ID") {
		t.Fatalf("missing source: %v", err)
	}
}

func TestMemoryInputRejectsOversizedAndMalformedJSON(t *testing.T) {
	for _, input := range []string{strings.Repeat("x", memory.MaxBatch+1), `{"kind":`, `{} {}`} {
		cmd := &cobra.Command{}
		cmd.SetIn(strings.NewReader(input))
		var in memory.Input
		if err := readMemoryInput(cmd, "-", &in); err == nil {
			t.Fatal("invalid input accepted")
		}
	}
}
