package daemon

import (
	"context"
	"fmt"
	"github.com/justinrush/q/internal/api"
	"log/slog"
	"os"
	"time"

	"github.com/justinrush/q/internal/paths"
)

// RunConfig is everything a daemon process needs to serve.
//
// The service and its hub arrive already assembled: which agents exist, which
// external tools were found, and what the user configured are all decisions the
// cmd package makes, so that this package holds only the process lifecycle.
type RunConfig struct {
	Dirs    paths.Dirs
	Version string
	Logger  *slog.Logger
	Service *Service
	Hub     *Hub
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

	svc := cfg.Service

	go svc.RunRuntimeWatchers(ctx)

	// The model catalog is learned in the background: the first probe starts each
	// agent, and nothing that opens a board should wait on that.
	go svc.RunModelRefresher(ctx)

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

	token, err := api.NewToken()
	if err != nil {
		return err
	}

	srv := NewServer(Config{
		Service: svc,
		Hub:     cfg.Hub,
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
		if err := api.RemoveHandle(cfg.Dirs.DaemonFile()); err != nil {
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

// publishHandle records how to reach this daemon.
//
// The token is generated fresh on every start. Live agent sessions re-read this file
// on each hook invocation rather than caching it, so rotating it is invisible to
// them.
func publishHandle(cfg RunConfig, addr, token string) (api.Handle, error) {
	handle := api.Handle{
		PID:       os.Getpid(),
		Addr:      addr,
		Token:     token,
		StartedAt: time.Now(),
		Version:   cfg.Version,
	}

	if err := api.WriteHandle(cfg.Dirs.DaemonFile(), handle); err != nil {
		return api.Handle{}, err
	}

	return handle, nil
}
