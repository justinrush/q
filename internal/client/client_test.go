package client

import (
	"context"
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/daemon"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/paths"
	"io"
)

// startDaemon runs a real daemon against a temp directory and returns a
// connected client. This exercises the handle file, the token, and the auth
// guard together, which is where the pieces are most likely to disagree.
func startDaemon(t *testing.T) (*Client, paths.Dirs) {
	t.Helper()

	root := t.TempDir()
	dirs := paths.Dirs{
		Data:   filepath.Join(root, "data"),
		State:  filepath.Join(root, "state"),
		Config: filepath.Join(root, "config"),
	}

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	errCh := make(chan error, 1)

	go func() {
		errCh <- daemon.Run(ctx, daemon.RunConfig{
			Dirs:    dirs,
			Version: "test",
			Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
			Ready:   ready,
		})
	}()

	select {
	case <-ready:
	case err := <-errCh:
		cancel()
		t.Fatalf("daemon exited before becoming ready: %v", err)
	case <-time.After(5 * time.Second):
		cancel()
		t.Fatal("daemon did not become ready")
	}

	t.Cleanup(func() {
		cancel()

		select {
		case <-errCh:
		case <-time.After(5 * time.Second):
			t.Error("daemon did not shut down")
		}
	})

	c, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return c, dirs
}

func TestClientRoundTrip(t *testing.T) {
	c, _ := startDaemon(t)
	ctx := t.Context()

	health, err := c.Health(ctx)
	if err != nil {
		t.Fatalf("Health: %v", err)
	}

	if health.Version != "test" {
		t.Errorf("Version = %q", health.Version)
	}

	operation, err := c.CreateOperation(ctx, api.CreateOperationRequest{
		Name:    "Discussions API",
		Summary: "wire discussions through",
		Repos:   []domain.Repo{{Name: "weave", Path: "/dev/weave"}},
	})
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	mission, err := c.CreateMission(ctx, api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "add endpoint",
		Prompt:      "do the thing",
		Tool:        domain.ToolClaude,
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	snap, err := c.State(ctx)
	if err != nil {
		t.Fatalf("State: %v", err)
	}

	if len(snap.Operations) != 1 || len(snap.Missions) != 1 {
		t.Fatalf("snapshot has %d operations and %d missions", len(snap.Operations), len(snap.Missions))
	}

	moved, err := c.SetStatus(ctx, mission.ID, api.SetStatusRequest{To: domain.StatusDebrief})
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	if moved.Status != domain.StatusDebrief {
		t.Errorf("Status = %q", moved.Status)
	}

	if _, err := c.DeleteMission(ctx, mission.ID, false); err != nil {
		t.Fatalf("DeleteMission: %v", err)
	}

	if err := c.DeleteOperation(ctx, operation.ID, false); err != nil {
		t.Fatalf("DeleteOperation: %v", err)
	}

	operations, err := c.Operations(ctx)
	if err != nil {
		t.Fatalf("Operations: %v", err)
	}

	if len(operations) != 0 {
		t.Errorf("expected no operations, got %d", len(operations))
	}
}

func TestClientReportsNotFound(t *testing.T) {
	c, _ := startDaemon(t)

	_, err := c.DeleteMission(t.Context(), "ms_000000000000", false)
	if err == nil {
		t.Fatal("expected an error")
	}

	if !IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false, want true", err)
	}
}

func TestClientSurfacesServiceErrorMessage(t *testing.T) {
	c, _ := startDaemon(t)

	_, err := c.CreateOperation(t.Context(), api.CreateOperationRequest{})
	if err == nil {
		t.Fatal("expected an error")
	}

	statusErr, ok := errors.AsType[*StatusError](err)
	if !ok {
		t.Fatalf("err type = %T, want *StatusError", err)
	}

	if statusErr.Code != 400 {
		t.Errorf("Code = %d, want 400", statusErr.Code)
	}

	// The daemon's own explanation must survive the trip, or CLI errors become
	// bare status codes.
	if statusErr.Message == "" {
		t.Error("Message should carry the daemon's explanation")
	}
}

// A reconnecting board resynchronizes from the snapshot frame rather than asking.
func TestStreamDeliversSnapshotThenChanges(t *testing.T) {
	c, _ := startDaemon(t)

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()

	events := make(chan Event, 16)
	streamErr := make(chan error, 1)

	go func() { streamErr <- c.Stream(ctx, events) }()

	first := waitForEvent(t, events, daemon.EventSnapshot)

	var snap struct {
		Operations []domain.Operation `json:"operations"`
	}

	if err := first.Decode(&snap); err != nil {
		t.Fatalf("decoding snapshot: %v", err)
	}

	if _, err := c.CreateOperation(ctx, api.CreateOperationRequest{Name: "Live"}); err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	operationEvent := waitForEvent(t, events, daemon.EventOperation)

	var operation domain.Operation
	if err := operationEvent.Decode(&operation); err != nil {
		t.Fatalf("decoding operation: %v", err)
	}

	if operation.Name != "Live" {
		t.Errorf("Name = %q, want %q", operation.Name, "Live")
	}

	cancel()

	select {
	case <-streamErr:
	case <-time.After(5 * time.Second):
		t.Error("Stream did not return after cancellation")
	}
}

// waitForEvent reads until an event of the given name arrives, ignoring
// heartbeats.
func waitForEvent(t *testing.T, events <-chan Event, name string) Event {
	t.Helper()

	deadline := time.After(5 * time.Second)

	for {
		select {
		case ev := <-events:
			if ev.Name == name {
				return ev
			}
		case <-deadline:
			t.Fatalf("timed out waiting for a %q event", name)
		}
	}
}

func TestConnectWithoutDaemonReportsErrNoDaemon(t *testing.T) {
	root := t.TempDir()
	dirs := paths.Dirs{Data: filepath.Join(root, "data"), State: filepath.Join(root, "state")}

	_, err := Connect(t.Context(), dirs)
	if !errors.Is(err, daemon.ErrNoDaemon) {
		t.Errorf("err = %v, want daemon.ErrNoDaemon", err)
	}
}

// A handle file outlives a daemon killed with SIGKILL, so reachability must be
// decided by an actual health check rather than by the file existing.
func TestConnectRejectsStaleHandle(t *testing.T) {
	root := t.TempDir()
	dirs := paths.Dirs{Data: filepath.Join(root, "data"), State: filepath.Join(root, "state")}

	if err := dirs.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	// 127.0.0.1:1 is reserved and refuses connections.
	stale := daemon.Handle{PID: 999999, Addr: "127.0.0.1:1", Token: "tok", Version: "test"}
	if err := daemon.WriteHandle(dirs.DaemonFile(), stale); err != nil {
		t.Fatalf("WriteHandle: %v", err)
	}

	if _, err := Connect(t.Context(), dirs); err == nil {
		t.Fatal("expected Connect to reject an unreachable daemon")
	}
}

func TestStopWithoutDaemonIsNotAnError(t *testing.T) {
	root := t.TempDir()
	dirs := paths.Dirs{Data: filepath.Join(root, "data"), State: filepath.Join(root, "state")}

	if err := Stop(dirs); err != nil {
		t.Errorf("Stop: %v", err)
	}
}

// A second daemon on the same directories must lose the instance lock rather
// than racing the first for the state file.
func TestSecondDaemonRefusesToStart(t *testing.T) {
	_, dirs := startDaemon(t)

	err := daemon.Run(t.Context(), daemon.RunConfig{
		Dirs:    dirs,
		Version: "test",
		Logger:  slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if !errors.Is(err, daemon.ErrAlreadyRunning) {
		t.Errorf("err = %v, want daemon.ErrAlreadyRunning", err)
	}
}
