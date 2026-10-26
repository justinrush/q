package daemon

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/memory"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/terminal"
)

type memoryProbe struct{ dead bool }

func (p *memoryProbe) HasSession(context.Context, string) bool { return !p.dead }
func (p *memoryProbe) ListPanes(context.Context, terminal.Target) ([]terminal.PaneInfo, error) {
	return []terminal.PaneInfo{{ID: "%1", Command: "claude", Dead: p.dead}}, nil
}
func (p *memoryProbe) CapturePane(context.Context, terminal.Target, int) (string, error) {
	return "", nil
}

type memoryMessenger struct {
	recordingMessenger
	calls  int
	text   string
	err    error
	onSend func(mission.Mission, string) error
}

func (m *memoryMessenger) SendMessage(_ context.Context, ms mission.Mission, text string) error {
	m.calls++
	m.text = text
	if m.onSend != nil {
		return m.onSend(ms, text)
	}
	return m.err
}
func (m *memoryMessenger) Relaunch(context.Context, mission.Operation, mission.Mission, string) (mission.Mission, error) {
	return mission.Mission{}, errors.New("must not relaunch for memory")
}

type memoryReclaimer struct {
	fakeReclaimer
	plan mission.Plan
}

func (r *memoryReclaimer) PlanReclaim(context.Context, mission.Operation, mission.Mission) (mission.Plan, error) {
	return r.plan, nil
}

func enableMemory(t *testing.T, s *Service) {
	t.Helper()
	mem, err := memory.Open(filepath.Join(s.dirs.Data, "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	s.apply(WithMemory(mem, true))
}
func captureFixture(t *testing.T) (*Service, mission.Mission, *memoryMessenger, *memoryReclaimer) {
	t.Helper()
	s := newTestService(t)
	enableMemory(t, s)
	ms := launchedServiceMission(t, s)
	if err := s.store.Mutate("test.ready", func(snap *mission.Snapshot) error {
		m, _ := snap.Mission(ms.ID)
		m.Status = mission.StatusDebrief
		m.AgentState = mission.AgentIdle
		m.TmuxSession = "q-test"
		m.AgentPaneID = "%1"
		m.HookEpoch = 1
		snap.PutMission(m)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	messenger, reclaimer := &memoryMessenger{}, &memoryReclaimer{}
	s.apply(WithMessenger(messenger), WithProbe(&memoryProbe{}), WithReclaimer(reclaimer))
	ms, _ = s.Snapshot().Mission(ms.ID)
	return s, ms, messenger, reclaimer
}
func requestCapture(t *testing.T, s *Service, id mission.MissionID) mission.Mission {
	t.Helper()
	ms, err := s.Dispatch(t.Context(), id, api.SetStatusRequest{To: mission.StatusClosed})
	if err != nil {
		t.Fatal(err)
	}
	return ms
}
func saveCapture(t *testing.T, s *Service, id mission.MissionID) []memory.Record {
	t.Helper()
	ms, _ := s.Snapshot().Mission(id)
	rows, err := s.SaveMemory(t.Context(), api.SaveMemoryRequest{Mission: id, Capture: ms.Finish.Token, Records: []memory.Input{{Kind: "summary", Title: "Implemented sync", Body: "Validated behavior; not yet published"}}})
	if err != nil {
		t.Fatal(err)
	}
	return rows
}
func idleCapture(t *testing.T, s *Service, id mission.MissionID) {
	t.Helper()
	s.applyReduction(id, mission.HookEvent{Event: mission.EventStop, LastAssistantMessage: "Shared memory saved."})
}

func TestCaptureAcknowledgmentThenIdleBeforeReclaim(t *testing.T) {
	s, ms, msg, reclaim := captureFixture(t)
	requested := requestCapture(t, s, ms.ID)
	if requested.Status == mission.StatusClosed || requested.Finish.Token == "" || reclaim.calls != 0 {
		t.Fatal("finished before capture")
	}
	duplicate := requestCapture(t, s, ms.ID)
	if duplicate.Finish.Token != requested.Finish.Token {
		t.Fatal("duplicate finish restarted capture")
	}
	s.advanceCapture(t.Context(), ms.ID)
	if msg.calls != 1 || !strings.Contains(msg.text, "--capture "+requested.Finish.Token) {
		t.Fatal("missing capture prompt")
	}
	idleCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	if reclaim.calls != 0 {
		t.Fatal("ordinary idle acknowledged memory")
	}
	rows := saveCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	if reclaim.calls != 0 {
		t.Fatal("reclaimed during save command")
	}
	idleCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	final, _ := s.Snapshot().Mission(ms.ID)
	if final.Status != mission.StatusClosed || reclaim.calls != 1 || reclaim.force {
		t.Fatalf("finish: %+v, calls %d", final.Finish, reclaim.calls)
	}
	if _, err := s.memory.Get(rows[0].ID); err != nil {
		t.Fatal("memory lost on cleanup", err)
	}
	if err := s.DeleteMission(ms.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.memory.Get(rows[0].ID); err != nil {
		t.Fatal("memory lost on source deletion", err)
	}
}

func TestCaptureFailureAndExplicitSkip(t *testing.T) {
	for _, scenario := range []string{"busy", "dead", "planning", "delivery", "timeout"} {
		t.Run(scenario, func(t *testing.T) {
			s, ms, msg, reclaim := captureFixture(t)
			requestCapture(t, s, ms.ID)
			switch scenario {
			case "busy":
				_ = s.changeFinish(ms.ID, func(m *mission.Mission) { m.AgentState = mission.AgentBusy })
			case "dead":
				s.probe = &memoryProbe{dead: true}
			case "planning":
				_ = s.store.Mutate("test.plan", func(snap *mission.Snapshot) error {
					m, _ := snap.Mission(ms.ID)
					m.PlanMode = true
					snap.PutMission(m)
					return nil
				})
			case "delivery":
				msg.err = errors.New("lost connection")
			case "timeout":
				s.now = func() time.Time { return time.Now().Add(6 * time.Minute) }
			}
			s.advanceCapture(t.Context(), ms.ID)
			if reclaim.calls != 0 {
				t.Fatal("failed capture reclaimed work")
			}
			current, _ := s.Snapshot().Mission(ms.ID)
			if scenario == "busy" {
				if msg.calls != 0 {
					t.Fatal("interrupted busy agent")
				}
			} else if current.Finish.Error == "" {
				t.Fatal("failure invisible")
			}
			done, err := s.Dispatch(t.Context(), ms.ID, api.SetStatusRequest{To: mission.StatusClosed, SkipMemory: true})
			if err != nil || done.Status != mission.StatusClosed {
				t.Fatal("cannot skip", err)
			}
		})
	}
}

func TestCaptureCancelRetryAndStaleTokens(t *testing.T) {
	s, ms, _, reclaim := captureFixture(t)
	first := requestCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	_, err := s.Dispatch(t.Context(), ms.ID, api.SetStatusRequest{To: mission.StatusClosed, CancelFinish: true})
	if err != nil {
		t.Fatal(err)
	}
	_, err = s.SaveMemory(t.Context(), api.SaveMemoryRequest{Mission: ms.ID, Capture: first.Finish.Token})
	if !errors.Is(err, ErrConflict) {
		t.Fatal("accepted canceled capture", err)
	}
	requestCapture(t, s, ms.ID)
	retried, err := s.Dispatch(t.Context(), ms.ID, api.SetStatusRequest{To: mission.StatusClosed, RetryCapture: true})
	if err != nil {
		t.Fatal(err)
	}
	if retried.Finish.Token == first.Finish.Token {
		t.Fatal("reused canceled token")
	}
	s.applyReduction(ms.ID, mission.HookEvent{Event: mission.EventUserPromptSubmit, Prompt: "Continue implementing"})
	current, _ := s.Snapshot().Mission(ms.ID)
	if current.Finish.Token != "" || reclaim.calls != 0 {
		t.Fatal("ordinary prompt did not cancel finish")
	}
}

func TestCaptureFreshCleanupConfirmation(t *testing.T) {
	s, ms, _, reclaim := captureFixture(t)
	reclaim.plan.NeedsForce = true
	_, err := s.Dispatch(t.Context(), ms.ID, api.SetStatusRequest{To: mission.StatusClosed, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	s.advanceCapture(t.Context(), ms.ID)
	saveCapture(t, s, ms.ID)
	idleCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	current, _ := s.Snapshot().Mission(ms.ID)
	if reclaim.calls != 0 || current.Finish.Phase != mission.FinishReady {
		t.Fatal("old discard permission reused")
	}
	_, err = s.Dispatch(t.Context(), ms.ID, api.SetStatusRequest{To: mission.StatusClosed, Force: true})
	if err != nil {
		t.Fatal(err)
	}
	if reclaim.calls != 1 || !reclaim.force {
		t.Fatal("fresh confirmation not honored")
	}
}

func TestCaptureFinishesWhenBranchesAreKept(t *testing.T) {
	s, ms, _, reclaim := captureFixture(t)
	reclaim.plan.KeptBranches = []string{"feature"}
	requestCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	saveCapture(t, s, ms.ID)
	idleCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	current, _ := s.Snapshot().Mission(ms.ID)
	if current.Status != mission.StatusClosed || reclaim.calls != 1 || reclaim.force {
		t.Fatalf("finish with retained branch: status %s, reclaim calls %d, force %t", current.Status, reclaim.calls, reclaim.force)
	}
}

func TestCaptureRecoversSaveAndRejectsLeaseChange(t *testing.T) {
	s, ms, _, reclaim := captureFixture(t)
	requestCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	current, _ := s.Snapshot().Mission(ms.ID)
	_, err := s.memory.Save([]memory.Input{{Kind: "summary", Title: "saved", Body: "durable"}}, memory.Source{}, current.Finish.Token, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	s.advanceCapture(t.Context(), ms.ID)
	recovered, _ := s.Snapshot().Mission(ms.ID)
	if recovered.Finish.AcknowledgedAt.IsZero() || reclaim.calls != 0 {
		t.Fatal("save recovery failed")
	}
	if err = s.store.Mutate("lease change", func(snap *mission.Snapshot) error {
		m, _ := snap.Mission(ms.ID)
		m.Lease.Epoch++
		snap.PutMission(m)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	s.advanceCapture(t.Context(), ms.ID)
	final, _ := s.Snapshot().Mission(ms.ID)
	if final.Finish.Token != "" || reclaim.calls != 0 {
		t.Fatal("stale lease continued capture")
	}
}

type memoryWire struct{ *wire }

func (w *memoryWire) MemorySync(_ context.Context, req api.MemorySyncRequest) (api.MemorySyncResponse, error) {
	var sent api.MemorySyncRequest
	if err := roundTrip(req, &sent); err != nil {
		return api.MemorySyncResponse{}, err
	}
	response, err := w.peer.MemoryExchange(sent)
	if err != nil {
		return response, err
	}
	var received api.MemorySyncResponse
	return received, roundTrip(response, &received)
}
func TestMemoryPairedSyncAndDeletion(t *testing.T) {
	a, b := newTestService(t), newTestService(t)
	enableMemory(t, a)
	enableMemory(t, b)
	w := &memoryWire{&wire{peer: b}}
	a.apply(WithRemote(w))
	inputs := make([]memory.Input, memory.PageSize+2)
	for i := range inputs {
		inputs[i] = memory.Input{Kind: "lesson", Title: "portable", Body: "evidence"}
	}
	records, err := a.memory.Save(inputs, memory.Source{}, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = a.SyncNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	for _, r := range records {
		if _, err = b.memory.Get(r.ID); err != nil {
			t.Fatal("missing synced memory", err)
		}
	}
	if a.RemoteStatus().MemorySync != "available" {
		t.Fatal(a.RemoteStatus().MemorySync)
	}
	_, err = b.memory.Update(records[0].ID, memory.Input{}, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	_, err = a.memory.Update(records[0].ID, memory.Input{Kind: "lesson", Title: "offline edit", Body: "body"}, false, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if err = a.SyncNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if _, err = a.memory.Get(records[0].ID); !errors.Is(err, memory.ErrNotFound) {
		t.Fatal("deleted memory resurrected")
	}
	// An older peer still exchanges the mission board, without memory support.
	b.memory = nil
	if err = a.SyncNow(t.Context()); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(a.RemoteStatus().MemorySync, "unavailable") {
		t.Fatal("missing compatibility status")
	}
}

func TestCaptureRetryReusesSavedBatch(t *testing.T) {
	s, ms, _, reclaim := captureFixture(t)
	original := requestCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	first := saveCapture(t, s, ms.ID)
	if err := s.failCapture(ms.ID, "interrupted after save"); err != nil {
		t.Fatal(err)
	}
	retried, err := s.Dispatch(t.Context(), ms.ID, api.SetStatusRequest{To: mission.StatusClosed, RetryCapture: true})
	if err != nil || retried.Finish.Token != original.Finish.Token {
		t.Fatal("retry lost idempotency token", err)
	}
	s.advanceCapture(t.Context(), ms.ID)
	if len(s.memory.Search(memory.Query{})) != len(first) || reclaim.calls != 0 {
		t.Fatal("retry duplicated memory or reclaimed before idle")
	}
	idleCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	if reclaim.calls != 1 {
		t.Fatal("saved retry did not finish")
	}
}

func TestCaptureRequiresFreshRuntimeIdleObservation(t *testing.T) {
	s, ms, _, reclaim := captureFixture(t)
	requestCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	saveCapture(t, s, ms.ID)
	current, _ := s.Snapshot().Mission(ms.ID)
	s.applyRuntimeReadingNow(ms.ID, mission.Reading{Activity: mission.ActivityIdle, ObservedAt: current.Finish.AcknowledgedAt.Add(-time.Second), HookEpoch: ms.HookEpoch})
	s.advanceCapture(t.Context(), ms.ID)
	if reclaim.calls != 0 {
		t.Fatal("stale idle observation authorized cleanup")
	}
	s.applyRuntimeReadingNow(ms.ID, mission.Reading{Activity: mission.ActivityIdle, ObservedAt: current.Finish.AcknowledgedAt.Add(time.Second), HookEpoch: ms.HookEpoch})
	s.advanceCapture(t.Context(), ms.ID)
	if reclaim.calls != 1 {
		t.Fatal("fresh idle observation did not authorize cleanup")
	}
}

func TestCaptureRunAndOrdinaryResumeInvalidateToken(t *testing.T) {
	s, ms, _, reclaim := captureFixture(t)
	original := requestCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	if err := s.store.Mutate("test.new-run", func(snap *mission.Snapshot) error {
		m, _ := snap.Mission(ms.ID)
		m.HookEpoch++
		snap.PutMission(m)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_, err := s.SaveMemory(t.Context(), api.SaveMemoryRequest{Mission: ms.ID, Capture: original.Finish.Token})
	if !errors.Is(err, ErrConflict) {
		t.Fatal("old run could acknowledge capture", err)
	}
	s.advanceCapture(t.Context(), ms.ID)
	requestCapture(t, s, ms.ID)
	_, err = s.Resume(t.Context(), ms.ID, "Continue the work")
	if err != nil {
		t.Fatal(err)
	}
	current, _ := s.Snapshot().Mission(ms.ID)
	if current.Finish.Token != "" || reclaim.calls != 0 {
		t.Fatal("resume left automatic cleanup pending")
	}
}

func TestCaptureAcceptsProviderWithoutPromptText(t *testing.T) {
	s, ms, _, reclaim := captureFixture(t)
	requestCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	s.applyReduction(ms.ID, mission.HookEvent{Event: mission.EventUserPromptSubmit})
	current, _ := s.Snapshot().Mission(ms.ID)
	if current.Finish.Token == "" || !current.Finish.PromptSeen {
		t.Fatal("provider's empty capture prompt canceled finish")
	}
	saveCapture(t, s, ms.ID)
	idleCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	if reclaim.calls != 1 {
		t.Fatal("provider without prompt text could not finish")
	}
}

func TestCaptureAnotherEmptyPromptCancelsCleanup(t *testing.T) {
	s, ms, _, reclaim := captureFixture(t)
	requestCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	s.applyReduction(ms.ID, mission.HookEvent{Event: mission.EventUserPromptSubmit})
	s.applyReduction(ms.ID, mission.HookEvent{Event: mission.EventUserPromptSubmit})
	current, _ := s.Snapshot().Mission(ms.ID)
	if current.Finish.Token != "" || reclaim.calls != 0 {
		t.Fatal("second empty prompt left automatic cleanup pending")
	}
}

func TestCaptureSaveDuringDeliverySurvivesLostResponse(t *testing.T) {
	s, ms, msg, reclaim := captureFixture(t)
	requestCapture(t, s, ms.ID)
	msg.onSend = func(m mission.Mission, _ string) error {
		saveCapture(t, s, m.ID)
		return errors.New("delivery response lost after save")
	}
	s.advanceCapture(t.Context(), ms.ID)
	current, _ := s.Snapshot().Mission(ms.ID)
	if current.Finish.Phase != mission.FinishAwaitingIdle || current.Finish.Error != "" {
		t.Fatal("successful acknowledgment lost to delivery error")
	}
	idleCapture(t, s, ms.ID)
	s.advanceCapture(t.Context(), ms.ID)
	if reclaim.calls != 1 {
		t.Fatal("capture acknowledged during delivery did not finish")
	}
}

func TestCaptureRestartDoesNotResendUncertainDelivery(t *testing.T) {
	s, ms, msg, reclaim := captureFixture(t)
	requestCapture(t, s, ms.ID)
	if err := s.changeFinish(ms.ID, func(m *mission.Mission) { m.Finish.Phase = mission.FinishSending }); err != nil {
		t.Fatal(err)
	}
	reopened, err := mission.Open(s.dirs)
	if err != nil {
		t.Fatal(err)
	}
	s.store = reopened
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	s.RunFinishCaptures(ctx)
	current, _ := s.Snapshot().Mission(ms.ID)
	if current.Finish.Phase != mission.FinishFailed || msg.calls != 0 || reclaim.calls != 0 {
		t.Fatal("restart guessed delivery outcome")
	}
}
