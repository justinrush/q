package mission

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"sync"
	"time"

	"github.com/justinrush/q/internal/paths"
)

const (
	// defaultFlushDelay bounds how long a deferred mutation stays only in
	// memory. Everything deferred is re-derived from the agents' runtimes after
	// a restart, so losing at most this much of it costs nothing lasting.
	defaultFlushDelay = 5 * time.Second

	// maxEventsLog is the size at which the event log is rotated.
	maxEventsLog = 16 << 20
)

// quietLabels are mutations too frequent and too short-lived to be worth an
// event-log line. The state they produce is still persisted.
var quietLabels = map[string]bool{
	"mission.runtime_status":      true,
	"mission.reconcile":           true,
	"mission.runtime_unavailable": true,
}

// Store owns q's persisted state.
//
// Exactly one process writes it — the daemon — which is what makes a plain JSON
// file the right choice over an embedded database. Every mutation from the TUI,
// the CLI, the hook bridge, and the reconciler arrives through the daemon's HTTP
// API and is serialized here, so the multi-writer coordination an embedded
// database would provide has nothing to coordinate.
//
// The state is kept in two files. Closed missions, which are most of the bytes
// and almost never change, live in ClosedStateFile and are rewritten only when
// that set changes; everything else lives in StateFile. Each write is a whole
// file replaced atomically, with the predecessor kept as a hard-linked backup
// so keeping it costs no write.
type Store struct {
	mu   sync.RWMutex
	dirs paths.Dirs
	snap Snapshot
	now  func() time.Time

	// enc is snap encoded, so a mutation that changes nothing is recognized
	// without writing.
	enc   encoded
	codec missionCodec
	// diskClosed is what ClosedStateFile holds, and diskClosedIDs the missions
	// in it.
	diskClosed    []byte
	diskClosedIDs map[MissionID]bool

	// dirty says snap is ahead of the disk, waiting on a deferred flush.
	dirty        bool
	flushPending bool
	flushDelay   time.Duration
	flushErr     error
	closed       bool

	events     *os.File
	eventsSize int64
}

// encoded is a snapshot as its two files hold it.
type encoded struct {
	hot    []byte
	closed []byte
}

// closedState is the content of ClosedStateFile.
type closedState struct {
	SchemaVersion int                           `json:"schemaVersion"`
	Missions      []Mission                     `json:"missions"`
	Analytics     map[MissionID]AnalyticsRecord `json:"analytics,omitempty"`
}

// StoreOption configures a Store.
type StoreOption func(*Store)

// WithClock replaces the time source, for tests.
func WithStoreClock(now func() time.Time) StoreOption {
	return func(s *Store) { s.now = now }
}

// WithFlushDelay replaces how long a deferred mutation waits to be written.
func WithFlushDelay(d time.Duration) StoreOption {
	return func(s *Store) { s.flushDelay = d }
}

// Open loads the state file, creating an empty one if absent.
func Open(dirs paths.Dirs, opts ...StoreOption) (*Store, error) {
	s := &Store{dirs: dirs, now: time.Now, flushDelay: defaultFlushDelay}
	for _, opt := range opts {
		opt(s)
	}

	if err := dirs.Ensure(); err != nil {
		return nil, err
	}

	snap, diskClosed, err := load(dirs)
	if err != nil {
		return nil, err
	}

	enc, err := s.codec.encode(snap)
	if err != nil {
		return nil, err
	}

	s.snap = snap
	s.enc = enc
	s.diskClosed = diskClosed
	s.diskClosedIDs = closedIDs(snap)

	if diskClosed == nil {
		s.diskClosedIDs = nil
	}

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
	if !adoptIdentity(&snap) && (snap.Analytics != nil || len(snap.Missions) == 0) {
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

	snap.captureAnalytics(Snapshot{})
	snap.SchemaVersion = SchemaVersion
	snap.UpdatedAt = s.now()

	enc, err := s.codec.encode(snap)
	if err != nil {
		return err
	}

	if err := s.persist(snap, enc); err != nil {
		return err
	}

	s.snap = snap
	s.enc = enc

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
//
// A mutation that leaves the state exactly as it was writes nothing.
func (s *Store) Mutate(label string, fn func(*Snapshot) error) error {
	return s.mutate(label, plain(fn), true, false)
}

// MutateDeferred is Mutate for frequent changes that a restart can afford to
// lose: what an agent's runtime reports each second, say, which the next poll
// after a restart reports again.
//
// The change is visible to Snapshot at once, but reaches the disk with the
// next synchronous mutation or within the flush delay, whichever is first, so
// a burst of them costs one write. fn returns urgent when this particular
// change must not wait, such as a new agent session id a restart would
// otherwise fail to recognize.
func (s *Store) MutateDeferred(label string, fn func(*Snapshot) (urgent bool, err error)) error {
	return s.mutate(label, fn, true, true)
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
	return s.mutate(label, plain(fn), false, false)
}

// plain adapts a mutation function that is never urgent.
func plain(fn func(*Snapshot) error) func(*Snapshot) (bool, error) {
	return func(snap *Snapshot) (bool, error) { return true, fn(snap) }
}

// ErrUnchanged is returned by a mutation function to say it found nothing to
// change. The store then writes nothing and reports success.
//
// It exists for work that runs on a timer. A peer exchange happens every few
// seconds and nearly always finds the two sides already agree; without a way to
// say so, a board left open overnight would rewrite the state file a few
// thousand times to record that nothing happened.
var ErrUnchanged = errors.New("unchanged")

// mutate is the single write path behind Mutate, MutateDeferred and Apply.
func (s *Store) mutate(label string, fn func(*Snapshot) (bool, error), stamped, deferrable bool) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	next := s.snap.Clone()

	urgent, err := fn(&next)
	if err != nil {
		if errors.Is(err, ErrUnchanged) {
			return nil
		}

		return err
	}

	now := s.now()

	next.SchemaVersion = SchemaVersion
	// Held at its old value until the change is known to be real, so that an
	// unchanged snapshot encodes to exactly what it did before.
	next.UpdatedAt = s.snap.UpdatedAt

	// A mutation may not rewrite who this host is.
	next.Self.ID = s.snap.Self.ID

	if stamped {
		stamp(s.snap, &next, now)
	}

	next.captureAnalytics(s.snap)

	hot, closed := split(next)

	enc, err := s.codec.encodeParts(hot, closed)
	if err != nil {
		return err
	}

	if bytes.Equal(enc.hot, s.enc.hot) && bytes.Equal(enc.closed, s.enc.closed) {
		return nil
	}

	next.UpdatedAt = now
	hot.UpdatedAt = now

	if enc.hot, err = s.codec.marshalState(hot); err != nil {
		return err
	}

	if deferrable && !urgent && !s.closed {
		s.snap = next
		s.enc = enc
		s.dirty = true
		s.scheduleFlush()
	} else {
		if err := s.persist(next, enc); err != nil {
			return err
		}

		s.snap = next
		s.enc = enc
		s.dirty = false
	}

	s.appendEvent(label, now)

	return nil
}

// scheduleFlush arranges for a deferred mutation to reach the disk.
func (s *Store) scheduleFlush() {
	if s.flushPending {
		return
	}

	s.flushPending = true

	time.AfterFunc(s.flushDelay, func() {
		s.mu.Lock()
		defer s.mu.Unlock()

		s.flushPending = false

		if err := s.flushLocked(); err != nil {
			// Try again later; Flush reports the failure to whoever asks.
			s.scheduleFlush()
		}
	})
}

// Flush writes any deferred mutation now.
func (s *Store) Flush() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	return s.flushLocked()
}

func (s *Store) flushLocked() error {
	if !s.dirty {
		return nil
	}

	if err := s.persist(s.snap, s.enc); err != nil {
		s.flushErr = err

		return err
	}

	s.dirty = false
	s.flushErr = nil

	return nil
}

// Close writes any deferred mutation and releases the event log. Mutations
// after Close are written synchronously.
func (s *Store) Close() error {
	s.mu.Lock()
	defer s.mu.Unlock()

	s.closed = true
	err := s.flushLocked()

	if s.events != nil {
		_ = s.events.Close()
		s.events = nil
	}

	return err
}

// persist writes the snapshot durably, keeping each file's previous version as
// a backup.
//
// The closed-mission file is written only when its content changed, and is
// written so that no mission is ever absent from both files: missions newly
// closed reach it before they leave the main file, and missions reopened stay
// in it until the main file holds them. Where both hold a mission, the main
// file's copy wins on load.
func (s *Store) persist(snap Snapshot, enc encoded) error {
	if bytes.Equal(enc.closed, s.diskClosed) {
		return writeWithBackup(s.dirs.StateFile(), s.dirs.BackupFile(), enc.hot)
	}

	ids := closedIDs(snap)

	var reopened []Mission

	for _, ms := range snap.Missions {
		if !ms.Status.Terminal() && s.diskClosedIDs[ms.ID] {
			reopened = append(reopened, ms)
		}
	}

	if len(reopened) > 0 {
		_, closed := split(snap)
		closed.Missions = append(closed.Missions, reopened...)

		interim, err := json.Marshal(closed)
		if err != nil {
			return fmt.Errorf("encoding closed missions: %w", err)
		}

		if err := writeWithBackup(s.dirs.ClosedStateFile(), s.dirs.ClosedBackupFile(), interim); err != nil {
			return err
		}
	} else if err := writeWithBackup(s.dirs.ClosedStateFile(), s.dirs.ClosedBackupFile(), enc.closed); err != nil {
		return err
	}

	if err := writeWithBackup(s.dirs.StateFile(), s.dirs.BackupFile(), enc.hot); err != nil {
		return err
	}

	if len(reopened) > 0 {
		if err := writeAtomic(s.dirs.ClosedStateFile(), enc.closed); err != nil {
			return err
		}
	}

	s.diskClosed = enc.closed
	s.diskClosedIDs = ids

	return nil
}

// split divides a snapshot into what each file holds: closed missions and
// their analytics in one, everything else in the other.
func split(snap Snapshot) (Snapshot, closedState) {
	hot := snap
	hot.Missions = make([]Mission, 0, len(snap.Missions))
	closed := closedState{SchemaVersion: snap.SchemaVersion}
	live := map[MissionID]bool{}

	for _, ms := range snap.Missions {
		if ms.Status.Terminal() {
			closed.Missions = append(closed.Missions, ms)

			continue
		}

		hot.Missions = append(hot.Missions, ms)
		live[ms.ID] = true
	}

	if snap.Analytics != nil {
		hot.Analytics = map[MissionID]AnalyticsRecord{}

		for id, rec := range snap.Analytics {
			if live[id] {
				hot.Analytics[id] = rec

				continue
			}

			if closed.Analytics == nil {
				closed.Analytics = map[MissionID]AnalyticsRecord{}
			}

			closed.Analytics[id] = rec
		}
	}

	return hot, closed
}

// merge reassembles a snapshot from its two files. The main file's copy of a
// mission wins over the closed file's.
func merge(hot Snapshot, closed closedState) Snapshot {
	if len(closed.Missions) == 0 && len(closed.Analytics) == 0 {
		return hot
	}

	seen := make(map[MissionID]bool, len(hot.Missions))
	for _, ms := range hot.Missions {
		seen[ms.ID] = true
	}

	for _, ms := range closed.Missions {
		if !seen[ms.ID] {
			hot.Missions = append(hot.Missions, ms)
		}
	}

	// Missions are kept in the order they were created.
	sort.SliceStable(hot.Missions, func(i, j int) bool {
		return hot.Missions[i].CreatedAt.Before(hot.Missions[j].CreatedAt)
	})

	if len(closed.Analytics) > 0 {
		analytics := make(map[MissionID]AnalyticsRecord, len(closed.Analytics)+len(hot.Analytics))
		for id, rec := range closed.Analytics {
			analytics[id] = rec
		}

		for id, rec := range hot.Analytics {
			analytics[id] = rec
		}

		hot.Analytics = analytics
	}

	return hot
}

// closedIDs lists the closed missions in a snapshot.
func closedIDs(snap Snapshot) map[MissionID]bool {
	ids := map[MissionID]bool{}

	for _, ms := range snap.Missions {
		if ms.Status.Terminal() {
			ids[ms.ID] = true
		}
	}

	return ids
}

// writeWithBackup replaces path with data, first keeping the current file as
// backup.
//
// The backup is a hard link to the outgoing file rather than a copy, so keeping
// it writes nothing: the atomic replacement below gives path a new inode and
// leaves the old one to the backup. path itself is never absent.
func writeWithBackup(path, backup string, data []byte) error {
	if err := linkBackup(path, backup); err != nil {
		return err
	}

	return writeAtomic(path, data)
}

// linkBackup points backup at path's current content, treating a missing
// source as success so the first ever write has nothing to back up.
func linkBackup(path, backup string) error {
	if _, err := os.Lstat(path); errors.Is(err, os.ErrNotExist) {
		return nil
	}

	if err := os.Remove(backup); err != nil && !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("removing %s: %w", backup, err)
	}

	if err := os.Link(path, backup); err == nil {
		return nil
	}

	// A filesystem without hard links still gets a backup, at the old cost.
	return copyFile(path, backup)
}

// appendEvent records an accepted mutation. A failure here never fails the
// mutation: the event log is an audit aid, not the source of truth.
//
// The file stays open between events and is rotated once it grows past
// maxEventsLog, keeping one predecessor.
func (s *Store) appendEvent(label string, at time.Time) {
	if quietLabels[label] {
		return
	}

	entry, err := json.Marshal(struct {
		At    time.Time `json:"at"`
		Label string    `json:"label"`
	}{At: at, Label: label})
	if err != nil {
		return
	}

	if s.events == nil && !s.openEvents() {
		return
	}

	if s.eventsSize >= maxEventsLog {
		_ = s.events.Close()
		s.events = nil

		if err := os.Rename(s.dirs.EventsFile(), s.dirs.EventsArchiveFile()); err != nil {
			return
		}

		if !s.openEvents() {
			return
		}
	}

	n, _ := s.events.Write(append(entry, '\n'))
	s.eventsSize += int64(n)
}

// openEvents opens the event log for appending.
func (s *Store) openEvents() bool {
	f, err := os.OpenFile(s.dirs.EventsFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, paths.FileMode)
	if err != nil {
		return false
	}

	info, err := f.Stat()
	if err != nil {
		_ = f.Close()

		return false
	}

	s.events = f
	s.eventsSize = info.Size()

	return true
}

// load reads both state files, each falling back to its backup when it is
// unreadable or corrupt. It also returns the closed file's bytes as read, nil
// when it was absent or came from the backup.
func load(dirs paths.Dirs) (Snapshot, []byte, error) {
	hot, err := loadFile(dirs.StateFile(), dirs.BackupFile(), readSnapshot)
	if err != nil {
		return Snapshot{}, nil, err
	}

	closed, err := loadFile(dirs.ClosedStateFile(), dirs.ClosedBackupFile(), readClosed)
	if err != nil {
		return Snapshot{}, nil, err
	}

	var diskClosed []byte
	if closed.fromPrimary {
		diskClosed = closed.raw
	}

	return migrate(merge(hot.value, closed.value)), diskClosed, nil
}

// loaded is one file's decoded content.
type loaded[T any] struct {
	value       T
	raw         []byte
	fromPrimary bool
}

// loadFile reads path, falling back to backup when path is missing, empty or
// corrupt. With neither present it returns the zero value.
func loadFile[T any](path, backup string, read func(string) (T, []byte, error)) (loaded[T], error) {
	value, raw, err := read(path)
	if err == nil {
		return loaded[T]{value: value, raw: raw, fromPrimary: true}, nil
	}

	primaryMissing := errors.Is(err, os.ErrNotExist)

	value, _, backupErr := read(backup)
	if backupErr == nil {
		return loaded[T]{value: value}, nil
	}

	if primaryMissing && errors.Is(backupErr, os.ErrNotExist) {
		return loaded[T]{}, nil
	}

	if primaryMissing {
		err = backupErr
	}

	// Report the original failure, not the backup's: the primary file is what
	// the user needs to look at. Never silently start from empty here, which
	// would discard recoverable state.
	return loaded[T]{}, fmt.Errorf("reading %s: %w (backup also unusable: %v)", path, err, backupErr)
}

// readSnapshot decodes the main state file.
func readSnapshot(path string) (Snapshot, []byte, error) {
	var snap Snapshot

	data, err := readJSON(path, &snap)

	return snap, data, err
}

// readClosed decodes the closed-mission file.
func readClosed(path string) (closedState, []byte, error) {
	var closed closedState

	data, err := readJSON(path, &closed)

	return closed, data, err
}

// readJSON decodes one state file into v, returning its bytes.
func readJSON(path string, v any) ([]byte, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}

	if len(data) == 0 {
		return nil, errors.New("file is empty")
	}

	if err := json.Unmarshal(data, v); err != nil {
		return nil, fmt.Errorf("decoding JSON: %w", err)
	}

	return data, nil
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

	if snap.SchemaVersion < 4 {
		snap.SchemaVersion = 4
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
