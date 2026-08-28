package mission

import (
	"maps"
	"slices"
	"strings"
	"time"
)

// SchemaVersion is the current on-disk format version. Bump it when a change
// requires migrating existing state, and add a case to migrate.
const SchemaVersion = 1

// Snapshot is the complete persisted state of q.
type Snapshot struct {
	SchemaVersion int         `json:"schemaVersion"`
	Operations    []Operation `json:"operations"`
	Missions      []Mission   `json:"missions"`
	UpdatedAt     time.Time   `json:"updatedAt"`
}

// Clone returns a deep copy.
//
// Every read handed outside the store is a clone. Returning the live slices
// would let a caller mutate shared state without holding the lock, which with
// hooks, the reconciler, and the TUI all touching the store concurrently would
// be a data race waiting to happen.
func (s Snapshot) Clone() Snapshot {
	out := s
	out.Operations = make([]Operation, len(s.Operations))
	out.Missions = make([]Mission, len(s.Missions))

	for i, t := range s.Operations {
		t.Repos = slices.Clone(t.Repos)
		out.Operations[i] = t
	}

	for i, t := range s.Missions {
		out.Missions[i] = cloneMission(t)
	}

	return out
}

// cloneMission deep copies a mission's reference types.
func cloneMission(t Mission) Mission {
	t.Badges = slices.Clone(t.Badges)
	t.ExtraRepos = slices.Clone(t.ExtraRepos)
	t.LaunchRepos = slices.Clone(t.LaunchRepos)

	if t.Work != nil {
		work := make(map[string]RepoWork, len(t.Work))
		maps.Copy(work, t.Work)
		t.Work = work
	}

	if t.StartedAt != nil {
		started := *t.StartedAt
		t.StartedAt = &started
	}

	if t.FinishedAt != nil {
		finished := *t.FinishedAt
		t.FinishedAt = &finished
	}

	return t
}

// Operation returns the operation with the given id.
func (s Snapshot) Operation(id OperationID) (Operation, bool) {
	for _, t := range s.Operations {
		if t.ID == id {
			return t, true
		}
	}

	return Operation{}, false
}

// Mission returns the mission with the given id.
func (s Snapshot) Mission(id MissionID) (Mission, bool) {
	for _, t := range s.Missions {
		if t.ID == id {
			return t, true
		}
	}

	return Mission{}, false
}

// MissionBySession returns the mission running the given agent session.
//
// This is the second of the reducer's three identity strategies, used when a
// hook arrives without the mission id in its environment.
func (s Snapshot) MissionBySession(tool Tool, sessionID string) (Mission, bool) {
	if sessionID == "" {
		return Mission{}, false
	}

	for _, t := range s.Missions {
		if t.Tool == tool && t.AgentSessionID == sessionID {
			return t, true
		}
	}

	return Mission{}, false
}

// MissionByDir returns the mission whose working directory is dir.
//
// This is the reducer's last-resort identity strategy. It works because mission
// directories are unique per mission by construction, which is one of the payoffs
// of keeping worktrees in a central q-owned tree rather than scattered
// beside the user's checkouts.
func (s Snapshot) MissionByDir(dir string) (Mission, bool) {
	if dir == "" {
		return Mission{}, false
	}

	for _, t := range s.Missions {
		if t.MissionDir == dir {
			return t, true
		}
	}

	return Mission{}, false
}

// MissionsInLane returns the missions in a lane, ordered for display.
func (s Snapshot) MissionsInLane(status Status) []Mission {
	var out []Mission

	for _, t := range s.Missions {
		if t.Status == status {
			out = append(out, t)
		}
	}

	sortForDisplay(out)

	return out
}

// MissionsForOperation returns every mission belonging to an operation.
func (s Snapshot) MissionsForOperation(id OperationID) []Mission {
	var out []Mission

	for _, t := range s.Missions {
		if t.OperationID == id {
			out = append(out, t)
		}
	}

	sortForDisplay(out)

	return out
}

// ActiveMissionsForOperation returns the operation's missions that are not yet done, which
// is what makes deleting an operation unsafe.
func (s Snapshot) ActiveMissionsForOperation(id OperationID) []Mission {
	var out []Mission

	for _, t := range s.MissionsForOperation(id) {
		if t.Status != StatusClosed {
			out = append(out, t)
		}
	}

	return out
}

// sortForDisplay orders missions by explicit position, then by recency, then by id
// so the ordering is total and stable across processes.
func sortForDisplay(missions []Mission) {
	slices.SortFunc(missions, func(a, b Mission) int {
		if a.Order != b.Order {
			return a.Order - b.Order
		}

		if !a.CreatedAt.Equal(b.CreatedAt) {
			return a.CreatedAt.Compare(b.CreatedAt)
		}

		return strings.Compare(string(a.ID), string(b.ID))
	})
}

// NextOrder returns an Order value that places a mission at the end of a lane.
func (s Snapshot) NextOrder(status Status) int {
	next := 0

	for _, t := range s.Missions {
		if t.Status == status && t.Order >= next {
			next = t.Order + 1
		}
	}

	return next
}

// NextColorIdx returns the lowest palette index no active operation is using,
// wrapping once every slot is taken.
//
// Recycling freed indices keeps stripes distinguishable in the common case where
// operations are created and finished over time, which a monotonic counter would
// not.
func (s Snapshot) NextColorIdx() int {
	used := make(map[int]bool, len(s.Operations))
	for _, t := range s.Operations {
		used[t.ColorIdx] = true
	}

	for i := range PaletteSize {
		if !used[i] {
			return i
		}
	}

	return len(s.Operations) % PaletteSize
}

// PutOperation inserts or replaces an operation.
func (s *Snapshot) PutOperation(t Operation) {
	for i := range s.Operations {
		if s.Operations[i].ID == t.ID {
			s.Operations[i] = t

			return
		}
	}

	s.Operations = append(s.Operations, t)
}

// DeleteOperation removes an operation, reporting whether it was present.
func (s *Snapshot) DeleteOperation(id OperationID) bool {
	for i := range s.Operations {
		if s.Operations[i].ID == id {
			s.Operations = slices.Delete(s.Operations, i, i+1)

			return true
		}
	}

	return false
}

// PutMission inserts or replaces a mission.
func (s *Snapshot) PutMission(t Mission) {
	for i := range s.Missions {
		if s.Missions[i].ID == t.ID {
			s.Missions[i] = t

			return
		}
	}

	s.Missions = append(s.Missions, t)
}

// DeleteMission removes a mission, reporting whether it was present.
func (s *Snapshot) DeleteMission(id MissionID) bool {
	for i := range s.Missions {
		if s.Missions[i].ID == id {
			s.Missions = slices.Delete(s.Missions, i, i+1)

			return true
		}
	}

	return false
}
