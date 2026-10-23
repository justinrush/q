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

// permissionMode renders the record claude writes when a session starts or
// its permission mode changes.
func permissionMode(mode string) string {
	return fmt.Sprintf(`{"type":"permission-mode","permissionMode":%q}`, mode)
}

// spawning renders an assistant record that starts a subagent with the given
// tool call.
func spawning(requestID, toolUseID string, out int64) string {
	return fmt.Sprintf(
		`{"type":"assistant","requestId":%q,"message":{"model":"model-a",`+
			`"content":[{"type":"tool_use","id":%q,"name":"Agent"}],"usage":{"output_tokens":%d}}}`,
		requestID, toolUseID, out,
	)
}

// writeSubagentMeta records which tool call started a subagent.
func writeSubagentMeta(t *testing.T, transcript, name, toolUseID string) {
	t.Helper()

	path := filepath.Join(filepath.Dir(transcript), "session", "subagents", name+".meta.json")
	if err := os.WriteFile(path, []byte(fmt.Sprintf(`{"agentType":"Explore","toolUseId":%q}`, toolUseID)), 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestClaudeMeterSplitsCostByPermissionMode(t *testing.T) {
	path := writeTranscript(t, t.TempDir(),
		permissionMode("plan"),
		`{"type":"user","permissionMode":"plan","message":{"content":"plan the work"}}`,
		assistant("r1", "model-a", 1000, 0, 0, 0, 0),
		spawning("r2", "toolu_plan", 1000),
		permissionMode("default"),
		// A streaming update to a plan request, written after the mode
		// changed, is still the plan request.
		assistant("r1", "model-a", 1000, 0, 0, 0, 0),
		assistant("r3", "model-a", 0, 10_000, 0, 0, 0),
		spawning("r4", "toolu_build", 0),
	)
	// One subagent started while planning, one while implementing, and one
	// started by the planning subagent.
	writeSubagent(t, path, "agent-p.jsonl", spawning("s1", "toolu_nested", 2000))
	writeSubagentMeta(t, path, "agent-p", "toolu_plan")
	writeSubagent(t, path, "agent-b.jsonl", assistant("s2", "model-a", 0, 3000, 0, 0, 0))
	writeSubagentMeta(t, path, "agent-b", "toolu_build")
	writeSubagent(t, path, "agent-n.jsonl", assistant("s3", "model-a", 0, 4000, 0, 0, 0))
	writeSubagentMeta(t, path, "agent-n", "toolu_nested")

	meter := NewClaude(testPricing)
	for range 2 { // Also exercise the cached scan.
		got, measured, err := meter.Meter(mission.Mission{TranscriptPath: path})
		if err != nil || !measured {
			t.Fatalf("Meter: %v, %v", measured, err)
		}

		plan, build := got.Usage.Phases[mission.PhasePlan], got.Usage.Phases[mission.PhaseImplement]
		// Plan: $0.01 + $0.10 + $0.20 + $0.40; implement: $1 + $0.30.
		if math.Abs(plan.USD-0.71) > 1e-9 || math.Abs(build.USD-1.3) > 1e-9 || math.Abs(got.Usage.USD-2.01) > 1e-9 {
			t.Fatalf("phases = %+v, total %v", got.Usage.Phases, got.Usage.USD)
		}
	}
}

func TestClaudeMeterWithholdsAnUnplacedSplit(t *testing.T) {
	cases := []struct {
		name     string
		lines    []string
		subagent bool
	}{
		{
			name:  "a transcript that never recorded a mode",
			lines: []string{assistant("r1", "model-a", 1000, 0, 0, 0, 0)},
		},
		{
			name:  "a request made before the first recorded mode",
			lines: []string{assistant("r1", "model-a", 1000, 0, 0, 0, 0), permissionMode("plan"), assistant("r2", "model-a", 1000, 0, 0, 0, 0)},
		},
		{
			name:     "a subagent with no record of what started it",
			lines:    []string{permissionMode("plan"), assistant("r1", "model-a", 1000, 0, 0, 0, 0)},
			subagent: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			path := writeTranscript(t, t.TempDir(), tc.lines...)
			if tc.subagent {
				writeSubagent(t, path, "agent-x.jsonl", assistant("s1", "model-a", 1000, 0, 0, 0, 0))
			}

			got, measured, err := NewClaude(testPricing).Meter(mission.Mission{TranscriptPath: path})
			if err != nil || !measured || got.Usage.USD == 0 {
				t.Fatalf("Meter: %+v, %v, %v", got.Usage, measured, err)
			}

			if got.Usage.Phases != nil {
				t.Errorf("phases = %+v, want none", got.Usage.Phases)
			}
		})
	}
}

// prompt renders a prompt a human typed in the given mode.
func prompt(mode, text string) string {
	return fmt.Sprintf(`{"type":"user","permissionMode":%q,"origin":{"kind":"human"},"message":{"content":%q}}`, mode, text)
}

func TestClaudeMeterSplitsIterationFromTheInitialImplementation(t *testing.T) {
	path := writeTranscript(t, t.TempDir(),
		permissionMode("plan"),
		prompt("plan", "plan the work"),
		assistant("r1", "model-a", 1000, 0, 0, 0, 0),
		// A second prompt while planning is still planning.
		prompt("plan", "also cover the tests"),
		assistant("r2", "model-a", 1000, 0, 0, 0, 0),
		permissionMode("default"),
		assistant("r3", "model-a", 0, 10_000, 0, 0, 0),
		// None of these start a turn.
		`{"type":"user","message":{"content":[{"type":"tool_result","tool_use_id":"t1","content":"ok"}]}}`,
		`{"type":"user","message":{"content":[{"type":"text","text":"[Request interrupted by user]"}]}}`,
		`{"type":"user","isMeta":true,"message":{"content":"<local-command-caveat>"}}`,
		`{"type":"user","origin":{"kind":"task-notification"},"message":{"content":"<task-notification>"}}`,
		assistant("r4", "model-a", 0, 1000, 0, 0, 0),
		prompt("default", "the test fails"),
		assistant("r5", "model-a", 0, 2000, 0, 0, 0),
		spawning("r6", "toolu_fix", 0),
		// Planning again after implementing is still iterating, as is a
		// switch to another model.
		permissionMode("plan"),
		prompt("plan", "plan the fix"),
		assistant("r7", "model-b", 0, 3000, 0, 0, 0),
	)
	writeSubagent(t, path, "agent-f.jsonl", assistant("s1", "model-a", 0, 4000, 0, 0, 0))
	writeSubagentMeta(t, path, "agent-f", "toolu_fix")

	pricing := Pricing{Models: map[string]ModelPrice{"model-a": {Input: 10, Output: 100}, "model-b": {Input: 1, Output: 10}}}
	got, measured, err := NewClaude(pricing).Meter(mission.Mission{TranscriptPath: path})
	if err != nil || !measured {
		t.Fatalf("Meter: %v, %v", measured, err)
	}

	phases := got.Usage.Phases
	// Plan: $0.02; implement: $1 + $0.10; iterate: $0.20 + $0.40 + $0.03.
	for phase, want := range map[mission.Phase]float64{mission.PhasePlan: 0.02, mission.PhaseImplement: 1.1, mission.PhaseIterate: 0.63} {
		if math.Abs(phases[phase].USD-want) > 1e-9 {
			t.Errorf("%s = %v, want %v (phases %+v)", phase, phases[phase].USD, want, phases)
		}
	}

	iterate := map[string]float64{}
	for _, slice := range got.Usage.Breakdown {
		if slice.Phase == mission.PhaseIterate {
			iterate[slice.Model] += slice.USD
		}
	}

	if math.Abs(iterate["model-a"]-0.6) > 1e-9 || math.Abs(iterate["model-b"]-0.03) > 1e-9 {
		t.Errorf("iterate by model = %v", iterate)
	}
}

func TestClaudeMeterIteratesAfterAnUnplannedBrief(t *testing.T) {
	path := writeTranscript(t, t.TempDir(),
		permissionMode("default"),
		// A transcript written before claude recorded origins.
		`{"type":"user","permissionMode":"default","message":{"content":"do the work"}}`,
		assistant("r1", "model-a", 0, 1000, 0, 0, 0),
		`{"type":"user","message":{"content":"<command-name>/model</command-name>"}}`,
		assistant("r2", "model-a", 0, 1000, 0, 0, 0),
		`{"type":"user","permissionMode":"default","message":{"content":[{"type":"text","text":"now fix it"}]}}`,
		assistant("r3", "model-a", 0, 2000, 0, 0, 0),
	)

	got, measured, err := NewClaude(testPricing).Meter(mission.Mission{TranscriptPath: path})
	if err != nil || !measured {
		t.Fatalf("Meter: %v, %v", measured, err)
	}

	if _, ok := got.Usage.Phases[mission.PhasePlan]; ok {
		t.Errorf("phases = %+v, want no plan", got.Usage.Phases)
	}

	if build, iterate := got.Usage.Phases[mission.PhaseImplement], got.Usage.Phases[mission.PhaseIterate]; math.Abs(build.USD-0.2) > 1e-9 || math.Abs(iterate.USD-0.2) > 1e-9 {
		t.Errorf("phases = %+v", got.Usage.Phases)
	}
}
