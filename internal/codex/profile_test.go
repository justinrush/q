package codex

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

// write applies an agent's artifacts the way the launcher does, so this test
// exercises the merge contract rather than a copy of it.
func write(t *testing.T, agent *Agent, inv mission.Invocation) []byte {
	t.Helper()

	artifacts, err := agent.Artifacts(inv)
	if err != nil {
		t.Fatalf("Artifacts: %v", err)
	}

	if len(artifacts) != 1 {
		t.Fatalf("Artifacts returned %d files, want 1", len(artifacts))
	}

	artifact := artifacts[0]

	existing, err := os.ReadFile(artifact.Path)
	if err != nil && !os.IsNotExist(err) {
		t.Fatalf("reading the profile: %v", err)
	}

	merged, changed := artifact.Merge(artifact.Data, existing)
	if !changed {
		return existing
	}

	if err := os.MkdirAll(filepath.Dir(artifact.Path), 0o700); err != nil {
		t.Fatalf("creating the profile directory: %v", err)
	}

	if err := os.WriteFile(artifact.Path, merged, 0o600); err != nil {
		t.Fatalf("writing the profile: %v", err)
	}

	return merged
}

func TestProfilePreservesHookApprovalState(t *testing.T) {
	home := t.TempDir()
	agent := New("/usr/bin/codex", Options{ConfigDir: filepath.Join(home, ".codex")})

	oldMissionDir := filepath.Join(home, "missions", "old-mission")
	newMissionDir := filepath.Join(home, "missions", "new-mission")
	inv := mission.Invocation{QBin: "/usr/local/bin/q", MissionDirs: []string{oldMissionDir}}

	write(t, agent, inv)

	profilePath := filepath.Join(home, ".codex", "q.config.toml")

	profile, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("reading initial profile: %v", err)
	}

	hookState := `
[hooks.state]

[hooks.state."q.config.toml:session_start:0:0"]
trusted_hash = "sha256:approved"
`

	if err := os.WriteFile(profilePath, append(profile, hookState...), 0o600); err != nil {
		t.Fatalf("adding Codex hook state: %v", err)
	}

	inv.MissionDirs = []string{newMissionDir}
	refreshed := string(write(t, agent, inv))

	if strings.Contains(refreshed, oldMissionDir) {
		t.Errorf("refreshed profile retained old mission directory:\n%s", refreshed)
	}

	if !strings.Contains(refreshed, newMissionDir) {
		t.Errorf("refreshed profile omitted new mission directory:\n%s", refreshed)
	}

	if !strings.HasSuffix(refreshed, hookState) {
		t.Errorf("refreshed profile did not preserve Codex hook state:\n%s", refreshed)
	}

	if repeated := string(write(t, agent, inv)); repeated != refreshed {
		t.Error("an unchanged profile refresh was not byte-stable")
	}
}
