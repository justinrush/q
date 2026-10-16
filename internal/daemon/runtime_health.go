package daemon

import "github.com/justinrush/q/internal/mission"

// Report an unavailable observer once per outage, without claiming that the
// agent itself failed or overwriting a hook's more recent status.
func (s *Service) markRuntimeUnavailable(observed mission.Mission, cause error) {
	var updated mission.Mission
	err := s.store.Mutate("mission.runtime_unavailable", func(snap *mission.Snapshot) error {
		ms, ok := snap.Mission(observed.ID)
		if !ok || ms.Status.Terminal() || !s.holds(ms) ||
			ms.HookEpoch != observed.HookEpoch || !ms.UpdatedAt.Equal(observed.UpdatedAt) ||
			ms.HasBadge(mission.BadgeRuntimeUnavailable) {
			return nil
		}
		ms.Badges = ms.WithBadge(mission.BadgeRuntimeUnavailable, "")
		ms.UpdatedAt = s.now()
		updated = ms
		snap.PutMission(ms)
		return nil
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
