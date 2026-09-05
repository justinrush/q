package claude_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/claude"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// probeArgv is the argv the prober is expected to run, in [runner.Spec.String]
// form. A change to it should fail these tests loudly: it is a protocol
// invocation, not an implementation detail.
const probeArgv = "/bin/claude --print --verbose --no-session-persistence " +
	"--input-format stream-json --output-format stream-json"

// frames builds a response stream with the system chatter claude emits ahead of
// the answer, which the parser has to skip.
//
// The model list is compacted first: the fixtures below are written across
// several lines to stay readable, but claude emits one frame per line and a
// parser that coped with a frame split across lines would be testing something
// the real stream never does.
func frames(models string) string {
	return strings.Join([]string{
		`{"type":"system","subtype":"hook_started","hook_name":"SessionStart:startup"}`,
		`{"type":"system","subtype":"hook_response"}`,
		`{"type":"control_response","response":{"subtype":"success","request_id":"q-models",` +
			`"response":{"models":` + compact(models) + `}}}`,
		"",
	}, "\n")
}

// compact renders a JSON fixture on one line.
func compact(doc string) string {
	var buf bytes.Buffer

	if err := json.Compact(&buf, []byte(doc)); err != nil {
		panic("test fixture is not JSON: " + err.Error())
	}

	return buf.String()
}

// twoModels is a trimmed copy of what claude 2.1.261 actually answers.
const twoModels = `[
	{"value":"default","resolvedModel":"claude-sonnet-5","displayName":"Default (recommended)",
	 "description":"Sonnet 5","supportsEffort":true,
	 "supportedEffortLevels":["low","medium","high","xhigh","max"]},
	{"value":"sonnet","resolvedModel":"claude-sonnet-5","displayName":"Sonnet",
	 "description":"Sonnet 5 · Efficient","supportsEffort":true,
	 "supportedEffortLevels":["low","medium","high"]},
	{"value":"haiku","resolvedModel":"claude-haiku-4-5","displayName":"Haiku",
	 "description":"Haiku 4.5 · Fastest"}
]`

func TestProberProbe(t *testing.T) {
	cases := []struct {
		name string
		// stdout is what the fake claude prints.
		stdout string
		// exitErr makes the invocation fail rather than answer.
		exitErr bool
		wantErr string
		// wantValues are the model values, in order.
		wantValues []string
		// wantEfforts is the effort list for the model named in wantEffortsFor.
		wantEffortsFor string
		wantEfforts    []string
	}{
		{
			name:           "reads the model list past the system frames",
			stdout:         frames(twoModels),
			wantValues:     []string{"default", "sonnet", "haiku"},
			wantEffortsFor: "sonnet",
			wantEfforts:    []string{"low", "medium", "high"},
		},
		{
			name:           "a model that declines effort reports none",
			stdout:         frames(twoModels),
			wantValues:     []string{"default", "sonnet", "haiku"},
			wantEffortsFor: "haiku",
			wantEfforts:    nil,
		},
		{
			name: "effort levels are ignored when the model does not support them",
			stdout: frames(`[{"value":"sonnet","displayName":"Sonnet","supportsEffort":false,
				"supportedEffortLevels":["low","high"]}]`),
			wantValues:     []string{"sonnet"},
			wantEffortsFor: "sonnet",
			wantEfforts:    nil,
		},
		{
			name:    "a response for another request is not the answer",
			stdout:  strings.ReplaceAll(frames(twoModels), "q-models", "someone-else"),
			wantErr: "did not answer",
		},
		{
			name:    "a truncated stream is not an answer",
			stdout:  `{"type":"control_response","response":{"subtype":"suc`,
			wantErr: "did not answer",
		},
		{
			name:    "no output at all is not an answer",
			stdout:  "",
			wantErr: "did not answer",
		},
		{
			name: "a refusal is reported with its reason",
			stdout: `{"type":"control_response","response":{"subtype":"error",` +
				`"request_id":"q-models","error":"not supported"}}`,
			wantErr: "not supported",
		},
		{
			name:    "an empty model list is a failure rather than an empty board",
			stdout:  frames(`[]`),
			wantErr: "no models",
		},
		{
			name:    "a claude that will not run is reported",
			exitErr: true,
			wantErr: "asking claude for its models",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := runner.NewFake()

			if tc.exitErr {
				fake.ExpectExit(probeArgv, 1, "not logged in")
			} else {
				fake.Expect(probeArgv, tc.stdout)
			}

			prober := claude.NewProber("/bin/claude", fake, claude.ProberOptions{
				Home:      t.TempDir(),
				Managed:   filepath.Join(t.TempDir(), "absent.json"),
				LookupEnv: func(string) (string, bool) { return "", false },
			})

			set, err := prober.Probe(context.Background())

			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error = %v, want it to contain %q", err, tc.wantErr)
				}

				return
			}

			if err != nil {
				t.Fatalf("Probe: %v", err)
			}

			var got []string
			for _, opt := range set.Options {
				got = append(got, opt.Value)
			}

			if strings.Join(got, ",") != strings.Join(tc.wantValues, ",") {
				t.Errorf("values = %v, want %v", got, tc.wantValues)
			}

			efforts := set.Efforts(tc.wantEffortsFor)
			if strings.Join(efforts, ",") != strings.Join(tc.wantEfforts, ",") {
				t.Errorf("efforts(%s) = %v, want %v", tc.wantEffortsFor, efforts, tc.wantEfforts)
			}

			if set.ProbedAt.IsZero() {
				t.Error("ProbedAt is zero, so the board would report a set that was never probed")
			}
		})
	}
}

// TestProberDefault covers the precedence q mirrors from claude: the user's own
// configuration outranks the account default, because a machine set to opus
// should brief opus missions even when the account recommends sonnet.
func TestProberDefault(t *testing.T) {
	cases := []struct {
		name string
		// env is ANTHROPIC_MODEL, unset when empty.
		env string
		// managed and user are the settings documents, absent when empty.
		managed string
		user    string
		// models is the probe's own answer.
		models string
		want   string
	}{
		{
			name:   "the account default resolves to its selectable alias",
			models: twoModels,
			want:   "sonnet",
		},
		{
			name:   "the user's settings outrank the account default",
			user:   `{"model":"opus"}`,
			models: twoModels,
			want:   "opus",
		},
		{
			name:    "managed settings outrank the user's",
			managed: `{"model":"haiku"}`,
			user:    `{"model":"opus"}`,
			models:  twoModels,
			want:    "haiku",
		},
		{
			name:    "the environment outranks everything, as it does for claude",
			env:     "sonnet",
			managed: `{"model":"haiku"}`,
			user:    `{"model":"opus"}`,
			models:  twoModels,
			want:    "sonnet",
		},
		{
			name:   "a settings file with no model expresses no preference",
			user:   `{"hooks":{}}`,
			models: twoModels,
			want:   "sonnet",
		},
		{
			name:   "an unparseable settings file is not a preference either",
			user:   `{not json`,
			models: twoModels,
			want:   "sonnet",
		},
		{
			name: "an unaliased default falls back to the literal value claude accepts",
			models: `[{"value":"default","resolvedModel":"claude-opus-5-secret",
				"displayName":"Default"}]`,
			want: "default",
		},
		{
			name:   "a list with no default entry establishes none",
			models: `[{"value":"sonnet","displayName":"Sonnet"}]`,
			want:   "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			home := t.TempDir()
			managed := filepath.Join(t.TempDir(), "managed-settings.json")

			if tc.user != "" {
				dir := filepath.Join(home, ".claude")
				if err := os.MkdirAll(dir, 0o755); err != nil {
					t.Fatal(err)
				}

				if err := os.WriteFile(filepath.Join(dir, "settings.json"), []byte(tc.user), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			if tc.managed != "" {
				if err := os.WriteFile(managed, []byte(tc.managed), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			fake := runner.NewFake().Expect(probeArgv, frames(tc.models))

			prober := claude.NewProber("/bin/claude", fake, claude.ProberOptions{
				Home:    home,
				Managed: managed,
				LookupEnv: func(key string) (string, bool) {
					if key == claude.EnvModel && tc.env != "" {
						return tc.env, true
					}

					return "", false
				},
			})

			set, err := prober.Probe(context.Background())
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}

			if set.Default != tc.want {
				t.Errorf("Default = %q, want %q", set.Default, tc.want)
			}
		})
	}
}

// TestProberRequest pins the bytes written to claude's standard input. The
// probe is a protocol exchange, so the request shape is part of the contract.
func TestProberRequest(t *testing.T) {
	fake := runner.NewFake().Expect(probeArgv, frames(twoModels))

	prober := claude.NewProber("/bin/claude", fake, claude.ProberOptions{
		Home:      t.TempDir(),
		Managed:   filepath.Join(t.TempDir(), "absent.json"),
		LookupEnv: func(string) (string, bool) { return "", false },
	})

	if _, err := prober.Probe(context.Background()); err != nil {
		t.Fatalf("Probe: %v", err)
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("ran %d commands, want exactly one", len(calls))
	}

	var got struct {
		Type      string `json:"type"`
		RequestID string `json:"request_id"`
		Request   struct {
			Subtype string `json:"subtype"`
		} `json:"request"`
	}

	if err := json.Unmarshal(calls[0].Stdin, &got); err != nil {
		t.Fatalf("stdin is not one JSON frame: %v (%q)", err, calls[0].Stdin)
	}

	if got.Type != "control_request" || got.Request.Subtype != "initialize" {
		t.Errorf("request = %+v, want a control_request/initialize", got)
	}

	if !strings.HasSuffix(string(calls[0].Stdin), "\n") {
		t.Error("the request is not newline-terminated, so claude would wait for the rest of it")
	}
}

// TestProberTool is the identity the daemon keys its catalog on.
func TestProberTool(t *testing.T) {
	if got := claude.NewProber("/bin/claude", runner.NewFake(), claude.ProberOptions{}).Tool(); got != mission.ToolClaude {
		t.Errorf("Tool() = %q, want %q", got, mission.ToolClaude)
	}
}

// errBoom stands in for a runner that cannot start the process at all, as
// opposed to one whose process exits non-zero.
var errBoom = errors.New("boom")

func TestProberLaunchFailure(t *testing.T) {
	fake := runner.NewFake().ExpectError(probeArgv, errBoom)

	prober := claude.NewProber("/bin/claude", fake, claude.ProberOptions{
		Home:      t.TempDir(),
		Managed:   filepath.Join(t.TempDir(), "absent.json"),
		LookupEnv: func(string) (string, bool) { return "", false },
	})

	if _, err := prober.Probe(context.Background()); !errors.Is(err, errBoom) {
		t.Errorf("error = %v, want it to wrap errBoom", err)
	}
}
