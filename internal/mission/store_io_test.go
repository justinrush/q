package mission

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/paths"
)

// seedMissions opens a store holding the given missions.
func seedMissions(t *testing.T, dirs paths.Dirs, opts []StoreOption, missions ...Mission) *Store {
	t.Helper()

	store, err := Open(dirs, opts...)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := store.Mutate("seed", func(s *Snapshot) error {
		s.PutOperation(sampleOperation("Operation"))

		for _, m := range missions {
			s.PutMission(m)
		}

		return nil
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	return store
}

func fileInfo(t *testing.T, path string) os.FileInfo {
	t.Helper()

	info, err := os.Stat(path)
	if err != nil {
		t.Fatalf("stat %s: %v", path, err)
	}

	return info
}

func readFile(t *testing.T, path string) []byte {
	t.Helper()

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}

	return data
}

// A mutation that changes nothing must not touch the disk, whatever its
// function returned: this is what keeps timers and hooks from rewriting the
// state to record nothing.
func TestNoOpMutationWritesNothing(t *testing.T) {
	dirs := testDirs(t)
	store := seedMissions(t, dirs, nil, ms("ms_111111111111", StatusActive, 0))

	before := fileInfo(t, dirs.StateFile())
	events := readFile(t, dirs.EventsFile())

	for range 3 {
		if err := store.Mutate("mission.hook.pre-tool-use", func(s *Snapshot) error { return nil }); err != nil {
			t.Fatal(err)
		}
	}

	if after := fileInfo(t, dirs.StateFile()); !os.SameFile(before, after) {
		t.Error("an unchanged mutation replaced the state file")
	}

	if got := readFile(t, dirs.EventsFile()); !bytes.Equal(got, events) {
		t.Errorf("an unchanged mutation was logged: %q", got[len(events):])
	}
}

func TestDeferredMutationIsVisibleAtOnceAndWrittenLater(t *testing.T) {
	dirs := testDirs(t)
	store := seedMissions(t, dirs, []StoreOption{WithFlushDelay(time.Hour)}, ms("ms_111111111111", StatusActive, 0))
	before := fileInfo(t, dirs.StateFile())

	for _, state := range []AgentState{AgentBusy, AgentIdle, AgentBusy} {
		if err := store.MutateDeferred("mission.runtime_status", func(s *Snapshot) (bool, error) {
			m, _ := s.Mission("ms_111111111111")
			m.AgentState = state
			s.PutMission(m)

			return false, nil
		}); err != nil {
			t.Fatal(err)
		}
	}

	if got, _ := store.Snapshot().Mission("ms_111111111111"); got.AgentState != AgentBusy {
		t.Errorf("AgentState = %q, want the deferred change visible at once", got.AgentState)
	}

	if !os.SameFile(before, fileInfo(t, dirs.StateFile())) {
		t.Fatal("a deferred mutation was written before its flush")
	}

	if err := store.Flush(); err != nil {
		t.Fatal(err)
	}

	reopened, err := Open(dirs)
	if err != nil {
		t.Fatal(err)
	}

	if got, _ := reopened.Snapshot().Mission("ms_111111111111"); got.AgentState != AgentBusy {
		t.Errorf("after Flush, AgentState on disk = %q", got.AgentState)
	}

	if strings.Contains(string(readFile(t, dirs.EventsFile())), "runtime_status") {
		t.Error("runtime status was written to the event log")
	}
}

func TestDeferredMutationFlushesOnItsOwn(t *testing.T) {
	dirs := testDirs(t)
	store := seedMissions(t, dirs, []StoreOption{WithFlushDelay(10 * time.Millisecond)}, ms("ms_111111111111", StatusActive, 0))

	if err := store.MutateDeferred("mission.runtime_status", func(s *Snapshot) (bool, error) {
		m, _ := s.Mission("ms_111111111111")
		m.WaitingFor = "flushed"
		s.PutMission(m)

		return false, nil
	}); err != nil {
		t.Fatal(err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if bytes.Contains(readFile(t, dirs.StateFile()), []byte("flushed")) {
			return
		}

		time.Sleep(5 * time.Millisecond)
	}

	t.Fatal("the deferred mutation never reached the disk")
}

func TestUrgentDeferredMutationIsWrittenAtOnce(t *testing.T) {
	dirs := testDirs(t)
	store := seedMissions(t, dirs, []StoreOption{WithFlushDelay(time.Hour)}, ms("ms_111111111111", StatusActive, 0))

	if err := store.MutateDeferred("mission.runtime_status", func(s *Snapshot) (bool, error) {
		m, _ := s.Mission("ms_111111111111")
		m.AgentSessionID = "session-2"
		s.PutMission(m)

		return true, nil
	}); err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(readFile(t, dirs.StateFile()), []byte("session-2")) {
		t.Error("an urgent change waited for the flush")
	}
}

func TestCloseWritesDeferredMutations(t *testing.T) {
	dirs := testDirs(t)
	store := seedMissions(t, dirs, []StoreOption{WithFlushDelay(time.Hour)}, ms("ms_111111111111", StatusActive, 0))

	if err := store.MutateDeferred("mission.reconcile", func(s *Snapshot) (bool, error) {
		m, _ := s.Mission("ms_111111111111")
		m.WaitingFor = "closing"
		s.PutMission(m)

		return false, nil
	}); err != nil {
		t.Fatal(err)
	}

	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	if !bytes.Contains(readFile(t, dirs.StateFile()), []byte("closing")) {
		t.Error("Close lost a deferred change")
	}
}

// The backup must hold the previous snapshot without being written itself.
func TestBackupIsTheOutgoingFile(t *testing.T) {
	dirs := testDirs(t)
	store := seedMissions(t, dirs, nil)

	first := fileInfo(t, dirs.StateFile())
	firstData := readFile(t, dirs.StateFile())

	if err := store.Mutate("rename", func(s *Snapshot) error {
		s.PutOperation(sampleOperation("Renamed"))

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	backup := fileInfo(t, dirs.BackupFile())
	if !os.SameFile(first, backup) {
		t.Error("backup is a new file rather than the outgoing one")
	}

	if !bytes.Equal(readFile(t, dirs.BackupFile()), firstData) {
		t.Error("backup does not hold the previous snapshot")
	}

	if err := os.Remove(dirs.StateFile()); err != nil {
		t.Fatal(err)
	}

	recovered, err := Open(dirs)
	if err != nil {
		t.Fatal(err)
	}

	if got, _ := recovered.Snapshot().Operation("op_aabbccddeeff"); got.Name != "Operation" {
		t.Errorf("a missing state file did not fall back to the backup, got %q", got.Name)
	}
}

func TestClosedMissionsLiveInTheirOwnFile(t *testing.T) {
	dirs := testDirs(t)
	closed := ms("ms_c00000000001", StatusClosed, 0)
	closed.Prompt = "a long finished prompt"
	closed.CreatedAt = time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	live := ms("ms_111111111111", StatusActive, 0)
	live.CreatedAt = time.Date(2026, 2, 1, 0, 0, 0, 0, time.UTC)

	store := seedMissions(t, dirs, nil, live, closed)

	if bytes.Contains(readFile(t, dirs.StateFile()), []byte(closed.Prompt)) {
		t.Error("the main state file holds a closed mission")
	}

	closedFile := fileInfo(t, dirs.ClosedStateFile())

	// A change to a live mission leaves the closed file alone.
	if err := store.Mutate("mission.status", func(s *Snapshot) error {
		m, _ := s.Mission(live.ID)
		m.WaitingFor = "input"
		s.PutMission(m)

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if !os.SameFile(closedFile, fileInfo(t, dirs.ClosedStateFile())) {
		t.Error("a live-mission change rewrote the closed file")
	}

	reopened, err := Open(dirs)
	if err != nil {
		t.Fatal(err)
	}

	got := reopened.Snapshot()
	if len(got.Missions) != 2 || got.Missions[0].ID != closed.ID || got.Missions[1].ID != live.ID {
		t.Fatalf("missions after reopen = %+v, want both in creation order", got.Missions)
	}

	// Reopening the closed mission moves it back.
	if err := reopened.Mutate("mission.status", func(s *Snapshot) error {
		m, _ := s.Mission(closed.ID)
		m.Status = StatusDebrief
		s.PutMission(m)

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(readFile(t, dirs.ClosedStateFile()), []byte(closed.Prompt)) {
		t.Error("a reopened mission stayed in the closed file")
	}

	again, err := Open(dirs)
	if err != nil {
		t.Fatal(err)
	}

	if m, ok := again.Snapshot().Mission(closed.ID); !ok || m.Status != StatusDebrief {
		t.Errorf("reopened mission = %+v, %v", m, ok)
	}
}

// A state file written before the split holds every mission. It must load
// whole, and the first write moves its closed missions out.
func TestLegacySingleFileStateLoads(t *testing.T) {
	dirs := testDirs(t)
	if err := dirs.Ensure(); err != nil {
		t.Fatal(err)
	}

	legacy := Snapshot{
		SchemaVersion: SchemaVersion,
		Self:          HostInfo{ID: "host_aaaaaaaaaaaa"},
		Operations:    []Operation{sampleOperation("Operation")},
		Missions: []Mission{
			ms("ms_c00000000001", StatusClosed, 0),
			ms("ms_111111111111", StatusActive, 0),
		},
	}

	data, err := json.MarshalIndent(legacy, "", "  ")
	if err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(dirs.StateFile(), data, paths.FileMode); err != nil {
		t.Fatal(err)
	}

	store, err := Open(dirs)
	if err != nil {
		t.Fatal(err)
	}

	if n := len(store.Snapshot().Missions); n != 2 {
		t.Fatalf("legacy state loaded %d missions, want 2", n)
	}

	if err := store.Mutate("mission.status", func(s *Snapshot) error {
		m, _ := s.Mission("ms_111111111111")
		m.WaitingFor = "input"
		s.PutMission(m)

		return nil
	}); err != nil {
		t.Fatal(err)
	}

	if bytes.Contains(readFile(t, dirs.StateFile()), []byte("ms_c00000000001")) {
		t.Error("the first write left the closed mission in the main file")
	}

	reopened, err := Open(dirs)
	if err != nil {
		t.Fatal(err)
	}

	if n := len(reopened.Snapshot().Missions); n != 2 {
		t.Errorf("after the split %d missions loaded, want 2", n)
	}
}

func TestEventLogRotates(t *testing.T) {
	dirs := testDirs(t)
	if err := dirs.Ensure(); err != nil {
		t.Fatal(err)
	}

	// An oversized log from before rotation existed.
	if err := os.WriteFile(dirs.EventsFile(), bytes.Repeat([]byte("x"), maxEventsLog+1), paths.FileMode); err != nil {
		t.Fatal(err)
	}

	seedMissions(t, dirs, nil)

	if size := fileInfo(t, dirs.EventsArchiveFile()).Size(); size != maxEventsLog+1 {
		t.Errorf("archive holds %d bytes, want the old log", size)
	}

	if got := string(readFile(t, dirs.EventsFile())); !strings.Contains(got, "seed") || len(got) > 200 {
		t.Errorf("new log = %q, want just the latest event", got)
	}
}
