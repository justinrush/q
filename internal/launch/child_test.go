package launch

import (
	"github.com/justinrush/q/internal/mission"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestChildBriefSnapshotsHistoryWithoutInliningIt(t *testing.T) {
	root := t.TempDir()
	transcript := filepath.Join(root, "parent.jsonl")
	secret := "long private history entry"
	if err := os.WriteFile(transcript, []byte(secret), 0600); err != nil {
		t.Fatal(err)
	}
	parent := mission.Mission{ID: "ms_parent", Name: "Parent", MissionDir: filepath.Join(root, "parent"), TranscriptPath: transcript, Work: map[string]mission.RepoWork{"repo": {RepoName: "repo", WorktreePath: filepath.Join(root, "parent", "repo"), Branch: "feature", Created: true}}}
	child := mission.Mission{ID: "ms_child", ParentMissionID: parent.ID, Name: "Child", Prompt: "Do the task", MissionDir: filepath.Join(root, "child")}
	if err := os.MkdirAll(filepath.Join(child.MissionDir, mission.ArtifactDir), 0700); err != nil {
		t.Fatal(err)
	}
	launcher := &Launcher{hostSnapshot: func() mission.Snapshot { return mission.Snapshot{Missions: []mission.Mission{parent}} }}
	brief, err := launcher.childBrief(child)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(brief, parent.Work["repo"].WorktreePath) || !strings.Contains(brief, "Do the task") || strings.Contains(brief, secret) {
		t.Fatalf("child brief: %s", brief)
	}
	copied, err := os.ReadFile(filepath.Join(child.MissionDir, mission.ArtifactDir, "parent-history.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if string(copied) != secret {
		t.Fatalf("copied history: %q", copied)
	}
}
