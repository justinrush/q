package main

import (
	"context"
	"errors"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/runner"
	"github.com/justinrush/q/internal/state"
)

func TestReportCodexAppServer(t *testing.T) {
	tests := map[string]struct {
		failure error
		want    string
	}{
		"available": {
			want: "codex app-server  available  remote TUI + status proxy",
		},
		"unavailable": {
			failure: errors.New("unsupported"),
			want:    "codex app-server  UNAVAILABLE  unsupported",
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			run := runner.NewFake()
			argv := "/bin/codex app-server proxy --help"
			if test.failure != nil {
				run.ExpectError(argv, test.failure)
			} else {
				run.Expect(argv, "help")
			}

			rep := newReport()
			reportCodexAppServer(context.Background(), rep, run, "/bin/codex")

			if got := strings.TrimSpace(rep.String()); got != test.want {
				t.Errorf("report = %q, want %q", got, test.want)
			}
		})
	}
}

func TestOwnedMissionDirs(t *testing.T) {
	tests := map[string]struct {
		missionDir    string
		wantActual    bool
		wantPredicted bool
	}{
		"draft reserves predicted directory": {
			wantPredicted: true,
		},
		"launched mission owns only actual directory": {
			missionDir:    "/data/missions/operation--mission--aabbccddeeff",
			wantActual:    true,
			wantPredicted: false,
		},
	}

	for name, test := range tests {
		t.Run(name, func(t *testing.T) {
			dirs := paths.Dirs{Data: "/data"}
			snap := state.Snapshot{
				Operations: []domain.Operation{{
					ID:   "op_aabbccddeeff",
					Slug: "operation",
				}},
				Missions: []domain.Mission{{
					ID:          "ms_aabbccddeeff",
					OperationID: "op_aabbccddeeff",
					Slug:        "mission",
					MissionDir:  test.missionDir,
				}},
			}

			known := ownedMissionDirs(dirs, snap)
			predicted := filepath.Join("/data", "missions", "operation--mission")

			if known[predicted] != test.wantPredicted {
				t.Errorf("predicted directory known = %v, want %v", known[predicted], test.wantPredicted)
			}

			if test.missionDir != "" && known[test.missionDir] != test.wantActual {
				t.Errorf("actual directory known = %v, want %v", known[test.missionDir], test.wantActual)
			}
		})
	}
}
