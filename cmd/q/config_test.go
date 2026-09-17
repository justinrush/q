package main

import (
	"bytes"
	"strings"
	"testing"
)

func TestDefaultSettingsEnablesMouse(t *testing.T) {
	s := defaultSettings()
	if !s.TUI.Mouse {
		t.Errorf("defaultSettings().TUI.Mouse = false, want true")
	}
}

func TestApplyFileConfigSetsMouse(t *testing.T) {
	falseVal := false
	file := fileConfig{
		TUI: &tuiConfig{Mouse: &falseVal},
	}
	s := defaultSettings()
	applyFile(&s, file)
	if s.TUI.Mouse {
		t.Errorf("s.TUI.Mouse = true, want false")
	}

	trueVal := true
	fileTrue := fileConfig{
		TUI: &tuiConfig{Mouse: &trueVal},
	}
	applyFile(&s, fileTrue)
	if !s.TUI.Mouse {
		t.Errorf("s.TUI.Mouse = false, want true")
	}
}

func TestApplyEnvSetsMouse(t *testing.T) {
	s := defaultSettings()

	t.Setenv(EnvMouse, "false")
	applyEnv(&s)
	if s.TUI.Mouse {
		t.Errorf("Q_MOUSE=false: s.TUI.Mouse = true, want false")
	}

	t.Setenv(EnvMouse, "true")
	applyEnv(&s)
	if !s.TUI.Mouse {
		t.Errorf("Q_MOUSE=true: s.TUI.Mouse = false, want true")
	}
}

func TestWriteSampleConfigIncludesTUI(t *testing.T) {
	s := defaultSettings()
	var buf bytes.Buffer
	if err := writeSampleConfig(&buf, s); err != nil {
		t.Fatalf("writeSampleConfig: %v", err)
	}

	out := buf.String()
	if !strings.Contains(out, `"tui"`) || !strings.Contains(out, `"mouse": true`) {
		t.Errorf("sample config missing tui.mouse:\n%s", out)
	}
}
