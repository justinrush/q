package git

import (
	"encoding/json"
	"errors"
	"fmt"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
	"os"
	"path/filepath"
)

const provisionStateFile = "provision.json"

// provisionState claims a mission directory for one mission and journals each
// successfully provisioned worktree. The journal lets a launch resume after the
// daemon exits between git worktree add and committing the mission to the store.
type provisionState struct {
	MissionID mission.MissionID           `json:"missionId"`
	Work      map[string]mission.RepoWork `json:"work,omitempty"`
}

// prepareMissionDir chooses and claims the directory used by mission.
//
// Older q versions did not write an ownership marker. An existing
// unmarked directory is therefore not safe to adopt: another mission with the same
// slug may own it. In that case the mission ID provides a stable unique suffix.
func (p *Provisioner) prepareMissionDir(operation mission.Operation, ms *mission.Mission) (provisionState, error) {
	name := mission.MissionDirName(operation.Slug, ms.Slug)
	candidates := []string{
		p.dirs.MissionDir(name),
		p.dirs.MissionDir(name + "--" + ms.ID.Short()),
	}

	for index, candidate := range candidates {
		state, available, err := claimMissionDir(candidate, ms.ID)
		if err != nil {
			return provisionState{}, err
		}

		if available {
			ms.MissionDir = candidate

			return state, nil
		}

		if index == len(candidates)-1 {
			return provisionState{}, fmt.Errorf("mission directory %s belongs to another mission", candidate)
		}
	}

	return provisionState{}, errors.New("no mission directory candidate available")
}

// claimMissionDir reports whether dir is either new or already owned by missionID.
func claimMissionDir(dir string, missionID mission.MissionID) (provisionState, bool, error) {
	artifactDir := filepath.Join(dir, mission.ArtifactDir)
	statePath := filepath.Join(artifactDir, provisionStateFile)

	_, statErr := os.Stat(dir)
	switch {
	case statErr == nil:
		state, err := readProvisionState(statePath)
		if errors.Is(err, os.ErrNotExist) {
			return provisionState{}, false, nil
		}

		if err != nil {
			return provisionState{}, false, err
		}

		return state, state.MissionID == missionID, nil
	case !errors.Is(statErr, os.ErrNotExist):
		return provisionState{}, false, fmt.Errorf("checking mission directory %s: %w", dir, statErr)
	}

	err := os.MkdirAll(artifactDir, paths.DirMode)
	if err != nil {
		return provisionState{}, false, fmt.Errorf("creating mission directory: %w", err)
	}

	state := provisionState{MissionID: missionID, Work: make(map[string]mission.RepoWork)}
	err = createProvisionState(statePath, state)
	if err == nil {
		return state, true, nil
	}

	if !errors.Is(err, os.ErrExist) {
		return provisionState{}, false, err
	}

	state, err = readProvisionState(statePath)
	if err != nil {
		return provisionState{}, false, err
	}

	return state, state.MissionID == missionID, nil
}

func createProvisionState(path string, state provisionState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding provision state: %w", err)
	}

	file, err := os.OpenFile(path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, paths.FileMode)
	if err != nil {
		return fmt.Errorf("claiming mission directory: %w", err)
	}

	_, writeErr := file.Write(append(data, '\n'))
	closeErr := file.Close()
	if writeErr != nil {
		return fmt.Errorf("writing provision state: %w", writeErr)
	}

	if closeErr != nil {
		return fmt.Errorf("closing provision state: %w", closeErr)
	}

	return nil
}

func readProvisionState(path string) (provisionState, error) {
	var state provisionState

	data, err := os.ReadFile(path)
	if err != nil {
		return state, err
	}

	err = json.Unmarshal(data, &state)
	if err != nil {
		return state, fmt.Errorf("reading provision state %s: %w", path, err)
	}

	if state.Work == nil {
		state.Work = make(map[string]mission.RepoWork)
	}

	return state, nil
}

func writeProvisionState(missionDir string, state provisionState) error {
	data, err := json.MarshalIndent(state, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding provision state: %w", err)
	}

	dir := filepath.Join(missionDir, mission.ArtifactDir)
	tmp, err := os.CreateTemp(dir, ".provision-*")
	if err != nil {
		return fmt.Errorf("creating provision state temporary file: %w", err)
	}

	tmpName := tmp.Name()
	defer func() {
		_ = os.Remove(tmpName)
	}()

	err = tmp.Chmod(paths.FileMode)
	if err != nil {
		_ = tmp.Close()

		return fmt.Errorf("setting provision state permissions: %w", err)
	}

	_, err = tmp.Write(append(data, '\n'))
	if err != nil {
		_ = tmp.Close()

		return fmt.Errorf("writing provision state: %w", err)
	}

	err = tmp.Close()
	if err != nil {
		return fmt.Errorf("closing provision state: %w", err)
	}

	err = os.Rename(tmpName, filepath.Join(dir, provisionStateFile))
	if err != nil {
		return fmt.Errorf("replacing provision state: %w", err)
	}

	return nil
}
