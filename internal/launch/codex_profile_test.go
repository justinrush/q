package launch

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestWriteCodexProfilePreservesHookApprovalState(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	launcher, _, dirs := newTestLauncher(t)
	oldMissionDir := dirs.MissionDir("old-mission")
	newMissionDir := dirs.MissionDir("new-mission")
	qBin := "/usr/local/bin/q"

	err := launcher.writeCodexProfile(qBin, oldMissionDir)
	if err != nil {
		t.Fatalf("writeCodexProfile initial: %v", err)
	}

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
	profile = append(profile, []byte(hookState)...)
	err = os.WriteFile(profilePath, profile, 0o600)
	if err != nil {
		t.Fatalf("adding Codex hook state: %v", err)
	}

	err = launcher.writeCodexProfile(qBin, newMissionDir)
	if err != nil {
		t.Fatalf("writeCodexProfile refresh: %v", err)
	}

	refreshed, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("reading refreshed profile: %v", err)
	}

	if strings.Contains(string(refreshed), oldMissionDir) {
		t.Errorf("refreshed profile retained old mission directory:\n%s", refreshed)
	}

	if !strings.Contains(string(refreshed), newMissionDir) {
		t.Errorf("refreshed profile omitted new mission directory:\n%s", refreshed)
	}

	if !strings.HasSuffix(string(refreshed), hookState) {
		t.Errorf("refreshed profile did not preserve Codex hook state:\n%s", refreshed)
	}

	err = launcher.writeCodexProfile(qBin, newMissionDir)
	if err != nil {
		t.Fatalf("writeCodexProfile repeat: %v", err)
	}

	repeated, err := os.ReadFile(profilePath)
	if err != nil {
		t.Fatalf("reading repeated profile: %v", err)
	}

	if string(repeated) != string(refreshed) {
		t.Error("an unchanged profile refresh was not byte-stable")
	}
}
