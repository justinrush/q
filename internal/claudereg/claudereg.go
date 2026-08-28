// Package claudereg reads claude's live session registry.
//
// claude publishes a small JSON file per running process describing what that
// session is doing. q uses it to self-heal: if a hook is ever missed, the
// registry still reports that the session is waiting or idle, so a card can be
// corrected rather than left lying.
//
// # Safety
//
// The registry directory also contains per-session key files holding messaging
// credentials. This package reads only *.json and must never be widened to read
// anything else. A test feeds it a decoy key file to prove it stays closed.
package claudereg

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

// Registry status values claude publishes.
const (
	StatusBusy     = "busy"
	StatusIdle     = "idle"
	StatusAwaiting = "waiting"
)

// Session is one entry from the registry.
type Session struct {
	PID       int    `json:"pid"`
	SessionID string `json:"sessionId"`
	CWD       string `json:"cwd"`
	// Status is one of busy, idle, or waiting.
	Status string `json:"status"`
	// WaitingFor describes what a waiting session is blocked on, e.g.
	// "input needed" or "sandbox request".
	WaitingFor string `json:"waitingFor"`
	// Tmux locates the session's pane, formatted as "session:@window.%pane".
	Tmux string `json:"tmux"`
	// UpdatedAt is a millisecond epoch timestamp.
	UpdatedAt int64 `json:"updatedAt"`
	// Name is claude's display name for the session.
	Name string `json:"name"`
	// Kind distinguishes interactive sessions from background jobs.
	Kind string `json:"kind"`
}

// PaneID extracts the tmux pane id, e.g. "%13", from the registry's location
// string.
//
// This is worth using in preference to guessing: pane ids cannot be computed from
// an index because the user's tmux config may set pane-base-index to 1.
func (s Session) PaneID() string {
	if idx := strings.LastIndex(s.Tmux, "%"); idx >= 0 {
		return s.Tmux[idx:]
	}

	return ""
}

// Updated returns the entry's timestamp.
func (s Session) Updated() time.Time {
	if s.UpdatedAt == 0 {
		return time.Time{}
	}

	return time.UnixMilli(s.UpdatedAt)
}

// Alive reports whether the recorded process still exists.
//
// Entries are keyed by pid and outlive the process when it dies abruptly, so a
// liveness check is required before trusting one.
func (s Session) Alive() bool {
	if s.PID <= 0 {
		return false
	}

	return syscall.Kill(s.PID, 0) == nil
}

// DefaultDir returns claude's registry directory.
func DefaultDir() (string, error) {
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating the home directory: %w", err)
	}

	return filepath.Join(home, ".claude", "sessions"), nil
}

// Scan reads every live session from the registry.
//
// A missing directory is not an error: it simply means claude has not run. Entries
// that cannot be parsed, or whose process is gone, are skipped rather than reported,
// because the registry is a best-effort signal and q already has hooks as its
// primary source.
func Scan(dir string) ([]Session, error) {
	// Only *.json. The sibling *.<hash>.key files are credentials.
	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil {
		return nil, fmt.Errorf("scanning the claude session registry: %w", err)
	}

	if len(matches) == 0 {
		return nil, nil
	}

	sessions := make([]Session, 0, len(matches))

	for _, path := range matches {
		session, ok := read(path)
		if !ok {
			continue
		}

		sessions = append(sessions, session)
	}

	return sessions, nil
}

// read decodes one registry entry, reporting whether it is usable.
func read(path string) (Session, bool) {
	// Belt and braces: never open anything that is not plainly a .json entry, even
	// if the glob above is ever changed.
	if !strings.HasSuffix(path, ".json") {
		return Session{}, false
	}

	data, err := os.ReadFile(path)
	if err != nil {
		if !errors.Is(err, os.ErrNotExist) {
			return Session{}, false
		}

		return Session{}, false
	}

	var session Session
	if err := json.Unmarshal(data, &session); err != nil {
		return Session{}, false
	}

	if session.SessionID == "" || !session.Alive() {
		return Session{}, false
	}

	return session, true
}

// BySessionID indexes live sessions by their session identifier.
func BySessionID(sessions []Session) map[string]Session {
	index := make(map[string]Session, len(sessions))
	for _, session := range sessions {
		index[session.SessionID] = session
	}

	return index
}
