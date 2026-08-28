package spool

import (
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
)

// entry builds a spooled hook event observed at the given offset.
func entry(offsetSeconds int, event string) Entry {
	base := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	return Entry{
		ObservedAt: base.Add(time.Duration(offsetSeconds) * time.Second),
		Hook: api.HookRequest{
			Tool:      mission.ToolClaude,
			Event:     event,
			MissionID: "ms_aabbccddeeff",
			HookEpoch: 1,
			Payload:   json.RawMessage(`{"session_id":"s"}`),
		},
	}
}

func TestWriteThenDrainRoundTrips(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")

	if err := Write(dir, entry(0, "SessionStart")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	entries, err := Drain(dir)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if len(entries) != 1 {
		t.Fatalf("len = %d, want 1", len(entries))
	}

	if entries[0].Hook.Event != "SessionStart" || entries[0].Hook.MissionID != "ms_aabbccddeeff" {
		t.Errorf("entry = %+v", entries[0].Hook)
	}
}

// A backlog must replay in the order the agent produced it, or a Stop could be
// applied before the PermissionRequest that preceded it.
func TestDrainReturnsEntriesInObservedOrder(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")

	// Written out of order on purpose.
	for _, e := range []Entry{
		entry(2, "PostToolUse"),
		entry(0, "SessionStart"),
		entry(3, "Stop"),
		entry(1, "PermissionRequest"),
	} {
		if err := Write(dir, e); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	entries, err := Drain(dir)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	want := []string{"SessionStart", "PermissionRequest", "PostToolUse", "Stop"}
	if len(entries) != len(want) {
		t.Fatalf("len = %d, want %d", len(entries), len(want))
	}

	for i, event := range want {
		if entries[i].Hook.Event != event {
			t.Errorf("entries[%d] = %q, want %q", i, entries[i].Hook.Event, event)
		}
	}
}

func TestDrainRemovesEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")

	if err := Write(dir, entry(0, "Stop")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	if _, err := Drain(dir); err != nil {
		t.Fatalf("Drain: %v", err)
	}

	remaining, err := Drain(dir)
	if err != nil {
		t.Fatalf("second Drain: %v", err)
	}

	if len(remaining) != 0 {
		t.Errorf("len = %d, want the spool emptied", len(remaining))
	}
}

// Two hooks firing in the same nanosecond must not overwrite each other.
func TestWriteDoesNotCollideOnIdenticalTimestamps(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")

	for range 20 {
		if err := Write(dir, entry(0, "Stop")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	entries, err := Drain(dir)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if len(entries) != 20 {
		t.Errorf("len = %d, want 20", len(entries))
	}
}

// A corrupt entry must be discarded rather than retried forever, or it would block
// every later event on every startup.
func TestDrainDiscardsCorruptEntries(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")

	if err := Write(dir, entry(1, "Stop")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	corrupt := filepath.Join(dir, "00000000000000000000-deadbeef.json")
	if err := os.WriteFile(corrupt, []byte("{not json"), paths.FileMode); err != nil {
		t.Fatalf("writing corrupt entry: %v", err)
	}

	entries, err := Drain(dir)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if len(entries) != 1 || entries[0].Hook.Event != "Stop" {
		t.Errorf("entries = %+v, want only the good one", entries)
	}

	if _, err := os.Stat(corrupt); !os.IsNotExist(err) {
		t.Error("the corrupt entry should have been removed")
	}
}

// A half-written entry is named with a leading dot until it is renamed into place, so
// a concurrent drain cannot read it.
func TestDrainIgnoresInProgressWrites(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")

	if err := os.MkdirAll(dir, paths.DirMode); err != nil {
		t.Fatalf("MkdirAll: %v", err)
	}

	partial := filepath.Join(dir, ".00000000000000000000-deadbeef.json")
	if err := os.WriteFile(partial, []byte("half"), paths.FileMode); err != nil {
		t.Fatalf("writing partial entry: %v", err)
	}

	entries, err := Drain(dir)
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("entries = %+v, want none", entries)
	}

	if _, err := os.Stat(partial); err != nil {
		t.Error("an in-progress write should be left alone")
	}
}

func TestDrainToleratesMissingDirectory(t *testing.T) {
	entries, err := Drain(filepath.Join(t.TempDir(), "absent"))
	if err != nil {
		t.Fatalf("Drain: %v", err)
	}

	if len(entries) != 0 {
		t.Errorf("entries = %+v, want none", entries)
	}
}

func TestCount(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")

	if got, err := Count(dir); err != nil || got != 0 {
		t.Errorf("Count on empty = %d, %v", got, err)
	}

	for i := range 3 {
		if err := Write(dir, entry(i, "Stop")); err != nil {
			t.Fatalf("Write: %v", err)
		}
	}

	if got, err := Count(dir); err != nil || got != 3 {
		t.Errorf("Count = %d, %v; want 3", got, err)
	}
}

// Spooled events carry mission prompts and paths, so they must not be world-readable.
func TestSpoolFilesAreUserOnly(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "spool")

	if err := Write(dir, entry(0, "Stop")); err != nil {
		t.Fatalf("Write: %v", err)
	}

	matches, err := filepath.Glob(filepath.Join(dir, "*.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("glob = %v, %v", matches, err)
	}

	info, err := os.Stat(matches[0])
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if perm := info.Mode().Perm(); perm != paths.FileMode {
		t.Errorf("mode = %v, want %v", perm, paths.FileMode)
	}
}
