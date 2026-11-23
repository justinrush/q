package daemon

import (
	"fmt"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
	"os"
	"path/filepath"
	"strings"
)

const maxChildResult = 64 << 10

// PublishChildResult stores a child's report outside its disposable session directory.
func (s *Service) PublishChildResult(id mission.MissionID, report string) (mission.Mission, error) {
	s.finishMu.Lock()
	defer s.finishMu.Unlock()
	if strings.TrimSpace(report) == "" || len(report) > maxChildResult {
		return mission.Mission{}, fmt.Errorf("%w: report must contain 1 to %d bytes", ErrInvalid, maxChildResult)
	}
	snap := s.store.Snapshot()
	child, ok := snap.Mission(id)
	if !ok {
		return child, fmt.Errorf("%w: mission %s", ErrNotFound, id)
	}
	if child.ParentMissionID == "" {
		return child, fmt.Errorf("%w: mission is not a child", ErrInvalid)
	}
	parent, ok := snap.Mission(child.ParentMissionID)
	if !ok || parent.MissionDir == "" {
		return child, fmt.Errorf("%w: parent workspace unavailable", ErrConflict)
	}
	dir := filepath.Join(parent.MissionDir, mission.ArtifactDir, "children", string(child.ID))
	if err := os.MkdirAll(dir, paths.DirMode); err != nil {
		return child, err
	}
	temp, err := os.CreateTemp(dir, ".result-*")
	if err != nil {
		return child, err
	}
	defer os.Remove(temp.Name())
	if err := temp.Chmod(paths.FileMode); err != nil {
		temp.Close()
		return child, err
	}
	if _, err := temp.WriteString(report); err != nil {
		temp.Close()
		return child, err
	}
	if err := temp.Close(); err != nil {
		return child, err
	}
	target := filepath.Join(dir, "result.md")
	if err := os.Rename(temp.Name(), target); err != nil {
		return child, err
	}
	err = s.store.Mutate("mission.child_result", func(snap *mission.Snapshot) error {
		child, ok = snap.Mission(id)
		if !ok {
			return fmt.Errorf("%w: mission %s", ErrNotFound, id)
		}
		parent, ok = snap.Mission(child.ParentMissionID)
		if !ok {
			return fmt.Errorf("%w: parent mission", ErrNotFound)
		}
		child.ResultPath = target
		if parent.ChildReports == nil {
			parent.ChildReports = map[mission.MissionID]string{}
		}
		parent.ChildReports[child.ID] = target
		parent.LocalBadges = parent.WithLocalBadge(mission.BadgeChildResult, string(child.ID))
		snap.PutMission(child)
		snap.PutMission(parent)
		return nil
	})
	if err != nil {
		return child, err
	}
	child, _ = s.store.Mission(id)
	parent, _ = s.store.Mission(child.ParentMissionID)
	s.announce(child)
	s.announce(parent)
	return child, nil
}
