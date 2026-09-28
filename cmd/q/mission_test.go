package main

import (
	"maps"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

// --base names a repo rather than a path, because a mission's base branches are
// keyed by name, which is also how the board and the operation refer to a repo.
func TestParseBaseFlags(t *testing.T) {
	cases := []struct {
		name    string
		values  []string
		want    map[string]string
		wantErr bool
	}{
		{
			name: "no flags means no overrides",
		},
		{
			name:   "one repo on a branch",
			values: []string{"q=feat/x"},
			want:   map[string]string{"q": "feat/x"},
		},
		{
			name:   "repeated flags accumulate",
			values: []string{"q=feat/x", "mac=main"},
			want:   map[string]string{"q": "feat/x", "mac": "main"},
		},
		{
			name:   "a branch containing an equals sign survives",
			values: []string{"q=feat/a=b"},
			want:   map[string]string{"q": "feat/a=b"},
		},
		{
			name:   "surrounding space is trimmed",
			values: []string{" q = feat/x "},
			want:   map[string]string{"q": "feat/x"},
		},
		{
			name:    "a value with no branch is refused",
			values:  []string{"q"},
			wantErr: true,
		},
		{
			name:    "an empty branch is refused",
			values:  []string{"q="},
			wantErr: true,
		},
		{
			name:    "an empty repo is refused",
			values:  []string{"=main"},
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseBaseFlags(tc.values)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseBaseFlags(%q) succeeded, want an error", tc.values)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseBaseFlags(%q): %v", tc.values, err)
			}

			if !maps.Equal(got, tc.want) {
				t.Errorf("parseBaseFlags(%q) = %v, want %v", tc.values, got, tc.want)
			}
		})
	}
}

// An unflagged --tool has to reach the daemon as empty. If the client filled in a
// default of its own, every request would name an agent, and --from could never
// inherit one.
func TestParseToolFlag(t *testing.T) {
	cases := []struct {
		name    string
		value   string
		want    mission.Tool
		wantErr bool
	}{
		{
			name:  "an unflagged tool stays empty for the daemon to resolve",
			value: "",
			want:  "",
		},
		{
			name:  "whitespace only is still empty",
			value: "  ",
			want:  "",
		},
		{
			name:  "a named agent is taken",
			value: "codex",
			want:  mission.ToolCodex,
		},
		{
			name:    "an unknown agent is refused",
			value:   "emacs",
			wantErr: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseToolFlag(tc.value)

			if tc.wantErr {
				if err == nil {
					t.Fatalf("parseToolFlag(%q) succeeded, want an error", tc.value)
				}

				return
			}

			if err != nil {
				t.Fatalf("parseToolFlag(%q): %v", tc.value, err)
			}

			if got != tc.want {
				t.Errorf("parseToolFlag(%q) = %q, want %q", tc.value, got, tc.want)
			}
		})
	}
}

func TestOrDefaultTool(t *testing.T) {
	if got := orDefaultTool(""); got != mission.DefaultTool {
		t.Errorf("orDefaultTool(\"\") = %q, want %q", got, mission.DefaultTool)
	}

	if got := orDefaultTool(mission.ToolAgy); got != mission.ToolAgy {
		t.Errorf("orDefaultTool(agy) = %q, want agy", got)
	}
}

// An agent running inside a mission can create a follow-up without naming the
// mission it should match, because q tells it which one it is in.
func TestInheritTargetFallsBackToTheEnvironment(t *testing.T) {
	t.Setenv(mission.EnvMissionID, "ms_running")

	if got := inheritTarget(""); got != "ms_running" {
		t.Errorf("inheritTarget(\"\") = %q, want ms_running", got)
	}
}

func TestInheritTargetPrefersTheFlag(t *testing.T) {
	t.Setenv(mission.EnvMissionID, "ms_running")

	if got := inheritTarget("ms_explicit"); got != "ms_explicit" {
		t.Errorf("inheritTarget = %q, want the flag to win", got)
	}
}

func TestInheritTargetIsEmptyOutsideAMission(t *testing.T) {
	t.Setenv(mission.EnvMissionID, "")

	if got := inheritTarget(""); got != "" {
		t.Errorf("inheritTarget(\"\") = %q, want empty", got)
	}
}

func TestWantsModelDefault(t *testing.T) {
	cases := []struct {
		name  string
		model string
		from  mission.MissionID
		want  bool
	}{
		{
			name: "no model and no parent means resolve the default",
			want: true,
		},
		{
			name:  "a named model is used as given",
			model: "claude-opus-5",
			want:  false,
		},
		{
			// The parent may have run on no model at all, in which case the
			// daemon's catalog has no business upgrading its child out of step.
			name: "a parent owns the answer, even an empty one",
			from: "ms_parent",
			want: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := wantsModelDefault(tc.model, tc.from); got != tc.want {
				t.Errorf("wantsModelDefault(%q, %q) = %v, want %v", tc.model, tc.from, got, tc.want)
			}
		})
	}
}
