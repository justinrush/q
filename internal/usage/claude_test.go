package usage

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/justinrush/q/internal/mission"
)

// testPricing is a table with round numbers, so an expected dollar figure in a
// case can be read rather than computed.
var testPricing = Pricing{Models: map[string]ModelPrice{
	"model-a": {Input: 10, Output: 100},
}}

// assistant renders one assistant record with the usage claude writes.
func assistant(requestID, model string, in, out, cacheRead, write5m, write1h int64) string {
	return fmt.Sprintf(
		`{"type":"assistant","requestId":%q,"timestamp":"2026-09-05T14:00:00Z","message":`+
			`{"model":%q,"usage":{"input_tokens":%d,"output_tokens":%d,`+
			`"cache_read_input_tokens":%d,"cache_creation":`+
			`{"ephemeral_5m_input_tokens":%d,"ephemeral_1h_input_tokens":%d}}}}`,
		requestID, model, in, out, cacheRead, write5m, write1h,
	)
}

// writeTranscript writes a transcript and returns its path. A name containing a
// separator is written beneath the session's subagent directory.
func writeTranscript(t *testing.T, dir string, lines ...string) string {
	t.Helper()

	path := filepath.Join(dir, "session.jsonl")

	var body string
	for _, line := range lines {
		body += line + "\n"
	}

	if err := os.WriteFile(path, []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}

	return path
}

// writeSubagent writes one subagent transcript beside a session transcript.
func writeSubagent(t *testing.T, transcript, name string, lines ...string) {
	t.Helper()

	dir := filepath.Join(filepath.Dir(transcript), "session", "subagents")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	var body string
	for _, line := range lines {
		body += line + "\n"
	}

	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeMeterTokens(t *testing.T) {
	cases := []struct {
		name string
		// lines are the session transcript.
		lines []string
		// subagent, when set, is written as one subagent transcript.
		subagent     []string
		wantMeasured bool
		wantTokens   map[string]mission.ModelTokens
		wantUSD      float64
		wantUnpriced bool
	}{
		{
			name:         "one request",
			lines:        []string{assistant("r1", "model-a", 100, 200, 0, 0, 0)},
			wantMeasured: true,
			wantTokens:   map[string]mission.ModelTokens{"model-a": {Input: 100, Output: 200}},
			wantUSD:      100*10e-6 + 200*100e-6,
		},
		{
			name: "a repeated request id is counted once",
			lines: []string{
				assistant("r1", "model-a", 100, 200, 0, 0, 0),
				assistant("r1", "model-a", 100, 200, 0, 0, 0),
				assistant("r1", "model-a", 100, 200, 0, 0, 0),
			},
			wantMeasured: true,
			wantTokens:   map[string]mission.ModelTokens{"model-a": {Input: 100, Output: 200}},
			wantUSD:      100*10e-6 + 200*100e-6,
		},
		{
			name: "distinct requests are summed",
			lines: []string{
				assistant("r1", "model-a", 100, 0, 0, 0, 0),
				assistant("r2", "model-a", 50, 0, 0, 0, 0),
			},
			wantMeasured: true,
			wantTokens:   map[string]mission.ModelTokens{"model-a": {Input: 150}},
			wantUSD:      150 * 10e-6,
		},
		{
			name:         "subagent transcripts are summed in",
			lines:        []string{assistant("r1", "model-a", 100, 0, 0, 0, 0)},
			subagent:     []string{assistant("r2", "model-a", 900, 0, 0, 0, 0)},
			wantMeasured: true,
			wantTokens:   map[string]mission.ModelTokens{"model-a": {Input: 1000}},
			wantUSD:      1000 * 10e-6,
		},
		{
			name:         "the cache TTL split is preserved",
			lines:        []string{assistant("r1", "model-a", 0, 0, 700, 200, 100)},
			wantMeasured: true,
			wantTokens: map[string]mission.ModelTokens{
				"model-a": {CacheRead: 700, CacheWrite5m: 200, CacheWrite1h: 100},
			},
			wantUSD: 700*1e-6 + 200*12.5e-6 + 100*20e-6,
		},
		{
			name: "an undifferentiated cache total is charged at the cheaper rate",
			lines: []string{
				`{"type":"assistant","requestId":"r1","message":{"model":"model-a",` +
					`"usage":{"cache_creation_input_tokens":200}}}`,
			},
			wantMeasured: true,
			wantTokens:   map[string]mission.ModelTokens{"model-a": {CacheWrite5m: 200}},
			wantUSD:      200 * 12.5e-6,
		},
		{
			name: "a model with no published rate makes the total a floor",
			lines: []string{
				assistant("r1", "model-a", 100, 0, 0, 0, 0),
				assistant("r2", "model-z", 100, 0, 0, 0, 0),
			},
			wantMeasured: true,
			wantTokens: map[string]mission.ModelTokens{
				"model-a": {Input: 100},
				"model-z": {Input: 100},
			},
			wantUSD:      100 * 10e-6,
			wantUnpriced: true,
		},
		{
			name: "a synthetic zero-token record is not a model",
			lines: []string{
				assistant("r1", "model-a", 100, 0, 0, 0, 0),
				assistant("r2", "<synthetic>", 0, 0, 0, 0, 0),
			},
			wantMeasured: true,
			wantTokens:   map[string]mission.ModelTokens{"model-a": {Input: 100}},
			wantUSD:      100 * 10e-6,
		},
		{
			name: "a half-written trailing line does not lose the rest of the file",
			lines: []string{
				assistant("r1", "model-a", 100, 0, 0, 0, 0),
				`{"type":"assistant","requestId":"r2","mess`,
			},
			wantMeasured: true,
			wantTokens:   map[string]mission.ModelTokens{"model-a": {Input: 100}},
			wantUSD:      100 * 10e-6,
		},
		{
			name: "records that are not assistant turns are ignored",
			lines: []string{
				`{"type":"user","message":{"content":"hi"}}`,
				`{"type":"custom-title","customTitle":"q: something"}`,
				assistant("r1", "model-a", 100, 0, 0, 0, 0),
			},
			wantMeasured: true,
			wantTokens:   map[string]mission.ModelTokens{"model-a": {Input: 100}},
			wantUSD:      100 * 10e-6,
		},
		{
			name:         "an assistant record with no request id is skipped",
			lines:        []string{assistant("", "model-a", 100, 0, 0, 0, 0)},
			wantMeasured: false,
		},
		{
			name:         "an empty transcript measures nothing",
			wantMeasured: false,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			path := writeTranscript(t, dir, tc.lines...)

			if tc.subagent != nil {
				writeSubagent(t, path, "agent-1.jsonl", tc.subagent...)
			}

			got, measured, err := NewClaude(testPricing).Meter(
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

			if len(got.Usage.PerModel) != len(tc.wantTokens) {
				t.Fatalf("PerModel = %v, want %v", got.Usage.PerModel, tc.wantTokens)
			}

			for model, want := range tc.wantTokens {
				if got.Usage.PerModel[model] != want {
					t.Errorf("PerModel[%q] = %+v, want %+v", model, got.Usage.PerModel[model], want)
				}
			}

			if math.Abs(got.Usage.USD-tc.wantUSD) > 1e-9 {
				t.Errorf("USD = %v, want %v", got.Usage.USD, tc.wantUSD)
			}

			if got.Usage.Unpriced != tc.wantUnpriced {
				t.Errorf("Unpriced = %v, want %v", got.Usage.Unpriced, tc.wantUnpriced)
			}
		})
	}
}

func TestClaudeMeterQuota(t *testing.T) {
	quota := func(status, kind string, resetsAt int64) string {
		return fmt.Sprintf(
			`{"type":"assistant","requestId":"r9","timestamp":"2026-09-05T14:00:00Z",`+
				`"quotaLimits":{"status":%q,"rateLimitType":%q,"resetsAt":%d},`+
				`"message":{"model":"model-a","usage":{"input_tokens":1}}}`,
			status, kind, resetsAt,
		)
	}

	cases := []struct {
		name     string
		lines    []string
		wantKind string
		wantAt   int64
	}{
		{
			name:     "a refusal is recorded with the window it named",
			lines:    []string{quota("rejected", "five_hour", 1788000000)},
			wantKind: "five_hour",
			wantAt:   1788000000,
		},
		{
			name:  "an allowed request records no limit",
			lines: []string{quota("allowed", "five_hour", 1788000000)},
		},
		{
			name:  "a refusal with no reset time is not actionable",
			lines: []string{quota("rejected", "five_hour", 0)},
		},
		{
			name:  "an ordinary turn records no limit",
			lines: []string{assistant("r1", "model-a", 1, 0, 0, 0, 0)},
		},
		{
			name: "the newest refusal wins",
			lines: []string{
				quota("rejected", "five_hour", 1788000000),
				`{"type":"assistant","requestId":"r8","timestamp":"2026-09-05T15:00:00Z",` +
					`"quotaLimits":{"status":"rejected","rateLimitType":"weekly","resetsAt":1789000000},` +
					`"message":{"model":"model-a","usage":{"input_tokens":1}}}`,
			},
			wantKind: "weekly",
			wantAt:   1789000000,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTranscript(t, t.TempDir(), tc.lines...)

			got, _, err := NewClaude(testPricing).Meter(mission.Mission{TranscriptPath: path})
			if err != nil {
				t.Fatalf("Meter() error = %v", err)
			}

			if got.Limit.Kind != tc.wantKind {
				t.Fatalf("Limit.Kind = %q, want %q", got.Limit.Kind, tc.wantKind)
			}

			if tc.wantKind == "" {
				return
			}

			if want := time.Unix(tc.wantAt, 0); !got.Limit.ResetsAt.Equal(want) {
				t.Fatalf("Limit.ResetsAt = %v, want %v", got.Limit.ResetsAt, want)
			}

			if got.Limit.Tool != mission.ToolClaude {
				t.Fatalf("Limit.Tool = %q, want %q", got.Limit.Tool, mission.ToolClaude)
			}
		})
	}
}

func TestClaudeMeterMissingInput(t *testing.T) {
	cases := []struct {
		name       string
		transcript func(t *testing.T) string
	}{
		{
			name:       "no transcript path at all",
			transcript: func(*testing.T) string { return "" },
		},
		{
			name: "a path to a transcript that does not exist yet",
			transcript: func(t *testing.T) string {
				return filepath.Join(t.TempDir(), "not-written-yet.jsonl")
			},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			got, measured, err := NewClaude(testPricing).Meter(
				mission.Mission{TranscriptPath: tc.transcript(t)},
			)
			if err != nil {
				t.Fatalf("Meter() error = %v, want none", err)
			}

			if measured {
				t.Fatalf("Meter() measured = true, want false")
			}

			if !got.Usage.Empty() {
				t.Fatalf("Usage = %+v, want empty", got.Usage)
			}
		})
	}
}

func TestClaudeMeterRereadsAChangedTranscript(t *testing.T) {
	dir := t.TempDir()
	path := writeTranscript(t, dir, assistant("r1", "model-a", 100, 0, 0, 0, 0))
	meter := NewClaude(testPricing)
	ms := mission.Mission{TranscriptPath: path}

	first, _, err := meter.Meter(ms)
	if err != nil {
		t.Fatal(err)
	}

	if got := first.Usage.PerModel["model-a"].Input; got != 100 {
		t.Fatalf("first read input = %d, want 100", got)
	}

	// A cached read must not outlive the file it described. The modification
	// time has one-second granularity on some filesystems, so the size changing
	// is what this case relies on.
	writeTranscript(t, dir,
		assistant("r1", "model-a", 100, 0, 0, 0, 0),
		assistant("r2", "model-a", 50, 0, 0, 0, 0),
	)

	second, _, err := meter.Meter(ms)
	if err != nil {
		t.Fatal(err)
	}

	if got := second.Usage.PerModel["model-a"].Input; got != 150 {
		t.Fatalf("second read input = %d, want 150", got)
	}
}
