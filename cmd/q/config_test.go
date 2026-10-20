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

func TestLanesComeFromFileThenEnv(t *testing.T) {
	s := defaultSettings()
	if s.TUI.Lanes != lanesVertical {
		t.Fatalf("default tui.lanes = %q, want %q", s.TUI.Lanes, lanesVertical)
	}

	horizontal := lanesHorizontal
	applyFile(&s, fileConfig{TUI: &tuiConfig{Lanes: &horizontal}})
	if s.TUI.Lanes != lanesHorizontal {
		t.Errorf("file: tui.lanes = %q, want %q", s.TUI.Lanes, lanesHorizontal)
	}

	t.Setenv(EnvLanes, lanesVertical)
	applyEnv(&s)
	if s.TUI.Lanes != lanesVertical {
		t.Errorf("Q_LANES: tui.lanes = %q, want %q", s.TUI.Lanes, lanesVertical)
	}

	var buf bytes.Buffer
	if err := writeSampleConfig(&buf, s); err != nil {
		t.Fatalf("writeSampleConfig: %v", err)
	}

	if !strings.Contains(buf.String(), `"lanes": "vertical"`) {
		t.Errorf("sample config missing tui.lanes:\n%s", buf.String())
	}
}

func TestHorizontalLanesRejectsUnknownValues(t *testing.T) {
	for value, want := range map[string]bool{"": false, lanesVertical: false, lanesHorizontal: true} {
		got, err := horizontalLanes(value)
		if err != nil || got != want {
			t.Errorf("horizontalLanes(%q) = %v, %v; want %v", value, got, err, want)
		}
	}

	if _, err := horizontalLanes("diagonal"); err == nil {
		t.Errorf("horizontalLanes accepted an unknown value")
	}
}
