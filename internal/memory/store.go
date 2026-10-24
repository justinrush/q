// Package memory keeps portable, agent-written accounts of past work.
package memory

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"time"
)

const (
	MaxRecord    = 16 << 10
	MaxBatch     = 128 << 10
	PageSize     = 128
	TransferSize = 32
)

var (
	ErrInvalid  = errors.New("invalid memory")
	ErrNotFound = errors.New("memory not found")
)

type Repo struct {
	Name     string `json:"name"`
	Identity string `json:"identity"`
}

type Source struct {
	MissionID     string `json:"missionId"`
	MissionName   string `json:"missionName"`
	OperationID   string `json:"operationId"`
	OperationName string `json:"operationName"`
	Agent         string `json:"agent"`
	Repos         []Repo `json:"repos,omitempty"`
}

type Input struct {
	Kind  string   `json:"kind"`
	Title string   `json:"title"`
	Body  string   `json:"body"`
	Tags  []string `json:"tags,omitempty"`
}

type Record struct {
	Input
	Source    Source    `json:"source"`
	ID        string    `json:"id"`
	Capture   string    `json:"capture,omitempty"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
	Revision  uint64    `json:"revision"`
	Deleted   bool      `json:"deleted,omitempty"`
}

type disk struct {
	Version int               `json:"version"`
	Records map[string]Record `json:"records"`
}

// Store has one writer process, the daemon. Its mutex serializes CLI requests
// and peer exchanges; callers never receive aliases into the stored records.
type Store struct {
	mu   sync.RWMutex
	path string
	data disk
}

func Open(path string) (*Store, error) {
	s := &Store{path: path, data: disk{Version: 1, Records: map[string]Record{}}}
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return s, nil
	}
	if err == nil {
		err = decodeDisk(b, &s.data)
	}
	if err != nil {
		backup, e := os.ReadFile(path + ".bak")
		if e != nil {
			return nil, fmt.Errorf("reading memory: %w (backup: %v)", err, e)
		}
		if e = decodeDisk(backup, &s.data); e != nil {
			return nil, fmt.Errorf("reading memory backup: %w", e)
		}
	}
	return s, nil
}

func decodeDisk(b []byte, d *disk) error {
	if err := json.Unmarshal(b, d); err != nil {
		return err
	}
	if d.Version != 1 || d.Records == nil {
		return errors.New("unsupported memory file")
	}
	for id, r := range d.Records {
		if id != r.ID {
			return errors.New("memory ID mismatch")
		}
		if err := validateRecord(r); err != nil {
			return err
		}
	}
	return nil
}

func clone[T any](v T) T {
	b, _ := json.Marshal(v)
	var out T
	_ = json.Unmarshal(b, &out)
	return out
}

func (s *Store) mutate(fn func(map[string]Record) error) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	next := clone(s.data)
	if err := fn(next.Records); err != nil {
		return err
	}
	b, err := json.MarshalIndent(next, "", "  ")
	if err != nil {
		return err
	}
	// Back up the known-good in-memory state, not a potentially corrupt file.
	old, err := json.MarshalIndent(s.data, "", "  ")
	if err != nil {
		return err
	}
	if err = atomicWrite(s.path+".bak", old); err != nil {
		return err
	}
	if err = atomicWrite(s.path, b); err != nil {
		return err
	}
	s.data = clone(next)
	return nil
}

func atomicWrite(path string, b []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	f, err := os.CreateTemp(filepath.Dir(path), ".q-memory-*")
	if err != nil {
		return err
	}
	defer func() { _ = f.Close(); _ = os.Remove(f.Name()) }()
	if err = f.Chmod(0o600); err != nil {
		return err
	}
	if _, err = f.Write(b); err != nil {
		return err
	}
	if err = f.Sync(); err != nil {
		return err
	}
	if err = f.Close(); err != nil {
		return err
	}
	if err = os.Rename(f.Name(), path); err != nil {
		return err
	}
	d, err := os.Open(filepath.Dir(path))
	if err != nil {
		return err
	}
	defer func() { _ = d.Close() }()
	return d.Sync()
}

func validateInput(in Input) error {
	if in.Kind != "summary" && in.Kind != "lesson" {
		return fmt.Errorf("%w: kind must be summary or lesson", ErrInvalid)
	}
	if strings.TrimSpace(in.Title) == "" || strings.TrimSpace(in.Body) == "" {
		return fmt.Errorf("%w: title and body are required", ErrInvalid)
	}
	return nil
}

func validateRecord(r Record) error {
	if r.ID == "" || len(r.ID) > 128 || r.Revision == 0 {
		return fmt.Errorf("%w: missing ID or revision", ErrInvalid)
	}
	if !r.Deleted {
		if err := validateInput(r.Input); err != nil {
			return err
		}
	}
	b, err := json.Marshal(r)
	if err != nil {
		return err
	}
	if len(b) > MaxRecord {
		return fmt.Errorf("%w: record exceeds 16 KiB", ErrInvalid)
	}
	return nil
}

func NewID() string { return "mem_" + rand.Text() }

func (s *Store) Save(inputs []Input, source Source, capture string, now time.Time) ([]Record, error) {
	b, _ := json.Marshal(inputs)
	if len(inputs) == 0 || len(b) > MaxBatch {
		return nil, fmt.Errorf("%w: empty batch or batch exceeds 128 KiB", ErrInvalid)
	}
	var out []Record
	err := s.mutate(func(records map[string]Record) error {
		if capture != "" {
			for _, r := range records {
				if r.Capture == capture {
					out = append(out, r)
				}
			}
			if len(out) > 0 {
				return nil
			}
		}
		for _, in := range inputs {
			r := Record{Input: in, Source: source, ID: NewID(), Capture: capture, CreatedAt: now, UpdatedAt: now, Revision: 1}
			if err := validateRecord(r); err != nil {
				return err
			}
			records[r.ID] = r
			out = append(out, r)
		}
		return nil
	})
	return clone(out), err
}

func (s *Store) Get(id string) (Record, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	r, ok := s.data.Records[id]
	if !ok || r.Deleted {
		return Record{}, ErrNotFound
	}
	return clone(r), nil
}

func (s *Store) Captured(token string) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	if token == "" {
		return false
	}
	for _, r := range s.data.Records {
		if r.Capture == token {
			return true
		}
	}
	return false
}

func (s *Store) Update(id string, in Input, remove bool, now time.Time) (Record, error) {
	var out Record
	err := s.mutate(func(records map[string]Record) error {
		r, ok := records[id]
		if !ok || r.Deleted {
			return ErrNotFound
		}
		r.Input = in
		r.Deleted = remove
		r.Revision++
		r.UpdatedAt = now
		if remove {
			r.Source = Source{}
		}
		if err := validateRecord(r); err != nil {
			return err
		}
		records[id], out = r, r
		return nil
	})
	return clone(out), err
}

type Query struct {
	Keywords  string `json:"keywords"`
	Mission   string `json:"mission,omitempty"`
	Operation string `json:"operation,omitempty"`
	Repo      string `json:"repo,omitempty"`
	Kind      string `json:"kind,omitempty"`
	Limit     int    `json:"limit,omitempty"`
}

type Result struct {
	Record
	Snippet string `json:"snippet"`
	Score   int    `json:"score"`
}

func (s *Store) Search(q Query) []Result {
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Result{}
	terms := strings.Fields(strings.ToLower(q.Keywords))
	for _, r := range s.data.Records {
		if r.Deleted || (q.Kind != "" && r.Kind != q.Kind) || !matches(q.Mission, r.Source.MissionID, r.Source.MissionName) || !matches(q.Operation, r.Source.OperationID, r.Source.OperationName) {
			continue
		}
		if !matchesRepo(q.Repo, r.Source.Repos) {
			continue
		}

		high := strings.ToLower(r.Title + " " + strings.Join(r.Tags, " "))
		all := strings.ToLower(high + " " + r.Body + " " + r.Source.MissionName + " " + r.Source.OperationName)
		score, found := 0, true
		for _, term := range terms {
			if !strings.Contains(all, term) {
				found = false
				break
			}
			score++
			if strings.Contains(high, term) {
				score += 4
			}
		}
		if !found {
			continue
		}
		runes := []rune(r.Body)
		if len(runes) > 240 {
			runes = append(runes[:240], '…')
		}
		result := Result{Record: clone(r), Snippet: string(runes), Score: score}
		result.Body = "" // Search stays compact; show returns the full text.
		out = append(out, result)
	}
	slices.SortFunc(out, func(a, b Result) int {
		if a.Score != b.Score {
			return b.Score - a.Score
		}
		if n := b.UpdatedAt.Compare(a.UpdatedAt); n != 0 {
			return n
		}
		return strings.Compare(a.ID, b.ID)
	})
	limit := q.Limit
	if limit <= 0 {
		limit = 10
	}
	limit = min(limit, 50)
	return out[:min(len(out), limit)]
}

func matches(q string, values ...string) bool {
	if q == "" {
		return true
	}
	for _, v := range values {
		if strings.EqualFold(q, v) {
			return true
		}
	}
	return false
}

type Entry struct {
	ID       string `json:"id"`
	Revision uint64 `json:"revision"`
	Deleted  bool   `json:"deleted,omitempty"`
	Digest   string `json:"digest"`
}

func entry(r Record) Entry {
	b, _ := json.Marshal(r)
	return Entry{ID: r.ID, Revision: r.Revision, Deleted: r.Deleted, Digest: fmt.Sprintf("%x", sha256.Sum256(b))}
}

func (s *Store) Manifest(after string) ([]Entry, string) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	ids := []string{}
	for id := range s.data.Records {
		if id > after {
			ids = append(ids, id)
		}
	}
	slices.Sort(ids)
	next := ""
	if len(ids) > PageSize {
		ids = ids[:PageSize]
		next = ids[len(ids)-1]
	}
	out := make([]Entry, 0, len(ids))
	for _, id := range ids {
		out = append(out, entry(s.data.Records[id]))
	}
	return out, next
}

func (s *Store) Records(ids []string) ([]Record, error) {
	if len(ids) > TransferSize {
		return nil, fmt.Errorf("%w: too many requested records", ErrInvalid)
	}
	s.mu.RLock()
	defer s.mu.RUnlock()
	out := []Record{}
	for _, id := range ids {
		if r, ok := s.data.Records[id]; ok {
			out = append(out, clone(r))
		}
	}
	return out, nil
}

// IncomingWins is clock independent. A tombstone is permanent: corrections
// after deletion must create a new record, so an offline edit cannot revive it.
func IncomingWins(local, incoming Entry, primary bool) bool {
	if local.Digest == incoming.Digest {
		return false
	}
	if local.Deleted != incoming.Deleted {
		return incoming.Deleted
	}
	return incoming.Revision > local.Revision || (incoming.Revision == local.Revision && !primary)
}

func (s *Store) Merge(records []Record, primary bool) error {
	if len(records) > TransferSize {
		return fmt.Errorf("%w: too many records", ErrInvalid)
	}
	for _, r := range records {
		if err := validateRecord(r); err != nil {
			return err
		}
	}
	return s.mutate(func(all map[string]Record) error {
		for _, r := range records {
			local, exists := all[r.ID]
			if !exists || IncomingWins(entry(local), entry(r), primary) {
				all[r.ID] = r
			}
		}
		return nil
	})
}

func matchesRepo(query string, repos []Repo) bool {
	if query == "" {
		return true
	}
	for _, repo := range repos {
		if matches(query, repo.Identity, repo.Name) {
			return true
		}
	}
	return false
}
