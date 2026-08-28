package daemon

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/debrief"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/launch"
	"github.com/justinrush/q/internal/state"
)

type fakeReclaimer struct {
	report launch.Report
	err    error
	calls  int
	force  bool
}

type partialDebriefer struct {
	err error
}

func (r partialDebriefer) Open(
	_ context.Context,
	mission domain.Mission,
	_ debrief.Mode,
) (debrief.Result, domain.Mission, error) {
	work := mission.Work["repo"]
	work.DebriefPaneID = "%21"
	mission.Work["repo"] = work

	return debrief.Result{PanesAdded: 1}, mission, r.err
}

func (partialDebriefer) Touched(context.Context, domain.Mission) ([]debrief.Touched, error) {
	return nil, nil
}

func (f *fakeReclaimer) PlanReclaim(
	context.Context,
	domain.Operation,
	domain.Mission,
) (launch.Plan, error) {
	return launch.Plan{}, nil
}

func (f *fakeReclaimer) Reclaim(
	_ context.Context,
	_ domain.Operation,
	_ domain.Mission,
	force bool,
) (launch.Report, error) {
	f.calls++
	f.force = force

	return f.report, f.err
}

func launchedServiceMission(t *testing.T, svc *Service) domain.Mission {
	t.Helper()

	operation := seedOperation(t, svc)
	mission, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "mission",
		Prompt:      "do it",
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	started := time.Now()
	mission.StartedAt = &started
	mission.MissionDir = "/missions/mission"
	mission.Work = make(map[string]domain.RepoWork)
	mission.Work["repo"] = domain.RepoWork{RepoName: "repo", Created: true}

	err = svcStore(svc).Mutate("test.launch", func(snap *state.Snapshot) error {
		snap.PutMission(mission)

		return nil
	})
	if err != nil {
		t.Fatalf("storing launched mission: %v", err)
	}

	return mission
}

func TestOpenDebriefPersistsPanesCreatedBeforeFailure(t *testing.T) {
	svc := newTestService(t)
	mission := launchedServiceMission(t, svc)
	openErr := errors.New("no space for new pane")
	svc.SetDebriefer(partialDebriefer{err: openErr})

	_, err := svc.OpenDebrief(t.Context(), mission.ID, debrief.ModePrepare)
	if !errors.Is(err, openErr) {
		t.Fatalf("err = %v, want %v", err, openErr)
	}

	stored, ok := svc.Snapshot().Mission(mission.ID)
	if !ok {
		t.Fatal("mission disappeared after the failed open")
	}

	if stored.Work["repo"].DebriefPaneID != "%21" {
		t.Errorf("partial pane was not persisted: %+v", stored.Work["repo"])
	}
}

func TestDeleteMissionRetainsRecordAfterPartialReclaim(t *testing.T) {
	svc := newTestService(t)
	mission := launchedServiceMission(t, svc)
	svc.SetReclaimer(&fakeReclaimer{report: launch.Report{
		Failures: []string{"repo: worktree is locked"},
	}})

	report, err := svc.DeleteMissionAndReclaim(t.Context(), mission.ID, false)
	if err == nil || !strings.Contains(err.Error(), "worktree is locked") {
		t.Fatalf("err = %v, want the reclaim failure", err)
	}

	if len(report.Failures) != 1 {
		t.Fatalf("Failures = %v, want the partial result", report.Failures)
	}

	if _, ok := svc.Snapshot().Mission(mission.ID); !ok {
		t.Error("a mission with resources left behind must remain retryable")
	}
}

func TestDeleteMissionRetainsRecordAfterReclaimError(t *testing.T) {
	svc := newTestService(t)
	mission := launchedServiceMission(t, svc)
	reclaimErr := errors.New("tmux unavailable")
	svc.SetReclaimer(&fakeReclaimer{err: reclaimErr})

	_, err := svc.DeleteMissionAndReclaim(t.Context(), mission.ID, false)
	if !errors.Is(err, reclaimErr) {
		t.Fatalf("err = %v, want %v", err, reclaimErr)
	}

	if _, ok := svc.Snapshot().Mission(mission.ID); !ok {
		t.Error("a mission must survive a failed reclaim")
	}
}

func TestDeleteMissionForgetsRecordAfterSuccessfulReclaim(t *testing.T) {
	svc := newTestService(t)
	mission := launchedServiceMission(t, svc)
	svc.SetReclaimer(&fakeReclaimer{})

	_, err := svc.DeleteMissionAndReclaim(t.Context(), mission.ID, false)
	if err != nil {
		t.Fatalf("DeleteMissionAndReclaim: %v", err)
	}

	if _, ok := svc.Snapshot().Mission(mission.ID); ok {
		t.Error("a successfully reclaimed mission should be deleted")
	}
}

func TestFinishMissionReclaimsResourcesAndRetainsCard(t *testing.T) {
	svc := newTestService(t)
	mission := launchedServiceMission(t, svc)

	_, err := svc.SetStatus(mission.ID, domain.StatusDebrief)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	reclaimer := &fakeReclaimer{report: launch.Report{Removed: []string{"/missions/mission/repo"}}}
	svc.SetReclaimer(reclaimer)

	finished, report, err := svc.FinishMission(t.Context(), mission.ID, false)
	if err != nil {
		t.Fatalf("FinishMission: %v", err)
	}

	if reclaimer.calls != 1 || len(report.Removed) != 1 {
		t.Fatalf("reclaim calls = %d, report = %+v", reclaimer.calls, report)
	}

	if finished.Status != domain.StatusClosed || finished.FinishedAt == nil {
		t.Errorf("finished mission = %+v, want a filed card", finished)
	}

	if finished.MissionDir != "" || finished.TmuxSession != "" || finished.AgentPaneID != "" {
		t.Errorf("finished mission retains runtime paths: %+v", finished)
	}

	if finished.StartedAt != nil || finished.Launched() || len(finished.Work) != 0 {
		t.Errorf("finished mission still looks launched: %+v", finished)
	}

	if _, ok := svc.Snapshot().Mission(mission.ID); !ok {
		t.Error("finishing should retain the mission card")
	}
}

func TestFinishMissionDoesNotFileFailedReclaim(t *testing.T) {
	svc := newTestService(t)
	mission := launchedServiceMission(t, svc)

	_, err := svc.SetStatus(mission.ID, domain.StatusDebrief)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	svc.SetReclaimer(&fakeReclaimer{report: launch.Report{
		Failures: []string{"repo: worktree is locked"},
	}})

	_, _, err = svc.FinishMission(t.Context(), mission.ID, false)
	if err == nil {
		t.Fatal("FinishMission succeeded after a partial reclaim")
	}

	stored, ok := svc.Snapshot().Mission(mission.ID)
	if !ok {
		t.Fatal("the mission record was removed")
	}

	if stored.Status != domain.StatusDebrief || stored.MissionDir == "" {
		t.Errorf("failed finish changed mission state: %+v", stored)
	}
}

func TestFinishMissionIsIdempotent(t *testing.T) {
	svc := newTestService(t)
	mission := launchedServiceMission(t, svc)
	reclaimer := &fakeReclaimer{}
	svc.SetReclaimer(reclaimer)

	_, _, err := svc.FinishMission(t.Context(), mission.ID, false)
	if err != nil {
		t.Fatalf("first FinishMission: %v", err)
	}

	finished, _, err := svc.FinishMission(t.Context(), mission.ID, false)
	if err != nil {
		t.Fatalf("second FinishMission: %v", err)
	}

	if finished.Status != domain.StatusClosed || reclaimer.calls != 1 {
		t.Errorf("finished = %+v, reclaim calls = %d", finished, reclaimer.calls)
	}
}
