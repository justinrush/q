package state

import (
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/paths"
)

// testDirs returns Dirs rooted in a temp directory.
func testDirs(t *testing.T) paths.Dirs {
	t.Helper()

	root := t.TempDir()

	return paths.Dirs{
		Data:   filepath.Join(root, "data"),
		State:  filepath.Join(root, "state"),
		Config: filepath.Join(root, "config"),
	}
}

// sampleOperation returns an operation for tests.
func sampleOperation(name string) domain.Operation {
	return domain.Operation{
		ID:      domain.OperationID("op_aabbccddeeff"),
		Name:    name,
		Slug:    domain.Slug(name),
		Summary: "investigating the thing",
		Repos: []domain.Repo{
			{Name: "weave", Path: "/dev/weave", CommonDir: "/dev/weave/.git", DefaultBranch: "main"},
		},
	}
}

func TestOpenCreatesEmptyState(t *testing.T) {
	dirs := testDirs(t)

	store, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	snap := store.Snapshot()
	if snap.SchemaVersion != SchemaVersion {
		t.Errorf("SchemaVersion = %d, want %d", snap.SchemaVersion, SchemaVersion)
	}

	if len(snap.Operations) != 0 || len(snap.Missions) != 0 {
		t.Errorf("fresh state should be empty, got %d operations and %d missions", len(snap.Operations), len(snap.Missions))
	}
}

func TestMutatePersistsAcrossReopen(t *testing.T) {
	dirs := testDirs(t)

	store, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := store.Mutate("operation.create", func(s *Snapshot) error {
		s.PutOperation(sampleOperation("Discussions API"))

		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	reopened, err := Open(dirs)
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}

	got, ok := reopened.Snapshot().Operation(domain.OperationID("op_aabbccddeeff"))
	if !ok {
		t.Fatal("operation not persisted")
	}

	if got.Name != "Discussions API" {
		t.Errorf("Name = %q, want %q", got.Name, "Discussions API")
	}

	if len(got.Repos) != 1 || got.Repos[0].Name != "weave" {
		t.Errorf("Repos = %+v", got.Repos)
	}
}

// A validation failure part-way through a multi-step change must leave nothing
// applied, so callers can validate inside the mutation rather than before it.
func TestMutateRollsBackOnError(t *testing.T) {
	dirs := testDirs(t)

	store, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	sentinel := errors.New("nope")

	err = store.Mutate("operation.create", func(s *Snapshot) error {
		s.PutOperation(sampleOperation("Should Not Persist"))

		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("Mutate err = %v, want %v", err, sentinel)
	}

	if len(store.Snapshot().Operations) != 0 {
		t.Error("failed mutation must not be applied in memory")
	}

	if _, statErr := os.Stat(dirs.StateFile()); !os.IsNotExist(statErr) {
		t.Error("failed mutation must not write a state file")
	}
}

// Callers must not be able to reach into the store's own slices and mutate state
// without the lock.
func TestSnapshotIsADeepCopy(t *testing.T) {
	dirs := testDirs(t)

	store, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	started := time.Now()

	if err := store.Mutate("seed", func(s *Snapshot) error {
		s.PutOperation(sampleOperation("Operation"))
		s.PutMission(domain.Mission{
			ID:          domain.MissionID("ms_aabbccddeeff"),
			OperationID: domain.OperationID("op_aabbccddeeff"),
			Name:        "mission",
			Status:      domain.StatusBriefing,
			Badges:      []domain.Badge{{Kind: domain.BadgeStale}},
			ExtraRepos:  []domain.Repo{{Name: "mac", Path: "/dev/mac"}},
			LaunchRepos: []domain.Repo{{Name: "weave", Path: "/dev/weave"}},
			Work:        map[string]domain.RepoWork{"weave": {RepoName: "weave"}},
			StartedAt:   &started,
		})

		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	snap := store.Snapshot()
	snap.Operations[0].Name = "mutated"
	snap.Operations[0].Repos[0].Name = "mutated"
	snap.Missions[0].Badges[0].Kind = "mutated"
	snap.Missions[0].ExtraRepos[0].Name = "mutated"
	snap.Missions[0].LaunchRepos[0].Name = "mutated"
	snap.Missions[0].Work["weave"] = domain.RepoWork{RepoName: "mutated"}
	*snap.Missions[0].StartedAt = started.Add(time.Hour)

	fresh := store.Snapshot()
	if fresh.Operations[0].Name == "mutated" {
		t.Error("operation name aliased")
	}

	if fresh.Operations[0].Repos[0].Name == "mutated" {
		t.Error("repo slice aliased")
	}

	if fresh.Missions[0].Badges[0].Kind == "mutated" {
		t.Error("badge slice aliased")
	}

	if fresh.Missions[0].ExtraRepos[0].Name == "mutated" {
		t.Error("extra repo slice aliased")
	}

	if fresh.Missions[0].LaunchRepos[0].Name == "mutated" {
		t.Error("launch repo slice aliased")
	}

	if fresh.Missions[0].Work["weave"].RepoName == "mutated" {
		t.Error("work map aliased")
	}

	if !fresh.Missions[0].StartedAt.Equal(started) {
		t.Error("StartedAt pointer aliased")
	}
}

func TestStateFileIsUserOnly(t *testing.T) {
	dirs := testDirs(t)

	store, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := store.Mutate("seed", func(s *Snapshot) error {
		s.PutOperation(sampleOperation("Operation"))

		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	fi, err := os.Stat(dirs.StateFile())
	if err != nil {
		t.Fatalf("stat: %v", err)
	}

	if perm := fi.Mode().Perm(); perm != paths.FileMode {
		t.Errorf("state file mode = %v, want %v", perm, paths.FileMode)
	}
}

// A corrupt primary file must fall back to the backup rather than silently
// starting from empty, which would discard recoverable state.
func TestOpenRecoversFromBackupWhenPrimaryIsCorrupt(t *testing.T) {
	dirs := testDirs(t)

	store, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	// Two mutations, so the backup holds a real prior snapshot.
	for _, name := range []string{"First", "Second"} {
		if err := store.Mutate("seed", func(s *Snapshot) error {
			operation := sampleOperation(name)
			operation.ID = domain.OperationID("op_aabbccddeeff")
			s.PutOperation(operation)

			return nil
		}); err != nil {
			t.Fatalf("Mutate: %v", err)
		}
	}

	if err := os.WriteFile(dirs.StateFile(), []byte("{ this is not json"), paths.FileMode); err != nil {
		t.Fatalf("corrupting state: %v", err)
	}

	recovered, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open after corruption: %v", err)
	}

	got, ok := recovered.Snapshot().Operation(domain.OperationID("op_aabbccddeeff"))
	if !ok {
		t.Fatal("backup did not restore the operation")
	}

	if got.Name != "First" {
		t.Errorf("recovered Name = %q, want the backup's %q", got.Name, "First")
	}
}

func TestOpenFailsLoudlyWhenBothFilesAreCorrupt(t *testing.T) {
	dirs := testDirs(t)

	if err := dirs.Ensure(); err != nil {
		t.Fatalf("Ensure: %v", err)
	}

	for _, path := range []string{dirs.StateFile(), dirs.BackupFile()} {
		if err := os.WriteFile(path, []byte("garbage"), paths.FileMode); err != nil {
			t.Fatalf("writing %s: %v", path, err)
		}
	}

	_, err := Open(dirs)
	if err == nil {
		t.Fatal("expected Open to fail rather than discard unrecoverable state")
	}

	if !strings.Contains(err.Error(), "backup also unusable") {
		t.Errorf("error should mention both files, got %v", err)
	}
}

// An empty file is the signature of a crash between create and write.
func TestOpenTreatsEmptyPrimaryAsCorrupt(t *testing.T) {
	dirs := testDirs(t)

	store, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := store.Mutate("seed", func(s *Snapshot) error {
		s.PutOperation(sampleOperation("Kept"))

		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	// Make the backup hold the good copy, then truncate the primary.
	if err := copyFile(dirs.StateFile(), dirs.BackupFile()); err != nil {
		t.Fatalf("copyFile: %v", err)
	}

	if err := os.WriteFile(dirs.StateFile(), nil, paths.FileMode); err != nil {
		t.Fatalf("truncating: %v", err)
	}

	recovered, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if len(recovered.Snapshot().Operations) != 1 {
		t.Error("empty primary should have fallen back to the backup")
	}
}

// A crash mid-write leaves the temp file behind; the previous state must still
// load, and the stray temp must not be mistaken for state.
func TestInterruptedWriteLeavesPriorStateIntact(t *testing.T) {
	dirs := testDirs(t)

	store, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := store.Mutate("seed", func(s *Snapshot) error {
		s.PutOperation(sampleOperation("Original"))

		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	stray := filepath.Join(dirs.Data, ".q-state-interrupted")
	if err := os.WriteFile(stray, []byte("partial"), paths.FileMode); err != nil {
		t.Fatalf("writing stray temp: %v", err)
	}

	reopened, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	got, ok := reopened.Snapshot().Operation(domain.OperationID("op_aabbccddeeff"))
	if !ok || got.Name != "Original" {
		t.Errorf("prior state lost: %+v", got)
	}
}

func TestMutateAppendsToEventLog(t *testing.T) {
	dirs := testDirs(t)

	store, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	for _, label := range []string{"operation.create", "mission.create", "mission.status"} {
		if err := store.Mutate(label, func(s *Snapshot) error { return nil }); err != nil {
			t.Fatalf("Mutate(%q): %v", label, err)
		}
	}

	data, err := os.ReadFile(dirs.EventsFile())
	if err != nil {
		t.Fatalf("reading events: %v", err)
	}

	lines := strings.Split(strings.TrimSpace(string(data)), "\n")
	if len(lines) != 3 {
		t.Fatalf("event log has %d lines, want 3: %q", len(lines), data)
	}

	for i, label := range []string{"operation.create", "mission.create", "mission.status"} {
		if !strings.Contains(lines[i], label) {
			t.Errorf("line %d = %q, want it to mention %q", i, lines[i], label)
		}
	}
}

func TestMutateStampsUpdatedAt(t *testing.T) {
	dirs := testDirs(t)
	fixed := time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

	store, err := Open(dirs, WithClock(func() time.Time { return fixed }))
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	if err := store.Mutate("seed", func(s *Snapshot) error { return nil }); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	if got := store.Snapshot().UpdatedAt; !got.Equal(fixed) {
		t.Errorf("UpdatedAt = %v, want %v", got, fixed)
	}
}

// Hooks, the reconciler, and the TUI all mutate concurrently; this is the case
// make raceif exists to catch.
func TestConcurrentMutationsAndReadsAreSafe(t *testing.T) {
	dirs := testDirs(t)

	store, err := Open(dirs)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}

	const workers = 8

	done := make(chan struct{})

	for w := range workers {
		go func(w int) {
			defer func() { done <- struct{}{} }()

			for i := range 10 {
				_ = store.Mutate("concurrent", func(s *Snapshot) error {
					operation := sampleOperation("t")
					operation.ID = domain.OperationID("op_" + strings.Repeat("0", 10) + string(rune('a'+w)) + string(rune('a'+i%6)))
					s.PutOperation(operation)

					return nil
				})

				_ = store.Snapshot()
			}
		}(w)
	}

	for range workers {
		<-done
	}
}
