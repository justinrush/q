package mission

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"

	"github.com/justinrush/q/internal/paths"
)

// Store owns q's persisted state.
//
// Exactly one process writes it — the daemon — which is what makes a plain JSON
// file the right choice over an embedded database. Every mutation from the TUI,
// the CLI, the hook bridge, and the reconciler arrives through the daemon's HTTP
// API and is serialized here, so the multi-writer coordination an embedded
// database would provide has nothing to coordinate.
type Store struct {
	mu   sync.RWMutex
	dirs paths.Dirs
	snap Snapshot
	now  func() time.Time
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// WithClock replaces the time source, for tests.
func WithStoreClock(now func() time.Time) StoreOption {
	return func(s *Store) { s.now = now }
}

// Open loads the state file, creating an empty one if absent.
func Open(dirs paths.Dirs, opts ...StoreOption) (*Store, error) {
	s := &Store{dirs: dirs, now: time.Now}
	for _, opt := range opts {
		opt(s)
	}

	if err := dirs.Ensure(); err != nil {
		return nil, err
	}

	snap, err := load(dirs)
	if err != nil {
		return nil, err
	}

	s.snap = snap

	if err := s.ensureIdentity(); err != nil {
		return nil, err
	}

	return s, nil
}

// ensureIdentity gives this installation a host id the first time it is needed,
// and hands it every mission that predates leases.
//
// The result is persisted at once rather than with the next mutation. An id
// that changed between two starts of the daemon would orphan every mission its
// peer believed this host was running.
func (s *Store) ensureIdentity() error {
	snap := s.snap.Clone()
	if !adoptIdentity(&snap) {
		return nil
	}

	if snap.Self.ID == "" {
		id, err := NewHostID()
		if err != nil {
			return err
		}

		snap.Self.ID = id

		adoptIdentity(&snap)
	}

	snap.UpdatedAt = s.now()

	if err := s.persist(snap); err != nil {
		return err
	}

	s.snap = snap

	return nil
}

// adoptIdentity fills in the bookkeeping version 3 added, reporting whether
// anything was missing. With no host id yet it only reports.
func adoptIdentity(snap *Snapshot) bool {
	if snap.Self.ID == "" {
		return true
	}

	changed := false

	for i := range snap.Operations {
		if snap.Operations[i].Rev == 0 {
			snap.Operations[i].Rev = 1
			changed = true
		}
	}

	for i := range snap.Missions {
		ms := &snap.Missions[i]

		if ms.SpecRev == 0 {
			ms.SpecRev = 1
			changed = true
		}

		if ms.Lease.Holder == "" {
			ms.Lease = Lease{Holder: snap.Self.ID, Epoch: max(ms.Lease.Epoch, 1)}
			changed = true
		}
	}

	return changed
}

// Snapshot returns a deep copy of the current state.
func (s *Store) Snapshot() Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()

	return s.snap.Clone()
}

// Mutate applies fn to the state and persists the result.
//
// fn receives a copy: if it returns an error, nothing is written and the store
// is left exactly as it was, so a validation failure part-way through a
// multi-step change cannot leave half of it applied. label describes the change
// for the event log.
func (s *Store) Mutate(label string, fn func(*Snapshot) error) error {
	return s.mutate(label, fn, true)
}

// Apply is Mutate for state that arrived already decided: a peer's merged
// records, or a correction to this host's own copy of a mission it does not
// run.
//
// Mutate assumes a change was made here and stamps it accordingly, bumping
// revisions and settling leases. Doing that to state the peer wrote would count
// its edit a second time and send it straight back as a newer one, which the
// peer would count again.
func (s *Store) Apply(label string, fn func(*Snapshot) error) error {
	return s.mutate(label, fn, false)
}

// mutate is the single write path behind Mutate and Apply.
func (s *Store) mutate(label string, fn func(*Snapshot) error, stamped bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.snap.Clone()
	if err := fn(&next); err != nil {
		return err
	}

	next.SchemaVersion = SchemaVersion
	next.UpdatedAt = s.now()

	// A mutation may not rewrite who this host is.
	next.Self.ID = s.snap.Self.ID

	if stamped {
		stamp(s.snap, &next, next.UpdatedAt)
	}

	if err := s.persist(next); err != nil {
		return err
	}

	s.snap = next

	s.appendEvent(label, next.UpdatedAt)

	return nil
}

// persist writes the snapshot durably, keeping the previous version as a backup.
func (s *Store) persist(snap Snapshot) error {
	data, err := json.MarshalIndent(snap, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding state: %w", err)
	}

	data = append(data, '\n')

	// Keep the outgoing file as the backup before replacing it, so a snapshot
	// that somehow encodes badly still leaves a recoverable predecessor.
	if err := copyFile(s.dirs.StateFile(), s.dirs.BackupFile()); err != nil {
		return err
	}

	return writeAtomic(s.dirs.StateFile(), data)
}

// appendEvent records an accepted mutation. A failure here is logged into the
// file itself on the next successful write and never fails the mutation: the
// event log is an audit aid, not the source of truth.
func (s *Store) appendEvent(label string, at time.Time) {
	entry, err := json.Marshal(struct {
		At    time.Time `json:"at"`
		Label string    `json:"label"`
	}{At: at, Label: label})
	if err != nil {
		return
	}

	f, err := os.OpenFile(s.dirs.EventsFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, paths.FileMode)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	_, _ = f.Write(append(entry, '\n'))
}

// load reads the state file, falling back to the backup when the primary is
// unreadable or corrupt.
func load(dirs paths.Dirs) (Snapshot, error) {
	snap, err := readSnapshot(dirs.StateFile())
	if err == nil {
		return migrate(snap), nil
	}

	if errors.Is(err, os.ErrNotExist) {
		return Snapshot{SchemaVersion: SchemaVersion}, nil
	}

	backup, backupErr := readSnapshot(dirs.BackupFile())
	if backupErr != nil {
		// Report the original failure, not the backup's: the primary file is
		// what the user needs to look at. Never silently start from empty here,
		// which would discard recoverable state.
		return Snapshot{}, fmt.Errorf("reading %s: %w (backup also unusable: %v)",
			dirs.StateFile(), err, backupErr)
	}

	return migrate(backup), nil
}

// readSnapshot decodes one state file.
func readSnapshot(path string) (Snapshot, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return Snapshot{}, err
	}

	if len(data) == 0 {
		return Snapshot{}, errors.New("file is empty")
	}

	var snap Snapshot
	if err := json.Unmarshal(data, &snap); err != nil {
		return Snapshot{}, fmt.Errorf("decoding JSON: %w", err)
	}

	return snap, nil
}

// migrate brings an older snapshot up to the current schema.
func migrate(snap Snapshot) Snapshot {
	if snap.SchemaVersion == 0 {
		snap.SchemaVersion = SchemaVersion
	}

	// Version 2 added per-mission usage and per-agent limits. Both are additive
	// and zero-value correct — an unmeasured mission carries no cost, which is
	// exactly what a version 1 snapshot means — so the bump only has to be
	// recorded, and the next meter run fills the values in.
	if snap.SchemaVersion < 2 {
		snap.SchemaVersion = 2
	}

	// Version 3 added what two paired hosts need to share missions: a host
	// identity, a lease and revision on every mission, and a revision on every
	// operation. A version 2 snapshot has none of them, and the store fills them
	// in when it opens, since minting an identity can fail and migrate cannot.
	if snap.SchemaVersion < 3 {
		snap.SchemaVersion = 3
	}

	return snap
}

// writeAtomic replaces path with data, such that a crash leaves either the old
// contents or the new ones and never a partial file.
//
// The temporary file is created in the destination directory so the rename is
// within one filesystem, and the directory itself is synced afterwards so the
// rename is durable and not merely the file's contents.
func writeAtomic(path string, data []byte) error {
	dir := filepath.Dir(path)

	tmp, err := os.CreateTemp(dir, ".q-state-*")
	if err != nil {
		return fmt.Errorf("creating temp file in %s: %w", dir, err)
	}

	tmpName := tmp.Name()

	cleanup := func() {
		_ = tmp.Close()
		_ = os.Remove(tmpName)
	}

	if err := tmp.Chmod(paths.FileMode); err != nil {
		cleanup()

		return fmt.Errorf("setting mode on %s: %w", tmpName, err)
	}

	if _, err := tmp.Write(data); err != nil {
		cleanup()

		return fmt.Errorf("writing %s: %w", tmpName, err)
	}

	if err := tmp.Sync(); err != nil {
		cleanup()

		return fmt.Errorf("syncing %s: %w", tmpName, err)
	}

	if err := tmp.Close(); err != nil {
		_ = os.Remove(tmpName)

		return fmt.Errorf("closing %s: %w", tmpName, err)
	}

	if err := os.Rename(tmpName, path); err != nil {
		_ = os.Remove(tmpName)

		return fmt.Errorf("renaming %s to %s: %w", tmpName, path, err)
	}

	return syncDir(dir)
}

// syncDir flushes a directory entry so a completed rename survives a crash.
func syncDir(dir string) error {
	d, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening %s: %w", dir, err)
	}
	defer func() { _ = d.Close() }()

	if err := d.Sync(); err != nil {
		return fmt.Errorf("syncing %s: %w", dir, err)
	}

	return nil
}

// copyFile copies src to dst, treating a missing source as success so the first
// ever write has nothing to back up.
func copyFile(src, dst string) error {
	data, err := os.ReadFile(src)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err != nil {
		return fmt.Errorf("reading %s: %w", src, err)
	}

	return writeAtomic(dst, data)
}
