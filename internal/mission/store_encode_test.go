package mission

import (
	"bytes"
	"encoding/json"
	"fmt"
	"testing"
	"time"
)

// plainEncode is what the codec must reproduce byte for byte: the state files
// as json.Marshal writes them.
func plainEncode(t *testing.T, snap Snapshot) encoded {
	t.Helper()

	hot, closed := split(snap)

	hotData, err := json.Marshal(hot)
	if err != nil {
		t.Fatal(err)
	}

	closedData, err := json.Marshal(closed)
	if err != nil {
		t.Fatal(err)
	}

	return encoded{hot: append(hotData, '\n'), closed: append(closedData, '\n')}
}

func codecBoard() Snapshot {
	snap := Snapshot{SchemaVersion: SchemaVersion, Operations: []Operation{sampleOperation("Operation")}}

	for i := range 6 {
		status := StatusDebrief
		if i%2 == 0 {
			status = StatusClosed
		}

		m := ms(fmt.Sprintf("ms_%012d", i), status, i)
		m.Prompt = `a brief with "quotes", a fake "missions":null and </html>`
		m.CodexUsage = CodexUsage{"/rollout": {"resp": {Model: "gpt", At: time.Unix(int64(i), 0).UTC()}}}
		m.Usage = Usage{PerModel: map[string]ModelTokens{"gpt": {Input: int64(i)}}}
		snap.Missions = append(snap.Missions, m)
	}

	snap.captureAnalytics(Snapshot{})

	return snap
}

// The codec reuses an unchanged mission's encoding, so whatever happens to
// the board between encodings, its output must be what json.Marshal writes.
func TestCodecEncodesExactlyAsJSONDoes(t *testing.T) {
	// A fresh codec has no last encoding to reuse, which must not be mistaken
	// for an empty one.
	empty := Snapshot{SchemaVersion: SchemaVersion, Operations: []Operation{sampleOperation("Operation")}}

	var fresh missionCodec

	if got, err := fresh.encode(empty); err != nil || !bytes.Equal(got.hot, plainEncode(t, empty).hot) {
		t.Fatalf("empty board: %s, %v; want %s", got.hot, err, plainEncode(t, empty).hot)
	}

	var codec missionCodec

	snap := codecBoard()
	steps := []struct {
		name   string
		change func(*Snapshot)
	}{
		{"first encoding", func(*Snapshot) {}},
		{"unchanged", func(*Snapshot) {}},
		{"one mission changed", func(s *Snapshot) { s.Missions[1].WaitingFor = "Bash" }},
		{"a shared map replaced", func(s *Snapshot) {
			s.Missions[3].CodexUsage = CodexUsage{"/rollout": {"resp": {Model: "other"}}}
		}},
		{"a mission closed", func(s *Snapshot) { s.Missions[1].Status = StatusClosed }},
		{"a mission reopened", func(s *Snapshot) { s.Missions[0].Status = StatusDebrief }},
		{"a mission added", func(s *Snapshot) { s.Missions = append(s.Missions, ms("ms_new000000000", StatusActive, 9)) }},
		{"a mission deleted", func(s *Snapshot) { s.DeleteMission(s.Missions[2].ID) }},
		{"no hot missions", func(s *Snapshot) {
			for i := range s.Missions {
				s.Missions[i].Status = StatusClosed
			}
		}},
		{"no missions", func(s *Snapshot) { s.Missions = nil }},
	}

	for _, step := range steps {
		next := snap.Clone()
		step.change(&next)
		snap = next

		got, err := codec.encode(snap)
		if err != nil {
			t.Fatalf("%s: %v", step.name, err)
		}

		want := plainEncode(t, snap)
		if !bytes.Equal(got.hot, want.hot) {
			t.Errorf("%s: hot file differs\n got %s\nwant %s", step.name, got.hot, want.hot)
		}

		if !bytes.Equal(got.closed, want.closed) {
			t.Errorf("%s: closed file differs\n got %s\nwant %s", step.name, got.closed, want.closed)
		}

		if len(codec.cache) != len(snap.Missions) {
			t.Errorf("%s: codec caches %d missions, board has %d", step.name, len(codec.cache), len(snap.Missions))
		}
	}
}
