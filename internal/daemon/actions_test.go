package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

type fakeReclaimer struct {
	report mission.Report
	err    error
	calls  int
	force  bool
}

type partialDebriefer struct {
	err error
}

func (r partialDebriefer) Open(
	_ context.Context,
	ms mission.Mission,
	_ api.Mode,
) (api.Result, mission.Mission, error) {
	work := ms.Work["repo"]
	work.DebriefPaneID = "%21"
	ms.Work["repo"] = work

	return api.Result{PanesAdded: 1}, ms, r.err
}

func (partialDebriefer) Touched(context.Context, mission.Mission) ([]api.Touched, error) {
	return nil, nil
}

func (f *fakeReclaimer) PlanReclaim(
	context.Context,
	mission.Operation,
	mission.Mission,
) (mission.Plan, error) {
	return mission.Plan{}, nil
}

func (f *fakeReclaimer) Reclaim(
	_ context.Context,
	_ mission.Operation,
	_ mission.Mission,
	force bool,
) (mission.Report, error) {
	f.calls++
	f.force = force

	return f.report, f.err
}

func launchedServiceMission(t *testing.T, svc *Service) mission.Mission {
	t.Helper()

	operation := seedOperation(t, svc)
	ms, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "mission",
		Prompt:      "do it",
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	started := time.Now()
	ms.StartedAt = &started
	ms.MissionDir = "/missions/mission"
	ms.Work = make(map[string]mission.RepoWork)
	ms.Work["repo"] = mission.RepoWork{RepoName: "repo", Created: true}

	err = svcStore(svc).Mutate("test.launch", func(snap *mission.Snapshot) error {
		snap.PutMission(ms)

		return nil
	})
	if err != nil {
		t.Fatalf("storing launched mission: %v", err)
	}

	return ms
}

func TestOpenDebriefPersistsPanesCreatedBeforeFailure(t *testing.T) {
	svc := newTestService(t)
	ms := launchedServiceMission(t, svc)
	openErr := errors.New("no space for new pane")
	svc.apply(WithDebriefer(partialDebriefer{err: openErr}))

	_, err := svc.OpenDebrief(t.Context(), ms.ID, api.ModePrepare)
	if !errors.Is(err, openErr) {
		t.Fatalf("err = %v, want %v", err, openErr)
	}

	stored, ok := svc.Snapshot().Mission(ms.ID)
	if !ok {
		t.Fatal("mission disappeared after the failed open")
	}

	if stored.Work["repo"].DebriefPaneID != "%21" {
		t.Errorf("partial pane was not persisted: %+v", stored.Work["repo"])
	}
}

func TestDeleteMissionRetainsRecordAfterPartialReclaim(t *testing.T) {
	svc := newTestService(t)
	ms := launchedServiceMission(t, svc)
	svc.apply(WithReclaimer(&fakeReclaimer{report: mission.Report{
		Failures: []string{"repo: worktree is locked"},
	}}))

	report, err := svc.DeleteMissionAndReclaim(t.Context(), ms.ID, false)
	if err == nil || !strings.Contains(err.Error(), "worktree is locked") {
		t.Fatalf("err = %v, want the reclaim failure", err)
	}

	if len(report.Failures) != 1 {
		t.Fatalf("Failures = %v, want the partial result", report.Failures)
	}

	if _, ok := svc.Snapshot().Mission(ms.ID); !ok {
		t.Error("a mission with resources left behind must remain retryable")
	}
}

func TestDeleteMissionRetainsRecordAfterReclaimError(t *testing.T) {
	svc := newTestService(t)
	ms := launchedServiceMission(t, svc)
	reclaimErr := errors.New("tmux unavailable")
	svc.apply(WithReclaimer(&fakeReclaimer{err: reclaimErr}))

	_, err := svc.DeleteMissionAndReclaim(t.Context(), ms.ID, false)
	if !errors.Is(err, reclaimErr) {
		t.Fatalf("err = %v, want %v", err, reclaimErr)
	}

	if _, ok := svc.Snapshot().Mission(ms.ID); !ok {
		t.Error("a mission must survive a failed reclaim")
	}
}

func TestDeleteMissionForgetsRecordAfterSuccessfulReclaim(t *testing.T) {
	svc := newTestService(t)
	ms := launchedServiceMission(t, svc)
	svc.apply(WithReclaimer(&fakeReclaimer{}))

	_, err := svc.DeleteMissionAndReclaim(t.Context(), ms.ID, false)
	if err != nil {
		t.Fatalf("DeleteMissionAndReclaim: %v", err)
	}

	if _, ok := svc.Snapshot().Mission(ms.ID); ok {
		t.Error("a successfully reclaimed mission should be deleted")
	}
}

func TestFinishMissionReclaimsResourcesAndRetainsCard(t *testing.T) {
	svc := newTestService(t)
	ms := launchedServiceMission(t, svc)

	_, err := svc.SetStatus(ms.ID, mission.StatusDebrief)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	reclaimer := &fakeReclaimer{report: mission.Report{Removed: []string{"/missions/mission/repo"}}}
	svc.apply(WithReclaimer(reclaimer))

	finished, report, err := svc.FinishMission(t.Context(), ms.ID, false)
	if err != nil {
		t.Fatalf("FinishMission: %v", err)
	}

	if reclaimer.calls != 1 || len(report.Removed) != 1 {
		t.Fatalf("reclaim calls = %d, report = %+v", reclaimer.calls, report)
	}

	if finished.Status != mission.StatusClosed || finished.FinishedAt == nil {
		t.Errorf("finished mission = %+v, want a filed card", finished)
	}

	if finished.MissionDir != "" || finished.TmuxSession != "" || finished.AgentPaneID != "" {
		t.Errorf("finished mission retains runtime paths: %+v", finished)
	}

	if finished.StartedAt != nil || finished.Launched() || len(finished.Work) != 0 {
		t.Errorf("finished mission still looks launched: %+v", finished)
	}

	if _, ok := svc.Snapshot().Mission(ms.ID); !ok {
		t.Error("finishing should retain the mission card")
	}
}

func TestFinishMissionDoesNotFileFailedReclaim(t *testing.T) {
	svc := newTestService(t)
	ms := launchedServiceMission(t, svc)

	_, err := svc.SetStatus(ms.ID, mission.StatusDebrief)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	svc.apply(WithReclaimer(&fakeReclaimer{report: mission.Report{
		Failures: []string{"repo: worktree is locked"},
	}}))

	_, _, err = svc.FinishMission(t.Context(), ms.ID, false)
	if err == nil {
		t.Fatal("FinishMission succeeded after a partial reclaim")
	}

	stored, ok := svc.Snapshot().Mission(ms.ID)
	if !ok {
		t.Fatal("the mission record was removed")
	}

	if stored.Status != mission.StatusDebrief || stored.MissionDir == "" {
		t.Errorf("failed finish changed mission state: %+v", stored)
	}
}

func TestFinishMissionIsIdempotent(t *testing.T) {
	svc := newTestService(t)
	ms := launchedServiceMission(t, svc)
	reclaimer := &fakeReclaimer{}
	svc.apply(WithReclaimer(reclaimer))

	_, _, err := svc.FinishMission(t.Context(), ms.ID, false)
	if err != nil {
		t.Fatalf("first FinishMission: %v", err)
	}

	finished, _, err := svc.FinishMission(t.Context(), ms.ID, false)
	if err != nil {
		t.Fatalf("second FinishMission: %v", err)
	}

	if finished.Status != mission.StatusClosed || reclaimer.calls != 1 {
		t.Errorf("finished = %+v, reclaim calls = %d", finished, reclaimer.calls)
	}
}
