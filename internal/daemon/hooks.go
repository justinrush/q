package daemon

import (
	"encoding/json"
	"os"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/hookspec"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/spool"
	"github.com/justinrush/q/internal/state"
	"github.com/justinrush/q/internal/statem"
)

// coalesceWindow is how long a lane change is protected from being lowered.
//
// Hooks for one logical moment run as separate processes, so a Stop can be observed
// microseconds after the PermissionRequest that outranks it. Within this window a
// lower-precedence proposal is discarded unless it reports something that
// demonstrably already happened, such as a completed tool call.
const coalesceWindow = 750 * time.Millisecond

// ApplyHook applies one agent hook event.
//
// It never returns an error the agent could act on. A hook that failed loudly could
// block a tool call in claude, so problems are recorded and swallowed: an event that
// cannot be matched to a mission is written to the orphan log, and one for a mission that
// no longer exists is dropped.
func (s *Service) ApplyHook(req api.HookRequest) {
	payload, err := hookspec.ParseBytes(req.Payload, req.Event)
	if err != nil {
		s.recordOrphan(req, "unparseable payload: "+err.Error())

		return
	}

	mission, ok := s.resolveMission(req, payload)
	if !ok {
		s.recordOrphan(req, "no matching mission")

		return
	}

	// An epoch older than the mission's means this hook belongs to a session q
	// already gave up on and relaunched past.
	if req.HookEpoch > 0 && req.HookEpoch < mission.HookEpoch {
		return
	}

	s.applyReduction(mission.ID, payload)
}

// resolveMission identifies which mission an event belongs to.
//
// The environment-supplied id is authoritative because q set it at launch. The
// session id is next, and the working directory last: mission directories are unique
// per mission by construction, which is one of the payoffs of keeping worktrees in a
// central q-owned tree.
func (s *Service) resolveMission(req api.HookRequest, payload hookspec.Payload) (domain.Mission, bool) {
	snap := s.store.Snapshot()

	if req.MissionID != "" {
		if mission, ok := snap.Mission(req.MissionID); ok {
			return mission, true
		}
	}

	if mission, ok := snap.MissionBySession(req.Tool, payload.SessionID); ok {
		return mission, true
	}

	return snap.MissionByDir(payload.CWD)
}

// applyReduction runs the state machine and persists the outcome.
func (s *Service) applyReduction(id domain.MissionID, payload hookspec.Payload) {
	var (
		updated domain.Mission
		changed bool
	)

	err := s.store.Mutate("mission.hook."+hookspec.EventSlug(payload.Event), func(snap *state.Snapshot) error {
		mission, ok := snap.Mission(id)
		if !ok {
			return nil
		}

		now := s.now()
		res := statem.Reduce(mission, payload, now)
		s.stabilizeCodexApproval(mission, payload, &res, now)

		if !res.Changed {
			return nil
		}

		next := res.Mission

		if status, move := s.resolveLane(mission, res, now); move {
			next.Status = status
			next.Order = snap.NextOrder(status)
			next.StatusChangedAt = now
		}

		updated = next
		changed = true
		snap.PutMission(next)

		return nil
	})
	if err != nil {
		s.warn("applying a hook", "event", payload.Event, "error", err)

		return
	}

	if changed {
		s.publishMission(updated)
	}
}

func (s *Service) stabilizeCodexApproval(
	mission domain.Mission,
	payload hookspec.Payload,
	res *statem.Result,
	now time.Time,
) {
	if mission.Tool != domain.ToolCodex || mission.Status.Terminal() ||
		mission.Status == domain.StatusBriefing && payload.Event != hookspec.EventSessionStart ||
		mission.AgentSessionID != "" && payload.SessionID != "" &&
			mission.AgentSessionID != payload.SessionID {
		return
	}

	switch payload.Event {
	case hookspec.EventPermissionRequest:
		s.noteCodexApproval(mission.ID, payload.SessionID, res.Mission.WaitingFor, now)
		res.Mission.AgentState = domain.AgentBusy
		res.Mission.WaitingFor = ""
		res.ProposedStatus = ""
	case hookspec.EventPreToolUse:
		s.clearCodexApproval(mission.ID)
		res.Mission.AgentState = domain.AgentBusy
		res.Mission.WaitingFor = ""
		res.ProposedStatus = domain.StatusActive
		res.Definite = true
	case hookspec.EventPostToolUse, hookspec.EventUserPromptSubmit,
		hookspec.EventSessionEnd:
		s.clearCodexApproval(mission.ID)
	}
}

// resolveLane decides whether a proposed lane change should be applied.
//
// A proposal that raises the lane always wins. One that lowers it is discarded if
// the current lane was set moments ago and the proposal is not itself proof that the
// situation resolved, which is what keeps a Stop from making a card that is blocked
// on a permission prompt read as finished.
func (s *Service) resolveLane(current domain.Mission, res statem.Result, now time.Time) (domain.Status, bool) {
	if res.ProposedStatus == "" || res.ProposedStatus == current.Status {
		return "", false
	}

	if res.Definite || res.ProposedStatus.Precedence() > current.Status.Precedence() {
		return res.ProposedStatus, true
	}

	if !current.StatusChangedAt.IsZero() && now.Sub(current.StatusChangedAt) < coalesceWindow {
		return "", false
	}

	return res.ProposedStatus, true
}

// recordOrphan appends an unmatched event to the orphan log.
//
// Orphans are worth keeping rather than discarding: a run of them is the clearest
// evidence that hook wiring has drifted, for instance because the q binary
// moved out from under sessions that are still running.
func (s *Service) recordOrphan(req api.HookRequest, reason string) {
	if s.dirs.State == "" {
		return
	}

	entry, err := json.Marshal(struct {
		At        time.Time        `json:"at"`
		Reason    string           `json:"reason"`
		Tool      domain.Tool      `json:"tool"`
		Event     string           `json:"event"`
		MissionID domain.MissionID `json:"missionId,omitempty"`
		Payload   json.RawMessage  `json:"payload,omitempty"`
	}{
		At:        s.now(),
		Reason:    reason,
		Tool:      req.Tool,
		Event:     req.Event,
		MissionID: req.MissionID,
		Payload:   req.Payload,
	})
	if err != nil {
		return
	}

	f, err := os.OpenFile(s.dirs.OrphansFile(), os.O_APPEND|os.O_CREATE|os.O_WRONLY, paths.FileMode)
	if err != nil {
		return
	}
	defer func() { _ = f.Close() }()

	_, _ = f.Write(append(entry, '\n'))
}

// warn reports a problem without failing the caller.
func (s *Service) warn(msg string, args ...any) {
	if s.logger != nil {
		s.logger.Warn(msg, args...)
	}
}

// hookQueueSize bounds the buffered hook events awaiting reduction.
//
// The HTTP handler must answer instantly, so it never blocks on this channel; when
// it is full the event is spooled to disk instead.
const hookQueueSize = 1024

// hookQueue serializes hook reduction on a single goroutine, so no lock is held
// while git or tmux work happens elsewhere.
type hookQueue struct {
	events chan api.HookRequest
}

// newHookQueue returns an empty queue.
func newHookQueue() *hookQueue {
	return &hookQueue{events: make(chan api.HookRequest, hookQueueSize)}
}

// offer enqueues an event, reporting false if the queue is full.
func (q *hookQueue) offer(req api.HookRequest) bool {
	select {
	case q.events <- req:
		return true
	default:
		return false
	}
}

// run reduces queued events until the channel closes.
func (q *hookQueue) run(svc *Service) {
	for req := range q.events {
		svc.ApplyHook(req)
	}
}

// close stops the queue.
func (q *hookQueue) close() { close(q.events) }

// DrainSpool applies every hook event that was recorded while the daemon was
// unreachable, in the order it was observed.
func (s *Service) DrainSpool() {
	entries, err := spool.Drain(s.dirs.SpoolDir())
	if err != nil {
		s.warn("draining the hook spool", "error", err)
	}

	for _, entry := range entries {
		s.ApplyHook(entry.Hook)
	}

	if len(entries) > 0 {
		s.logger.Info("applied spooled hook events", "count", len(entries))
	}
}
