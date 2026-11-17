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
	if s.Memory.Enabled || s.Memory.CaptureEnabled() {
		t.Fatal("memory should default off")
	}
	if !s.Memory.CaptureOnFinish {
		t.Fatal("capture should default on once memory is enabled")
	}

	var file fileConfig
	if err := json.Unmarshal([]byte(`{"memory":{"enabled":true}}`), &file); err != nil {
		t.Fatal(err)
	}
	applyFile(&s, file)
	if !s.Memory.Enabled || !s.Memory.CaptureEnabled() {
		t.Fatal("explicit enable was ignored")
	}

	file = fileConfig{}
	if err := json.Unmarshal([]byte(`{"memory":{"captureOnFinish":false}}`), &file); err != nil {
		t.Fatal(err)
	}
	applyFile(&s, file)
	if s.Memory.CaptureOnFinish || s.Memory.CaptureEnabled() || !s.Memory.Enabled {
		t.Fatal("explicit capture false was ignored")
	}

	var buf bytes.Buffer
	if err := writeSampleConfig(&buf, defaultSettings()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"enabled": false`) {
		t.Fatal("disabled memory omitted from config export")
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
