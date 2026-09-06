package api_test

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"io"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/daemon"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
)

// startDaemon runs a real daemon against a temp directory and returns a
// connected client. This exercises the handle file, the token, and the auth
// guard together, which is where the pieces are most likely to disagree.
func startDaemon(t *testing.T) (*api.Client, paths.Dirs) {
	t.Helper()

	dirs := newDirs(t)
	runDaemon(t, dirs)

	c, err := api.OpenClient(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	return c, dirs
}

// newDirs returns an empty set of q directories under t.TempDir().
func newDirs(t *testing.T) paths.Dirs {
	t.Helper()

	root := t.TempDir()
	dirs := paths.Dirs{
		Data:   filepath.Join(root, "data"),
		State:  filepath.Join(root, "state"),
		Config: filepath.Join(root, "config"),
	}

	if err := dirs.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	return dirs
}

// runDaemon starts a daemon on dirs and returns a function that stops it and
// waits for it to be gone.
//
// Stopping is separated from t.Cleanup so a test can restart a daemon on the same
// directories: the handle file is exclusively locked for a daemon's lifetime, so
// the second one cannot come up until the first has finished going down.
func runDaemon(t *testing.T, dirs paths.Dirs) func() {
	t.Helper()

	store, err := mission.Open(dirs)
	if err != nil {
		t.Fatalf("mission.Open: %v", err)
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	hub := daemon.NewHub()
	// No agent tooling: this exercises the transport, not what a launch does.
	svc := daemon.NewService(store, hub, dirs, daemon.WithLogger(logger))

	ctx, cancel := context.WithCancel(context.Background())
	ready := make(chan struct{})
	errCh := make(chan error, 1)

	go func() {
		errCh <- daemon.Run(ctx, daemon.RunConfig{
			Dirs:    dirs,
			Version: "test",
			Logger:  logger,
			Service: svc,
			Hub:     hub,
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

	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()

			select {
			case <-errCh:
			case <-time.After(5 * time.Second):
				t.Error("daemon did not shut down")
			}
		})
	}

	t.Cleanup(stop)

	return stop
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
		Repos:   []mission.Repo{{Name: "weave", Path: "/dev/weave"}},
	})
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	ms, err := c.CreateMission(ctx, api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "add endpoint",
		Prompt:      "do the thing",
		Tool:        mission.ToolClaude,
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

	moved, err := c.SetStatus(ctx, ms.ID, api.SetStatusRequest{To: mission.StatusDebrief})
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	if moved.Status != mission.StatusDebrief {
		t.Errorf("Status = %q", moved.Status)
	}

	if _, err := c.DeleteMission(ctx, ms.ID, false); err != nil {
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

	if !api.IsNotFound(err) {
		t.Errorf("IsNotFound(%v) = false, want true", err)
	}
}

func TestClientSurfacesServiceErrorMessage(t *testing.T) {
	c, _ := startDaemon(t)

	_, err := c.CreateOperation(t.Context(), api.CreateOperationRequest{})
	if err == nil {
		t.Fatal("expected an error")
	}

	statusErr, ok := errors.AsType[*api.StatusError](err)
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

	events := make(chan api.Event, 16)
	streamErr := make(chan error, 1)

	go func() { streamErr <- c.Stream(ctx, events) }()

	first := waitForEvent(t, events, api.EventSnapshot)

	var snap struct {
		Operations []mission.Operation `json:"operations"`
	}

	if err := first.Decode(&snap); err != nil {
		t.Fatalf("decoding snapshot: %v", err)
	}

	if _, err := c.CreateOperation(ctx, api.CreateOperationRequest{Name: "Live"}); err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	operationEvent := waitForEvent(t, events, api.EventOperation)

	var operation mission.Operation
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
func waitForEvent(t *testing.T, events <-chan api.Event, name string) api.Event {
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

	_, err := api.Connect(t.Context(), dirs)
	if !errors.Is(err, api.ErrNoDaemon) {
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
	stale := api.Handle{PID: 999999, Addr: "127.0.0.1:1", Token: "tok", Version: "test"}
	if err := api.WriteHandle(dirs.DaemonFile(), stale); err != nil {
		t.Fatalf("WriteHandle: %v", err)
	}

	if _, err := api.Connect(t.Context(), dirs); err == nil {
		t.Fatal("expected Connect to reject an unreachable daemon")
	}
}

func TestStopWithoutDaemonIsNotAnError(t *testing.T) {
	root := t.TempDir()
	dirs := paths.Dirs{Data: filepath.Join(root, "data"), State: filepath.Join(root, "state")}

	if err := api.Stop(dirs); err != nil {
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

// A client must survive the daemon restarting under it. The daemon binds an
// ephemeral port and mints a fresh token every start, so a client that kept the
// handle it opened with would go on dialing an address nobody is listening on —
// which is what left a board rendering a frozen snapshot for as long as it stayed
// open.
func TestClientRefreshFollowsARestartedDaemon(t *testing.T) {
	dirs := newDirs(t)
	stop := runDaemon(t, dirs)

	c, err := api.OpenClient(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	before := c.Handle()

	if _, err := c.Health(t.Context()); err != nil {
		t.Fatalf("Health before restart: %v", err)
	}

	stop()
	runDaemon(t, dirs)

	// The old handle is now a dead address, and no amount of retrying it helps.
	if _, err := c.Health(t.Context()); err == nil {
		t.Fatal("Health should fail while the client holds the old daemon's handle")
	}

	moved, err := c.Refresh()
	if err != nil {
		t.Fatalf("Refresh: %v", err)
	}

	if !moved {
		t.Fatal("Refresh should report that the daemon moved")
	}

	if after := c.Handle(); after.Addr == before.Addr && after.Token == before.Token {
		t.Errorf("handle did not change: %s", after.Addr)
	}

	if _, err := c.Health(t.Context()); err != nil {
		t.Fatalf("Health after Refresh: %v", err)
	}
}

func TestClientRefresh(t *testing.T) {
	cases := []struct {
		name string
		// mutate rewrites the handle file before Refresh is called.
		mutate func(t *testing.T, dirs paths.Dirs, current api.Handle)
		// opener builds the client under test.
		opener    func(t *testing.T, dirs paths.Dirs) *api.Client
		wantMoved bool
		wantErr   bool
	}{
		{
			name:      "unchanged handle reports no move",
			mutate:    func(*testing.T, paths.Dirs, api.Handle) {},
			wantMoved: false,
		},
		{
			name: "a rewritten handle with the same values is still the same daemon",
			mutate: func(t *testing.T, dirs paths.Dirs, current api.Handle) {
				current.Version = "rewritten"
				writeHandle(t, dirs, current)
			},
			wantMoved: false,
		},
		{
			name: "a new address is a move",
			mutate: func(t *testing.T, dirs paths.Dirs, current api.Handle) {
				current.Addr = "127.0.0.1:1"
				writeHandle(t, dirs, current)
			},
			wantMoved: true,
		},
		{
			name: "a new token is a move",
			mutate: func(t *testing.T, dirs paths.Dirs, current api.Handle) {
				current.Token = "a-freshly-minted-token"
				writeHandle(t, dirs, current)
			},
			wantMoved: true,
		},
		{
			name: "a removed handle is an error, not a silent success",
			mutate: func(t *testing.T, dirs paths.Dirs, _ api.Handle) {
				if err := api.RemoveHandle(dirs.DaemonFile()); err != nil {
					t.Fatalf("RemoveHandle: %v", err)
				}
			},
			wantErr: true,
		},
		{
			name: "a client built from a caller's handle has nothing to re-read",
			mutate: func(t *testing.T, dirs paths.Dirs, current api.Handle) {
				current.Addr = "127.0.0.1:1"
				writeHandle(t, dirs, current)
			},
			opener: func(t *testing.T, dirs paths.Dirs) *api.Client {
				t.Helper()

				handle, err := api.ReadHandle(dirs.DaemonFile())
				if err != nil {
					t.Fatalf("ReadHandle: %v", err)
				}

				return api.NewClient(handle)
			},
			wantMoved: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dirs := newDirs(t)
			writeHandle(t, dirs, api.Handle{
				PID:     os.Getpid(),
				Addr:    "127.0.0.1:9",
				Token:   "the-original-token",
				Version: "test",
			})

			open := tc.opener
			if open == nil {
				open = func(t *testing.T, dirs paths.Dirs) *api.Client {
					t.Helper()

					c, err := api.OpenClient(dirs)
					if err != nil {
						t.Fatalf("OpenClient: %v", err)
					}

					return c
				}
			}

			c := open(t, dirs)
			tc.mutate(t, dirs, c.Handle())

			moved, err := c.Refresh()

			if tc.wantErr {
				if err == nil {
					t.Fatal("Refresh should have failed")
				}

				return
			}

			if err != nil {
				t.Fatalf("Refresh: %v", err)
			}

			if moved != tc.wantMoved {
				t.Errorf("moved = %v, want %v", moved, tc.wantMoved)
			}
		})
	}
}

// writeHandle stores a daemon handle for the refresh tests.
func writeHandle(t *testing.T, dirs paths.Dirs, h api.Handle) {
	t.Helper()

	if err := api.WriteHandle(dirs.DaemonFile(), h); err != nil {
		t.Fatalf("WriteHandle: %v", err)
	}
}
