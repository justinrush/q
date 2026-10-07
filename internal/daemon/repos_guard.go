package daemon

import (
	"fmt"
	"strings"

	"github.com/justinrush/q/internal/mission"
)

// A mission is only run on a host that has every repository it needs.
//
// With one machine this could not come up: an operation's repositories were
// picked from that machine's own disk. With two, a mission written on one can
// name a repository the other has never cloned. Starting an agent anyway would
// give it a workspace with a piece missing, and an agent that finds a
// repository absent does not stop and say so. It improvises around the gap,
// which is worse than not starting.

// unlocated names the repositories a mission needs that this host has no
// checkout of.
func unlocated(snap mission.Snapshot, ms mission.Mission) []string {
	operation, _ := snap.Operation(ms.OperationID)

	repos, err := mission.MissionRepos(operation, ms)
	if err != nil {
		return nil
	}

	var missing []string

	for _, repo := range repos {
		if repo.Path == "" {
			missing = append(missing, repoName(repo))
		}
	}

	return missing
}

// repoName names a repository for a person, with its origin when known, since
// the origin is what they would clone.
func repoName(repo mission.Repo) string {
	if repo.URL != "" {
		return repo.Name + " (" + repo.URL + ")"
	}

	return repo.Name
}

// errUnlocated explains that a mission cannot run here and what would fix it.
func errUnlocated(ms mission.Mission, missing []string) error {
	return fmt.Errorf(
		"%w: %s needs %s, which is not checked out on this machine; clone it under one of repos.roots",
		ErrConflict, ms.Name, strings.Join(missing, ", "))
}

// mirrorComplete reports whether this host has a worktree on disk for every
// repository a launched mission was given.
//
// A mirror is built one repository at a time as snapshots arrive, so for a
// while after a mission launches elsewhere it is partial. It is also partial,
// permanently, when one of the repositories is not checked out here.
func mirrorComplete(ms mission.Mission) bool {
	if ms.MissionDir == "" {
		return false
	}

	for _, repo := range ms.LaunchRepos {
		if _, ok := liveWork(ms, repo.Name); !ok {
			return false
		}
	}

	return true
}

// markUnlocated shows on a card, or clears from it, that the mission cannot
// run on this host for want of a repository.
func (s *Service) markUnlocated(ms mission.Mission, missing []string) {
	detail := strings.Join(missing, ", ")

	current := ""
	for _, badge := range ms.LocalBadges {
		if badge.Kind == mission.BadgeRepoMissing {
			current = badge.Detail
		}
	}

	if ms.HasLocalBadge(mission.BadgeRepoMissing) == (len(missing) > 0) && current == detail {
		return
	}

	s.updateLocal(ms.ID, "mission.repo_missing", func(stored *mission.Mission) {
		if len(missing) > 0 {
			stored.LocalBadges = stored.WithLocalBadge(mission.BadgeRepoMissing, detail)
		} else {
			stored.LocalBadges = stored.WithoutLocalBadge(mission.BadgeRepoMissing)
		}
	})
}
