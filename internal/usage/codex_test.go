package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justinrush/q/internal/mission"
)

// tokenRecord renders one token_usage_record line.
func tokenRecord(thread, response string, in, cached, cacheWrite, out int64) string {
	return fmt.Sprintf(
		`{"timestamp":"2026-09-05T14:00:00Z","type":"token_usage_record","payload":`+
			`{"thread_id":%q,"response_id":%q,"usage":{"input_tokens":%d,`+
			`"cached_input_tokens":%d,"cache_write_input_tokens":%d,"output_tokens":%d,`+
			`"reasoning_output_tokens":0,"total_tokens":%d}}}`,
		thread, response, in, cached, cacheWrite, out, in+out,
	)
}

// turnContext renders one turn_context line naming the model.
func turnContext(model string) string {
	return fmt.Sprintf(
		`{"timestamp":"2026-09-05T14:00:00Z","type":"turn_context","payload":`+
			`{"collaboration_mode":{"settings":{"model":%q}}}}`,
		model,
	)
}

// tokenCount renders one token_count event carrying a rate-limit window.
func tokenCount(usedPercent float64, windowMinutes, resetsAt int64) string {
	return fmt.Sprintf(
		`{"timestamp":"2026-09-05T14:00:00Z","type":"event_msg","payload":`+
			`{"type":"token_count","rate_limits":{"primary":`+
			`{"used_percent":%v,"window_minutes":%d,"resets_at":%d}}}}`,
		usedPercent, windowMinutes, resetsAt,
	)
}

func writeRollout(t *testing.T, lines ...string) string {
	t.Helper()

	path := filepath.Join(t.TempDir(), "rollout.jsonl")

	var body string
	for _, line := range lines {
		body += line + "\n"
	}

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// codexPricing prices the model the fixtures name, so the dollar path is
// exercised even though q ships no rate for a real codex model.
var codexPricing = Pricing{Models: map[string]ModelPrice{"gpt-test": {Input: 10, Output: 100}}}

func TestCodexMeterTokens(t *testing.T) {
	cases := []struct {
		name         string
		lines        []string
		wantMeasured bool
		wantModel    string
		wantTokens   mission.ModelTokens
	}{
		{
			name: "one response, with cached input excluded from fresh input",
			lines: []string{
				turnContext("gpt-test"),
				tokenRecord("t1", "resp-1", 1000, 400, 50, 200),
			},
			wantMeasured: true,
			wantModel:    "gpt-test",
			wantTokens: mission.ModelTokens{
				Input: 600, CacheRead: 400, CacheWrite5m: 50, Output: 200,
			},
		},
		{
			name: "a response replayed by a rate-limit refresh is counted once",
			lines: []string{
				turnContext("gpt-test"),
				tokenRecord("t1", "resp-1", 100, 0, 0, 10),
				tokenRecord("t1", "resp-1", 100, 0, 0, 10),
			},
			wantMeasured: true,
			wantModel:    "gpt-test",
			wantTokens:   mission.ModelTokens{Input: 100, Output: 10},
		},
		{
			name: "distinct responses are summed",
			lines: []string{
				turnContext("gpt-test"),
				tokenRecord("t1", "resp-1", 100, 0, 0, 10),
				tokenRecord("t1", "resp-2", 50, 0, 0, 5),
			},
			wantMeasured: true,
			wantModel:    "gpt-test",
			wantTokens:   mission.ModelTokens{Input: 150, Output: 15},
		},
		{
			name: "a parent's history replayed into a subagent rollout is not this session's",
			lines: []string{
				turnContext("gpt-test"),
				tokenRecord("t1", "resp-1", 100, 0, 0, 10),
				tokenRecord("parent", "resp-p1", 900_000, 0, 0, 90_000),
				tokenRecord("parent", "resp-p2", 900_000, 0, 0, 90_000),
			},
			wantMeasured: true,
			wantModel:    "gpt-test",
			wantTokens:   mission.ModelTokens{Input: 100, Output: 10},
		},
		{
			name: "usage with no recorded model is attributed to a name no price can match",
			lines: []string{
				tokenRecord("t1", "resp-1", 100, 0, 0, 10),
			},
			wantMeasured: true,
			wantModel:    unknownCodexModel,
			wantTokens:   mission.ModelTokens{Input: 100, Output: 10},
		},
		{
			name: "the flatter model nesting older rollouts used is still read",
			lines: []string{
				`{"timestamp":"2026-09-05T14:00:00Z","type":"turn_context","payload":` +
					`{"collaboration_mode":{"model":"gpt-test"}}}`,
				tokenRecord("t1", "resp-1", 100, 0, 0, 10),
			},
			wantMeasured: true,
			wantModel:    "gpt-test",
			wantTokens:   mission.ModelTokens{Input: 100, Output: 10},
		},
		{
			name: "a cached count larger than the input it is part of does not go negative",
			lines: []string{
				turnContext("gpt-test"),
				tokenRecord("t1", "resp-1", 100, 400, 0, 10),
			},
			wantMeasured: true,
			wantModel:    "gpt-test",
			wantTokens:   mission.ModelTokens{Input: 0, CacheRead: 400, Output: 10},
		},
		{
			name: "records with no response id cannot be deduplicated and are skipped",
			lines: []string{
				turnContext("gpt-test"),
				tokenRecord("t1", "", 100, 0, 0, 10),
			},
		},
		{
			name: "a rollout with no usage at all",
			lines: []string{
				turnContext("gpt-test"),
				`{"timestamp":"2026-09-05T14:00:00Z","type":"response_item","payload":{"type":"message"}}`,
			},
		},
		{
			name: "a half-written trailing line does not lose the rest",
			lines: []string{
				turnContext("gpt-test"),
				tokenRecord("t1", "resp-1", 100, 0, 0, 10),
				`{"timestamp":"2026-09-05T14:00:00Z","type":"token_usage`,
			},
			wantMeasured: true,
			wantModel:    "gpt-test",
			wantTokens:   mission.ModelTokens{Input: 100, Output: 10},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRollout(t, tc.lines...)

			got, measured, err := NewCodex(codexPricing).Meter(
				mission.Mission{TranscriptPath: path},
			)
			if err != nil {
				t.Fatalf("Meter() error = %v", err)
			}

			if measured != tc.wantMeasured {
				t.Fatalf("Meter() measured = %v, want %v", measured, tc.wantMeasured)
			}

			if !measured {
				return
			}

			if got.Usage.PerModel[tc.wantModel] != tc.wantTokens {
				t.Fatalf(
					"PerModel[%q] = %+v, want %+v",
					tc.wantModel, got.Usage.PerModel[tc.wantModel], tc.wantTokens,
				)
			}
		})
	}
}

func TestCodexMeterRateLimits(t *testing.T) {
	fiveHour := int64(300)
	weekly := int64(10080)
	soon := time.Now().Add(time.Hour).Unix()
	later := time.Now().Add(48 * time.Hour).Unix()

	cases := []struct {
		name     string
		lines    []string
		wantKind string
		wantAt   int64
	}{
		{
			name:     "an exhausted window",
			lines:    []string{tokenCount(100, fiveHour, soon)},
			wantKind: "five_hour",
			wantAt:   soon,
		},
		{
			name:  "a window with room left is not a limit",
			lines: []string{tokenCount(82, fiveHour, soon)},
		},
		{
			name:  "a rollout with no rate limits at all",
			lines: []string{tokenRecord("t1", "resp-1", 1, 0, 0, 1)},
		},
		{
			name: "the window that reopens last is the one that gates the next mission",
			lines: []string{
				`{"timestamp":"2026-09-05T14:00:00Z","type":"event_msg","payload":` +
					fmt.Sprintf(
						`{"type":"token_count","rate_limits":{"primary":`+
							`{"used_percent":100,"window_minutes":%d,"resets_at":%d},"secondary":`+
							`{"used_percent":100,"window_minutes":%d,"resets_at":%d}}}}`,
						fiveHour, soon, weekly, later,
					),
			},
			wantKind: "weekly",
			wantAt:   later,
		},
		{
			name:     "a window this version of q has no name for is still reported",
			lines:    []string{tokenCount(100, 43200, soon)},
			wantKind: "",
			wantAt:   soon,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeRollout(t, tc.lines...)

			got, _, err := NewCodex(codexPricing).Meter(mission.Mission{TranscriptPath: path})
			if err != nil {
				t.Fatalf("Meter() error = %v", err)
			}

			if tc.wantAt == 0 {
				if !got.Limit.ResetsAt.IsZero() {
					t.Fatalf("Limit = %+v, want none", got.Limit)
				}

				return
			}

			if !got.Limit.ResetsAt.Equal(time.Unix(tc.wantAt, 0)) {
				t.Fatalf("ResetsAt = %v, want %v", got.Limit.ResetsAt, time.Unix(tc.wantAt, 0))
			}

			if got.Limit.Kind != tc.wantKind {
				t.Fatalf("Kind = %q, want %q", got.Limit.Kind, tc.wantKind)
			}

			if got.Limit.Tool != mission.ToolCodex {
				t.Fatalf("Tool = %q, want %q", got.Limit.Tool, mission.ToolCodex)
			}

			// Stamped from the record rather than the clock, so a cached scan
			// does not make the same refusal look newer on every re-read.
			want := time.Date(2026, 9, 5, 14, 0, 0, 0, time.UTC)
			if !got.Limit.ObservedAt.Equal(want) {
				t.Fatalf("ObservedAt = %v, want %v", got.Limit.ObservedAt, want)
			}
		})
	}
}

// codex runs ephemeral sessions that keep no rollout, and q does not go looking
// for one by guessing at the layout of codex's session directory.
func TestCodexMeterWithoutARollout(t *testing.T) {
	_, measured, err := NewCodex(codexPricing).Meter(mission.Mission{})
	if err != nil {
		t.Fatalf("Meter() error = %v", err)
	}

	if measured {
		t.Fatal("Meter() measured = true, want false")
	}
}
