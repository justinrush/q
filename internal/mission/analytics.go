package mission

import (
	"reflect"
	"time"
)

// AnalyticsContribution is a host's cumulative measurement, never a delta.
// Only its owner advances Revision; receiving a snapshot must not increment it.
type AnalyticsContribution struct {
	Revision uint64 `json:"revision"`
	Usage    Usage  `json:"usage"`
}

// AnalyticsRecord survives mission deletion without retaining conversation data.
type AnalyticsRecord struct {
	ID            MissionID                        `json:"id"`
	OperationID   OperationID                      `json:"operationId"`
	OperationName string                           `json:"operationName"`
	Name          string                           `json:"name"`
	Tool          Tool                             `json:"tool"`
	Status        Status                           `json:"status"`
	StartedAt     *time.Time                       `json:"startedAt,omitempty"`
	FinishedAt    *time.Time                       `json:"finishedAt,omitempty"`
	Model         string                           `json:"model,omitempty"`
	Effort        string                           `json:"effort,omitempty"`
	PlanModel     string                           `json:"planModel,omitempty"`
	PlanEffort    string                           `json:"planEffort,omitempty"`
	PlanMode      bool                             `json:"planMode"`
	Deleted       bool                             `json:"deleted"`
	SpecRev       int                              `json:"specRev"`
	OperationRev  int                              `json:"operationRev"`
	Lease         Lease                            `json:"lease"`
	Revision      uint64                           `json:"revision"`
	Writer        HostID                           `json:"writer"`
	Contributions map[HostID]AnalyticsContribution `json:"contributions,omitempty"`
	Legacy        Usage                            `json:"legacy,omitzero"`
}

func (r AnalyticsRecord) Clone() AnalyticsRecord {
	r.Legacy = r.Legacy.Clone()
	c := make(map[HostID]AnalyticsContribution, len(r.Contributions))
	for host, v := range r.Contributions {
		v.Usage = v.Usage.Clone()
		c[host] = v
	}
	r.Contributions = c
	if r.StartedAt != nil {
		t := *r.StartedAt
		r.StartedAt = &t
	}
	if r.FinishedAt != nil {
		t := *r.FinishedAt
		r.FinishedAt = &t
	}
	return r
}

func (r AnalyticsRecord) Usage() Usage {
	if len(r.Contributions) == 0 {
		return r.Legacy.Clone()
	}
	hosts := make(map[HostID]Usage, len(r.Contributions))
	for host, c := range r.Contributions {
		hosts[host] = c.Usage
	}
	return TotalUsage(hosts)
}

func cloneAnalytics(records map[MissionID]AnalyticsRecord) map[MissionID]AnalyticsRecord {
	if records == nil {
		return nil
	}
	out := make(map[MissionID]AnalyticsRecord, len(records))
	for id, r := range records {
		out[id] = r.Clone()
	}
	return out
}

// captureAnalytics runs inside the same transaction as every state change. The
// before snapshot supplies metadata even when an operation and its missions go.
func (s *Snapshot) captureAnalytics(before Snapshot) {
	if s.Analytics == nil {
		s.Analytics = map[MissionID]AnalyticsRecord{}
	}
	for _, ms := range s.Missions {
		s.captureRecord(ms, *s, false)
	}
	for _, ms := range before.Missions {
		if _, ok := s.Mission(ms.ID); !ok {
			s.captureRecord(ms, before, true)
		}
	}
}

func (s *Snapshot) captureRecord(ms Mission, source Snapshot, deleted bool) {
	old := s.Analytics[ms.ID]
	// Its final record has already arrived; local cleanup must not replace it
	// with a stale pre-deletion mission snapshot.
	if old.Deleted {
		return
	}
	next := old.Clone()
	next.ID = ms.ID
	next.OperationID = ms.OperationID
	next.Name = ms.Name
	next.Tool = ms.Tool
	next.Status = ms.Status
	next.StartedAt = ms.StartedAt
	next.FinishedAt = ms.FinishedAt
	next.Model = ms.Model
	next.Effort = ms.Effort
	next.PlanModel = ms.PlanModel
	next.PlanEffort = ms.PlanEffort
	next.PlanMode = ms.PlanMode
	next.SpecRev = ms.SpecRev
	next.Lease = ms.Lease
	next.Deleted = old.Deleted || deleted
	if op, ok := source.Operation(ms.OperationID); ok {
		next.OperationName = op.Name
		next.OperationRev = op.Rev
	}
	// A legacy mission may have no host attribution. Replace its baseline when
	// real per-host usage becomes available, rather than adding both.
	if len(ms.UsageByHost) == 0 && len(next.Contributions) == 0 {
		next.Legacy = ms.Usage.Clone()
	}
	for host, u := range ms.UsageByHost {
		prev, exists := next.Contributions[host]
		if exists && (host != s.Self.ID || (prev.Revision > 0 && prev.Usage.Equal(u))) {
			continue
		}
		revision := uint64(0)
		if host == s.Self.ID {
			revision = prev.Revision + 1
		}
		next.Contributions[host] = AnalyticsContribution{Revision: revision, Usage: u.Clone()}
	}
	if len(next.Contributions) > 0 {
		next.Legacy = Usage{}
	}
	if !reflect.DeepEqual(old, next) {
		next.Revision = old.Revision + 1
		next.Writer = s.Self.ID
	}
	s.Analytics[ms.ID] = next
}

// mergeAnalytics retains deleted records independently of expiring tombstones.
// A contribution is replaced by its newer cumulative reading, never summed
// with another copy of itself. Old peers omitting history cannot erase it.
func (s *Snapshot) mergeAnalytics(incoming map[MissionID]AnalyticsRecord, primary bool) bool {
	if len(incoming) == 0 {
		return false
	}
	if s.Analytics == nil {
		s.Analytics = map[MissionID]AnalyticsRecord{}
	}
	changed := false
	for id, in := range incoming {
		old, exists := s.Analytics[id]
		next := old.Clone()
		if !exists || in.Revision > old.Revision || (in.Revision == old.Revision && in.Writer > old.Writer) {
			next = in.Clone()
		}
		// Spec revisions and operation revisions follow the same primary tie-break
		// as live missions, independently of how many measurements each host took.
		spec := old
		if !exists || incomingWins(old.SpecRev, in.SpecRev, primary) {
			spec = in
		}
		next.Name = spec.Name
		next.OperationID = spec.OperationID
		next.Tool = spec.Tool
		next.Model = spec.Model
		next.Effort = spec.Effort
		next.PlanModel = spec.PlanModel
		next.PlanEffort = spec.PlanEffort
		next.PlanMode = spec.PlanMode
		next.SpecRev = spec.SpecRev
		operation := old
		if !exists || incomingWins(old.OperationRev, in.OperationRev, primary) {
			operation = in
		}
		next.OperationName = operation.OperationName
		next.OperationRev = operation.OperationRev
		// A newer lease outranks any amount of history from the previous holder.
		run := next
		if old.Lease.Epoch > in.Lease.Epoch {
			run = old
		} else if in.Lease.Epoch > old.Lease.Epoch {
			run = in
		}
		next.Lease = run.Lease
		next.Status = run.Status
		next.StartedAt = run.StartedAt
		next.FinishedAt = run.FinishedAt
		next.Deleted = old.Deleted || in.Deleted
		// Metadata and measurements have independent revisions.
		for host, c := range old.Contributions {
			if other, ok := next.Contributions[host]; !ok || c.Revision > other.Revision {
				next.Contributions[host] = c
			}
		}
		for host, c := range in.Contributions {
			if other, ok := next.Contributions[host]; !ok || c.Revision > other.Revision {
				next.Contributions[host] = c
			}
		}
		if len(next.Contributions) > 0 {
			next.Legacy = Usage{}
		}
		if !reflect.DeepEqual(old, next) {
			s.Analytics[id] = next.Clone()
			changed = true
		}
	}
	return changed
}
