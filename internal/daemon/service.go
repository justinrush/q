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
// (a mission starts in briefing, a codex mission cannot use plan mode, an operation with
// live missions cannot be deleted) are unit-testable without a server or a git tree.
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
	// meters are the agents that can report what a session has consumed. An
	// agent without one simply carries no cost on its cards.
	meters map[mission.Tool]mission.Meter
	// probers are the agents that can be asked which models they offer, and
	// models is what they last answered. Without a prober the catalog is whatever
	// was last cached, and failing that empty, which leaves every mission on its
	// agent's own default.
	probers      []mission.ModelProber
	models       *catalog
	modelRefresh time.Duration
	// brancher answers which branches a repo could be based on. It is nil
	// without git, in which case the briefing form offers no list and a base
	// branch can still be typed.
	brancher Brancher
	branches *branchCache

	// maxConcurrent is how many queued missions this host runs at once.
	maxConcurrent int

	// remote is the paired daemon this one dials. It is nil unless this host is
	// the primary of a pair, and its presence is what makes it one.
	remote  Remote
	locator RepoLocator
	// syncInterval and takeoverAfter are the primary's settings for the pair.
	syncInterval  time.Duration
	takeoverAfter time.Duration
	// syncMu allows one exchange at a time, and syncKick asks for one soon.
	syncMu   sync.Mutex
	syncKick chan struct{}
	link     link
	// witness is where the pair's claim is kept, nil when there is none. vouch
	// is what it last said.
	witness Witness
	vouch   vouch
	// started is when this service was built, which a secondary treats as the
	// last time it heard from its primary until an exchange says otherwise.
	started  time.Time
	hostName string
	version  string

	// worktrees snapshots and mirrors mission worktrees; nil without git.
	worktrees Worktrees
	transfers transferLog
	// captured is this host's report from the exchange in progress. A secondary
	// takes it when the exchange arrives and settles against it a moment later,
	// when the primary says the snapshots have landed.
	captured []mission.RepoState

	// self is this installation's host id, fixed for the life of the store. It
	// is what every "do I run this mission" question is answered against.
	self mission.HostID

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

// WithMeter attaches an agent meter, which reports what its sessions consume.
func WithMeter(m mission.Meter) Option {
	return func(s *Service) { s.meters[m.Tool()] = m }
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
		meters:    make(map[mission.Tool]mission.Meter),
		models:    newCatalog(),
		branches:  newBranchCache(),
		approvals: make(map[mission.MissionID]approvalCandidate),

		maxConcurrent: DefaultMaxConcurrent,
		syncInterval:  DefaultSyncInterval,
		takeoverAfter: DefaultTakeoverAfter,
		syncKick:      make(chan struct{}, 1),
	}

	for _, opt := range opts {
		opt(s)
	}

	s.self = store.Snapshot().Self.ID
	s.started = s.now()
	s.adoptHostName()

	return s
}

// holds reports whether this host runs the mission.
//
// Everything that observes or drives an agent asks this first. A mission the
// peer runs has records and worktrees here but no session, and to the code that
// watches sessions a missing one looks exactly like a dead agent.
func (s *Service) holds(ms mission.Mission) bool { return ms.Lease.HeldBy(s.self) }

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

// CreateMission adds a mission in the briefing lane.
//
// Nothing here launches anything. A created mission is a written brief, and
// moving it to the active lane is a separate, explicit act.
func (s *Service) CreateMission(req api.CreateMissionRequest) (mission.Mission, error) {
	if strings.TrimSpace(req.Name) == "" {
		return mission.Mission{}, fmt.Errorf("%w: mission name is required", ErrInvalid)
	}

	if strings.TrimSpace(req.Prompt) == "" {
		return mission.Mission{}, fmt.Errorf("%w: mission prompt is required", ErrInvalid)
	}

	id, err := mission.NewMissionID()
	if err != nil {
		return mission.Mission{}, err
	}

	// Every read of stored state happens inside this one mutation: which mission
	// is inherited from, and whether the operation it names still exists. Keeping
	// them here is what makes reading the parent and writing the child a single
	// step, so a parent edited or deleted in between cannot be half applied.
	now := s.now()

	err = s.store.Mutate("mission.create", func(snap *mission.Snapshot) error {
		ms, err := resolveCreate(snap, req)
		if err != nil {
			return err
		}

		ms.ID = id
		ms.Status = mission.StatusBriefing
		ms.AgentState = mission.AgentUnknown
		ms.CreatedAt = now
		ms.UpdatedAt = now

		operation, ok := snap.Operation(ms.OperationID)
		if !ok {
			return fmt.Errorf("%w: operation %s", ErrNotFound, ms.OperationID)
		}

		if _, err := mission.MissionRepos(operation, ms); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}

		ms.Order = snap.NextOrder(mission.StatusBriefing)
		snap.PutMission(ms)

		return nil
	})
	if err != nil {
		return mission.Mission{}, err
	}

	stored, _ := s.store.Snapshot().Mission(id)
	s.announce(stored)

	return stored, nil
}

// resolveCreate fills in everything a create request leaves to q: the mission it
// inherits from, the default agent, and every check whose result depends on one or
// both of those.
//
// It reads the parent off snap rather than from a separate lookup, because the
// whole point of a create that can inherit is that the answer must be consistent
// with the write that follows it.
func resolveCreate(snap *mission.Snapshot, req api.CreateMissionRequest) (mission.Mission, error) {
	var zero mission.Mission

	if req.InheritFrom != "" {
		parent, ok := snap.Mission(req.InheritFrom)
		if !ok {
			return zero, fmt.Errorf("%w: mission %s", ErrNotFound, req.InheritFrom)
		}

		inheritDefaults(&req, parent)
	}

	// Checked here rather than left to the operation lookup below, so that a
	// request naming no operation at all is a 400 saying so, instead of a 404
	// looking for an operation whose id is the empty string.
	if req.OperationID == "" {
		return zero, fmt.Errorf(
			"%w: an operation is required, either named directly or inherited from a mission",
			ErrInvalid)
	}

	tool := req.Tool
	if tool == "" {
		tool = mission.DefaultTool
	}

	if !tool.Valid() {
		return zero, fmt.Errorf("%w: unknown tool %q", ErrInvalid, req.Tool)
	}

	if req.PlanMode && !tool.SupportsPlanMode() {
		return zero, fmt.Errorf("%w: %s does not support plan mode", ErrInvalid, tool)
	}

	if err := validateModelFlags(req.Model, req.Effort); err != nil {
		return zero, err
	}

	pin, err := resolveHost(snap, req.Pin)
	if err != nil {
		return zero, err
	}

	name := strings.TrimSpace(req.Name)

	return mission.Mission{
		OperationID:  req.OperationID,
		Name:         name,
		Slug:         mission.Slug(name),
		Tool:         tool,
		Prompt:       req.Prompt,
		PlanMode:     req.PlanMode,
		Model:        req.Model,
		Effort:       req.Effort,
		ExtraRepos:   normalizeRepos(req.ExtraRepos),
		BaseBranches: normalizeBaseBranches(req.BaseBranches),
		Queued:       req.Queued,
		Pin:          pin,
	}, nil
}

// resolveHost turns what a human typed for "which machine" into a host id.
//
// The two words that matter are relative: "local" is whichever host is asked
// and "remote" is its peer, so the same command means the right thing on
// either machine. A host's name or id is accepted too, and empty clears a pin.
func resolveHost(snap *mission.Snapshot, value string) (mission.HostID, error) {
	value = strings.TrimSpace(value)

	switch {
	case value == "":
		return "", nil
	case value == "local" || value == string(snap.Self.ID) || value == snap.Self.Name:
		return snap.Self.ID, nil
	case snap.Peer == nil:
		return "", fmt.Errorf("%w: no host %q; this q is not paired with another", ErrInvalid, value)
	case value == "remote" || value == string(snap.Peer.ID) || value == snap.Peer.Name:
		return snap.Peer.ID, nil
	default:
		return "", fmt.Errorf("%w: no host %q (want local, remote, %s, or %s)",
			ErrInvalid, value, snap.HostName(snap.Self.ID), snap.HostName(snap.Peer.ID))
	}
}

// inheritDefaults copies from parent every field the caller left unset.
//
// It is a default, not an override: a field the caller did send is kept. A list
// the caller did send replaces the parent's outright rather than being merged
// into it, so inheriting and then naming a repository reads as "the same as the
// parent, except this" instead of quietly widening the set of worktrees the
// mission will be given.
//
// PlanMode is left alone on purpose. It is a bool with no unset state on the
// wire, so an inherited value could never be turned back off, and whether a
// mission should stop for approval is the launcher of the mission's decision.
func inheritDefaults(req *api.CreateMissionRequest, parent mission.Mission) {
	if req.OperationID == "" {
		req.OperationID = parent.OperationID
	}

	if req.Tool == "" {
		req.Tool = parent.Tool
	}

	if req.Model == "" {
		req.Model = parent.Model
	}

	if req.Effort == "" {
		req.Effort = parent.Effort
	}

	if len(req.ExtraRepos) == 0 {
		req.ExtraRepos = parent.ExtraRepos
	}

	if len(req.BaseBranches) == 0 {
		req.BaseBranches = parent.BaseBranches
	}
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

	s.announce(updated)

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

	if err := applyQueuePatch(snap, ms, req); err != nil {
		return err
	}

	if err := applyWorktreePatch(ms, req); err != nil {
		return err
	}

	// Tool, plan mode, model, and effort are only meaningful before launch: they
	// are baked into the agent's argv, so changing one afterwards would describe a
	// session that is not the one running.
	if req.Tool != nil || req.PlanMode != nil || req.Model != nil || req.Effort != nil {
		if ms.Launched() {
			return fmt.Errorf("%w: cannot change tool, plan mode, model, or effort after launch", ErrConflict)
		}
	}

	if req.Model != nil {
		ms.Model = *req.Model
	}

	if req.Effort != nil {
		ms.Effort = *req.Effort
	}

	// Checked only when one of them was actually sent, so renaming a mission
	// cannot fail over a model value that was already stored.
	if req.Model != nil || req.Effort != nil {
		if err := validateModelFlags(ms.Model, ms.Effort); err != nil {
			return err
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

// applyQueuePatch applies the fields that say when and where a mission starts.
//
// Both only mean anything before launch. A running mission cannot be queued,
// and where it runs is by then a matter of who holds its lease rather than of
// a preference.
func applyQueuePatch(snap *mission.Snapshot, ms *mission.Mission, req api.UpdateMissionRequest) error {
	if req.Queued == nil && req.Pin == nil {
		return nil
	}

	if ms.Launched() {
		return fmt.Errorf("%w: mission %s has already been launched", ErrConflict, ms.ID)
	}

	if req.Queued != nil {
		ms.Queued = *req.Queued

		if ms.Queued {
			ms.LaunchError = ""
		}
	}

	if req.Pin != nil {
		pin, err := resolveHost(snap, *req.Pin)
		if err != nil {
			return err
		}

		ms.Pin = pin
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

	s.announce(updated)

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

// applyWorktreePatch applies the fields that describe a mission's worktrees.
//
// Both are fixed the moment provisioning runs — the repositories decide which
// worktrees exist and the base branches decide what is in them — so both are
// editable only while the mission is still in briefing.
func applyWorktreePatch(ms *mission.Mission, req api.UpdateMissionRequest) error {
	if req.ExtraRepos == nil && req.BaseBranches == nil {
		return nil
	}

	if ms.Status != mission.StatusBriefing {
		return fmt.Errorf("%w: cannot change repositories or base branches after launch", ErrConflict)
	}

	if req.ExtraRepos != nil {
		ms.ExtraRepos = normalizeRepos(*req.ExtraRepos)
	}

	if req.BaseBranches != nil {
		ms.BaseBranches = normalizeBaseBranches(*req.BaseBranches)
	}

	return nil
}

// normalizeBaseBranches trims entries and drops those naming no branch, so that
// clearing an override in a form arrives as an absent key rather than an empty
// string the provisioner would have to interpret.
//
// It returns nil for an empty result, which is what keeps the field out of the
// state file for the missions that never used it.
func normalizeBaseBranches(branches map[string]string) map[string]string {
	out := make(map[string]string, len(branches))

	for repo, branch := range branches {
		repo, branch = strings.TrimSpace(repo), strings.TrimSpace(branch)
		if repo == "" || branch == "" {
			continue
		}

		out[repo] = branch
	}

	if len(out) == 0 {
		return nil
	}

	return out
}

func (s *Service) publishOperation(t mission.Operation) {
	if s.hub != nil {
		s.hub.Broadcast(api.EventOperation, t)
	}

	s.kickSync()
}

// announce publishes a mission changed by a deliberate act — created, edited,
// launched, moved — and asks for an exchange with the paired q soon.
//
// The distinction from publishMission is the exchange. An agent's hooks change
// a mission several times a second and the peer can wait for the next tick to
// hear of those; a human's change is one the other machine may be about to act
// on, and a laptop lid can close in less time than a tick takes.
func (s *Service) announce(t mission.Mission) {
	s.publishMission(t)
	s.kickSync()
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

	s.kickSync()
}

func (s *Service) publishLimits(limits []mission.Limit) {
	if s.hub != nil {
		s.hub.Broadcast(api.EventLimits, api.Limits{Limits: limits})
	}
}

// validateModelFlags rejects a model or effort q cannot put on a command line.
//
// It deliberately does not check membership in the model catalog. A probe can be
// stale, or absent on a machine that has never reached the agent, and refusing a
// model the agent would have accepted is a worse failure than passing an unknown
// one through and letting the agent object to it.
func validateModelFlags(model, effort string) error {
	for _, check := range []struct{ kind, value string }{
		{"model", model},
		{"effort", effort},
	} {
		if err := mission.ValidateModelFlag(check.kind, check.value); err != nil {
			return fmt.Errorf("%w: %w", ErrInvalid, err)
		}
	}

	return nil
}
