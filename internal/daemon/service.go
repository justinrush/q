package daemon

import (
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"sync"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/state"
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
	store  *state.Store
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
	codex     CodexStatusReader

	codexApprovalMu sync.Mutex
	codexApprovals  map[domain.MissionID]codexApprovalCandidate
	inflight        inflight
}

// ServiceConfig configures a Service.
type ServiceConfig struct {
	Store  *state.Store
	Hub    *Hub
	Dirs   paths.Dirs
	Logger *slog.Logger
	Now    func() time.Time
}

// NewService returns a Service over the given store, publishing changes to its hub.
func NewService(cfg ServiceConfig) *Service {
	now := cfg.Now
	if now == nil {
		now = time.Now
	}

	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}

	return &Service{
		store:          cfg.Store,
		hub:            cfg.Hub,
		dirs:           cfg.Dirs,
		logger:         logger,
		now:            now,
		codexApprovals: make(map[domain.MissionID]codexApprovalCandidate),
	}
}

// SetLauncher attaches the component that provisions worktrees and starts agents.
func (s *Service) SetLauncher(l Launcher) { s.launcher = l }

// Snapshot returns the current state.
func (s *Service) Snapshot() state.Snapshot { return s.store.Snapshot() }

// CreateOperation adds an operation, assigning its slug and palette color.
func (s *Service) CreateOperation(req api.CreateOperationRequest) (domain.Operation, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return domain.Operation{}, fmt.Errorf("%w: operation name is required", ErrInvalid)
	}

	id, err := domain.NewOperationID()
	if err != nil {
		return domain.Operation{}, err
	}

	now := s.now()
	operation := domain.Operation{
		ID:        id,
		Name:      name,
		Slug:      domain.Slug(name),
		Summary:   strings.TrimSpace(req.Summary),
		Repos:     normalizeRepos(req.Repos),
		CreatedAt: now,
		UpdatedAt: now,
	}

	if err := s.store.Mutate("operation.create", func(snap *state.Snapshot) error {
		operation.ColorIdx = snap.NextColorIdx()
		snap.PutOperation(operation)

		return nil
	}); err != nil {
		return domain.Operation{}, err
	}

	// Re-read so the caller sees the color the store assigned.
	stored, _ := s.store.Snapshot().Operation(id)
	s.publishOperation(stored)

	return stored, nil
}

// UpdateOperation patches an operation in place.
func (s *Service) UpdateOperation(id domain.OperationID, req api.UpdateOperationRequest) (domain.Operation, error) {
	var updated domain.Operation

	err := s.store.Mutate("operation.update", func(snap *state.Snapshot) error {
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
			operation.Slug = domain.Slug(name)
		}

		if req.Summary != nil {
			operation.Summary = strings.TrimSpace(*req.Summary)
		}

		if req.Repos != nil {
			operation.Repos = normalizeRepos(*req.Repos)
		}

		if req.ColorIdx != nil {
			operation.ColorIdx = ((*req.ColorIdx % domain.PaletteSize) + domain.PaletteSize) % domain.PaletteSize
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
		return domain.Operation{}, err
	}

	s.publishOperation(updated)

	return updated, nil
}

// DeleteOperation removes an operation.
//
// Operations with missions that are not done are refused unless force is set. An operation
// is the only place a mission's repo list lives, so deleting one out from under a
// running agent would leave the mission unable to describe its own worktrees.
func (s *Service) DeleteOperation(id domain.OperationID, force bool) error {
	var removedMissions []domain.MissionID

	err := s.store.Mutate("operation.delete", func(snap *state.Snapshot) error {
		if _, ok := snap.Operation(id); !ok {
			return fmt.Errorf("%w: operation %s", ErrNotFound, id)
		}

		active := snap.ActiveMissionsForOperation(id)
		if len(active) > 0 && !force {
			return fmt.Errorf("%w: operation %s still has %d unfinished mission(s)", ErrConflict, id, len(active))
		}

		for _, mission := range snap.MissionsForOperation(id) {
			removedMissions = append(removedMissions, mission.ID)
			snap.DeleteMission(mission.ID)
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
func (s *Service) CreateMission(req api.CreateMissionRequest) (domain.Mission, error) {
	name := strings.TrimSpace(req.Name)
	if name == "" {
		return domain.Mission{}, fmt.Errorf("%w: mission name is required", ErrInvalid)
	}

	if strings.TrimSpace(req.Prompt) == "" {
		return domain.Mission{}, fmt.Errorf("%w: mission prompt is required", ErrInvalid)
	}

	tool := req.Tool
	if tool == "" {
		tool = domain.ToolClaude
	}

	if !tool.Valid() {
		return domain.Mission{}, fmt.Errorf("%w: unknown tool %q", ErrInvalid, req.Tool)
	}

	if req.PlanMode && !tool.SupportsPlanMode() {
		return domain.Mission{}, fmt.Errorf("%w: %s does not support plan mode", ErrInvalid, tool)
	}

	id, err := domain.NewMissionID()
	if err != nil {
		return domain.Mission{}, err
	}

	now := s.now()
	mission := domain.Mission{
		ID:          id,
		OperationID: req.OperationID,
		Name:        name,
		Slug:        domain.Slug(name),
		Tool:        tool,
		Prompt:      req.Prompt,
		PlanMode:    req.PlanMode,
		ExtraRepos:  normalizeRepos(req.ExtraRepos),
		Status:      domain.StatusBriefing,
		AgentState:  domain.AgentUnknown,
		CreatedAt:   now,
		UpdatedAt:   now,
	}

	err = s.store.Mutate("mission.create", func(snap *state.Snapshot) error {
		operation, ok := snap.Operation(req.OperationID)
		if !ok {
			return fmt.Errorf("%w: operation %s", ErrNotFound, req.OperationID)
		}

		_, combineErr := domain.MissionRepos(operation, mission)
		if combineErr != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, combineErr)
		}

		mission.Order = snap.NextOrder(domain.StatusBriefing)
		snap.PutMission(mission)

		return nil
	})
	if err != nil {
		return domain.Mission{}, err
	}

	stored, _ := s.store.Snapshot().Mission(id)
	s.publishMission(stored)

	return stored, nil
}

// UpdateMission patches a mission's editable fields.
func (s *Service) UpdateMission(id domain.MissionID, req api.UpdateMissionRequest) (domain.Mission, error) {
	var updated domain.Mission

	err := s.store.Mutate("mission.update", func(snap *state.Snapshot) error {
		mission, ok := snap.Mission(id)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}

		if err := applyMissionPatch(snap, &mission, req); err != nil {
			return err
		}

		mission.UpdatedAt = s.now()
		updated = mission
		snap.PutMission(mission)

		return nil
	})
	if err != nil {
		return domain.Mission{}, err
	}

	s.publishMission(updated)

	return updated, nil
}

// applyMissionPatch mutates mission according to req, validating as it goes.
func applyMissionPatch(snap *state.Snapshot, mission *domain.Mission, req api.UpdateMissionRequest) error {
	if req.OperationID != nil {
		if _, ok := snap.Operation(*req.OperationID); !ok {
			return fmt.Errorf("%w: operation %s", ErrNotFound, *req.OperationID)
		}

		mission.OperationID = *req.OperationID
	}

	if req.Name != nil {
		name := strings.TrimSpace(*req.Name)
		if name == "" {
			return fmt.Errorf("%w: mission name cannot be empty", ErrInvalid)
		}

		mission.Name = name
		mission.Slug = domain.Slug(name)
	}

	if req.Prompt != nil {
		mission.Prompt = *req.Prompt
	}

	if req.Order != nil {
		mission.Order = *req.Order
	}

	if req.ExtraRepos != nil {
		if mission.Status != domain.StatusBriefing {
			return fmt.Errorf("%w: cannot change repositories after launch", ErrConflict)
		}

		mission.ExtraRepos = normalizeRepos(*req.ExtraRepos)
	}

	// Tool and plan mode are only meaningful before launch: they are baked into
	// the agent's argv, so changing them afterwards would describe a session that
	// is not the one running.
	if req.Tool != nil || req.PlanMode != nil {
		if mission.Launched() {
			return fmt.Errorf("%w: cannot change tool or plan mode after launch", ErrConflict)
		}
	}

	if req.Tool != nil {
		if !req.Tool.Valid() {
			return fmt.Errorf("%w: unknown tool %q", ErrInvalid, *req.Tool)
		}

		mission.Tool = *req.Tool
	}

	if req.PlanMode != nil {
		mission.PlanMode = *req.PlanMode
	}

	if mission.PlanMode && !mission.Tool.SupportsPlanMode() {
		return fmt.Errorf("%w: %s does not support plan mode", ErrInvalid, mission.Tool)
	}

	if req.OperationID != nil || req.ExtraRepos != nil {
		operation, _ := snap.Operation(mission.OperationID)
		_, err := domain.MissionRepos(operation, *mission)
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
func (s *Service) SetStatus(id domain.MissionID, to domain.Status) (domain.Mission, error) {
	return s.setStatus(id, to, nil)
}

// setStatus applies a lane change and any state changes that must be stored with it.
// Keeping them in one mutation prevents clients from observing a half-finished mission.
func (s *Service) setStatus(
	id domain.MissionID,
	to domain.Status,
	mutate func(*domain.Mission),
) (domain.Mission, error) {
	if !to.Valid() {
		return domain.Mission{}, fmt.Errorf("%w: unknown status %q", ErrInvalid, to)
	}

	var updated domain.Mission

	err := s.store.Mutate("mission.status", func(snap *state.Snapshot) error {
		mission, ok := snap.Mission(id)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}

		if mission.Status == to {
			updated = mission

			return nil
		}

		now := s.now()
		mission.Status = to
		mission.Order = snap.NextOrder(to)
		mission.UpdatedAt = now

		if to == domain.StatusClosed {
			mission.FinishedAt = &now
		} else {
			mission.FinishedAt = nil
		}

		if mutate != nil {
			mutate(&mission)
		}

		updated = mission
		snap.PutMission(mission)

		return nil
	})
	if err != nil {
		return domain.Mission{}, err
	}

	s.publishMission(updated)

	return updated, nil
}

// DeleteMission removes a mission record.
//
// Reclaiming its worktrees and tmux session is the caller's responsibility;
// this only forgets the record.
func (s *Service) DeleteMission(id domain.MissionID) error {
	if err := s.store.Mutate("mission.delete", func(snap *state.Snapshot) error {
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
func normalizeRepos(repos []domain.Repo) []domain.Repo {
	out := make([]domain.Repo, 0, len(repos))

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

func (s *Service) publishOperation(t domain.Operation) {
	if s.hub != nil {
		s.hub.Broadcast(EventOperation, t)
	}
}

func (s *Service) publishMission(t domain.Mission) {
	if s.hub != nil {
		s.hub.Broadcast(EventMission, t)
	}
}

func (s *Service) publishDeleted(kind, id string) {
	if s.hub != nil {
		s.hub.Broadcast(EventDeleted, api.Deleted{Kind: kind, ID: id})
	}
}
