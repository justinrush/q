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
	if !s.Memory.CaptureOnFinish {
		t.Fatal("capture should default on")
	}
	var file fileConfig
	if err := json.Unmarshal([]byte(`{"memory":{"captureOnFinish":false}}`), &file); err != nil {
		t.Fatal(err)
	}
	applyFile(&s, file)
	if s.Memory.CaptureOnFinish {
		t.Fatal("explicit false was ignored")
	}
	var buf bytes.Buffer
	if err := writeSampleConfig(&buf, s); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(buf.String(), `"captureOnFinish": false`) {
		t.Fatal("false omitted from config export")
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
