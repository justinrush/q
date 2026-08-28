package client

import (
	"context"
	"errors"
	"fmt"
	"os"
	"time"

	"github.com/justinrush/q/internal/daemon"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/runner"
)

// Auto-start tuning.
const (
	// startupTimeout bounds how long we wait for a spawned daemon to answer.
	startupTimeout = 5 * time.Second
	// pollInterval is how often we retry the health check while waiting.
	pollInterval = 50 * time.Millisecond
)

// Ensure returns a client, starting a daemon first if none is reachable.
//
// A stale handle is common — the file outlives a daemon killed with SIGKILL — so
// reachability is decided by an actual health check rather than by the file's
// existence.
func Ensure(ctx context.Context, dirs paths.Dirs) (*Client, error) {
	if c, err := connect(ctx, dirs); err == nil {
		return c, nil
	}

	if err := spawn(dirs); err != nil {
		return nil, err
	}

	return waitForDaemon(ctx, dirs)
}

// Connect returns a client only if a daemon is already running, without starting
// one.
//
// The hook bridge uses this: a hook fires on every tool call, so a hook that
// spawned a daemon could start a stampede of them, and a hook must never make the
// agent wait. It spools the event to disk instead.
func Connect(ctx context.Context, dirs paths.Dirs) (*Client, error) {
	return connect(ctx, dirs)
}

// connect reads the handle and verifies the daemon answers.
func connect(ctx context.Context, dirs paths.Dirs) (*Client, error) {
	c, err := Open(dirs)
	if err != nil {
		return nil, err
	}

	if _, err := c.Health(ctx); err != nil {
		return nil, fmt.Errorf("%w: handle present but daemon unreachable: %w", daemon.ErrNoDaemon, err)
	}

	return c, nil
}

// spawn launches a detached daemon using this executable.
func spawn(dirs paths.Dirs) error {
	self, err := os.Executable()
	if err != nil {
		return fmt.Errorf("locating the q binary: %w", err)
	}

	if err := dirs.Ensure(); err != nil {
		return err
	}

	// A stale handle would otherwise let a later connect attempt read the dead
	// daemon's address and token.
	if err := daemon.RemoveHandle(dirs.DaemonFile()); err != nil {
		return err
	}

	_, err = runner.StartDetached(
		runner.Spec{Name: self, Args: []string{"daemon", "run"}, Env: os.Environ()},
		dirs.LogFile("daemon"),
	)
	if err != nil {
		return fmt.Errorf("starting the q daemon: %w", err)
	}

	return nil
}

// waitForDaemon polls until the daemon answers or the timeout expires.
func waitForDaemon(ctx context.Context, dirs paths.Dirs) (*Client, error) {
	ctx, cancel := context.WithTimeout(ctx, startupTimeout)
	defer cancel()

	ticker := time.NewTicker(pollInterval)
	defer ticker.Stop()

	var lastErr error

	for {
		if c, err := connect(ctx, dirs); err == nil {
			return c, nil
		} else {
			lastErr = err
		}

		select {
		case <-ctx.Done():
			return nil, fmt.Errorf("q daemon did not become ready within %s (see %s): %w",
				startupTimeout, dirs.LogFile("daemon"), lastErr)
		case <-ticker.C:
		}
	}
}

// Stop asks a running daemon to exit, returning nil if none is running.
//
// SIGTERM is used rather than an HTTP endpoint so the daemon's normal signal
// handling and deferred cleanup run, which is what removes the handle file.
func Stop(dirs paths.Dirs) error {
	handle, err := daemon.ReadHandle(dirs.DaemonFile())
	if errors.Is(err, daemon.ErrNoDaemon) {
		return nil
	}

	if err != nil {
		return err
	}

	proc, err := os.FindProcess(handle.PID)
	if err != nil {
		return daemon.RemoveHandle(dirs.DaemonFile())
	}

	if err := proc.Signal(os.Interrupt); err != nil {
		// The process is already gone; clear the handle it left behind.
		return daemon.RemoveHandle(dirs.DaemonFile())
	}

	return nil
}
