package api

import (
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"syscall"
	"time"

	"github.com/justinrush/q/internal/paths"
)

// tokenBytes is the size of the daemon's API token.
const tokenBytes = 32

// ErrNoDaemon reports that no daemon handle could be found.
var ErrNoDaemon = errors.New("q daemon is not running")

// Handle is the contents of daemon.json: everything a client needs to reach a
// running daemon.
//
// The token lives only in this file, which is mode 0600. It is deliberately never
// placed in an environment variable or argv, because `tmux show-environment`
// prints session environments in plaintext and `ps -E` can expose them. Children
// are told the path to this file and read it themselves.
type Handle struct {
	PID       int       `json:"pid"`
	Addr      string    `json:"addr"`
	Token     string    `json:"token"`
	StartedAt time.Time `json:"startedAt"`
	Version   string    `json:"version"`
}

// BaseURL returns the daemon's HTTP root.
func (h Handle) BaseURL() string { return "http://" + h.Addr }

// Alive reports whether the recorded process still exists.
func (h Handle) Alive() bool {
	if h.PID <= 0 {
		return false
	}

	return processAlive(h.PID)
}

// NewToken returns a fresh API token.
func NewToken() (string, error) {
	buf := make([]byte, tokenBytes)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating daemon token: %w", err)
	}

	return base64.RawURLEncoding.EncodeToString(buf), nil
}

// ReadHandle loads the daemon handle, returning [ErrNoDaemon] when absent.
func ReadHandle(path string) (Handle, error) {
	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return Handle{}, ErrNoDaemon
	}

	if err != nil {
		return Handle{}, fmt.Errorf("reading %s: %w", path, err)
	}

	var h Handle
	if err := json.Unmarshal(data, &h); err != nil {
		return Handle{}, fmt.Errorf("decoding %s: %w", path, err)
	}

	if h.Addr == "" || h.Token == "" {
		return Handle{}, fmt.Errorf("%s is incomplete: %w", path, ErrNoDaemon)
	}

	return h, nil
}

// WriteHandle stores the daemon handle with owner-only permissions.
func WriteHandle(path string, h Handle) error {
	data, err := json.MarshalIndent(h, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding daemon handle: %w", err)
	}

	// Create with the restrictive mode from the outset rather than widening then
	// narrowing, so the token is never briefly world-readable.
	f, err := os.OpenFile(path, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, paths.FileMode)
	if err != nil {
		return fmt.Errorf("creating %s: %w", path, err)
	}

	if _, err := f.Write(append(data, '\n')); err != nil {
		_ = f.Close()

		return fmt.Errorf("writing %s: %w", path, err)
	}

	if err := f.Close(); err != nil {
		return fmt.Errorf("closing %s: %w", path, err)
	}

	return nil
}

// RemoveHandle deletes the daemon handle, ignoring an already-absent file.
func RemoveHandle(path string) error {
	if err := os.Remove(path); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", path, err)
	}

	return nil
}

// processAlive reports whether a pid exists.
//
// Signal 0 runs the kernel's permission and existence checks without delivering
// anything, which is the cheapest liveness probe available for a process we do
// not own.
func processAlive(pid int) bool {
	return syscall.Kill(pid, 0) == nil
}
