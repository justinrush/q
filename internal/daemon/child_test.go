package daemon

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

func TestChildSharesParentAndBlocksCleanup(t *testing.T) {
	svc := newTestService(t)
	op := seedOperation(t, svc)
	parent := seedParentMission(t, svc, op)
	dir := t.TempDir()
	err := svc.store.Mutate("test.parent", func(snap *mission.Snapshot) error {
		ms, _ := snap.Mission(parent.ID)
		now := time.Now()
		ms.StartedAt = &now
		ms.Status = mission.StatusActive
		ms.MissionDir = dir
		ms.Lease = mission.Lease{Holder: svc.self, Epoch: 1}
		ms.Work = map[string]mission.RepoWork{"mac": {RepoName: "mac", WorktreePath: filepath.Join(dir, "mac"), Created: true, Branch: "feature"}}
		snap.PutMission(ms)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateMission(api.CreateMissionRequest{ParentMissionID: parent.ID, Name: "work", Prompt: "do work"}); err == nil {
		t.Fatal("disabled feature accepted child")
	}
	svc.apply(WithNestedMissions(true))
	child, err := svc.CreateMission(api.CreateMissionRequest{ParentMissionID: parent.ID, Name: "work", Prompt: "do work"})
	if err != nil {
		t.Fatal(err)
	}
	sibling, err := svc.CreateMission(api.CreateMissionRequest{ParentMissionID: parent.ID, Name: "second", Prompt: "x"})
	if err != nil {
		t.Fatal(err)
	}
	if sibling.SiblingOrder <= child.SiblingOrder {
		t.Fatal("sibling order not increasing")
	}
	target := sibling.SiblingOrder
	if _, err := svc.UpdateMission(child.ID, api.UpdateMissionRequest{Order: &target}); err != nil {
		t.Fatal(err)
	}
	moved, _ := svc.store.Snapshot().Mission(child.ID)
	other, _ := svc.store.Snapshot().Mission(sibling.ID)
	if moved.SiblingOrder != target || other.SiblingOrder != child.SiblingOrder {
		t.Fatalf("siblings were not swapped: %d %d", moved.SiblingOrder, other.SiblingOrder)
	}
	if child.ParentMissionID != parent.ID || child.Tool != parent.Tool || child.Model != parent.Model || child.Pin != svc.self {
		t.Fatalf("child inheritance: %+v", child)
	}
	if _, err := svc.CreateMission(api.CreateMissionRequest{ParentMissionID: child.ID, Name: "grandchild", Prompt: "x"}); err == nil {
		t.Fatal("recursive child accepted")
	}
	if _, err := svc.CreateMission(api.CreateMissionRequest{ParentMissionID: parent.ID, Name: "override", Prompt: "x", BaseBranches: map[string]string{"mac": "other"}}); err == nil {
		t.Fatal("branch override accepted")
	}
	if err := svc.DeleteMission(parent.ID); err == nil {
		t.Fatal("parent deleted with open child")
	}
	if _, err := svc.PublishChildResult(child.ID, "# Done\n\nTests pass.\n"); err != nil {
		t.Fatal(err)
	}
	got, _ := svc.store.Snapshot().Mission(child.ID)
	data, err := os.ReadFile(got.ResultPath)
	if err != nil || !strings.Contains(string(data), "Tests pass") {
		t.Fatalf("report: %q %v", data, err)
	}
	updated, _ := svc.store.Snapshot().Mission(parent.ID)
	if updated.ChildReports[child.ID] != got.ResultPath {
		t.Fatal("parent did not retain report path")
	}
	if err := svc.DeleteMission(child.ID); err != nil {
		t.Fatal(err)
	}
	updated, _ = svc.store.Snapshot().Mission(parent.ID)
	if updated.ChildReports[child.ID] != got.ResultPath {
		t.Fatal("report index lost with child record")
	}
	if !updated.HasLocalBadge(mission.BadgeChildResult) {
		t.Fatal("parent has no result badge")
	}
}
