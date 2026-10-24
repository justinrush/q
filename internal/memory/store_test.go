package memory

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func testStore(t *testing.T) *Store {
	t.Helper()
	s, err := Open(filepath.Join(t.TempDir(), "memory.json"))
	if err != nil {
		t.Fatal(err)
	}
	return s
}
func lesson(title, body string) Input {
	return Input{Kind: "lesson", Title: title, Body: body, Tags: []string{"git"}}
}
func saveOne(t *testing.T, s *Store, in Input) Record {
	t.Helper()
	rows, err := s.Save([]Input{in}, Source{MissionID: "ms_a", MissionName: "Fix sync", OperationID: "op_a", Repos: []Repo{{Name: "app", Identity: "host/a/app"}}}, "", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	return rows[0]
}

func TestPersistenceRecoveryAndIsolation(t *testing.T) {
	s := testStore(t)
	a := saveOne(t, s, lesson("first", "evidence"))
	_ = saveOne(t, s, lesson("second", "more evidence"))
	reopened, err := Open(s.path)
	if err != nil {
		t.Fatal(err)
	}
	got, err := reopened.Get(a.ID)
	if err != nil {
		t.Fatal(err)
	}
	got.Tags[0] = "changed"
	again, _ := reopened.Get(a.ID)
	if again.Tags[0] != "git" {
		t.Fatal("Get aliases store")
	}
	info, err := os.Stat(s.path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Fatalf("mode: %v, %v", info, err)
	}
	if err = os.WriteFile(s.path, []byte("broken"), 0o600); err != nil {
		t.Fatal(err)
	}
	recovered, err := Open(s.path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = recovered.Get(a.ID); err != nil {
		t.Fatal("backup lost first record:", err)
	}
	if len(recovered.Search(Query{})) != 1 {
		t.Fatal("expected previous good snapshot")
	}
}

func TestCaptureIdempotencyAndAtomicValidation(t *testing.T) {
	s := testStore(t)
	in := []Input{{Kind: "summary", Title: "mission", Body: "done"}, lesson("discovery", "why")}
	first, err := s.Save(in, Source{}, "capture-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	second, err := s.Save(in, Source{}, "capture-1", time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if len(first) != 2 || len(second) != 2 || len(s.Search(Query{})) != 2 || !s.Captured("capture-1") {
		t.Fatal("duplicate or missing capture")
	}
	_, err = s.Save([]Input{lesson("valid", "yes"), lesson("too big", strings.Repeat("x", MaxRecord))}, Source{}, "", time.Now())
	if !errors.Is(err, ErrInvalid) || len(s.Search(Query{})) != 2 {
		t.Fatal("partial or oversized batch accepted", err)
	}
	_, err = s.Save([]Input{lesson("big", strings.Repeat("x", MaxBatch+1))}, Source{}, "", time.Now())
	if !errors.Is(err, ErrInvalid) {
		t.Fatal("oversized batch accepted")
	}
}

func TestSearchFiltersRankingAndTombstones(t *testing.T) {
	s := testStore(t)
	a := saveOne(t, s, lesson("worktree cleanup", "specific evidence"))
	b := saveOne(t, s, lesson("other", "worktree cleanup evidence"))
	rows := s.Search(Query{Keywords: "WORKTREE cleanup", Repo: "host/a/app", Mission: "Fix sync", Operation: "op_a", Kind: "lesson"})
	if len(rows) != 2 || rows[0].ID != a.ID || rows[0].Body != "" || rows[0].Snippet == "" {
		t.Fatalf("results: %+v", rows)
	}
	if len(s.Search(Query{Keywords: "cleanup missing"})) != 0 || len(s.Search(Query{Repo: "host/b/app"})) != 0 {
		t.Fatal("filter mismatch")
	}
	if _, err := s.Update(a.ID, lesson("corrected", "current evidence"), false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Update(b.ID, Input{}, true, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, err := s.Get(b.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal(err)
	}
	if len(s.Search(Query{})) != 1 {
		t.Fatal("deleted record searchable")
	}
	entries, _ := s.Manifest("")
	if len(entries) != 2 {
		t.Fatal("deletion marker lost")
	}
}

func TestConcurrentWritesAndPagedManifest(t *testing.T) {
	s := testStore(t)
	var wg sync.WaitGroup
	for n := 0; n < PageSize+3; n++ {
		wg.Go(func() {
			_, err := s.Save([]Input{lesson(fmt.Sprintf("item %d", n), "body")}, Source{}, "", time.Now())
			if err != nil {
				t.Error(err)
			}
		})
	}
	wg.Wait()
	first, next := s.Manifest("")
	second, end := s.Manifest(next)
	if len(first) != PageSize || len(second) != 3 || next == "" || end != "" {
		t.Fatalf("pages %d %d %q %q", len(first), len(second), next, end)
	}
	if len(s.Search(Query{Limit: 999})) != 50 {
		t.Fatal("unbounded search")
	}
}

func TestMergeConvergesAndDeletionNeverResurrects(t *testing.T) {
	a, b := testStore(t), testStore(t)
	r := saveOne(t, a, lesson("initial", "body"))
	if err := b.Merge([]Record{r}, false); err != nil {
		t.Fatal(err)
	}
	ar, _ := a.Update(r.ID, lesson("primary", "body"), false, time.Now())
	br, _ := b.Update(r.ID, lesson("secondary", "body"), false, time.Now().Add(time.Hour))
	if err := a.Merge([]Record{br}, true); err != nil {
		t.Fatal(err)
	}
	if err := b.Merge([]Record{ar}, false); err != nil {
		t.Fatal(err)
	}
	got, _ := b.Get(r.ID)
	if got.Title != "primary" {
		t.Fatal("clock won over primary")
	}
	deleted, err := b.Update(r.ID, Input{}, true, time.Now())
	if err != nil {
		t.Fatal(err)
	}
	ar.Revision += 10
	if err = a.Merge([]Record{deleted}, true); err != nil {
		t.Fatal(err)
	}
	if err = a.Merge([]Record{ar}, true); err != nil {
		t.Fatal(err)
	}
	if _, err = a.Get(r.ID); !errors.Is(err, ErrNotFound) {
		t.Fatal("resurrected deletion")
	}
}
