package daemon

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
)

// fakeBrancher answers branch queries with fixed lists, and can fail one call at
// a time so each degraded path can be checked on its own.
type fakeBrancher struct {
	commonDir string
	local     []string
	remote    []string

	commonDirErr error
	localErr     error
	remoteErr    error

	remoteCalls int
}

func (f *fakeBrancher) CommonDir(context.Context, string) (string, error) {
	return f.commonDir, f.commonDirErr
}

func (f *fakeBrancher) LocalBranches(context.Context, string) ([]string, error) {
	return f.local, f.localErr
}

func (f *fakeBrancher) RemoteBranches(context.Context, string) ([]string, error) {
	f.remoteCalls++

	return f.remote, f.remoteErr
}

// The picker is worth more half-filled than not at all, so every way of failing
// to list branches degrades to a shorter list rather than an error.
func TestServiceBranches(t *testing.T) {
	cases := []struct {
		name     string
		brancher *fakeBrancher
		wired    bool
		repo     string
		refresh  bool
		want     []string
	}{
		{
			name:     "local only, without asking origin",
			brancher: &fakeBrancher{commonDir: "/dev/q/.git", local: []string{"main"}, remote: []string{"feat/x"}},
			wired:    true,
			repo:     "/dev/q",
			want:     []string{"main"},
		},
		{
			name:     "refresh merges origin's answer with the local refs",
			brancher: &fakeBrancher{commonDir: "/dev/q/.git", local: []string{"main"}, remote: []string{"feat/x", "main"}},
			wired:    true,
			repo:     "/dev/q",
			refresh:  true,
			want:     []string{"feat/x", "main"},
		},
		{
			name: "an unreachable origin still offers the local refs",
			brancher: &fakeBrancher{
				commonDir: "/dev/q/.git",
				local:     []string{"main"},
				remoteErr: errors.New("network is down"),
			},
			wired:   true,
			repo:    "/dev/q",
			refresh: true,
			want:    []string{"main"},
		},
		{
			name: "a repo git cannot resolve offers nothing",
			brancher: &fakeBrancher{
				commonDirErr: errors.New("not a git repository"),
			},
			wired: true,
			repo:  "/not/a/repo",
			want:  nil,
		},
		{
			name:     "no repo named offers nothing",
			brancher: &fakeBrancher{commonDir: "/dev/q/.git", local: []string{"main"}},
			wired:    true,
			want:     nil,
		},
		{
			name: "a daemon without git offers nothing rather than failing",
			repo: "/dev/q",
			want: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			svc := newTestService(t)
			if tc.wired {
				svc.apply(WithBrancher(tc.brancher))
			}

			got := svc.Branches(t.Context(), tc.repo, tc.refresh)
			if !slices.Equal(got, tc.want) {
				t.Errorf("Branches = %q, want %q", got, tc.want)
			}
		})
	}
}

// A form is opened and reopened while a mission is written, and asking origin on
// every keystroke would put a network round trip in front of the picker.
func TestServiceBranchesCachesOriginsAnswer(t *testing.T) {
	brancher := &fakeBrancher{commonDir: "/dev/q/.git", remote: []string{"feat/x"}}

	svc := newTestService(t)
	svc.apply(WithBrancher(brancher))

	for range 3 {
		svc.Branches(t.Context(), "/dev/q", true)
	}

	if brancher.remoteCalls != 1 {
		t.Errorf("asked origin %d times, want 1", brancher.remoteCalls)
	}
}

// The base branch is baked into the worktree when it is created, so changing it
// afterwards would describe a checkout that is not the one the agent is in.
func TestUpdateMissionBaseBranchesOnlyWhileDraft(t *testing.T) {
	svc := newTestService(t)
	operation := seedOperation(t, svc)

	ms, err := svc.CreateMission(api.CreateMissionRequest{
		OperationID:  operation.ID,
		Name:         "mission",
		Prompt:       "do it",
		BaseBranches: map[string]string{"q": " feat/x ", "mac": "  ", "": "main"},
	})
	if err != nil {
		t.Fatalf("CreateMission: %v", err)
	}

	// Entries naming no branch are dropped rather than stored empty, so the
	// provisioner never has to decide what an empty override means.
	want := map[string]string{"q": "feat/x"}
	if len(ms.BaseBranches) != 1 || ms.BaseBranches["q"] != want["q"] {
		t.Fatalf("BaseBranches = %v, want %v", ms.BaseBranches, want)
	}

	cleared := map[string]string{}

	updated, err := svc.UpdateMission(ms.ID, api.UpdateMissionRequest{BaseBranches: &cleared})
	if err != nil {
		t.Fatalf("UpdateMission: %v", err)
	}

	if len(updated.BaseBranches) != 0 {
		t.Fatalf("BaseBranches = %v, want cleared", updated.BaseBranches)
	}

	updated.Status = mission.StatusActive

	err = svcStore(svc).Mutate("test.launching", func(snap *mission.Snapshot) error {
		snap.PutMission(updated)

		return nil
	})
	if err != nil {
		t.Fatalf("Mutate: %v", err)
	}

	branches := map[string]string{"q": "feat/y"}

	_, err = svc.UpdateMission(ms.ID, api.UpdateMissionRequest{BaseBranches: &branches})
	if !errors.Is(err, ErrConflict) {
		t.Fatalf("UpdateMission() error = %v, want ErrConflict", err)
	}
}
