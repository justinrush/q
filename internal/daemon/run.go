package daemon

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/justinrush/q/internal/codexapp"
	"github.com/justinrush/q/internal/debrief"
	"github.com/justinrush/q/internal/gadgets"
	"github.com/justinrush/q/internal/gitx"
	"github.com/justinrush/q/internal/launch"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/runner"
	"github.com/justinrush/q/internal/settings"
	"github.com/justinrush/q/internal/state"
	"github.com/justinrush/q/internal/termopen"
	"github.com/justinrush/q/internal/tmuxc"
)

// RunConfig configures a daemon process.
type RunConfig struct {
	Dirs    paths.Dirs
	Version string
	Logger  *slog.Logger
	// Settings are the user's resolved standing orders. The daemon is started as
	// a subprocess of cmd/q, which loads them, so they arrive here already
	// merged and expanded.
	Settings settings.Settings
	// Ready, if non-nil, is closed once the daemon is accepting requests. Tests
	// and the auto-start path use it to avoid polling.
	Ready chan<- struct{}
}

// Run starts the daemon and blocks until ctx is canceled.
//
// It returns [ErrAlreadyRunning] if another daemon holds the instance lock, which
// callers treat as success: the goal is that a daemon is running, not that this
// process is the one running it.
func Run(ctx context.Context, cfg RunConfig) error {
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	if err := cfg.Dirs.Ensure(); err != nil {
		return err
	}

	lock, err := AcquireLock(cfg.Dirs.DaemonLockFile())
	if err != nil {
		return err
	}
	defer func() { _ = lock.Release() }()

	hub := NewHub()

	svc, err := buildService(cfg, hub, logger)
	if err != nil {
		return err
	}

	codex := attachCodexWatcher(ctx, cfg, svc, logger)
	if codex != nil {
		defer func() { _ = codex.Close() }()
		go svc.RunCodexWatcher(ctx)
	}

	// Hook events are reduced on a single goroutine, so the HTTP handler can answer
	// instantly and no lock is held while other work happens.
	queue := newHookQueue()
	go queue.run(svc)

	defer queue.close()

	// Anything an agent reported while the daemon was down is applied before the
	// listener opens, so a client's first snapshot is already current.
	svc.DrainSpool()
	svc.Reconcile(ctx)

	// Reconciliation continues in the background, which is the other reason the
	// daemon exists: cards stay honest even when no board is open.
	go svc.RunReconciler(ctx)

	token, err := NewToken()
	if err != nil {
		return err
	}

	srv := NewServer(Config{
		Service: svc,
		Hub:     hub,
		Queue:   queue,
		Dirs:    cfg.Dirs,
		Token:   token,
		Version: cfg.Version,
		Logger:  logger,
	})

	addr, err := srv.Listen()
	if err != nil {
		return err
	}

	handle, err := publishHandle(cfg, addr, token)
	if err != nil {
		return err
	}

	defer func() {
		if err := RemoveHandle(cfg.Dirs.DaemonFile()); err != nil {
			logger.Warn("removing the daemon handle", "error", err)
		}
	}()

	logger.Info("daemon listening", "addr", addr, "pid", handle.PID)

	if cfg.Ready != nil {
		close(cfg.Ready)
	}

	if err := srv.Serve(ctx); err != nil {
		return fmt.Errorf("serving: %w", err)
	}

	logger.Info("daemon stopped")

	return nil
}

// attachCodexWatcher configures a lazy app-server observer when Codex is
// installed. It starts no external process until a live Codex mission is polled.
func attachCodexWatcher(
	ctx context.Context,
	cfg RunConfig,
	svc *Service,
	logger *slog.Logger,
) *codexapp.Manager {
	bins := gadgets.New(gadgets.From(cfg.Settings))
	codexBin, err := bins.Path(gadgets.Codex)
	if err != nil {
		return nil
	}

	manager := codexapp.NewManager(ctx, codexBin, cfg.Version, runner.OS{Logger: logger})
	svc.SetCodexStatusReader(manager)

	return manager
}

// publishHandle records how to reach this daemon.
//
// The token is generated fresh on every start. Live agent sessions re-read this file
// on each hook invocation rather than caching it, so rotating it is invisible to
// them.
func publishHandle(cfg RunConfig, addr, token string) (Handle, error) {
	handle := Handle{
		PID:       os.Getpid(),
		Addr:      addr,
		Token:     token,
		StartedAt: time.Now(),
		Version:   cfg.Version,
	}

	if err := WriteHandle(cfg.Dirs.DaemonFile(), handle); err != nil {
		return Handle{}, err
	}

	return handle, nil
}

// buildService opens the store and assembles the service, attaching agent tooling
// when it is available.
//
// Missing tooling is a warning rather than a failure: browsing and editing operations
// still works without git or tmux, and launching is refused with an explanation
// instead of failing halfway through provisioning a mission.
func buildService(cfg RunConfig, hub *Hub, logger *slog.Logger) (*Service, error) {
	store, err := state.Open(cfg.Dirs)
	if err != nil {
		return nil, err
	}

	svc := NewService(ServiceConfig{
		Store:  store,
		Hub:    hub,
		Dirs:   cfg.Dirs,
		Logger: logger,
		Now:    time.Now,
	})

	tooling, err := buildTooling(cfg, logger)
	if err != nil {
		logger.Warn("agent tooling is unavailable", "error", err)

		return svc, nil
	}

	svc.SetLauncher(tooling.launcher)
	svc.SetProbe(tooling.tmux)
	svc.SetMessenger(tooling.launcher)
	svc.SetDebriefer(tooling.debriefer)
	svc.SetReclaimer(tooling.launcher)

	return svc, nil
}

// tooling holds the external-process components the daemon drives.
type tooling struct {
	launcher  *launch.Launcher
	debriefer *debrief.Opener
	tmux      *tmuxc.Tmux
}

// buildTooling wires the git, tmux, and terminal components the daemon drives.
func buildTooling(cfg RunConfig, logger *slog.Logger) (tooling, error) {
	bins := gadgets.New(gadgets.From(cfg.Settings))
	if err := bins.Check(); err != nil {
		return tooling{}, err
	}

	run := runner.OS{Logger: logger}

	paths := map[gadgets.Tool]string{}

	for _, tool := range gadgets.RequiredFor(cfg.Settings) {
		resolved, err := bins.Path(tool)
		if err != nil {
			return tooling{}, err
		}

		paths[tool] = resolved
	}

	git := gitx.New(paths[gadgets.Git], run)
	tmux := tmuxc.New(paths[gadgets.Tmux], run)
	term := termopen.New(termopen.Config{
		Mode:      cfg.Settings.Terminal.Mode,
		Command:   cfg.Settings.Terminal.Command,
		ScriptBin: paths[gadgets.OsaScript],
		Run:       run,
	})

	return tooling{
		launcher: launch.New(launch.Config{
			Dirs:         cfg.Dirs,
			Git:          git,
			Tmux:         tmux,
			Bins:         bins,
			Logger:       logger,
			BranchPrefix: cfg.Settings.Git.BranchPrefix,
			Agents:       cfg.Settings.Agents,
		}),
		debriefer: debrief.New(debrief.Config{
			Git:    git,
			Tmux:   tmux,
			Term:   term,
			Bins:   bins,
			Logger: logger,
			Editor: cfg.Settings.Editor.Command,
		}),
		tmux: tmux,
	}, nil
}
