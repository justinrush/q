package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
)

// Sentinel errors the HTTP layer maps to status codes.
var (
	// ErrNotFound reports a missing operation or mission.
	ErrNotFound = errors.New("not found")
	// ErrInvalid reports a request the caller must fix.
	ErrInvalid = errors.New("invalid request")
	// ErrConflict reports a request that is valid but not allowed in the current
	// state, such as deleting an operation that still has running missions.
	ErrConflict = errors.New("conflict")
)

// Service applies q's rules on top of the state store.
//
// It is deliberately free of HTTP concerns and of subprocess work, so the rules
// (a mission starts in draft, a codex mission cannot use plan mode, an operation with live
// missions cannot be deleted) are unit-testable without a server or a git tree.
type Service struct {
	store  *mission.Store
	hub    *Hub
	dirs   paths.Dirs
	logger *slog.Logger
	now    func() time.Time

	// launcher is nil in tests and in any daemon that has no agent tooling
	// available, in which case launching is refused rather than attempted.
	launcher Launcher
	// probe is nil when tmux is unavailable, in which case session liveness is not
	// checked and cards rely on hooks alone.
	probe SessionProbe
	// debriefer and messenger are nil without agent tooling, in which case opening a
	// debrief or talking to a session is refused with an explanation.
	debriefer Debriefer
	messenger Messenger
	reclaimer Reclaimer
	// runtimes and healers are the agents that can report on their own live
	// sessions. Neither is required: without them the board relies on hooks alone.
	runtimes map[mission.Tool]mission.Runtime
	healers  []mission.Healer

	approvalMu sync.Mutex
	approvals  map[mission.MissionID]approvalCandidate
	inflight   inflight
}

// Option configures a Service.
type Option func(*Service)

// WithClock replaces the time source, for tests.
func WithClock(now func() time.Time) Option {
	return func(s *Service) { s.now = now }
}

// WithLogger sets where the service reports problems it does not fail on.
func WithLogger(l *slog.Logger) Option {
	return func(s *Service) { s.logger = l }
}

// WithLauncher attaches the component that provisions worktrees and starts agents.
func WithLauncher(l Launcher) Option {
	return func(s *Service) { s.launcher = l }
}

// WithProbe attaches the component that reports whether a session is still live.
func WithProbe(p SessionProbe) Option {
	return func(s *Service) { s.probe = p }
}

// WithDebriefer attaches the component that opens a mission for debrief.
func WithDebriefer(d Debriefer) Option {
	return func(s *Service) { s.debriefer = d }
}

// WithRuntime attaches an agent runtime whose readings are authoritative.
func WithRuntime(tool mission.Tool, r mission.Runtime) Option {
	return func(s *Service) { s.runtimes[tool] = r }
}

// WithHealer attaches an agent registry whose readings correct a dropped hook.
func WithHealer(h mission.Healer) Option {
	return func(s *Service) { s.healers = append(s.healers, h) }
}

// NewService returns a Service over the given store, publishing changes to hub.
//
// Every dependency past the store is optional and supplied as an [Option], so a
// daemon with no agent tooling available still serves the board and refuses the
// actions it cannot perform, rather than failing to start.
func NewService(store *mission.Store, hub *Hub, dirs paths.Dirs, opts ...Option) *Service {
	s := &Service{
		store:     store,
		hub:       hub,
		dirs:      dirs,
		logger:    slog.Default(),
		now:       time.Now,
		runtimes:  make(map[mission.Tool]mission.Runtime),
		approvals: make(map[mission.MissionID]approvalCandidate),
	}

	for _, opt := range opts {
		opt(s)
	}

	return s
}

// Hub returns the service's event hub, which the server publishes from.
func (s *Service) Hub() *Hub { return s.hub }

// Snapshot returns the current state.
func (s *Service) Snapshot() mission.Snapshot { return s.store.Snapshot() }

// CreateOperation adds an operation, assigning its slug and palette color.
func (s *Service) CreateOperation(req api.CreateOperationRequest) (mission.Operation, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return mission.Operation{}, fmt.Errorf("%w: operation name is required", ErrInvalid)
	}

	id, err := mission.NewOperationID()
	if err != nil {
		return mission.Operation{}, err
	}

	now := s.now()
	operation := mission.Operation{
		ID:        id,
		Name:      name,
		Slug:      mission.Slug(name),
		Summary:   strings.TrimSpace(req.Summary),
		Repos:     normalizeRepos(req.Repos),
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.store.Mutate("operation.create", func(snap *mission.Snapshot) error {
		operation.ColorIdx = snap.NextColorIdx()
		snap.PutOperation(operation)

		return nil
	}); err != nil {
		return mission.Operation{}, err
	}

	// Re-read so the caller sees the color the store assigned.
	stored, _ := s.store.Snapshot().Operation(id)
	s.publishOperation(stored)

	return stored, nil
}

// UpdateOperation patches an operation in place.
func (s *Service) UpdateOperation(id mission.OperationID, req api.UpdateOperationRequest) (mission.Operation, error) {
	var updated mission.Operation

	err := s.store.Mutate("operation.update", func(snap *mission.Snapshot) error {
		operation, ok := snap.Operation(id)
		if !ok {
			return fmt.Errorf("%w: operation %s", ErrNotFound, id)
		}

		if req.Name != nil {
			name := strings.TrimSpace(*req.Name)
			if name == "" {
				return fmt.Errorf("%w: operation name cannot be empty", ErrInvalid)
			}

			operation.Name = name
			operation.Slug = mission.Slug(name)
		}

		if req.Summary != nil {
			operation.Summary = strings.TrimSpace(*req.Summary)
		}

		if req.Repos != nil {
			operation.Repos = normalizeRepos(*req.Repos)
		}

		if req.ColorIdx != nil {
			operation.ColorIdx = ((*req.ColorIdx % mission.PaletteSize) + mission.PaletteSize) % mission.PaletteSize
		}

		if req.Archived != nil {
			operation.Archived = *req.Archived
		}

		operation.UpdatedAt = s.now()
		updated = operation
		snap.PutOperation(operation)

		return nil
	})
	if err != nil {
		return mission.Operation{}, err
	}

	s.publishOperation(updated)

	return updated, nil
}

// DeleteOperation removes an operation.
//
// Operations with missions that are not done are refused unless force is set. An operation
// is the only place a mission's repo list lives, so deleting one out from under a
// running agent would leave the mission unable to describe its own worktrees.
func (s *Service) DeleteOperation(id mission.OperationID, force bool) error {
	var removedMissions []mission.MissionID

	err := s.store.Mutate("operation.delete", func(snap *mission.Snapshot) error {
		if _, ok := snap.Operation(id); !ok {
			return fmt.Errorf("%w: operation %s", ErrNotFound, id)
		}

		active := snap.ActiveMissionsForOperation(id)
		if len(active) > 0 && !force {
			return fmt.Errorf("%w: operation %s still has %d unfinished mission(s)", ErrConflict, id, len(active))
		}

		for _, ms := range snap.MissionsForOperation(id) {
			removedMissions = append(removedMissions, ms.ID)
			snap.DeleteMission(ms.ID)
		}

		snap.DeleteOperation(id)

		return nil
	})
	if err != nil {
		return err
	}

	for _, missionID := range removedMissions {
		s.publishDeleted(api.KindMission, string(missionID))
	}

	s.publishDeleted(api.KindOperation, string(id))

	return nil
}

// CreateMission adds a mission in the draft lane.
func (s *Service) CreateMission(req api.CreateMissionRequest) (mission.Mission, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return mission.Mission{}, fmt.Errorf("%w: mission name is required", ErrInvalid)
	}

	if strings.TrimSpace(req.Prompt) == "" {
		return mission.Mission{}, fmt.Errorf("%w: mission prompt is required", ErrInvalid)
	}

	tool := req.Tool
	if tool == "" {
		tool = mission.DefaultTool
	}

	if !tool.Valid() {
		return mission.Mission{}, fmt.Errorf("%w: unknown tool %q", ErrInvalid, req.Tool)
	}

	if req.PlanMode && !tool.SupportsPlanMode() {
		return mission.Mission{}, fmt.Errorf("%w: %s does not support plan mode", ErrInvalid, tool)
	}

	id, err := mission.NewMissionID()
	if err != nil {
		return mission.Mission{}, err
	}

	now := s.now()
	ms := mission.Mission{
		ID:          id,
		OperationID: req.OperationID,
		Name:        name,
		Slug:        mission.Slug(name),
		Tool:        tool,
		Prompt:      req.Prompt,
		PlanMode:    req.PlanMode,
		ExtraRepos:  normalizeRepos(req.ExtraRepos),
		Status:      mission.StatusBriefing,
		AgentState:  mission.AgentUnknown,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	err = s.store.Mutate("mission.create", func(snap *mission.Snapshot) error {
		operation, ok := snap.Operation(req.OperationID)
		if !ok {
			return fmt.Errorf("%w: operation %s", ErrNotFound, req.OperationID)
		}

		_, combineErr := mission.MissionRepos(operation, ms)
		if combineErr != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, combineErr)
		}

		ms.Order = snap.NextOrder(mission.StatusBriefing)
		snap.PutMission(ms)

		return nil
	})
	if err != nil {
		return mission.Mission{}, err
	}

	stored, _ := s.store.Snapshot().Mission(id)
	s.publishMission(stored)

	return stored, nil
}

// UpdateMission patches a mission's editable fields.
func (s *Service) UpdateMission(id mission.MissionID, req api.UpdateMissionRequest) (mission.Mission, error) {
	var updated mission.Mission

	err := s.store.Mutate("mission.update", func(snap *mission.Snapshot) error {
		ms, ok := snap.Mission(id)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}

		if err := applyMissionPatch(snap, &ms, req); err != nil {
			return err
		}

		ms.UpdatedAt = s.now()
		updated = ms
		snap.PutMission(ms)

		return nil
	})
	if err != nil {
		return mission.Mission{}, err
	}

	s.publishMission(updated)

	return updated, nil
}

// applyMissionPatch mutates mission according to req, validating as it goes.
func applyMissionPatch(snap *mission.Snapshot, ms *mission.Mission, req api.UpdateMissionRequest) error {
	if req.OperationID != nil {
		if _, ok := snap.Operation(*req.OperationID); !ok {
			return fmt.Errorf("%w: operation %s", ErrNotFound, *req.OperationID)
		}

		ms.OperationID = *req.OperationID
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return fmt.Errorf("%w: mission name cannot be empty", ErrInvalid)
		}

		ms.Name = name
		ms.Slug = mission.Slug(name)
	}

	if req.Prompt != nil {
		ms.Prompt = *req.Prompt
	}

	if req.Order != nil {
		ms.Order = *req.Order
	}

	if req.ExtraRepos != nil {
		if ms.Status != mission.StatusBriefing {
			return fmt.Errorf("%w: cannot change repositories after launch", ErrConflict)
		}

		ms.ExtraRepos = normalizeRepos(*req.ExtraRepos)
	}

	// Tool and plan mode are only meaningful before launch: they are baked into
	// the agent's argv, so changing them afterwards would describe a session that
	// is not the one running.
	if req.Tool != nil || req.PlanMode != nil {
		if ms.Launched() {
			return fmt.Errorf("%w: cannot change tool or plan mode after launch", ErrConflict)
		}
	}

	if req.Tool != nil {
		if !req.Tool.Valid() {
			return fmt.Errorf("%w: unknown tool %q", ErrInvalid, *req.Tool)
		}

		ms.Tool = *req.Tool
	}

	if req.PlanMode != nil {
		ms.PlanMode = *req.PlanMode
	}

	if ms.PlanMode && !ms.Tool.SupportsPlanMode() {
		return fmt.Errorf("%w: %s does not support plan mode", ErrInvalid, ms.Tool)
	}

	if req.OperationID != nil || req.ExtraRepos != nil {
		operation, _ := snap.Operation(ms.OperationID)
		_, err := mission.MissionRepos(operation, *ms)
		if err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}

	return nil
}

// SetStatus moves a mission to another lane.
//
// This is bookkeeping only. Launching an agent, delivering a follow-up message,
// and resuming a dead session are side effects layered on top of this by the
// orchestration packages; keeping them out of here is what makes the lane rules
// testable on their own.
func (s *Service) SetStatus(id mission.MissionID, to mission.Status) (mission.Mission, error) {
	return s.setStatus(id, to, nil)
}

// setStatus applies a lane change and any state changes that must be stored with it.
// Keeping them in one mutation prevents clients from observing a half-finished mission.
func (s *Service) setStatus(
	id mission.MissionID,
	to mission.Status,
	mutate func(*mission.Mission),
) (mission.Mission, error) {
	if !to.Valid() {
		return mission.Mission{}, fmt.Errorf("%w: unknown status %q", ErrInvalid, to)
	}

	var updated mission.Mission

	err := s.store.Mutate("mission.status", func(snap *mission.Snapshot) error {
		ms, ok := snap.Mission(id)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}

		if ms.Status == to {
			updated = ms

			return nil
		}

		now := s.now()
		ms.Status = to
		ms.Order = snap.NextOrder(to)
		ms.UpdatedAt = now

		if to == mission.StatusClosed {
			ms.FinishedAt = &now
		} else {
			ms.FinishedAt = nil
		}

		if mutate != nil {
			mutate(&ms)
		}

		updated = ms
		snap.PutMission(ms)

		return nil
	})
	if err != nil {
		return mission.Mission{}, err
	}

	s.publishMission(updated)

	return updated, nil
}

// DeleteMission removes a mission record.
//
// Reclaiming its worktrees and tmux session is the caller's responsibility;
// this only forgets the record.
func (s *Service) DeleteMission(id mission.MissionID) error {
	if err := s.store.Mutate("mission.delete", func(snap *mission.Snapshot) error {
		if !snap.DeleteMission(id) {
			return fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}

		return nil
	}); err != nil {
		return err
	}

	s.publishDeleted(api.KindMission, string(id))

	return nil
}

// normalizeRepos trims and drops entries missing a name or path.
func normalizeRepos(repos []mission.Repo) []mission.Repo {
	out := make([]mission.Repo, 0, len(repos))

	for _, r := range repos {
		r.Name = strings.TrimSpace(r.Name)
		r.Path = strings.TrimSpace(r.Path)
		r.CommonDir = strings.TrimSpace(r.CommonDir)
		r.DefaultBranch = strings.TrimSpace(r.DefaultBranch)

		if r.Name == "" || r.Path == "" {
			continue
		}

		out = append(out, r)
	}

	return out
}

func (s *Service) publishOperation(t mission.Operation) {
	if s.hub != nil {
		s.hub.Broadcast(api.EventOperation, t)
	}
}

func (s *Service) publishMission(t mission.Mission) {
	if s.hub != nil {
		s.hub.Broadcast(api.EventMission, t)
	}
}

func (s *Service) publishDeleted(kind, id string) {
	if s.hub != nil {
		s.hub.Broadcast(api.EventDeleted, api.Deleted{Kind: kind, ID: id})
	}
}
