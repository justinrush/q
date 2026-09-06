// Measuring what missions consume.
//
// Metering is deliberately kept off the hot path. Two of the ten hooks a claude
// session reports fire per tool call — around fifteen hundred events in an
// ordinary session — and re-reading a megabyte transcript on each of them would
// spend more effort accounting for the work than doing it. So a mission is
// measured when a turn ends, plus on the reconciler's tick while it is still
// running, which is what makes the number on the card a running total rather
// than a post-mortem.

package daemon

import (
	"slices"
	"time"

	"github.com/justinrush/q/internal/mission"
)

// meterEvents are the hook events after which a mission's consumption is worth
// re-reading. Each one ends a turn, which is the granularity a transcript
// records usage at anyway.
var meterEvents = map[string]bool{
	mission.EventStop:        true,
	mission.EventStopFailure: true,
	mission.EventSessionEnd:  true,
}

// meterMission re-reads one mission's consumption and stores what changed.
//
// Like the hook path it feeds, this never fails loudly: a meter that cannot
// read a transcript costs the board a cost figure, which is not worth
// interrupting anything else over.
func (s *Service) meterMission(id mission.MissionID) {
	snap := s.store.Snapshot()

	ms, ok := snap.Mission(id)
	if !ok {
		return
	}

	meter, ok := s.meters[ms.Tool]
	if !ok {
		return
	}

	metering, measured, err := meter.Meter(ms)
	if err != nil {
		s.warn("metering a mission", "mission", ms.ID, "error", err)

		return
	}

	if !measured && !metering.Limit.Live(s.now()) {
		return
	}

	s.storeMetering(ms.ID, metering)
}

// storeMetering persists a measurement, writing nothing when it says the same
// thing as the last one.
//
// The check matters because the reconciler meters every running mission every
// fifteen seconds. Without it, a board left open overnight would rewrite the
// state file and append an event a few thousand times to record that nothing
// happened.
func (s *Service) storeMetering(id mission.MissionID, metering mission.Metering) {
	now := s.now()

	snap := s.store.Snapshot()
	if !s.meteringChanges(snap, id, metering, now) {
		return
	}

	var (
		updated      mission.Mission
		missionMoved bool
		limits       []mission.Limit
	)

	err := s.store.Mutate("mission.usage", func(snap *mission.Snapshot) error {
		ms, ok := snap.Mission(id)
		if !ok {
			return nil
		}

		if !metering.Usage.Empty() && !ms.Usage.Equal(metering.Usage) {
			usage := metering.Usage
			usage.At = now
			ms.Usage = usage
			ms.UpdatedAt = now

			snap.PutMission(ms)

			updated, missionMoved = ms, true
		}

		snap.PutLimit(metering.Limit, now)
		limits = slices.Clone(snap.Limits)

		return nil
	})
	if err != nil {
		s.warn("storing a measurement", "mission", id, "error", err)

		return
	}

	if missionMoved {
		s.publishMission(updated)
	}

	s.publishLimits(limits)
}

// meteringChanges reports whether a measurement says anything the stored state
// does not already say.
func (s *Service) meteringChanges(
	snap mission.Snapshot,
	id mission.MissionID,
	metering mission.Metering,
	now time.Time,
) bool {
	ms, ok := snap.Mission(id)
	if !ok {
		return false
	}

	if !metering.Usage.Empty() && !ms.Usage.Equal(metering.Usage) {
		return true
	}

	// PutLimit also drops windows that have since reopened, so an expiring
	// limit is a change even when the measurement found no new one.
	before := slices.Clone(snap.Limits)
	snap.PutLimit(metering.Limit, now)

	return !slices.Equal(before, snap.Limits)
}
