package daemon

import (
	"errors"
	"log/slog"
	"path/filepath"
	"testing"
	"time"

	"io"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
)

// newTestService returns a Service backed by a temp-directory store.
func newTestService(t *testing.T) *Service {
	t.Helper()

	root := t.TempDir()
	dirs := paths.Dirs{Data: filepath.Join(root, "data"), State: filepath.Join(root, "state")}

	store, err := mission.Open(dirs)
	if err != nil {
		t.Fatalf("state.Open: %v", err)
	}

	hub := NewHub()
	t.Cleanup(hub.Close)

	return NewService(store, hub, dirs,
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithClock(time.Now),
	)
}

// apply attaches dependencies after construction. Production wiring passes every
// option to NewService; tests build one service and vary a single dependency per
// case, which is the only reason this exists.
func (s *Service) apply(opts ...Option) {
	for _, opt := range opts {
		opt(s)
	}
}

// seedOperation creates an operation for mission tests.
func seedOperation(t *testing.T, svc *Service) mission.Operation {
	t.Helper()

	operation, err := svc.CreateOperation(api.CreateOperationRequest{Name: "Discussions API"})
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	return operation
}

func TestCreateOperationAssignsSlugAndColor(t *testing.T) {
	svc := newTestService(t)

	first, err := svc.CreateOperation(api.CreateOperationRequest{
		Name:    "  Discussions API  ",
		Summary: "  wire it up  ",
	})
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	if first.Name != "Discussions API" {
		t.Errorf("Name = %q, want it trimmed", first.Name)
	}

	if first.Slug != "discussions-api" {
		t.Errorf("Slug = %q", first.Slug)
	}

	if first.Summary != "wire it up" {
		t.Errorf("Summary = %q, want it trimmed", first.Summary)
	}

	if !first.ID.Valid() {
		t.Errorf("ID = %q is not valid", first.ID)
	}

	second, err := svc.CreateOperation(api.CreateOperationRequest{Name: "Other"})
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	// Distinct colors are the whole point of the per-operation stripe.
	if first.ColorIdx == second.ColorIdx {
		t.Errorf("both operations got color %d", first.ColorIdx)
	}
}

func TestCreateOperationRequiresName(t *testing.T) {
	svc := newTestService(t)

	_, err := svc.CreateOperation(api.CreateOperationRequest{Name: "   "})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestCreateOperationDropsIncompleteRepos(t *testing.T) {
	svc := newTestService(t)

	operation, err := svc.CreateOperation(api.CreateOperationRequest{
		Name: "Operation",
		Repos: []mission.Repo{
			{Name: "weave", Path: "/dev/weave"},
			{Name: "", Path: ""},
			{Name: "nameless", Path: ""},
		},
	})
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	if len(operation.Repos) != 1 {
		t.Fatalf("len(Repos) = %d, want 1: %+v", len(operation.Repos), operation.Repos)
	}
}

// An operation is the only place a mission's repo list lives, so deleting one out from
// under a running agent would leave the mission unable to describe its worktrees.
func TestDeleteOperationRefusesUnfinishedMissionsUnlessForced(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	if _, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "mission",
		Prompt:      "do it",
	}); err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	if err := svc.DeleteOperation(operation.ID, false); !errors.Is(err, ErrConflict) {
		t.Fatalf("err = %v, want ErrConflict", err)
	}

	if err := svc.DeleteOperation(operation.ID, true); err != nil {
		t.Fatalf("forced DeleteOperation: %v", err)
	}

	snap := svc.Snapshot()
	if len(snap.Operations) != 0 || len(snap.Missions) != 0 {
		t.Errorf("forced delete left %d operations and %d missions", len(snap.Operations), len(snap.Missions))
	}
}

// An operation whose missions are all done can be deleted without force.
func TestDeleteOperationAllowsFinishedMissions(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	ms, err := svc.CreateMission(api.CreateMissionRequest{OperationID: operation.ID, Name: "mission", Prompt: "do it"})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	if _, err := svc.SetStatus(ms.ID, mission.StatusClosed); err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	if err := svc.DeleteOperation(operation.ID, false); err != nil {
		t.Errorf("DeleteOperation: %v", err)
	}
}

func TestDeleteOperationUnknown(t *testing.T) {
	svc := newTestService(t)

	if err := svc.DeleteOperation("op_000000000000", false); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateMissionStartsInDraft(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	ms, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "Add Endpoint",
		Prompt:      "do the thing",
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	if ms.Status != mission.StatusBriefing {
		t.Errorf("Status = %q, want draft", ms.Status)
	}

	if ms.AgentState != mission.AgentUnknown {
		t.Errorf("AgentState = %q, want unknown", ms.AgentState)
	}

	// Defaulting to claude means --tool is optional at every call site.
	if ms.Tool != mission.ToolClaude {
		t.Errorf("Tool = %q, want claude by default", ms.Tool)
	}

	if ms.Slug != "add-endpoint" {
		t.Errorf("Slug = %q", ms.Slug)
	}

	if ms.Launched() {
		t.Error("a new mission must not be marked launched")
	}
}

// seedParentMission creates a mission carrying the full setup a follow-up would
// inherit, so tests can assert one field at a time against a known parent.
func seedParentMission(t *testing.T, svc *Service, operation mission.Operation) mission.Mission {
	t.Helper()

	ms, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID:  operation.ID,
		Name:         "Wire Up The API",
		Prompt:       "do the thing",
		Tool:         mission.ToolOpencode,
		Model:        "gpt-5-codex",
		Effort:       "high",
		PlanMode:     true,
		ExtraRepos:   []mission.Repo{{Name: "mac", Path: "/dev/mac"}},
		BaseBranches: map[string]string{"mac": "trunk"},
	})
	if err != nil {
		t.Fatalf("CreateMission parent: %v", err)
	}

	return ms
}

func TestCreateMissionInheritsSetupFromParent(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)
	parent := seedParentMission(t, svc, operation)

	child, err := svc.CreateMission(api.CreateMissionRequest{
		InheritFrom: parent.ID,
		Name:        "Add The Docs",
		Prompt:      "write it up",
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	if child.OperationID != operation.ID {
		t.Errorf("OperationID = %q, want %q from parent", child.OperationID, operation.ID)
	}

	if child.Tool != mission.ToolOpencode {
		t.Errorf("Tool = %q, want opencode from parent", child.Tool)
	}

	if child.Model != "gpt-5-codex" {
		t.Errorf("Model = %q, want gpt-5-codex from parent", child.Model)
	}

	if child.Effort != "high" {
		t.Errorf("Effort = %q, want high from parent", child.Effort)
	}

	if len(child.ExtraRepos) != 1 || child.ExtraRepos[0].Path != "/dev/mac" {
		t.Errorf("ExtraRepos = %+v, want the parent's mac repo", child.ExtraRepos)
	}

	if child.BaseBranches["mac"] != "trunk" {
		t.Errorf("BaseBranches = %+v, want mac on trunk from parent", child.BaseBranches)
	}

	// The inherited agent is not inherited, so the child is still a brief that
	// nobody has launched.
	if child.Status != mission.StatusBriefing {
		t.Errorf("Status = %q, want briefing", child.Status)
	}

	if child.Launched() {
		t.Error("an inherited child must not inherit the parent's launch state")
	}

	if child.Name != "Add The Docs" || child.Slug != "add-the-docs" {
		t.Errorf("Name/Slug = %q/%q, want the child's own", child.Name, child.Slug)
	}
}

func TestCreateMissionInheritsOperationWithoutNamingOne(t *testing.T) {
	svc := newTestService(t)
	first := seedOperation(t, svc)
	second := seedOperation(t, svc)
	parent := seedParentMission(t, svc, first)

	// A child may be sent nothing but the parent and a name. This is the shape
	// `q mission add --from` produces when an agent has no operation in hand, and
	// it has to be enough.
	child, err := svc.CreateMission(api.CreateMissionRequest{
		InheritFrom: parent.ID,
		Name:        "Follow Up",
		Prompt:      "do more",
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	if child.OperationID != first.ID {
		t.Errorf("OperationID = %q, want %q", child.OperationID, first.ID)
	}

	if child.OperationID == second.ID {
		t.Error("OperationID came from somewhere other than the parent")
	}
}

func TestCreateMissionExplicitFieldsBeatParent(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)
	parent := seedParentMission(t, svc, operation)

	child, err := svc.CreateMission(api.CreateMissionRequest{
		InheritFrom:  parent.ID,
		OperationID:  operation.ID,
		Name:         "Diverge",
		Prompt:       "do it differently",
		Tool:         mission.ToolClaude,
		Model:        "claude-opus-5",
		Effort:       "low",
		ExtraRepos:   []mission.Repo{{Name: "q", Path: "/dev/q"}},
		BaseBranches: map[string]string{"q": "main"},
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	if child.Tool != mission.ToolClaude {
		t.Errorf("Tool = %q, want the explicit claude", child.Tool)
	}

	if child.Model != "claude-opus-5" {
		t.Errorf("Model = %q, want the explicit model", child.Model)
	}

	if child.Effort != "low" {
		t.Errorf("Effort = %q, want the explicit effort", child.Effort)
	}

	// A list the caller names replaces the parent's outright. Merging would make
	// it impossible to narrow a mission's worktrees back down to a subset of what
	// the parent was given, and would quietly grow the set of repos a mission is
	// handed every time it is re-created with one repo added.
	if len(child.ExtraRepos) != 1 || child.ExtraRepos[0].Name != "q" {
		t.Errorf("ExtraRepos = %+v, want only the explicit repo", child.ExtraRepos)
	}

	if _, ok := child.BaseBranches["mac"]; ok {
		t.Errorf("BaseBranches = %+v, want the parent's map replaced", child.BaseBranches)
	}

	if child.BaseBranches["q"] != "main" {
		t.Errorf("BaseBranches = %+v, want q on main", child.BaseBranches)
	}
}

func TestCreateMissionDoesNotInheritPlanMode(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)
	parent := seedParentMission(t, svc, operation)

	if !parent.PlanMode {
		t.Fatal("the parent fixture must be in plan mode for this test to mean anything")
	}

	child, err := svc.CreateMission(api.CreateMissionRequest{
		InheritFrom: parent.ID,
		Name:        "Just Do It",
		Prompt:      "go",
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	// PlanMode is a bool on the wire with no third state, so an inherited value
	// could never be turned back off. Whether a mission stops for approval is
	// settled by whoever launches it, not by whoever wrote the brief.
	if child.PlanMode {
		t.Error("PlanMode was inherited from the parent")
	}
}

func TestCreateMissionInheritsFromUnknownMission(t *testing.T) {
	svc := newTestService(t)
	seedOperation(t, svc)

	_, err := svc.CreateMission(api.CreateMissionRequest{
		InheritFrom: "ms_does_not_exist",
		Name:        "Orphan",
		Prompt:      "go",
	})
	if !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

func TestCreateMissionRequiresAnOperationSomewhere(t *testing.T) {
	svc := newTestService(t)
	seedOperation(t, svc)

	// Neither named nor inherited. This has to read as a bad request rather than
	// as a missing operation, because there is no operation id to be missing.
	_, err := svc.CreateMission(api.CreateMissionRequest{
		Name:   "Homeless",
		Prompt: "go",
	})
	if !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestCreateMissionValidatesInheritedModel(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	parent, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "Parent",
		Prompt:      "go",
		Model:       "not a model",
	})
	if err == nil {
		t.Fatal("expected the parent to be rejected for its whitespace in the model")
	}

	// A parent cannot hold a model that q would refuse to put on a command line,
	// so this only pins the ordering: inheritance is validated the same way an
	// explicit value is, and cannot smuggle something past the check.
	if parent.ID != "" {
		t.Errorf("rejected create still returned %q", parent.ID)
	}
}

func TestCreateMissionStoresAdditionalRepos(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	ms, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "mission",
		Prompt:      "do it",
		ExtraRepos:  []mission.Repo{{Name: "mac", Path: " /dev/mac "}},
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	if len(ms.ExtraRepos) != 1 || ms.ExtraRepos[0].Path != "/dev/mac" {
		t.Fatalf("ExtraRepos = %+v, want normalized mac repo", ms.ExtraRepos)
	}
}

func TestCreateMissionRejectsRepoConflicts(t *testing.T) {
	svc := newTestService(t)
	operation, err := svc.CreateOperation(api.CreateOperationRequest{
		Name:  "operation",
		Repos: []mission.Repo{{Name: "q", Path: "/dev/q"}},
	})
	if err != nil {
		t.Fatalf("CreateOperation: %v", err)
	}

	_, err = svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "mission",
		Prompt:      "do it",
		ExtraRepos:  []mission.Repo{{Name: "q", Path: "/other/q"}},
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("CreateMission() error = %v, want ErrInvalid", err)
	}
}

func TestUpdateMissionRepositoriesOnlyWhileDraft(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)
	ms, err := svc.CreateMission(api.CreateMissionRequest{OperationID: operation.ID, Name: "mission", Prompt: "do it"})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	repos := []mission.Repo{{Name: "mac", Path: "/dev/mac"}}
	updated, err := svc.UpdateMission(ms.ID, api.UpdateMissionRequest{ExtraRepos: &repos})
	if err != nil {
		t.Fatalf("UpdateMission: %v", err)
	}

	if len(updated.ExtraRepos) != 1 || updated.ExtraRepos[0].Name != "mac" {
		t.Fatalf("ExtraRepos = %+v, want mac", updated.ExtraRepos)
	}

	updated.Status = mission.StatusActive
	err = svcStore(svc).Mutate("test.launching", func(snap *mission.Snapshot) error {
		snap.PutMission(updated)

		return nil
	})
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	_, err = svc.UpdateMission(ms.ID, api.UpdateMissionRequest{ExtraRepos: &repos})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("UpdateMission() error = %v, want ErrConflict", err)
	}
}

func TestCreateMissionValidation(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	for _, tc := range []struct {
		name string
		req  api.CreateMissionRequest
		want error
	}{
		{
			name: "no name",
			req:  api.CreateMissionRequest{OperationID: operation.ID, Prompt: "x"},
			want: ErrInvalid,
		},
		{
			name: "no prompt",
			req:  api.CreateMissionRequest{OperationID: operation.ID, Name: "x"},
			want: ErrInvalid,
		},
		{
			name: "unknown tool",
			req:  api.CreateMissionRequest{OperationID: operation.ID, Name: "x", Prompt: "y", Tool: "cursor"},
			want: ErrInvalid,
		},
		{
			name: "unknown operation",
			req:  api.CreateMissionRequest{OperationID: "op_000000000000", Name: "x", Prompt: "y"},
			want: ErrNotFound,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.CreateMission(tc.req); !errors.Is(err, tc.want) {
				t.Errorf("err = %v, want %v", err, tc.want)
			}
		})
	}
}

// codex 0.147.0 has no --permission-mode flag, so accepting the request would
// silently produce a mission that never stops for approval.
func TestCreateMissionRejectsPlanModeForCodex(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	_, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "mission",
		Prompt:      "x",
		Tool:        mission.ToolCodex,
		PlanMode:    true,
	})
	if !errors.Is(err, ErrInvalid) {
		t.Fatalf("err = %v, want ErrInvalid", err)
	}

	// The same request is fine for claude.
	if _, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "mission",
		Prompt:      "x",
		Tool:        mission.ToolClaude,
		PlanMode:    true,
	}); err != nil {
		t.Errorf("claude plan mode should be accepted: %v", err)
	}
}

// Switching a draft mission to codex must not silently keep a plan flag codex
// cannot honour.
func TestUpdateMissionRejectsPlanModeWhenSwitchingToCodex(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	ms, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID: operation.ID,
		Name:        "mission",
		Prompt:      "x",
		Tool:        mission.ToolClaude,
		PlanMode:    true,
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	codex := mission.ToolCodex

	if _, err := svc.UpdateMission(ms.ID, api.UpdateMissionRequest{Tool: &codex}); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

// Tool and plan mode are baked into the agent's argv, so changing them after
// launch would describe a session that is not the one running.
func TestUpdateMissionRefusesToolChangeAfterLaunch(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	ms, err := svc.CreateMission(api.CreateMissionRequest{OperationID: operation.ID, Name: "mission", Prompt: "x"})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	// Simulate a launch by stamping StartedAt directly.
	started := time.Now()
	ms.StartedAt = &started

	if err := svcStore(svc).Mutate("test.launch", func(snap *mission.Snapshot) error {
		snap.PutMission(ms)

		return nil
	}); err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	codex := mission.ToolCodex

	if _, err := svc.UpdateMission(ms.ID, api.UpdateMissionRequest{Tool: &codex}); !errors.Is(err, ErrConflict) {
		t.Errorf("tool change err = %v, want ErrConflict", err)
	}

	plan := true

	if _, err := svc.UpdateMission(ms.ID, api.UpdateMissionRequest{PlanMode: &plan}); !errors.Is(err, ErrConflict) {
		t.Errorf("plan change err = %v, want ErrConflict", err)
	}

	// Renaming a launched mission is still fine.
	name := "renamed"
	if _, err := svc.UpdateMission(ms.ID, api.UpdateMissionRequest{Name: &name}); err != nil {
		t.Errorf("rename after launch should be allowed: %v", err)
	}
}

func TestUpdateMissionRejectsEmptyName(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	ms, err := svc.CreateMission(api.CreateMissionRequest{OperationID: operation.ID, Name: "mission", Prompt: "x"})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	blank := "  "

	if _, err := svc.UpdateMission(ms.ID, api.UpdateMissionRequest{Name: &blank}); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

func TestSetStatusManagesFinishedAt(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	ms, err := svc.CreateMission(api.CreateMissionRequest{OperationID: operation.ID, Name: "mission", Prompt: "x"})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	done, err := svc.SetStatus(ms.ID, mission.StatusClosed)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	if done.FinishedAt == nil {
		t.Error("moving to closed should stamp FinishedAt")
	}

	// Pulling a card back out of done must clear the finish time, or the card
	// would claim to be both unfinished and finished.
	reopened, err := svc.SetStatus(ms.ID, mission.StatusDebrief)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	if reopened.FinishedAt != nil {
		t.Error("moving out of done should clear FinishedAt")
	}
}

func TestSetStatusValidation(t *testing.T) {
	svc := newTestService(t)

	if _, err := svc.SetStatus("ms_000000000000", mission.StatusDebrief); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}

	operation := seedOperation(t, svc)

	ms, err := svc.CreateMission(api.CreateMissionRequest{OperationID: operation.ID, Name: "mission", Prompt: "x"})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	if _, err := svc.SetStatus(ms.ID, mission.Status("nonsense")); !errors.Is(err, ErrInvalid) {
		t.Errorf("err = %v, want ErrInvalid", err)
	}
}

// Moving a mission to the lane it already occupies must be a no-op rather than
// reshuffling its position.
func TestSetStatusToSameLaneIsANoop(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	ms, err := svc.CreateMission(api.CreateMissionRequest{OperationID: operation.ID, Name: "mission", Prompt: "x"})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	same, err := svc.SetStatus(ms.ID, mission.StatusBriefing)
	if err != nil {
		t.Fatalf("SetStatus: %v", err)
	}

	if same.Order != ms.Order {
		t.Errorf("Order changed from %d to %d", ms.Order, same.Order)
	}
}

func TestDeleteMissionUnknown(t *testing.T) {
	svc := newTestService(t)

	if err := svc.DeleteMission("ms_000000000000"); !errors.Is(err, ErrNotFound) {
		t.Errorf("err = %v, want ErrNotFound", err)
	}
}

// svcStore exposes the service's store for tests that need to fabricate state a
// public method cannot produce, such as a launched mission.
func svcStore(s *Service) *mission.Store { return s.store }
