package mission

import (
	"strings"
	"testing"
)

func TestCombineRepos(t *testing.T) {
	tests := map[string]struct {
		inherited  []Repo
		additional []Repo
		wantNames  []string
		wantErr    string
	}{
		"combines inherited and additional repos": {
			inherited:  []Repo{{Name: "q", Path: "/dev/q"}},
			additional: []Repo{{Name: "mac", Path: "/dev/mac"}},
			wantNames:  []string{"q", "mac"},
		},
		"ignores an exact duplicate": {
			inherited:  []Repo{{Name: "q", Path: "/dev/q"}},
			additional: []Repo{{Name: "q", Path: "/dev/q"}},
			wantNames:  []string{"q"},
		},
		"rejects one name for different paths": {
			inherited:  []Repo{{Name: "q", Path: "/dev/work/q"}},
			additional: []Repo{{Name: "q", Path: "/dev/personal/q"}},
			wantErr:    "repo name",
		},
		"rejects one path with different names": {
			inherited:  []Repo{{Name: "q", Path: "/dev/q"}},
			additional: []Repo{{Name: "quartermaster", Path: "/dev/q"}},
			wantErr:    "repo path",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			got, err := CombineRepos(test.inherited, test.additional)
			if test.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), test.wantErr) {
					t.Fatalf("CombineRepos() error = %v, want containing %q", err, test.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("CombineRepos(): %v", err)
			}

			if len(got) != len(test.wantNames) {
				t.Fatalf("len = %d, want %d: %+v", len(got), len(test.wantNames), got)
			}

			for index, want := range test.wantNames {
				if got[index].Name != want {
					t.Errorf("repo %d name = %q, want %q", index, got[index].Name, want)
				}
			}
		})
	}
}

func TestMissionReposUsesFrozenLaunchRepos(t *testing.T) {
	operation := Operation{Repos: []Repo{{Name: "new", Path: "/dev/new"}}}
	ms := Mission{
		ExtraRepos:        []Repo{{Name: "extra", Path: "/dev/extra"}},
		LaunchRepos:       []Repo{{Name: "original", Path: "/dev/original"}},
		LaunchReposFrozen: true,
	}

	got, err := MissionRepos(operation, ms)
	if err != nil {
		t.Fatalf("MissionRepos(): %v", err)
	}

	if len(got) != 1 || got[0].Name != "original" {
		t.Fatalf("MissionRepos() = %+v, want frozen launch repo", got)
	}
}

func TestMissionReposKeepsRepoLessLaunchFrozen(t *testing.T) {
	operation := Operation{Repos: []Repo{{Name: "later", Path: "/dev/later"}}}
	ms := Mission{LaunchReposFrozen: true}

	got, err := MissionRepos(operation, ms)
	if err != nil {
		t.Fatalf("MissionRepos(): %v", err)
	}

	if len(got) != 0 {
		t.Fatalf("MissionRepos() = %+v, want no repos", got)
	}
}
