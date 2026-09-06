package daemon

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// fakeProber stands in for an agent that can be asked what it offers.
type fakeProber struct {
	tool  mission.Tool
	set   mission.ModelSet
	err   error
	calls int
}

func (p *fakeProber) Tool() mission.Tool { return p.tool }

func (p *fakeProber) Probe(context.Context) (mission.ModelSet, error) {
	p.calls++

	if p.err != nil {
		return mission.ModelSet{}, p.err
	}

	return p.set, nil
}

// claudeSet is a small catalog shaped like a real answer.
func claudeSet() mission.ModelSet {
	return mission.ModelSet{
		Default:  "opus",
		ProbedAt: time.Now(),
		Options: []mission.ModelOption{
			{Value: "opus", Label: "Opus", Efforts: []string{"low", "high"}},
			{Value: "haiku", Label: "Haiku"},
		},
	}
}

func TestRefreshModels(t *testing.T) {
	cases := []struct {
		name    string
		probers []*fakeProber
		// wantDefault is the default expected for claude, "" when none.
		wantDefault string
		// wantErr is a fragment of the error recorded against claude.
		wantErr string
	}{
		{
			name:        "a successful probe becomes the catalog",
			probers:     []*fakeProber{{tool: mission.ToolClaude, set: claudeSet()}},
			wantDefault: "opus",
		},
		{
			name: "a failed probe is recorded without clearing what is known",
			probers: []*fakeProber{
				{tool: mission.ToolClaude, err: errors.New("not logged in")},
			},
			wantErr: "not logged in",
		},
		{
			name: "each agent is asked independently",
			probers: []*fakeProber{
				{tool: mission.ToolClaude, set: claudeSet()},
				{tool: mission.ToolCodex, err: errors.New("no codex")},
			},
			wantDefault: "opus",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)

			for _, p := range tc.probers {
				WithProber(p)(svc)
			}

			sets := svc.RefreshModels(context.Background())

			got := sets[mission.ToolClaude]

			if got.Default != tc.wantDefault {
				t.Errorf("claude default = %q, want %q", got.Default, tc.wantDefault)
			}

			if tc.wantErr != "" && !strings.Contains(got.Err, tc.wantErr) {
				t.Errorf("claude error = %q, want it to contain %q", got.Err, tc.wantErr)
			}

			for _, p := range tc.probers {
				if p.calls != 1 {
					t.Errorf("%s was probed %d times, want once", p.tool, p.calls)
				}
			}
		})
	}
}

// TestRefreshModelsKeepsStaleAnswer covers the case that matters when an agent
// goes away: the board should keep offering the models it knew, marked stale,
// rather than emptying its list.
func TestRefreshModelsKeepsStaleAnswer(t *testing.T) {
	svc := newTestService(t)
	prober := &fakeProber{tool: mission.ToolClaude, set: claudeSet()}
	WithProber(prober)(svc)

	svc.RefreshModels(context.Background())

	prober.err = errors.New("gone")
	sets := svc.RefreshModels(context.Background())

	got := sets[mission.ToolClaude]

	if got.Default != "opus" || len(got.Options) != 2 {
		t.Errorf("catalog = %+v, want the previous answer retained", got)
	}

	if got.Err == "" {
		t.Error("Err is empty, so nothing would tell the board the answer is stale")
	}
}

func TestModelCacheRoundTrips(t *testing.T) {
	svc := newTestService(t)
	WithProber(&fakeProber{tool: mission.ToolClaude, set: claudeSet()})(svc)

	svc.RefreshModels(context.Background())

	// A second service over the same directories stands in for a restarted
	// daemon, which must serve models before its own first probe returns.
	restarted := NewService(svc.store, svc.hub, svc.dirs)
	restarted.loadModelCache()

	got := restarted.ModelsFor(mission.ToolClaude)
	if got.Default != "opus" {
		t.Errorf("default after restart = %q, want opus", got.Default)
	}

	if len(got.Options) != 2 {
		t.Errorf("options after restart = %d, want 2", len(got.Options))
	}
}

func TestModelCacheToleratesRubbish(t *testing.T) {
	cases := []struct {
		name string
		// content is what sits at the cache path, absent when nil.
		content []byte
	}{
		{name: "no cache at all"},
		{name: "unparseable json", content: []byte("{not json")},
		{name: "an empty document", content: []byte("{}")},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)

			if tc.content != nil {
				if err := os.MkdirAll(svc.dirs.State, 0o755); err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(svc.modelCachePath(), tc.content, 0o600); err != nil {
					t.Fatal(err)
				}
			}

			// Loading must not panic and must leave an empty catalog, which the
			// board renders as "not asked yet" rather than as an agent with no models.
			svc.loadModelCache()

			if len(svc.Models()) != 0 {
				t.Errorf("Models() = %v, want empty", svc.Models())
			}
		})
	}
}

// TestModelCacheIsValidJSON guards the file the next daemon start has to read.
func TestModelCacheIsValidJSON(t *testing.T) {
	svc := newTestService(t)
	WithProber(&fakeProber{tool: mission.ToolClaude, set: claudeSet()})(svc)

	svc.RefreshModels(context.Background())

	data, err := os.ReadFile(svc.modelCachePath())
	if err != nil {
		t.Fatalf("reading the cache: %v", err)
	}

	var sets map[mission.Tool]mission.ModelSet
	if err := json.Unmarshal(data, &sets); err != nil {
		t.Fatalf("the cache is not valid JSON: %v", err)
	}

	if _, ok := sets[mission.ToolClaude]; !ok {
		t.Errorf("cache = %v, want a claude entry", sets)
	}
}

// TestRefreshModelsWithoutProbersWritesNothing keeps a daemon with no agents
// from replacing a cache written by one that had them.
func TestRefreshModelsWithoutProbersWritesNothing(t *testing.T) {
	svc := newTestService(t)

	svc.RefreshModels(context.Background())

	if _, err := os.Stat(svc.modelCachePath()); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("stat = %v, want the cache never to have been written", err)
	}
}

func TestCreateMissionModel(t *testing.T) {
	cases := []struct {
		name    string
		model   string
		effort  string
		wantErr string
	}{
		{name: "a model and effort are stored", model: "opus", effort: "high"},
		{name: "neither is required"},
		{
			name:    "a model that would be read as a flag is refused",
			model:   "--dangerously-skip-permissions",
			wantErr: "must not start with a dash",
		},
		{
			name:    "a model containing whitespace is refused",
			model:   "opus --effort max",
			wantErr: "must not contain whitespace",
		},
		{
			name:    "an effort is checked the same way",
			effort:  "-high",
			wantErr: "must not start with a dash",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			operation := seedOperation(t, svc)

			ms, err := svc.CreateMission(api.CreateMissionRequest{
				OperationID: operation.ID,
				Name:        "probe",
				Prompt:      "do the thing",
				Model:       tc.model,
				Effort:      tc.effort,
			})

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}

				if !errors.Is(err, ErrInvalid) {
					t.Errorf("error = %v, want it to wrap ErrInvalid", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("CreateMission: %v", err)
			}

			if ms.Model != tc.model || ms.Effort != tc.effort {
				t.Errorf("model/effort = %q/%q, want %q/%q", ms.Model, ms.Effort, tc.model, tc.effort)
			}
		})
	}
}

// TestUpdateMissionModelFrozenAfterLaunch covers the rule the model shares with
// the tool: it is baked into the running agent's argv, so changing it afterwards
// would make the card describe a session that is not the one running.
func TestUpdateMissionModelFrozenAfterLaunch(t *testing.T) {
	cases := []struct {
		name string
		// launched marks the mission as started before the patch.
		launched bool
		wantErr  bool
	}{
		{name: "a briefing mission can change model", launched: false},
		{name: "a launched mission cannot", launched: true, wantErr: true},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			operation := seedOperation(t, svc)

			ms, err := svc.CreateMission(api.CreateMissionRequest{
				OperationID: operation.ID,
				Name:        "probe",
				Prompt:      "do the thing",
				Model:       "opus",
			})
			if err != nil {
				t.Fatalf("CreateMission: %v", err)
			}

			if tc.launched {
				started := time.Now()

				err = svc.store.Mutate("test.launch", func(snap *mission.Snapshot) error {
					stored, _ := snap.Mission(ms.ID)
					stored.StartedAt = &started
					snap.PutMission(stored)

					return nil
				})
				if err != nil {
					t.Fatalf("marking the mission launched: %v", err)
				}
			}

			sonnet := "sonnet"

			updated, err := svc.UpdateMission(ms.ID, api.UpdateMissionRequest{Model: &sonnet})

			if tc.wantErr {
				if !errors.Is(err, ErrConflict) {
					t.Fatalf("error = %v, want it to wrap ErrConflict", err)
				}

				return
			}

			if err != nil {
				t.Fatalf("UpdateMission: %v", err)
			}

			if updated.Model != "sonnet" {
				t.Errorf("Model = %q, want sonnet", updated.Model)
			}
		})
	}
}

// TestUpdateMissionLeavesModelAloneWhenUnset pins the patch semantics: a client
// that sends only a name must not clear the model as a side effect.
func TestUpdateMissionLeavesModelAloneWhenUnset(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	ms, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "probe",
		Prompt:      "do the thing",
		Model:       "opus",
		Effort:      "high",
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	name := "renamed"

	updated, err := svc.UpdateMission(ms.ID, api.UpdateMissionRequest{Name: &name})
	if err != nil {
		t.Fatalf("UpdateMission: %v", err)
	}

	if updated.Model != "opus" || updated.Effort != "high" {
		t.Errorf("model/effort = %q/%q, want them untouched", updated.Model, updated.Effort)
	}
}

// TestWriteFileAtomicReplaces covers the property the cache depends on: the
// previous file survives until the new one is complete.
func TestWriteFileAtomicReplaces(t *testing.T) {
	path := filepath.Join(t.TempDir(), "cache.json")

	if err := writeFileAtomic(path, []byte("first")); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	if err := writeFileAtomic(path, []byte("second")); err != nil {
		t.Fatalf("writeFileAtomic: %v", err)
	}

	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}

	if string(data) != "second" {
		t.Errorf("content = %q, want second", data)
	}

	// A leftover temporary file would accumulate on every refresh.
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}

	if len(entries) != 1 {
		t.Errorf("directory holds %d files, want only the target", len(entries))
	}
}
