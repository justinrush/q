// Package spool stores hook events that could not be delivered, so a stopped or
// restarting daemon does not silently lose agent status updates.
//
// A hook must never make the agent wait and must never fail loudly, so when the
// daemon is unreachable the event is written here and the hook exits successfully.
// The daemon drains the spool at startup.
package spool

import (
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/paths"
)

// fileSuffix marks spooled entries.
const fileSuffix = ".json"

// randSuffixLen is the length of the random part of a spool filename.
const randSuffixLen = 8

// Entry is one spooled hook event.
type Entry struct {
	// ObservedAt is when the hook ran. It orders the spool but is deliberately not
	// used to order live events, because hook processes have independent clocks and
	// codex's login-shell startup adds enough jitter to invert them.
	ObservedAt time.Time       `json:"observedAt"`
	Hook       api.HookRequest `json:"hook"`
}

// Write stores an entry.
//
// The filename leads with a nanosecond timestamp so a lexical sort is a
// chronological one, which is how the daemon replays a backlog in order.
func Write(dir string, entry Entry) error {
	if err := os.MkdirAll(dir, paths.DirMode); err != nil {
		return fmt.Errorf("creating the spool directory: %w", err)
	}

	data, err := json.Marshal(entry)
	if err != nil {
		return fmt.Errorf("encoding spool entry: %w", err)
	}

	suffix, err := randomSuffix()
	if err != nil {
		return err
	}

	name := fmt.Sprintf("%020d-%s%s", entry.ObservedAt.UnixNano(), suffix, fileSuffix)

	// Written to a temporary name and renamed so the daemon never reads a
	// half-written entry it is concurrently draining.
	tmp := filepath.Join(dir, "."+name)
	if err := os.WriteFile(tmp, data, paths.FileMode); err != nil {
		return fmt.Errorf("writing spool entry: %w", err)
	}

	if err := os.Rename(tmp, filepath.Join(dir, name)); err != nil {
		_ = os.Remove(tmp)

		return fmt.Errorf("publishing spool entry: %w", err)
	}

	return nil
}

// Drain reads and removes every spooled entry, oldest first.
//
// Unreadable entries are removed rather than retried forever: a corrupt entry would
// otherwise block every later one on every startup.
func Drain(dir string) ([]Entry, error) {
	names, err := list(dir)
	if err != nil {
		return nil, err
	}

	entries := make([]Entry, 0, len(names))

	for _, name := range names {
		path := filepath.Join(dir, name)

		data, err := os.ReadFile(path)
		if err != nil {
			_ = os.Remove(path)

			continue
		}

		var entry Entry
		if err := json.Unmarshal(data, &entry); err == nil {
			entries = append(entries, entry)
		}

		if err := os.Remove(path); err != nil {
			return entries, fmt.Errorf("removing drained spool entry %s: %w", name, err)
		}
	}

	return entries, nil
}

// Count reports how many entries are waiting.
func Count(dir string) (int, error) {
	names, err := list(dir)

	return len(names), err
}

// list returns spooled filenames in chronological order.
func list(dir string) ([]string, error) {
	dirEntries, err := os.ReadDir(dir)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, fmt.Errorf("reading the spool directory: %w", err)
	}

	var names []string

	for _, dirEntry := range dirEntries {
		name := dirEntry.Name()

		// Skip in-progress writes, which are prefixed with a dot.
		if dirEntry.IsDir() || strings.HasPrefix(name, ".") || !strings.HasSuffix(name, fileSuffix) {
			continue
		}

		names = append(names, name)
	}

	slices.Sort(names)

	return names, nil
}

// randomSuffix returns a short random string, so two hooks firing in the same
// nanosecond cannot collide.
func randomSuffix() (string, error) {
	buf := make([]byte, randSuffixLen/2)
	if _, err := rand.Read(buf); err != nil {
		return "", fmt.Errorf("generating a spool filename: %w", err)
	}

	return hex.EncodeToString(buf), nil
}
