// What an exchange with a paired q requires of this machine.
//
// The merge decides what the records say. This file makes the disk and the
// terminal agree with them: finding the repositories the peer named, stopping
// an agent whose mission moved away, and removing worktrees for missions that
// no longer need them.

package daemon

import (
	"context"

	"github.com/justinrush/q/internal/mission"
)

// forEachRepo calls fn for every repository recorded in the snapshot, writing
// back the ones fn changes and reporting whether any were.
func forEachRepo(snap *mission.Snapshot, fn func(*mission.Repo) bool) bool {
	changed := false

	visit := func(repos []mission.Repo) bool {
		touched := false

		for i := range repos {
			if fn(&repos[i]) {
				touched = true
			}
		}

		return touched
	}

	for i := range snap.Operations {
		if visit(snap.Operations[i].Repos) {
			changed = true
		}
	}

	for i := range snap.Missions {
		extra, launch := visit(snap.Missions[i].ExtraRepos), visit(snap.Missions[i].LaunchRepos)
		if extra || launch {
			changed = true
		}
	}

	return changed
}

// fillRepoURLs records the origin of every repository that does not have one
// yet, so the peer can recognize it.
//
// It is done lazily, just before an exchange, rather than when an operation is
// created. Most q installations are never paired and should not pay a
// subprocess per repository for an identifier nothing reads.
func (s *Service) fillRepoURLs(ctx context.Context) {
	if s.locator == nil {
		return
	}

	origins := map[string]string{}

	snap := s.store.Snapshot()

	forEachRepo(&snap, func(repo *mission.Repo) bool {
		if repo.URL != "" || repo.Path == "" {
			return false
		}

		if _, asked := origins[repo.Path]; !asked {
			origin, err := s.locator.OriginURL(ctx, repo.Path)
			if err != nil {
				s.warn("reading a repository's origin", "repo", repo.Path, "error", err)
			}

			origins[repo.Path] = origin
		}

		return false
	})

	fill := func(repo *mission.Repo) bool {
		if repo.URL != "" || origins[repo.Path] == "" {
			return false
		}

		repo.URL = origins[repo.Path]

		return true
	}

	// A repository with no origin has nothing to record, and asking again on
	// every exchange would be a subprocess every few seconds for the same
	// answer. Checking for work first keeps that to a map lookup.
	if !forEachRepo(&snap, fill) {
		return
	}

	err := s.store.Mutate("repos.origin", func(snap *mission.Snapshot) error {
		if !forEachRepo(snap, fill) {
			return mission.ErrUnchanged
		}

		return nil
	})
	if err != nil {
		s.warn("recording repository origins", "error", err)
	}
}

// locateRepos finds this machine's checkout of every repository the peer named
// and this host has no path for.
//
// A repository that cannot be found is left without a path. The mission that
// needs it then fails to launch here with an error naming the repository,
// which is the right outcome: the fix is to clone it, and q should not guess.
func (s *Service) locateRepos(ctx context.Context) {
	if s.locator == nil {
		return
	}

	snap := s.store.Snapshot()
	found := map[string]string{}

	// A checkout this host already uses for the same origin is the best answer,
	// and costs nothing to find.
	forEachRepo(&snap, func(repo *mission.Repo) bool {
		if repo.URL != "" && repo.Path != "" {
			found[repo.URL] = repo.Path
		}

		return false
	})

	locate := func(repo *mission.Repo) bool {
		if repo.Path != "" || repo.URL == "" {
			return false
		}

		path, ok := found[repo.URL]
		if !ok {
			if path, ok = s.locator.Locate(ctx, repo.URL); !ok {
				return false
			}

			found[repo.URL] = path
		}

		repo.Path = path

		return true
	}

	if !forEachRepo(&snap, locate) {
		return
	}

	err := s.store.Apply("repos.locate", func(snap *mission.Snapshot) error {
		if !forEachRepo(snap, locate) {
			return mission.ErrUnchanged
		}

		return nil
	})
	if err != nil {
		s.warn("recording located repositories", "error", err)
	}
}

// reclaimLocal removes whatever this host provisioned for a mission the peer
// deleted.
//
// It is forced. The delete was a decision a human made on the other machine,
// and what is here is that machine's work mirrored; refusing would leave a
// worktree that no card will ever point at again.
func (s *Service) reclaimLocal(ctx context.Context, ms mission.Mission) {
	if s.reclaimer == nil || ms.MissionDir == "" {
		return
	}

	operation, _ := s.store.Snapshot().Operation(ms.OperationID)

	report, err := s.reclaimer.Reclaim(ctx, operation, ms, true)
	if err != nil {
		s.warn("reclaiming a mission the paired q deleted", "mission", ms.ID, "error", err)

		return
	}

	for _, failure := range report.Failures {
		s.warn("reclaiming a mission the paired q deleted", "mission", ms.ID, "error", failure)
	}
}

// reclaimStale removes this host's copy of missions that no longer run
// anywhere.
//
// When the holder closes a mission its own worktrees go with it, in the same
// step. The other host learns of the close from an exchange and is left with a
// mirror nothing will update again. A provisioned directory on a mission that
// is not launched is exactly that case.
func (s *Service) reclaimStale(ctx context.Context) {
	for _, ms := range s.store.Snapshot().Missions {
		if ms.MissionDir == "" || ms.Launched() {
			continue
		}

		s.reclaimLocal(ctx, ms)
		s.clearLocal(ms.ID)
	}
}

// clearLocal forgets what this host provisioned for a mission.
func (s *Service) clearLocal(id mission.MissionID) {
	s.updateLocal(id, "mission.mirror_reclaimed", func(ms *mission.Mission) {
		ms.MissionDir = ""
		ms.TmuxSession = ""
		ms.AgentPaneID = ""
		ms.AgentSessionID = ""
		ms.TranscriptPath = ""
		ms.LocalBadges = nil
		ms.Work = nil
		ms.HookEpoch++
	})
}

// standDown stops whatever session this host has for a mission whose lease just
// moved.
//
// For a mission that moved away, this is the other half of a takeover. The peer took the mission because this
// host went quiet, but a sleeping laptop is frozen rather than dead: its agent
// wakes with it and carries on. Leaving it would put two agents on one
// mission. Its hooks are already ignored; this stops the process.
//
// The agent's own session id is kept. If the mission later returns here, the
// agent is resumed into the conversation it had, which is worth more than a
// fresh start even though part of the story happened on another machine.
func (s *Service) standDown(ctx context.Context, id mission.MissionID) {
	ms, ok := s.store.Snapshot().Mission(id)
	if !ok || ms.TmuxSession == "" {
		return
	}

	if s.messenger != nil {
		if err := s.messenger.Stop(ctx, ms); err != nil {
			s.warn("stopping an agent whose mission moved to the paired q", "mission", id, "error", err)
		}
	}

	s.updateLocal(id, "mission.stood_down", func(ms *mission.Mission) {
		ms.TmuxSession = ""
		ms.AgentPaneID = ""
		ms.HookEpoch++
	})
}

// updateLocal changes fields that are true on this host only.
//
// It goes through Apply rather than Mutate. Nothing here is a change the peer
// should hear about, and Mutate would treat a write to a mission this host
// does not run as a claim on it.
func (s *Service) updateLocal(id mission.MissionID, label string, fn func(*mission.Mission)) {
	var updated mission.Mission

	err := s.store.Apply(label, func(snap *mission.Snapshot) error {
		ms, ok := snap.Mission(id)
		if !ok {
			return mission.ErrUnchanged
		}

		fn(&ms)
		updated = ms
		snap.PutMission(ms)

		return nil
	})
	if err != nil {
		s.warn("updating a mission's local state", "mission", id, "error", err)

		return
	}

	if updated.ID != "" {
		s.publishMission(updated)
	}
}
