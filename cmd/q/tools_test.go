package main

import (
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"time"
)

func TestToolEnvVar(t *testing.T) {
	for _, tc := range []struct{ tool, want string }{
		{string(toolCodex), "Q_CODEX_BIN"},
		{string(toolTmux), "Q_TMUX_BIN"},
	} {
		if got := toolName(tc.tool).EnvVar(); got != tc.want {
			t.Errorf("Tool(%q).EnvVar() = %q, want %q", tc.tool, got, tc.want)
		}
	}
}

// writeExecutable creates an executable stub and returns its path.
func writeExecutable(t *testing.T, name string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatalf("writing stub: %v", err)
	}

	return path
}

func TestPathHonorsAbsoluteOverride(t *testing.T) {
	stub := writeExecutable(t, "codex")
	t.Setenv(toolCodex.EnvVar(), stub)

	got, err := newToolResolver(toolOptions{}).Path(toolCodex)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}

	if got != stub {
		t.Errorf("Path = %q, want %q", got, stub)
	}
}

// A relative override would resolve differently depending on where q was
// launched from, which for a daemon is unpredictable.
func TestPathRejectsRelativeOverride(t *testing.T) {
	t.Setenv(toolCodex.EnvVar(), "bin/codex")

	_, err := newToolResolver(toolOptions{}).Path(toolCodex)
	if err == nil {
		t.Fatal("expected an error for a relative override")
	}

	if !strings.Contains(err.Error(), "absolute") {
		t.Errorf("error %q should mention that an absolute path is required", err)
	}
}

func TestPathRejectsNonExecutableOverride(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(path, []byte("not executable"), 0o600); err != nil {
		t.Fatalf("writing file: %v", err)
	}

	t.Setenv(toolCodex.EnvVar(), path)

	if _, err := newToolResolver(toolOptions{}).Path(toolCodex); err == nil {
		t.Fatal("expected an error for a non-executable override")
	}
}

func TestPathRejectsDirectoryOverride(t *testing.T) {
	t.Setenv(toolCodex.EnvVar(), t.TempDir())

	if _, err := newToolResolver(toolOptions{}).Path(toolCodex); err == nil {
		t.Fatal("expected an error for a directory override")
	}
}

// Resolution walks PATH and several fallback locations, so it must happen once
// per process rather than once per launched mission.
func TestPathCachesSuccess(t *testing.T) {
	stub := writeExecutable(t, "codex")
	t.Setenv(toolCodex.EnvVar(), stub)

	r := newToolResolver(toolOptions{})

	first, err := r.Path(toolCodex)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}

	// Point the override elsewhere; the cached answer must win.
	t.Setenv(toolCodex.EnvVar(), writeExecutable(t, "other"))

	second, err := r.Path(toolCodex)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}

	if first != second {
		t.Errorf("Path not cached: %q then %q", first, second)
	}
}

func TestPathCachesFailure(t *testing.T) {
	t.Setenv(toolCodex.EnvVar(), "relative")

	r := newToolResolver(toolOptions{})

	if _, err := r.Path(toolCodex); err == nil {
		t.Fatal("expected first call to fail")
	}

	if _, err := r.Path(toolCodex); err == nil {
		t.Fatal("expected cached failure to be returned")
	}
}

func TestMustPathReturnsEmptyOnFailure(t *testing.T) {
	t.Setenv(toolCodex.EnvVar(), "relative")

	if got := newToolResolver(toolOptions{}).MustPath(toolCodex); got != "" {
		t.Errorf("MustPath = %q, want empty string", got)
	}
}

// A configured path is used when no environment override is set, and the
// environment still wins over it: a one-off run must be able to point q at a
// different build without editing the config file.
func TestConfiguredPathIsUsedAndOverriddenByTheEnvironment(t *testing.T) {
	configured := writeExecutable(t, "codex-configured")

	r := newToolResolver(toolOptions{Overrides: map[toolName]string{toolCodex: configured}})

	got, err := r.Path(toolCodex)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}

	if got != configured {
		t.Errorf("Path = %q, want the configured %q", got, configured)
	}

	fromEnv := writeExecutable(t, "codex-env")
	t.Setenv(toolCodex.EnvVar(), fromEnv)

	got, err = newToolResolver(toolOptions{Overrides: map[toolName]string{toolCodex: configured}}).Path(toolCodex)
	if err != nil {
		t.Fatalf("Path: %v", err)
	}

	if got != fromEnv {
		t.Errorf("Path = %q, want the environment override %q", got, fromEnv)
	}
}

// osascript is a macOS-only tool and only the Ghostty terminal mode calls it, so
// requiring it anywhere else would refuse to start over a tool q never uses.
func TestRequiredForOnlyDemandsOsascriptForGhostty(t *testing.T) {
	ghostty := requiredToolsFor(settings{Terminal: terminalSettings{Mode: terminalGhostty}})
	if !slices.Contains(ghostty, toolOsaScript) {
		t.Errorf("RequiredFor(ghostty) = %v, want osascript included", ghostty)
	}

	none := requiredToolsFor(settings{Terminal: terminalSettings{Mode: terminalNone}})
	if slices.Contains(none, toolOsaScript) {
		t.Errorf("RequiredFor(none) = %v, want osascript excluded", none)
	}
}

// Every required tool must be reachable on a working developer machine; this
// doubles as a guard that the fallback lists stay correct.
func TestCheckFindsRequiredTools(t *testing.T) {
	err := newToolResolver(toolOptions{}).Check()
	if err != nil {
		t.Skipf("required tools are not available in this environment: %v", err)
	}
}

func TestAllIncludesEveryRequiredTool(t *testing.T) {
	present := map[toolName]bool{}
	for _, tool := range allTools {
		present[tool] = true
	}

	for _, tool := range defaultRequiredTools {
		if !present[tool] {
			t.Errorf("required tool %q missing from All", tool)
		}
	}
}

func TestExpandResolvesHomePrefix(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	got := expand("~/bin/codex")
	want := filepath.Join(home, "bin", "codex")

	if len(got) != 1 || got[0] != want {
		t.Errorf("expand = %q, want [%q]", got, want)
	}
}

// codex lives at ~/.nvm/versions/node/<version>/bin/codex, so glob expansion is
// the only way to find it when PATH lookup fails.
func TestExpandGlobPrefersNewest(t *testing.T) {
	root := t.TempDir()

	older := filepath.Join(root, "v20", "bin")
	newer := filepath.Join(root, "v26", "bin")

	for _, dir := range []string{older, newer} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			t.Fatalf("mkdir: %v", err)
		}

		if err := os.WriteFile(filepath.Join(dir, "codex"), []byte("#!/bin/sh\n"), 0o700); err != nil {
			t.Fatalf("writing stub: %v", err)
		}
	}

	// Back-date the v20 stub so the ordering cannot depend on filesystem
	// timestamp granularity.
	newest := filepath.Join(newer, "codex")
	stale := time.Now().Add(-time.Hour)

	if err := os.Chtimes(filepath.Join(older, "codex"), stale, stale); err != nil {
		t.Fatalf("chtimes: %v", err)
	}

	got := expand(filepath.Join(root, "*", "bin", "codex"))
	if len(got) != 2 {
		t.Fatalf("expand returned %d matches, want 2: %q", len(got), got)
	}

	if got[0] != newest {
		t.Errorf("expand[0] = %q, want the newest match %q", got[0], newest)
	}
}

func TestExpandReturnsNilForUnmatchedGlob(t *testing.T) {
	if got := expand(filepath.Join(t.TempDir(), "*", "nothing")); got != nil {
		t.Errorf("expand = %q, want nil", got)
	}
}
