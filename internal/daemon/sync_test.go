package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/remote"
)

// wire carries requests from one in-process service to another, through JSON,
// as the ssh relay would. Encoding matters: it is what proves nothing depends
// on a value that does not survive the trip, such as a monotonic clock reading.
type wire struct {
	peer *Service
	// down makes every call fail as an unreachable peer does.
	down bool
	// statusCalls records lane moves asked of the peer.
	statusCalls []api.SetStatusRequest
}

func (w *wire) Sync(ctx context.Context, req api.SyncRequest) (api.SyncResponse, error) {
	if w.down {
		return api.SyncResponse{}, fmt.Errorf("%w: connection timed out", remote.ErrUnreachable)
	}

	var sent api.SyncRequest
	if err := roundTrip(req, &sent); err != nil {
		return api.SyncResponse{}, err
	}

	resp, err := w.peer.SyncExchange(ctx, sent)
	if err != nil {
		return api.SyncResponse{}, asStatus(err)
	}

	var received api.SyncResponse

	return received, roundTrip(resp, &received)
}

func (w *wire) SetStatus(
	ctx context.Context,
	id mission.MissionID,
	req api.SetStatusRequest,
) (mission.Mission, error) {
	if w.down {
		return mission.Mission{}, fmt.Errorf("%w: connection timed out", remote.ErrUnreachable)
	}

	w.statusCalls = append(w.statusCalls, req)

	ms, err := w.peer.Dispatch(ctx, id, req)
	if err != nil {
		return mission.Mission{}, asStatus(err)
	}

	return ms, nil
}

func (w *wire) Settle(ctx context.Context, req api.SettleRequest) (api.SettleResponse, error) {
	if w.down {
		return api.SettleResponse{}, fmt.Errorf("%w: connection timed out", remote.ErrUnreachable)
	}

	var sent api.SettleRequest
	if err := roundTrip(req, &sent); err != nil {
		return api.SettleResponse{}, err
	}

	resp, err := w.peer.SettleWorktrees(ctx, sent)
	if err != nil {
		return api.SettleResponse{}, asStatus(err)
	}

	var received api.SettleResponse

	return received, roundTrip(resp, &received)
}

// GitURL addresses the peer's repository by its path. Both services are on one
// machine, and a path is a remote git already understands.
func (w *wire) GitURL(path string) string { return path }

func (w *wire) Release(ctx context.Context, id mission.MissionID) error {
	if w.down {
		return fmt.Errorf("%w: connection timed out", remote.ErrUnreachable)
	}

	if _, err := w.peer.Release(ctx, id); err != nil {
		return asStatus(err)
	}

	return nil
}

func (w *wire) Forget(ctx context.Context) error {
	if w.down {
		return fmt.Errorf("%w: connection timed out", remote.ErrUnreachable)
	}

	_, err := w.peer.ForgetPeer(ctx)

	return err
}

func (w *wire) AttachArgv(id mission.MissionID) []string {
	return []string{"/usr/bin/ssh", "-t", "mini", "q", "attach", string(id)}
}

// roundTrip copies a value through JSON.
func roundTrip(from, to any) error {
	data, err := json.Marshal(from)
	if err != nil {
		return err
	}

	return json.Unmarshal(data, to)
}

// asStatus turns a service error into what the peer's HTTP layer would send.
func asStatus(err error) error {
	code := 500

	switch {
	case errors.Is(err, ErrNotFound):
		code = 404
	case errors.Is(err, ErrInvalid):
		code = 400
	case errors.Is(err, ErrConflict):
		code = 409
	}

	return &api.StatusError{Code: code, Message: err.Error()}
}

// pair returns a primary and a secondary, joined by a wire and each able to
// launch.
func pairedServices(t *testing.T) (primary, secondary *Service, link *wire) {
	t.Helper()

	primary, secondary = newTestService(t), newTestService(t)
	primary.apply(WithLauncher(&recordingLauncher{fail: map[string]error{}}), WithHostName("laptop"))
	secondary.apply(WithLauncher(&recordingLauncher{fail: map[string]error{}}), WithHostName("mini"))
	primary.adoptHostName()
	secondary.adoptHostName()

	link = &wire{peer: secondary}
	primary.apply(WithRemote(link))

	return primary, secondary, link
}

// sync runs one exchange and fails the test if it does not complete.
func exchange(t *testing.T, primary *Service) {
	t.Helper()

	if err := primary.SyncNow(t.Context()); err != nil {
		t.Fatalf("SyncNow: %v", err)
	}
}

// briefOn creates an operation and one briefed mission on svc.
func briefOn(t *testing.T, svc *Service, name string) mission.Mission {
	t.Helper()

	operation := seedOperation(t, svc)

	ms, err := svc.CreateMission(api.CreateMissionRequest{OperationID: operation.ID, Name: name, Prompt: "do it"})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	return ms
}

func TestFirstExchangePairsBothSides(t *testing.T) {
	primary, secondary, _ := pairedServices(t)

	exchange(t, primary)

	p, s := primary.RemoteStatus(), secondary.RemoteStatus()

	if p.Role != mission.RolePrimary || p.Peer == nil || p.Peer.Name != "mini" {
		t.Errorf("primary status = %+v, want it paired with mini as primary", p)
	}

	if s.Role != mission.RoleSecondary || s.Peer == nil || s.Peer.Name != "laptop" {
		t.Errorf("secondary status = %+v, want it paired with laptop as secondary", s)
	}

	if !p.Linked() || !s.Linked() {
		t.Errorf("linked: primary %v secondary %v, want both", p.Linked(), s.Linked())
	}
}

func TestMissionsAndOperationsReachThePeerInBothDirections(t *testing.T) {
	primary, secondary, _ := pairedServices(t)

	fromLaptop := briefOn(t, primary, "written on the laptop")
	fromMini := briefOn(t, secondary, "written on the mini")

	exchange(t, primary)

	for name, svc := range map[string]*Service{"primary": primary, "secondary": secondary} {
		snap := svc.Snapshot()

		if len(snap.Missions) != 2 || len(snap.Operations) != 2 {
			t.Errorf("%s has %d missions and %d operations, want 2 of each",
				name, len(snap.Missions), len(snap.Operations))
		}
	}

	if got, ok := secondary.Snapshot().Mission(fromLaptop.ID); !ok || got.Lease.Holder != primary.self {
		t.Errorf("the laptop's mission on the mini = %+v, want it present and still the laptop's", got.Lease)
	}

	if got, ok := primary.Snapshot().Mission(fromMini.ID); !ok || got.Lease.Holder != secondary.self {
		t.Errorf("the mini's mission on the laptop = %+v, want it present and still the mini's", got.Lease)
	}
}

// An exchange that finds the two sides agreeing must write nothing. It happens
// every few seconds for as long as both daemons run.
func TestAnIdleExchangeDoesNotRewriteState(t *testing.T) {
	primary, secondary, _ := pairedServices(t)
	briefOn(t, primary, "settled")

	exchange(t, primary)
	exchange(t, primary)

	before := []time.Time{primary.Snapshot().UpdatedAt, secondary.Snapshot().UpdatedAt}

	exchange(t, primary)

	after := []time.Time{primary.Snapshot().UpdatedAt, secondary.Snapshot().UpdatedAt}

	if !before[0].Equal(after[0]) || !before[1].Equal(after[1]) {
		t.Errorf("state was rewritten by an exchange with nothing to say: %v -> %v", before, after)
	}
}

func TestAnEditOnEitherSideReachesTheOther(t *testing.T) {
	primary, secondary, _ := pairedServices(t)
	ms := briefOn(t, primary, "draft")

	exchange(t, primary)

	prompt := "rewritten on the mini"
	if _, err := secondary.UpdateMission(ms.ID, api.UpdateMissionRequest{Prompt: &prompt}); err != nil {
		t.Fatalf("editing on the secondary: %v", err)
	}

	exchange(t, primary)

	if got, _ := primary.Snapshot().Mission(ms.ID); got.Prompt != prompt {
		t.Errorf("prompt on the primary = %q, want the secondary's edit", got.Prompt)
	}
}

func TestADeleteReachesThePeerAndStaysDeleted(t *testing.T) {
	primary, secondary, _ := pairedServices(t)
	ms := briefOn(t, primary, "doomed")

	exchange(t, primary)

	if err := secondary.DeleteMission(ms.ID); err != nil {
		t.Fatalf("deleting on the secondary: %v", err)
	}

	exchange(t, primary)
	exchange(t, primary)

	for name, svc := range map[string]*Service{"primary": primary, "secondary": secondary} {
		if _, ok := svc.Snapshot().Mission(ms.ID); ok {
			t.Errorf("the deleted mission is still on the %s", name)
		}
	}
}

// A machine already paired with one laptop must not be taken over by a second.
func TestASecondPrimaryIsRefused(t *testing.T) {
	primary, secondary, _ := pairedServices(t)
	exchange(t, primary)

	intruder := newTestService(t)
	intruder.apply(WithRemote(&wire{peer: secondary}), WithHostName("other-laptop"))
	intruder.adoptHostName()

	err := intruder.SyncNow(t.Context())
	if err == nil || !strings.Contains(err.Error(), "already paired with laptop") {
		t.Fatalf("err = %v, want a refusal naming the existing pairing", err)
	}

	if peer := secondary.RemoteStatus().Peer; peer == nil || peer.Name != "laptop" {
		t.Errorf("the secondary's peer = %+v, want it unchanged", peer)
	}
}

func TestAProtocolMismatchIsRefusedWithBothVersions(t *testing.T) {
	_, secondary, _ := pairedServices(t)

	_, err := secondary.SyncExchange(t.Context(), api.SyncRequest{Protocol: api.SyncProtocol + 1, Version: "9.9.9"})
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "9.9.9") {
		t.Errorf("err = %v, want a conflict naming the other side's version", err)
	}
}

// runOnSecondary briefs a mission on the secondary and starts it there.
func runOnSecondary(t *testing.T, primary, secondary *Service) mission.Mission {
	t.Helper()

	ms := briefOn(t, secondary, "runs on the mini")

	if _, err := secondary.Start(t.Context(), ms.ID); err != nil {
		t.Fatalf("starting on the secondary: %v", err)
	}

	exchange(t, primary)

	return ms
}

// The primary's copy of a mission the secondary runs must follow it, and must
// not carry anything that is only true on the secondary.
func TestTheHoldersRunIsMirroredWithoutItsLocalState(t *testing.T) {
	primary, secondary, _ := pairedServices(t)
	ms := runOnSecondary(t, primary, secondary)

	mirrored, _ := primary.Snapshot().Mission(ms.ID)

	if mirrored.Status != mission.StatusActive || !mirrored.Launched() {
		t.Errorf("mirror is %s launched=%v, want it active", mirrored.Status, mirrored.Launched())
	}

	if mirrored.MissionDir != "" || mirrored.TmuxSession != "" {
		t.Errorf("mirror has dir %q session %q, want neither: they are the mini's", mirrored.MissionDir, mirrored.TmuxSession)
	}

	if mirrored.HolderSession == "" {
		t.Error("mirror has no holder session, so there would be nothing to attach to")
	}
}

func TestMovingAMissionThePeerRunsIsCarriedOutThere(t *testing.T) {
	primary, secondary, link := pairedServices(t)
	ms := runOnSecondary(t, primary, secondary)

	if _, err := primary.Dispatch(t.Context(), ms.ID, api.SetStatusRequest{To: mission.StatusDebrief}); err != nil {
		t.Fatalf("Dispatch: %v", err)
	}

	if len(link.statusCalls) != 1 || link.statusCalls[0].To != mission.StatusDebrief {
		t.Fatalf("peer was asked %+v, want one move to debrief", link.statusCalls)
	}

	if got, _ := secondary.Snapshot().Mission(ms.ID); got.Status != mission.StatusDebrief {
		t.Errorf("status on the holder = %s, want debrief", got.Status)
	}
}

// With the peer unreachable, a request to resume waits for the next exchange.
// A request to finish does not: it destroys worktrees.
func TestAnUnreachablePeerQueuesAResumeAndRefusesAFinish(t *testing.T) {
	primary, secondary, link := pairedServices(t)
	ms := runOnSecondary(t, primary, secondary)

	if _, err := secondary.SetStatus(ms.ID, mission.StatusDebrief); err != nil {
		t.Fatal(err)
	}

	exchange(t, primary)

	link.down = true

	_, err := primary.Dispatch(t.Context(), ms.ID, api.SetStatusRequest{To: mission.StatusClosed})
	if !errors.Is(err, ErrConflict) {
		t.Errorf("finishing while unreachable: err = %v, want a refusal", err)
	}

	queued, err := primary.Dispatch(t.Context(), ms.ID, api.SetStatusRequest{To: mission.StatusActive, Message: "carry on"})
	if err != nil {
		t.Fatalf("resuming while unreachable: %v", err)
	}

	if !queued.HasLocalBadge(mission.BadgePending) || primary.RemoteStatus().Pending != 1 {
		t.Errorf("badges %v pending %d, want the request held and the card marked",
			queued.LocalBadges, primary.RemoteStatus().Pending)
	}

	// The secondary needs something to resume into.
	secondary.apply(WithMessenger(&recordingMessenger{}))

	link.down = false
	exchange(t, primary)

	if primary.RemoteStatus().Pending != 0 {
		t.Errorf("pending = %d after delivery, want 0", primary.RemoteStatus().Pending)
	}

	if got, _ := primary.Snapshot().Mission(ms.ID); got.HasLocalBadge(mission.BadgePending) {
		t.Error("the card is still marked pending after its request was delivered")
	}

	messenger, _ := secondary.messenger.(*recordingMessenger)
	if !slices.Equal(messenger.relaunched, []string{"carry on"}) {
		t.Errorf("the holder relaunched with %v, want the queued message once", messenger.relaunched)
	}
}

// The secondary cannot reach the primary, so it cannot ask it to do anything.
func TestTheSecondaryRefusesToMoveAMissionThePrimaryRuns(t *testing.T) {
	primary, secondary, _ := pairedServices(t)

	ms := briefOn(t, primary, "runs on the laptop")
	if _, err := primary.Start(t.Context(), ms.ID); err != nil {
		t.Fatal(err)
	}

	exchange(t, primary)

	_, err := secondary.Dispatch(t.Context(), ms.ID, api.SetStatusRequest{To: mission.StatusDebrief})
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "laptop") {
		t.Errorf("err = %v, want a refusal that says where it is running", err)
	}
}

// Forgetting the peer must not leave cards that say "running elsewhere" with
// nobody left to report on them.
func TestForgettingThePeerTakesOverItsMissions(t *testing.T) {
	primary, secondary, _ := pairedServices(t)
	ms := runOnSecondary(t, primary, secondary)

	forgotten, err := primary.ForgetPeer(t.Context())
	if err != nil {
		t.Fatalf("ForgetPeer: %v", err)
	}

	// The other side is told, or it would wait out the takeover window and
	// start running the missions of a primary that has only stopped calling.
	if !forgotten.PeerTold || secondary.RemoteStatus().Peer != nil {
		t.Errorf("peerTold=%v and the secondary still records %+v; want it to have forgotten too",
			forgotten.PeerTold, secondary.RemoteStatus().Peer)
	}

	got, _ := primary.Snapshot().Mission(ms.ID)

	if got.Lease.Holder != primary.self || got.Status != mission.StatusDebrief {
		t.Errorf("lease %+v status %s, want it held here and asking to be looked at", got.Lease, got.Status)
	}

	if primary.RemoteStatus().Peer != nil {
		t.Error("the peer is still recorded")
	}
}

// recordingMessenger stands in for the launcher's ability to talk to sessions.
type recordingMessenger struct {
	relaunched []string
	stopped    []mission.MissionID
	viewed     []mission.MissionID
}

func (m *recordingMessenger) SendMessage(context.Context, mission.Mission, string) error { return nil }

func (m *recordingMessenger) Relaunch(
	_ context.Context,
	_ mission.Operation,
	ms mission.Mission,
	message string,
) (mission.Mission, error) {
	m.relaunched = append(m.relaunched, message)
	ms.TmuxSession = "q-relaunched"

	return ms, nil
}

func (m *recordingMessenger) Stop(_ context.Context, ms mission.Mission) error {
	m.stopped = append(m.stopped, ms.ID)

	return nil
}

func (m *recordingMessenger) View(
	_ context.Context,
	_ mission.Operation,
	ms mission.Mission,
) (mission.Mission, error) {
	m.viewed = append(m.viewed, ms.ID)
	ms.TmuxSession, ms.AgentPaneID = "q-viewer", "%9"

	return ms, nil
}

// Opening a mission the peer runs must show the peer's agent, not start one
// here, and must refuse rather than open something half-built.
func TestOpeningAMissionThePeerRunsStartsAViewer(t *testing.T) {
	primary, secondary, _ := pairedServices(t)
	ms := runOnSecondary(t, primary, secondary)

	messenger := &recordingMessenger{}
	primary.apply(WithMessenger(messenger), WithDebriefer(stubDebriefer{}))

	// Not mirrored yet: there is no code here to open editors on.
	if _, err := primary.OpenDebrief(t.Context(), ms.ID, api.ModePrepare); !errors.Is(err, ErrConflict) {
		t.Fatalf("opening before the mirror exists: err = %v, want a refusal", err)
	}

	primary.updateLocal(ms.ID, "test.mirrored", func(stored *mission.Mission) { stored.MissionDir = "/mirror" })

	result, err := primary.OpenDebrief(t.Context(), ms.ID, api.ModePrepare)
	if err != nil {
		t.Fatalf("OpenDebrief: %v", err)
	}

	if !slices.Equal(messenger.viewed, []mission.MissionID{ms.ID}) {
		t.Fatalf("viewers started for %v, want one for the mission", messenger.viewed)
	}

	// What the viewer runs is asked for when it runs, so it is still right
	// after the mission has moved.
	command, err := primary.AttachCommand(ms.ID)
	if err != nil || !slices.Contains(command.Argv, "attach") || command.Host != "mini" {
		t.Errorf("attach command = %+v, err %v; want one that attaches to the agent on mini", command, err)
	}

	if _, err := secondary.AttachCommand(ms.ID); !errors.Is(err, ErrConflict) {
		t.Errorf("asking the host that runs it: err = %v, want a refusal", err)
	}

	if result.Session != "q-viewer" {
		t.Errorf("opened session %q, want the viewer", result.Session)
	}

	stored, _ := primary.Snapshot().Mission(ms.ID)
	if stored.TmuxSession != "q-viewer" || stored.Lease.Holder != secondary.self {
		t.Errorf("session %q holder %s, want the viewer recorded and the lease untouched",
			stored.TmuxSession, stored.Lease.Holder)
	}
}

// The secondary has no way to reach the primary, so it cannot show its agent.
func TestTheSecondaryCannotOpenAMissionThePrimaryRuns(t *testing.T) {
	primary, secondary, _ := pairedServices(t)

	ms := briefOn(t, primary, "on the laptop")
	if _, err := primary.Start(t.Context(), ms.ID); err != nil {
		t.Fatal(err)
	}

	exchange(t, primary)

	secondary.apply(WithMessenger(&recordingMessenger{}), WithDebriefer(stubDebriefer{}))

	_, err := secondary.OpenDebrief(t.Context(), ms.ID, api.ModePrepare)
	if !errors.Is(err, ErrConflict) || !strings.Contains(err.Error(), "laptop") {
		t.Errorf("err = %v, want a refusal naming where it runs", err)
	}
}

// stubDebriefer opens nothing and reports the session it was asked to open.
type stubDebriefer struct{}

func (stubDebriefer) Open(_ context.Context, ms mission.Mission, _ api.Mode) (api.Result, mission.Mission, error) {
	return api.Result{Session: ms.TmuxSession}, ms, nil
}

func (stubDebriefer) Touched(context.Context, mission.Mission) ([]api.Touched, error) {
	return nil, nil
}

// With the other machine unreachable, forgetting still works here, and says
// plainly that the other side has not been told.
func TestForgettingAnUnreachablePeerSaysItWasNotTold(t *testing.T) {
	primary, secondary, link := pairedServices(t)
	exchange(t, primary)

	link.down = true

	forgotten, err := primary.ForgetPeer(t.Context())
	if err != nil {
		t.Fatalf("ForgetPeer: %v", err)
	}

	if forgotten.PeerTold || forgotten.Peer == nil || forgotten.Peer.Name != "mini" {
		t.Errorf("forgotten = %+v, want it to name mini and say it was not told", forgotten)
	}

	if primary.RemoteStatus().Peer != nil || secondary.RemoteStatus().Peer == nil {
		t.Error("want the pairing gone here and, unavoidably, still recorded there")
	}
}

// A laptop that remembers one machine and is pointed at another must not leave
// the second believing it has been paired. The laptop is about to refuse, and
// a machine holding half a pairing would start taking over missions.
func TestAPrimaryPairedElsewhereLeavesNoTraceOnTheWrongMachine(t *testing.T) {
	primary, _, _ := pairedServices(t)
	briefOn(t, primary, "the laptop's mission")
	exchange(t, primary)

	other := newTestService(t)
	other.apply(WithHostName("vm"))
	other.adoptHostName()

	primary.apply(WithRemote(&wire{peer: other}))

	err := primary.SyncNow(t.Context())
	if err == nil || !strings.Contains(err.Error(), "paired with a different machine") {
		t.Fatalf("err = %v, want the wrong machine to refuse", err)
	}

	if status := other.RemoteStatus(); status.Peer != nil || status.Role != mission.RoleStandalone {
		t.Errorf("the wrong machine recorded a pairing: %+v", status)
	}

	if n := len(other.Snapshot().Missions); n != 0 {
		t.Errorf("the wrong machine took %d of the laptop's missions", n)
	}
}
