package mission

import (
	"strings"
	"testing"
)

func TestComposeReviewPrompt(t *testing.T) {
	source := Mission{
		ID:          "ms_aabbccddeeff",
		Name:        "Fix login",
		Tool:        ToolClaude,
		Model:       "opus",
		Effort:      "high",
		Prompt:      "Fix the login bug.\n\n## Notes\nKeep the API stable.",
		LastMessage: "Fixed and tested.",
		Work: map[string]RepoWork{
			"app": {
				RepoName: "app", WorktreePath: "/missions/dev--fix-login/app", Branch: "jane/fix-login",
				BaseRef: "refs/remotes/origin/main", BaseSHA: "1234567890abcdef", Created: true,
			},
			"failed": {RepoName: "failed", WorktreePath: "/missions/dev--fix-login/failed"},
		},
	}

	cases := []struct {
		name       string
		source     Mission
		review     Mission
		transcript string
		want       []string
		absent     []string
	}{
		{
			name:       "with transcript and focus",
			source:     source,
			review:     Mission{Prompt: "  Check the session handling.  "},
			transcript: "/missions/dev--review-fix-login/.q/source-transcript.jsonl",
			want: []string{
				"# Review of mission: Fix login",
				"(claude, opus, high effort)",
				"## Operation: Development",
				"Everything about dev.",
				"> Fix the login bug.\n>\n> ## Notes\n> Keep the API stable.",
				"- app: /missions/dev--fix-login/app (branch jane/fix-login, started from origin/main at 1234567)",
				"git -C /missions/dev--fix-login/app diff 1234567890abcdef",
				"git -C /missions/dev--fix-login/app log --stat 1234567890abcdef..HEAD",
				"> Fixed and tested.",
				"is at /missions/dev--review-fix-login/.q/source-transcript.jsonl",
				"Check the session handling.",
				"Write the report to .q/review.md",
				"Do not edit, commit, stash, reset, or push",
			},
			absent: []string{"dev--fix-login/failed", "No conversation log", DefaultReviewFocus},
		},
		{
			name:   "without transcript or focus",
			source: source,
			review: Mission{},
			want:   []string{"No conversation log from that agent is available", DefaultReviewFocus},
			absent: []string{"A copy of that agent's own conversation log"},
		},
		{
			name:   "source without repositories or a closing message",
			source: Mission{Name: "Plan", Tool: ToolCodex, Prompt: "Make a plan."},
			review: Mission{},
			want:   []string{"(codex)", "That mission had no repositories.", "> Make a plan."},
			absent: []string{"git -C", "closing message:"},
		},
	}

	operation := Operation{Name: "Development", Summary: " Everything about dev. "}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			prompt, err := ComposeReviewPrompt(operation, tc.source, tc.review, tc.transcript)
			if err != nil {
				t.Fatal(err)
			}

			for _, want := range tc.want {
				if !strings.Contains(prompt, want) {
					t.Errorf("prompt missing %q\n%s", want, prompt)
				}
			}

			for _, absent := range tc.absent {
				if strings.Contains(prompt, absent) {
					t.Errorf("prompt contains %q", absent)
				}
			}
		})
	}
}

func TestMissionReposForReview(t *testing.T) {
	operation := Operation{Repos: []Repo{{Name: "app", Path: "/dev/app"}}}

	cases := []struct {
		name string
		ms   Mission
		want int
	}{
		{name: "ordinary mission inherits the operation's", ms: Mission{}, want: 1},
		{name: "review has none", ms: Mission{ReviewOf: "ms_aabbccddeeff"}, want: 0},
		{
			name: "review ignores its own additions",
			ms:   Mission{ReviewOf: "ms_aabbccddeeff", ExtraRepos: []Repo{{Name: "docs", Path: "/dev/docs"}}},
			want: 0,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			repos, err := MissionRepos(operation, tc.ms)
			if err != nil {
				t.Fatal(err)
			}

			if len(repos) != tc.want {
				t.Errorf("repos = %d, want %d", len(repos), tc.want)
			}
		})
	}
}
