package mission

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"
)

func analyticsFixture() Snapshot {
	now := time.Date(2026, 10, 1, 0, 0, 0, 0, time.UTC)
	return Snapshot{Self: HostInfo{ID: "host-a"}, Operations: []Operation{{ID: "op-a", Name: "Operation"}}, Missions: []Mission{{ID: "mission-a", OperationID: "op-a", Name: "Mission", Prompt: "secret prompt", TranscriptPath: "/secret/path", Status: StatusClosed, StartedAt: &now, UsageByHost: map[HostID]Usage{"host-a": {PerModel: map[string]ModelTokens{"sol": {Input: 100}}, USD: 2, Breakdown: []UsageSlice{{Model: "sol", Effort: "high", Phase: PhaseImplement, Tokens: ModelTokens{Input: 100}, USD: 2}}}}}}}
}

func TestAnalyticsRetentionAndClone(t *testing.T) {
	s := analyticsFixture()
	s.captureAnalytics(Snapshot{})
	original := s.Analytics["mission-a"].Revision
	s.captureAnalytics(s.Clone())
	if s.Analytics["mission-a"].Revision != original {
		t.Fatal("unchanged capture advanced revision")
	}
	before := s.Clone()
	s.Missions = nil
	s.Operations = nil
	s.captureAnalytics(before)
	r := s.Analytics["mission-a"]
	if !r.Deleted || r.OperationName != "Operation" || r.Usage().USD != 2 {
		t.Fatalf("lost deleted history: %+v", r)
	}
	cloned := s.Clone()
	delete(cloned.Analytics["mission-a"].Contributions, "host-a")
	if s.Analytics["mission-a"].Usage().USD != 2 {
		t.Fatal("clone aliases history")
	}
	data, _ := json.Marshal(r)
	if strings.Contains(string(data), "secret") {
		t.Fatal("history retained private data")
	}
}

func TestAnalyticsConvergesAfterOfflineDeletion(t *testing.T) {
	a := analyticsFixture()
	a.captureAnalytics(Snapshot{})
	b := Snapshot{Self: HostInfo{ID: "host-b"}}
	if !b.MergePeer(a.Payload(), false, time.Now()).Changed() {
		t.Fatal("initial merge unchanged")
	}
	b.captureAnalytics(Snapshot{})
	before := a.Clone()
	a.Missions = nil
	a.Operations = nil
	a.captureAnalytics(before)
	// History persists even after tombstones expire and without live missions.
	a.Tombstones = nil
	if !b.MergePeer(a.Payload(), false, time.Now()).AnalyticsChanged {
		t.Fatal("history-only change not detected")
	}
	if b.Analytics["mission-a"].Usage().USD != 2 || !b.Analytics["mission-a"].Deleted {
		t.Fatal("deleted history lost")
	}
	if b.mergeAnalytics(a.Analytics, false) {
		t.Fatal("repeated sync changed history")
	}
	stale := before.Analytics
	b.mergeAnalytics(stale, false)
	if !b.Analytics["mission-a"].Deleted {
		t.Fatal("stale snapshot resurrected history")
	}
}

func TestAnalyticsContributionsReplaceNotAdd(t *testing.T) {
	a := analyticsFixture()
	a.captureAnalytics(Snapshot{})
	b := a.Clone()
	b.Self.ID = "host-b"
	ms := b.Missions[0]
	ms.UsageByHost["host-b"] = Usage{PerModel: map[string]ModelTokens{"astra": {Input: 50}}, USD: 3}
	b.Missions[0] = ms
	b.captureAnalytics(a)
	a.mergeAnalytics(b.Analytics, true)
	a.mergeAnalytics(b.Analytics, true)
	if got := a.Analytics["mission-a"].Usage().USD; got != 5 {
		t.Fatalf("total %v, want 5", got)
	}
	// The owning host can correct a cumulative reading downwards.
	ms.UsageByHost["host-b"] = Usage{PerModel: map[string]ModelTokens{"astra": {Input: 25}}, USD: 1}
	b.Missions[0] = ms
	b.captureAnalytics(b.Clone())
	a.mergeAnalytics(b.Analytics, true)
	if got := a.Analytics["mission-a"].Usage().USD; got != 3 {
		t.Fatalf("corrected total %v, want 3", got)
	}
}

func TestAnalyticsLegacyPreservesDollars(t *testing.T) {
	s := analyticsFixture()
	ms := &s.Missions[0]
	ms.UsageByHost = nil
	ms.Usage = Usage{USD: 17, PerModel: map[string]ModelTokens{"a": {Input: 100}, "b": {Output: 20}}}
	s.captureAnalytics(Snapshot{})
	u := s.Analytics[ms.ID].Usage()
	rows := u.Slices()
	if len(rows) != 1 || rows[0].Model != "" || rows[0].Effort != "" || rows[0].USD != 17 {
		t.Fatalf("invented legacy attribution: %+v", rows)
	}
	ms.UsageByHost = map[HostID]Usage{"host-a": ms.Usage}
	s.captureAnalytics(s.Clone())
	if s.Analytics[ms.ID].Usage().USD != 17 {
		t.Fatal("legacy baseline counted twice")
	}
}

func TestAnalyticsOwnerReplacesSeedAndMetadataUsesSpecRevision(t *testing.T) {
	a := analyticsFixture()
	a.Missions[0].SpecRev = 2
	a.captureAnalytics(Snapshot{})
	b := analyticsFixture()
	b.Self.ID = "host-b"
	b.Missions[0].SpecRev = 1
	b.Missions[0].Name = "stale name"
	b.Missions[0].UsageByHost["host-a"] = Usage{USD: 1, PerModel: map[string]ModelTokens{"sol": {Input: 50}}}
	b.captureAnalytics(Snapshot{})
	if b.Analytics["mission-a"].Contributions["host-a"].Revision != 0 {
		t.Fatal("non-owner authored a revision")
	}
	b.mergeAnalytics(a.Analytics, false)
	if r := b.Analytics["mission-a"]; r.Usage().USD != 2 || r.Name != "Mission" {
		t.Fatalf("owner/spec update lost: %+v", r)
	}
	// A deleted record cannot be re-seeded from a stale live mission during sync.
	before := a.Clone()
	a.Missions = nil
	a.captureAnalytics(before)
	b.mergeAnalytics(a.Analytics, false)
	b.captureAnalytics(b.Clone())
	if r := b.Analytics["mission-a"]; r.Usage().USD != 2 || r.Name != "Mission" || !r.Deleted {
		t.Fatalf("stale capture overwrote deletion: %+v", r)
	}
}

func TestAnalyticsPersistsMigrationDeletionAndReopen(t *testing.T) {
	dirs := testDirs(t)
	if err := dirs.Ensure(); err != nil {
		t.Fatal(err)
	}
	legacy := analyticsFixture()
	legacy.SchemaVersion = 3
	data, err := json.Marshal(legacy)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(dirs.StateFile(), data, 0600); err != nil {
		t.Fatal(err)
	}
	store, err := Open(dirs)
	if err != nil {
		t.Fatal(err)
	}
	seeded := store.Snapshot().Analytics["mission-a"]
	if seeded.Usage().USD != 2 {
		t.Fatal("migration did not seed usage")
	}
	reopened, err := Open(dirs)
	if err != nil {
		t.Fatal(err)
	}
	if reopened.Snapshot().Analytics["mission-a"].Revision != seeded.Revision {
		t.Fatal("migration not idempotent")
	}
	if err := store.Mutate("delete", func(s *Snapshot) error { s.DeleteMission("mission-a"); return nil }); err != nil {
		t.Fatal(err)
	}
	reopened, err = Open(dirs)
	if err != nil {
		t.Fatal(err)
	}
	r := reopened.Snapshot().Analytics["mission-a"]
	if !r.Deleted || r.Usage().USD != 2 {
		t.Fatalf("deletion not durably retained: %+v", r)
	}
}
