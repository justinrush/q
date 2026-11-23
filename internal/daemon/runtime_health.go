package daemon

import "github.com/justinrush/q/internal/mission"

// Report an unavailable observer once per outage, without claiming that the
// agent itself failed or overwriting a hook's more recent status.
func (s *Service) markRuntimeUnavailable(observed mission.Mission, cause error) {
	var updated mission.Mission
	// A badge the next poll after a restart would raise again.
	err := s.store.MutateDeferred("mission.runtime_unavailable", func(snap *mission.Snapshot) (bool, error) {
		ms, ok := snap.Mission(observed.ID)
		if !ok || ms.Status.Terminal() || !s.holds(ms) ||
			ms.HookEpoch != observed.HookEpoch || !ms.UpdatedAt.Equal(observed.UpdatedAt) ||
			ms.HasBadge(mission.BadgeRuntimeUnavailable) {
			return false, mission.ErrUnchanged
		}
		ms.Badges = ms.WithBadge(mission.BadgeRuntimeUnavailable, "")
		ms.UpdatedAt = s.now()
		updated = ms
		snap.PutMission(ms)
		return false, nil
	})
	if err != nil {
		s.warn("recording unavailable agent runtime", "error", err)
		return
	}
	if updated.ID != "" {
		s.warn("agent runtime status unavailable", "mission", updated.ID, "error", cause)
		s.publishMission(updated)
	}
}
