// Sharing missions with a paired q.
//
// Two daemons exchange their whole shared state every few seconds. The primary
// dials and the secondary answers, and each folds the other's copy into its own
// by the rules in [mission.Snapshot.MergePeer]. Everything in this file is the
// plumbing around that merge: who the peer is, when an exchange happens, and
// what has to follow one in the world outside the store.

package daemon

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"sync"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/remote"
)

// Pairing defaults.
const (
	// DefaultSyncInterval is how often the primary starts an exchange. It
	// bounds how stale the other machine's view of a mission can be.
	DefaultSyncInterval = 15 * time.Second
	// DefaultTakeoverAfter is how long the secondary waits for a silent primary
	// before running its missions. It has to outlast a laptop changing networks
	// or a lid closed for a walk to a meeting room, and five minutes does, while
	// still being short against a night.
	DefaultTakeoverAfter = 5 * time.Minute
	// syncTimeout bounds one exchange, including whatever the peer does to
	// answer it.
	syncTimeout = 90 * time.Second
)

// Remote is the paired daemon as the service uses it.
//
// It is declared here rather than taken from the remote package so the rules
// for sharing missions can be tested against a second in-process service, with
// no ssh and no subprocess between them.
type Remote interface {
	Sync(ctx context.Context, req api.SyncRequest) (api.SyncResponse, error)
	SetStatus(ctx context.Context, id mission.MissionID, req api.SetStatusRequest) (mission.Mission, error)
}

// RepoLocator maps a repository between its origin and this machine's checkout
// of it.
type RepoLocator interface {
	OriginURL(ctx context.Context, path string) (string, error)
	Locate(ctx context.Context, url string) (string, bool)
}

// WithRemote makes this daemon the primary of a pair, dialing the given peer.
func WithRemote(r Remote) Option {
	return func(s *Service) { s.remote = r }
}

// WithSyncInterval overrides how often the primary starts an exchange.
func WithSyncInterval(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.syncInterval = d
		}
	}
}

// WithTakeoverAfter overrides how long the secondary waits for a silent
// primary. It is the primary's setting; the secondary learns it from the
// exchange.
func WithTakeoverAfter(d time.Duration) Option {
	return func(s *Service) {
		if d > 0 {
			s.takeoverAfter = d
		}
	}
}

// WithLocator attaches the component that finds this machine's checkout of a
// repository another machine named.
func WithLocator(l RepoLocator) Option {
	return func(s *Service) { s.locator = l }
}

// WithHostName sets the name this installation shows on cards and reports to
// its peer.
func WithHostName(name string) Option {
	return func(s *Service) { s.hostName = name }
}

// WithVersion records the q version reported in an exchange.
func WithVersion(v string) Option {
	return func(s *Service) { s.version = v }
}

// link is what this daemon remembers about its conversation with the peer.
//
// It is held in memory on purpose. After a restart a daemon does not know when
// it last spoke to its peer, and each role resolves that doubt in the safe
// direction: the secondary assumes the primary was just there, so a restart is
// never by itself a reason to take over, and the primary assumes it has not
// been confirmed, so it starts nothing until an exchange succeeds.
type link struct {
	mu       sync.Mutex
	lastSync time.Time
	err      string
	// delivered remembers commands already carried out, so an exchange retried
	// after the peer acted on it does not resume an agent twice.
	delivered map[string]bool
}

// ok records a completed exchange.
func (l *link) ok(now time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()

	l.lastSync, l.err = now, ""
}

// fail records why an exchange did not complete, reporting whether that is news.
func (l *link) fail(err error) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	changed := l.err != err.Error()
	l.err = err.Error()

	return changed
}

// state returns the last completed exchange and the current error.
func (l *link) state() (time.Time, string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	return l.lastSync, l.err
}

// once reports whether a command is being seen for the first time.
func (l *link) once(id string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()

	if l.delivered == nil {
		l.delivered = map[string]bool{}
	}

	if l.delivered[id] {
		return false
	}

	l.delivered[id] = true

	return true
}

// roleOf reports the part this daemon plays.
//
// Neither role is configured as such. A daemon told how to reach a peer is the
// primary, because dialing is what a primary does. One that has been dialed is
// the secondary, which is why the always-on machine needs no configuration at
// all: it learns it is half of a pair when the other half first calls.
func (s *Service) roleOf(snap mission.Snapshot) mission.Role {
	switch {
	case s.remote != nil:
		return mission.RolePrimary
	case snap.Peer != nil:
		return mission.RoleSecondary
	default:
		return mission.RoleStandalone
	}
}

// takeoverWindow is how long a silent peer is still considered present.
func (s *Service) takeoverWindow(snap mission.Snapshot) time.Duration {
	if s.remote == nil && snap.Peer != nil && snap.Peer.TakeoverAfter > 0 {
		return snap.Peer.TakeoverAfter
	}

	return s.takeoverAfter
}

// peerSeen reports whether an exchange completed recently enough that the peer
// should be treated as present.
func (s *Service) peerSeen(snap mission.Snapshot, now time.Time) bool {
	if snap.Peer == nil {
		return false
	}

	last, _ := s.link.state()

	// A secondary that has just started gives the primary the benefit of the
	// doubt for one full window.
	if s.remote == nil && last.Before(s.started) {
		last = s.started
	}

	return !last.IsZero() && now.Sub(last) <= s.takeoverWindow(snap)
}

// RunSync exchanges state with the peer on an interval, and at once whenever
// something here changes that the peer should hear about without waiting.
//
// Only a primary runs it. The secondary never dials: it cannot count on
// reaching a laptop, and does not need to, because the laptop calls it.
func (s *Service) RunSync(ctx context.Context) {
	if s.remote == nil {
		return
	}

	s.syncOnce(ctx)

	ticker := time.NewTicker(s.syncInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		case <-s.syncKick:
		}

		s.syncOnce(ctx)
	}
}

// kickSync asks for an exchange soon, without waiting for one.
func (s *Service) kickSync() {
	select {
	case s.syncKick <- struct{}{}:
	default:
	}
}

// syncOnce runs one exchange, logging a failure only when it is a new one. A
// laptop off the network fails every few seconds for hours, and that is one
// event, not several thousand.
func (s *Service) syncOnce(ctx context.Context) {
	if err := s.SyncNow(ctx); err != nil && s.link.fail(err) {
		s.warn("exchanging state with the paired q", "error", err)
		s.publishRemote()
	}
}

// SyncNow performs one exchange with the peer and applies the result.
func (s *Service) SyncNow(ctx context.Context) error {
	if s.remote == nil {
		return fmt.Errorf("%w: this q does not dial a peer; run this on the primary", ErrConflict)
	}

	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	ctx, cancel := context.WithTimeout(ctx, syncTimeout)
	defer cancel()

	s.fillRepoURLs(ctx)

	snap := s.store.Snapshot()

	resp, err := s.remote.Sync(ctx, api.SyncRequest{
		Protocol:      api.SyncProtocol,
		Version:       s.version,
		Payload:       snap.Payload(),
		TakeoverAfter: s.takeoverAfter,
		Commands:      snap.Outbox,
	})
	if err != nil {
		return err
	}

	if resp.Protocol != api.SyncProtocol {
		return protocolMismatch(resp.Protocol, resp.Version)
	}

	_, wasLinked := s.link.state()

	if err := s.applyPeer(ctx, resp.Payload, 0, resp.Results); err != nil {
		return err
	}

	s.link.ok(s.now())

	if wasLinked != "" || snap.Peer == nil {
		s.publishRemote()
	}

	return nil
}

// SyncExchange answers the primary's half of an exchange with this daemon's.
//
// The primary's state is merged first and the answer built afterwards, so one
// round trip is enough for both sides to agree: the answer already reflects
// what this side took from the request.
func (s *Service) SyncExchange(ctx context.Context, req api.SyncRequest) (api.SyncResponse, error) {
	if s.remote != nil {
		return api.SyncResponse{}, fmt.Errorf(
			"%w: this q is itself configured to dial a peer, so it cannot be another's secondary", ErrConflict)
	}

	if req.Protocol != api.SyncProtocol {
		return api.SyncResponse{}, protocolMismatch(req.Protocol, req.Version)
	}

	s.syncMu.Lock()
	defer s.syncMu.Unlock()

	s.fillRepoURLs(ctx)

	window := req.TakeoverAfter
	if window <= 0 {
		window = DefaultTakeoverAfter
	}

	if err := s.applyPeer(ctx, req.Payload, window, nil); err != nil {
		return api.SyncResponse{}, err
	}

	results := s.runCommands(ctx, req.Commands)

	_, wasLinked := s.link.state()
	s.link.ok(s.now())

	if wasLinked != "" {
		s.publishRemote()
	}

	return api.SyncResponse{
		Protocol: api.SyncProtocol,
		Version:  s.version,
		Payload:  s.store.Snapshot().Payload(),
		Results:  results,
	}, nil
}

// protocolMismatch explains that the two daemons cannot talk, naming the
// version to compare against.
func protocolMismatch(protocol int, version string) error {
	return fmt.Errorf(
		"%w: the paired q speaks sync protocol %d (q %s) and this one speaks %d; upgrade both to the same q",
		ErrConflict, protocol, version, api.SyncProtocol)
}

// applyPeer merges the peer's payload and does what follows from it.
//
// takeoverAfter is what the primary asked for, zero on the primary itself.
// results are the peer's answers to commands this side had queued.
func (s *Service) applyPeer(
	ctx context.Context,
	payload mission.Payload,
	takeoverAfter time.Duration,
	results []api.CommandResult,
) error {
	var (
		merge    mission.Merge
		after    mission.Snapshot
		answered []mission.Command
	)

	primary := s.remote != nil

	err := s.store.Apply("sync.merge", func(snap *mission.Snapshot) error {
		paired, err := pair(snap, payload.From, takeoverAfter)
		if err != nil {
			return err
		}

		merge = snap.MergePeer(payload, primary, s.now())
		answered = dropAnswered(snap, results)

		after = *snap

		if !paired && !merge.Changed() && len(answered) == 0 {
			return mission.ErrUnchanged
		}

		return nil
	})
	if err != nil {
		return err
	}

	s.reportRefusals(answered, results)
	s.publishMerge(after, merge)
	s.afterMerge(ctx, merge)

	return nil
}

// pair records who the peer is, reporting whether the record changed.
//
// The first host to call is remembered, and a different one is then refused
// until this side is told to forget. Reaching this daemon at all takes shell
// access to the machine, so this is not a defense against an attacker; it is a
// defense against pointing a second laptop at a machine that is already
// another's secondary and having the two fight over its missions.
func pair(snap *mission.Snapshot, from mission.HostInfo, takeoverAfter time.Duration) (bool, error) {
	if !from.ID.Valid() {
		return false, fmt.Errorf("%w: the paired q sent no host id", ErrInvalid)
	}

	if from.ID == snap.Self.ID {
		return false, fmt.Errorf(
			"%w: both installations have host id %s, which happens when one was copied from the other; "+
				"delete the \"self\" entry from one state file and restart its daemon", ErrConflict, from.ID)
	}

	if snap.Peer == nil {
		snap.Peer = &mission.Peer{HostInfo: from, TakeoverAfter: takeoverAfter}

		return true, nil
	}

	if snap.Peer.ID != from.ID {
		return false, fmt.Errorf(
			"%w: already paired with %s (%s); run `q remote forget` here before pairing with %s",
			ErrConflict, snap.Peer.Name, snap.Peer.ID, from.Name)
	}

	changed := false

	if from.Name != "" && snap.Peer.Name != from.Name {
		snap.Peer.Name, changed = from.Name, true
	}

	if takeoverAfter > 0 && snap.Peer.TakeoverAfter != takeoverAfter {
		snap.Peer.TakeoverAfter, changed = takeoverAfter, true
	}

	return changed, nil
}

// dropAnswered removes from the outbox every command the peer answered,
// returning what it removed.
func dropAnswered(snap *mission.Snapshot, results []api.CommandResult) []mission.Command {
	if len(results) == 0 || len(snap.Outbox) == 0 {
		return nil
	}

	done := make(map[string]bool, len(results))
	for _, r := range results {
		done[r.ID] = true
	}

	var (
		kept    []mission.Command
		dropped []mission.Command
	)

	for _, cmd := range snap.Outbox {
		if done[cmd.ID] {
			dropped = append(dropped, cmd)
		} else {
			kept = append(kept, cmd)
		}
	}

	snap.Outbox = kept

	for _, cmd := range dropped {
		clearPending(snap, cmd.Mission)
	}

	return dropped
}

// clearPending removes a mission's pending badge once nothing is queued for it.
func clearPending(snap *mission.Snapshot, id mission.MissionID) {
	for _, cmd := range snap.Outbox {
		if cmd.Mission == id {
			return
		}
	}

	if ms, ok := snap.Mission(id); ok && ms.HasLocalBadge(mission.BadgePending) {
		ms.LocalBadges = ms.WithoutLocalBadge(mission.BadgePending)
		snap.PutMission(ms)
	}
}

// reportRefusals logs commands the peer declined. The card already shows what
// actually happened to the mission; this is the record of why a request did
// not take.
func (s *Service) reportRefusals(answered []mission.Command, results []api.CommandResult) {
	refused := make(map[string]string, len(results))

	for _, r := range results {
		if r.Error != "" {
			refused[r.ID] = r.Error
		}
	}

	for _, cmd := range answered {
		if reason, ok := refused[cmd.ID]; ok {
			s.warn("the paired q refused a queued request", "mission", cmd.Mission, "to", cmd.To, "error", reason)
		}
	}
}

// publishMerge tells open boards what an exchange changed.
func (s *Service) publishMerge(snap mission.Snapshot, merge mission.Merge) {
	for _, id := range merge.Operations {
		if op, ok := snap.Operation(id); ok {
			s.publishOperation(op)
		}
	}

	for _, id := range merge.Missions {
		if ms, ok := snap.Mission(id); ok {
			s.publishMission(ms)
		}
	}

	for _, ms := range merge.Removed {
		s.publishDeleted(api.KindMission, string(ms.ID))
	}

	for _, id := range merge.RemovedOperations {
		s.publishDeleted(api.KindOperation, string(id))
	}
}

// afterMerge does what a merge implies outside the store.
//
// The store only holds records. A mission the peer deleted still has worktrees
// here, one whose lease moved away may still have an agent running here, and a
// repository the peer named still has to be found on this machine's disk.
func (s *Service) afterMerge(ctx context.Context, merge mission.Merge) {
	for _, ms := range merge.Removed {
		s.reclaimLocal(ctx, ms)
	}

	for _, id := range merge.Lost {
		s.standDown(ctx, id)
	}

	if merge.Changed() {
		s.locateRepos(ctx)
	}

	s.reclaimStale(ctx)
}

// runCommands carries out the requests the primary had queued, in the order
// they were made.
func (s *Service) runCommands(ctx context.Context, commands []mission.Command) []api.CommandResult {
	results := make([]api.CommandResult, 0, len(commands))

	for _, cmd := range commands {
		result := api.CommandResult{ID: cmd.ID}

		if s.link.once(cmd.ID) {
			if _, err := s.Dispatch(ctx, cmd.Mission, api.SetStatusRequest{To: cmd.To, Message: cmd.Message}); err != nil {
				result.Error = err.Error()
			}
		}

		results = append(results, result)
	}

	return results
}

// dispatchToPeer asks the host running a mission to move it.
//
// This host has no agent to deliver a message to and no session to stop, so a
// request about a mission the peer runs is the peer's to carry out. When the
// peer cannot be reached, a request to resume is kept and delivered with the
// next exchange: that is the request a human makes from a laptop on a train,
// and it loses nothing by arriving a little late. Finishing a mission is not
// kept, because it destroys worktrees and should not happen at some later
// moment the human is no longer thinking about.
func (s *Service) dispatchToPeer(
	ctx context.Context,
	current mission.Mission,
	req api.SetStatusRequest,
) (mission.Mission, error) {
	snap := s.store.Snapshot()
	host := snap.HostName(current.Lease.Holder)

	if s.remote == nil {
		return mission.Mission{}, fmt.Errorf(
			"%w: %s is running on %s; move it from there", ErrConflict, current.Name, host)
	}

	ms, err := s.remote.SetStatus(ctx, current.ID, req)
	if err == nil {
		s.kickSync()

		return ms, nil
	}

	if !errors.Is(err, remote.ErrUnreachable) {
		return mission.Mission{}, peerError(err)
	}

	if req.To != mission.StatusActive {
		return mission.Mission{}, fmt.Errorf(
			"%w: %s is running on %s, which cannot be reached; try again when it can", ErrConflict, current.Name, host)
	}

	return s.enqueue(current.ID, req)
}

// peerError maps a refusal from the peer's daemon back onto the sentinel a
// local refusal would have used, so a caller cannot tell which daemon said no.
func peerError(err error) error {
	status, ok := errors.AsType[*api.StatusError](err)
	if !ok {
		return err
	}

	switch status.Code {
	case http.StatusNotFound:
		return fmt.Errorf("%w: %s", ErrNotFound, status.Message)
	case http.StatusBadRequest:
		return fmt.Errorf("%w: %s", ErrInvalid, status.Message)
	case http.StatusConflict:
		return fmt.Errorf("%w: %s", ErrConflict, status.Message)
	default:
		return err
	}
}

// enqueue keeps a request for the next exchange and marks the card.
func (s *Service) enqueue(id mission.MissionID, req api.SetStatusRequest) (mission.Mission, error) {
	commandID, err := mission.NewCommandID()
	if err != nil {
		return mission.Mission{}, err
	}

	var updated mission.Mission

	err = s.store.Apply("sync.enqueue", func(snap *mission.Snapshot) error {
		ms, ok := snap.Mission(id)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}

		snap.Outbox = append(snap.Outbox, mission.Command{
			ID: commandID, Mission: id, To: req.To, Message: req.Message, At: s.now(),
		})

		ms.LocalBadges = ms.WithLocalBadge(mission.BadgePending, "")
		updated = ms
		snap.PutMission(ms)

		return nil
	})
	if err != nil {
		return mission.Mission{}, err
	}

	s.publishMission(updated)
	s.publishRemote()

	return updated, nil
}

// RemoteStatus reports this daemon's pairing.
func (s *Service) RemoteStatus() api.RemoteStatus {
	snap := s.store.Snapshot()
	last, errText := s.link.state()

	status := api.RemoteStatus{
		Self:       snap.Self,
		Role:       s.roleOf(snap),
		LastSyncAt: last,
		Error:      errText,
		Pending:    len(snap.Outbox),
	}

	if snap.Peer != nil {
		peer := snap.Peer.HostInfo
		status.Peer = &peer
	}

	return status
}

// ForgetPeer ends the pairing on this side.
//
// Missions the peer was running are handed to this host, because with the
// pairing gone nobody else will ever report on them, and a card that says
// "running elsewhere" forever is worse than one that asks to be looked at.
func (s *Service) ForgetPeer() error {
	var touched []mission.Mission

	err := s.store.Mutate("sync.forget", func(snap *mission.Snapshot) error {
		if snap.Peer == nil {
			return mission.ErrUnchanged
		}

		for _, ms := range snap.Missions {
			if s.holds(ms) {
				continue
			}

			ms.Lease = ms.Lease.Take(s.self)
			ms.Pin = ""
			ms.LocalBadges = nil
			orphan(&ms, s.now())

			snap.PutMission(ms)
			touched = append(touched, ms)
		}

		snap.Peer = nil
		snap.Outbox = nil

		return nil
	})
	if err != nil {
		return err
	}

	for _, ms := range touched {
		s.publishMission(ms)
	}

	s.publishRemote()

	return nil
}

// orphan marks a mission whose agent this host cannot account for.
//
// It moves to debrief, the lane that asks for a human, and is marked as having
// no session, which is what makes resuming it relaunch the agent here.
func orphan(ms *mission.Mission, now time.Time) {
	if !ms.Running() {
		return
	}

	ms.AgentState = mission.AgentDead
	ms.WaitingFor = ""
	ms.Badges = ms.WithBadge(mission.BadgeTmuxGone, "")

	if ms.Status == mission.StatusActive || ms.Status == mission.StatusAwaiting {
		ms.Status = mission.StatusDebrief
		ms.StatusChangedAt = now
	}
}

// publishRemote tells open boards the pairing changed.
func (s *Service) publishRemote() {
	if s.hub != nil {
		s.hub.Broadcast(api.EventRemote, s.RemoteStatus())
	}
}

// adoptHostName records this installation's display name.
func (s *Service) adoptHostName() {
	if s.hostName == "" {
		return
	}

	err := s.store.Apply("host.name", func(snap *mission.Snapshot) error {
		if snap.Self.Name == s.hostName {
			return mission.ErrUnchanged
		}

		snap.Self.Name = s.hostName

		return nil
	})
	if err != nil {
		s.warn("recording this host's name", "error", err)
	}
}
