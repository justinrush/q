package main

import (
	"bytes"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

func TestResolveReviewer(t *testing.T) {
	configured := reviewSettings{Tool: "codex", Model: "gpt-5.4", Effort: "high"}

	cases := []struct {
		name     string
		defaults reviewSettings
		tool     mission.Tool
		model    string
		effort   string
		want     reviewer
		wantErr  string
	}{
		{
			name:     "configured reviewer, model, and effort",
			defaults: configured,
			want:     reviewer{tool: mission.ToolCodex, model: "gpt-5.4", effort: "high"},
		},
		{
			name:     "a flagged effort beats the configured one",
			defaults: configured, effort: "low",
			want: reviewer{tool: mission.ToolCodex, model: "gpt-5.4", effort: "low"},
		},
		{
			name:     "a flagged model does not take the configured effort",
			defaults: configured, model: "gpt-5.4-mini",
			want: reviewer{tool: mission.ToolCodex, model: "gpt-5.4-mini"},
		},
		{
			name:     "a different flagged agent takes nothing from the configured one",
			defaults: configured, tool: mission.ToolClaude,
			want: reviewer{tool: mission.ToolClaude},
		},
		{
			name: "flags alone",
			tool: mission.ToolClaude, model: "opus", effort: "max",
			want: reviewer{tool: mission.ToolClaude, model: "opus", effort: "max"},
		},
		{name: "nobody named", wantErr: "--tool"},
		{name: "an unknown configured agent", defaults: reviewSettings{Tool: "gpt"}, wantErr: "review.tool"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, err := resolveReviewer(tc.defaults, tc.tool, tc.model, tc.effort)

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want one mentioning %q", err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatal(err)
			}

			if got != tc.want {
				t.Errorf("reviewer = %+v, want %+v", got, tc.want)
			}
		})
	}
}

func TestReviewConfig(t *testing.T) {
	cases := []struct {
		name string
		file fileConfig
		want reviewSettings
	}{
		{name: "unset", file: fileConfig{}, want: reviewSettings{}},
		{
			name: "set",
			file: fileConfig{Review: &reviewConfig{Tool: "codex", Model: "gpt-5.4", Effort: "high"}},
			want: reviewSettings{Tool: "codex", Model: "gpt-5.4", Effort: "high"},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			s := defaultSettings()
			applyFile(&s, tc.file)

			if s.Review != tc.want {
				t.Errorf("Review = %+v, want %+v", s.Review, tc.want)
			}

			var buf bytes.Buffer
			if err := writeSampleConfig(&buf, s); err != nil {
				t.Fatal(err)
			}

			if !strings.Contains(buf.String(), `"review"`) {
				t.Errorf("sample config has no review section:\n%s", buf.String())
			}
		})
	}
}
