package domain

import (
	"fmt"
	"slices"
)

// MissionRepos returns the repositories a mission should use.
//
// A launched mission uses its frozen repository set so later operation edits cannot
// change the worktrees that must be resumed or reclaimed. A draft mission combines
// its operation's repositories with its own additions.
func MissionRepos(operation Operation, mission Mission) ([]Repo, error) {
	if mission.LaunchReposFrozen {
		return slices.Clone(mission.LaunchRepos), nil
	}

	return CombineRepos(operation.Repos, mission.ExtraRepos)
}

// CombineRepos appends additional repositories to an inherited repository set.
// Exact duplicates are ignored. A name or path that identifies two different
// repositories is rejected because mission work is keyed and laid out by repo name.
func CombineRepos(inherited, additional []Repo) ([]Repo, error) {
	byName := make(map[string]Repo)
	byPath := make(map[string]Repo)
	var combined []Repo

	for _, repo := range append(slices.Clone(inherited), additional...) {
		if existing, ok := byName[repo.Name]; ok {
			if existing.Path == repo.Path {
				continue
			}

			return nil, fmt.Errorf("repo name %q refers to both %s and %s", repo.Name, existing.Path, repo.Path)
		}

		if existing, ok := byPath[repo.Path]; ok {
			return nil, fmt.Errorf("repo path %s is named both %q and %q", repo.Path, existing.Name, repo.Name)
		}

		byName[repo.Name] = repo
		byPath[repo.Path] = repo
		combined = append(combined, repo)
	}

	return combined, nil
}
