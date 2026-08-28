package daemon

import (
	"errors"
	"fmt"
	"os"
	"syscall"

	"github.com/justinrush/q/internal/paths"
)

// ErrAlreadyRunning reports that another daemon holds the instance lock.
var ErrAlreadyRunning = errors.New("another q daemon is already running")

// Lock is an advisory exclusive lock held for the daemon's lifetime.
//
// An flock is used rather than a pid file because the kernel releases it when the
// process dies, however it dies. A pid file would have to be validated against a
// possibly-recycled pid on every start, and a stale one left by a SIGKILL would
// block the next daemon indefinitely.
type Lock struct {
	f *os.File
}

// AcquireLock takes the exclusive instance lock without blocking, returning
// [ErrAlreadyRunning] if another daemon holds it.
func AcquireLock(path string) (*Lock, error) {
	f, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, paths.FileMode)
	if err != nil {
		return nil, fmt.Errorf("opening %s: %w", path, err)
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX|syscall.LOCK_NB); err != nil {
		_ = f.Close()

		if errors.Is(err, syscall.EWOULDBLOCK) {
			return nil, ErrAlreadyRunning
		}

		return nil, fmt.Errorf("locking %s: %w", path, err)
	}

	return &Lock{f: f}, nil
}

// Release unlocks and closes the lock file. The file itself is left in place;
// its existence carries no meaning, only the lock does.
func (l *Lock) Release() error {
	if l == nil || l.f == nil {
		return nil
	}

	unlockErr := syscall.Flock(int(l.f.Fd()), syscall.LOCK_UN)
	closeErr := l.f.Close()
	l.f = nil

	if unlockErr != nil {
		return fmt.Errorf("unlocking: %w", unlockErr)
	}

	if closeErr != nil {
		return fmt.Errorf("closing lock file: %w", closeErr)
	}

	return nil
}
