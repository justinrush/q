package main

import (
	"context"
	"os"
	"path/filepath"
	"slices"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
)

// reportOrphans lists mission directories and tmux sessions with no mission behind them.
//
// These accumulate from crashes, from a mission record deleted before its resources were
// reclaimed, and from earlier versions of q that did not reclaim at all. They are
// reported rather than removed: q cannot tell whether a stray worktree holds work
// somebody still wants, so deleting it is the human's call.
func reportOrphans(ctx context.Context, rep *report, dirs paths.Dirs) {
	rep.line("orphans")

	known, ok := knownMissionDirs(ctx, dirs)
	if !ok {
		rep.line("  cannot check: the daemon is not reachable")
		rep.line("")

		return
	}

	strays := strayMissionDirs(dirs, known)
	if len(strays) == 0 {
		rep.line("  none")
		rep.line("")

		return
	}

	rep.line("  %d mission directory(ies) with no mission:", len(strays))

	for _, dir := range strays {
		rep.line("    %s", dir)
	}

	rep.line("")
	rep.line("  Nothing was removed. Inspect them, then reclaim with:")
	rep.line("    git -C <your checkout> worktree remove <path>")
	rep.line("")
}

// knownMissionDirs returns the mission directories the daemon still accounts for.
//
// The second result is false when the daemon cannot be reached, which is different from
// there being no missions: without it, every directory would look orphaned.
func knownMissionDirs(ctx context.Context, dirs paths.Dirs) (map[string]bool, bool) {
	c, err := api.Connect(ctx, dirs)
	if err != nil {
		return nil, false
	}

	snap, err := c.State(ctx)
	if err != nil {
		return nil, false
	}

	return ownedMissionDirs(dirs, snap), true
}

// ownedMissionDirs reports the directory each mission currently owns. A mission in briefing has no
// persisted MissionDir yet, so its future slug-based path is reserved. Once a
// launch chooses an actual directory, only that path belongs to the mission.
func ownedMissionDirs(dirs paths.Dirs, snap mission.Snapshot) map[string]bool {
	known := make(map[string]bool, len(snap.Missions))

	for _, ms := range snap.Missions {
		if ms.MissionDir != "" {
			known[ms.MissionDir] = true

			continue
		}

		// A mission that has never launched still reserves the directory name it would
		// get, so a directory created immediately before state commit is not
		// reported as a stray.
		if operation, ok := snap.Operation(ms.OperationID); ok {
			known[dirs.MissionDir(mission.MissionDirName(operation.Slug, ms.Slug))] = true
		}
	}

	return known
}

// strayMissionDirs lists directories under the missions directory that no mission claims.
func strayMissionDirs(dirs paths.Dirs, known map[string]bool) []string {
	entries, err := os.ReadDir(dirs.MissionsDir())
	if err != nil {
		return nil
	}

	var strays []string

	for _, entry := range entries {
		if !entry.IsDir() {
			continue
		}

		path := filepath.Join(dirs.MissionsDir(), entry.Name())
		if !known[path] {
			strays = append(strays, path)
		}
	}

	slices.Sort(strays)

	return strays
}
