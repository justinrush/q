package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

// catalog is a trimmed copy of what codex 0.153.4 answers model/list with.
func catalog() []Model {
	return []Model{
		{
			ID: "gpt-6-astra", Model: "gpt-6-astra",
			DisplayName: "GPT-6-Astra", Description: "Our most capable model.",
			IsDefault: true, DefaultReasoningEffort: "low",
			SupportedReasoningEfforts: []ReasoningEffortOption{
				{ReasoningEffort: "low"}, {ReasoningEffort: "medium"}, {ReasoningEffort: "high"},
			},
		},
		{
			ID: "gpt-reserve", Model: "gpt-reserve",
			DisplayName: "GPT-Reserve", Description: "Fast and affordable.",
			Hidden: true, DefaultReasoningEffort: "medium",
			SupportedReasoningEfforts: []ReasoningEffortOption{
				{ReasoningEffort: "medium"}, {ReasoningEffort: "high"},
			},
		},
	}
}

// probe runs a prober over a config.toml written into a temporary codex
// directory. An empty document writes no file, which is a machine that has never
// configured codex. A nil catalog stands in for a codex that cannot be reached.
func probe(t *testing.T, document, profile string, models []Model, listErr error) mission.ModelSet {
	t.Helper()

	dir := t.TempDir()

	if document != "" {
		if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
	}

	prober := NewProber(ProberOptions{ConfigDir: dir, Profile: profile})
	if models != nil || listErr != nil {
		prober.list = func(context.Context) ([]Model, error) { return models, listErr }
	}

	set, err := prober.Probe(context.Background())
	if err != nil {
		t.Fatalf("Probe: %v", err)
	}

	return set
}

// TestProberAsksCodex covers the path that matters: codex is reachable, so the
// catalog is its own rather than anything q guessed at.
func TestProberAsksCodex(t *testing.T) {
	set := probe(t, "", "q", catalog(), nil)

	if set.Default != "gpt-6-astra" {
		t.Errorf("Default = %q, want the model codex marks default", set.Default)
	}

	if set.DefaultEffort != "low" {
		t.Errorf("DefaultEffort = %q, want the default model's own", set.DefaultEffort)
	}

	if got := strings.Join(set.Efforts("gpt-6-astra"), ","); got != "low,medium,high" {
		t.Errorf("efforts = %q, want codex's own list", got)
	}

	// Hidden models are asked for on purpose, so a model named in the user's
	// configuration is still recognized rather than reported as unknown.
	if _, ok := set.Option("gpt-reserve"); !ok {
		t.Error("hidden models are missing, so a configured one would read as unknown")
	}

	if opt, _ := set.Option("gpt-6-astra"); opt.Label != "GPT-6-Astra" {
		t.Errorf("label = %q, want codex's display name", opt.Label)
	}

	if set.Err != "" {
		t.Errorf("Err = %q, want none", set.Err)
	}
}

func TestProberDefault(t *testing.T) {
	cases := []struct {
		name string
		// document is the config.toml, absent when empty.
		document string
		profile  string
		// models is codex's catalog; nil means codex could not be asked.
		models     []Model
		want       string
		wantEffort string
	}{
		{
			name:       "codex's own default when nothing is configured",
			profile:    "q",
			models:     catalog(),
			want:       "gpt-6-astra",
			wantEffort: "low",
		},
		{
			name:       "the top-level model outranks codex's default",
			document:   "model = \"gpt-reserve\"\n",
			profile:    "q",
			models:     catalog(),
			want:       "gpt-reserve",
			wantEffort: "medium",
		},
		{
			name: "the launched profile outranks the top level",
			document: "model = \"gpt-6-astra\"\n\n" +
				"[profiles.q]\nmodel = \"gpt-reserve\"\n",
			profile:    "q",
			models:     catalog(),
			want:       "gpt-reserve",
			wantEffort: "medium",
		},
		{
			name: "another profile's model is not this mission's default",
			document: "model = \"gpt-6-astra\"\n\n" +
				"[profiles.other]\nmodel = \"gpt-reserve\"\n",
			profile:    "q",
			models:     catalog(),
			want:       "gpt-6-astra",
			wantEffort: "low",
		},
		{
			name:       "a configured effort outranks the model's own",
			document:   "model = \"gpt-6-astra\"\nmodel_reasoning_effort = \"high\"\n",
			profile:    "q",
			models:     catalog(),
			want:       "gpt-6-astra",
			wantEffort: "high",
		},
		{
			name:       "an effort the model does not accept is dropped",
			document:   "model = \"gpt-6-astra\"\nmodel_reasoning_effort = \"ultra\"\n",
			profile:    "q",
			models:     catalog(),
			want:       "gpt-6-astra",
			wantEffort: "",
		},
		{
			name:       "keys in an unrelated table do not leak into the top level",
			document:   "[mcp_servers.example]\nmodel = \"not-a-model\"\n",
			profile:    "q",
			models:     catalog(),
			want:       "gpt-6-astra",
			wantEffort: "low",
		},
		{
			name:       "comments and stray whitespace are ignored",
			document:   "# the model to use\n\n   model = \"gpt-reserve\"   \n",
			profile:    "q",
			models:     catalog(),
			want:       "gpt-reserve",
			wantEffort: "medium",
		},
		{
			name:       "a key with no quoted value is not read",
			document:   "model = gpt-reserve\n",
			profile:    "q",
			models:     catalog(),
			want:       "gpt-6-astra",
			wantEffort: "low",
		},
		{
			name:       "a quoted profile name still matches",
			document:   "[profiles.\"q\"]\nmodel = \"gpt-reserve\"\n",
			profile:    "q",
			models:     catalog(),
			want:       "gpt-reserve",
			wantEffort: "medium",
		},
		{
			name:     "an unreachable codex still yields the configured model",
			document: "model = \"gpt-reserve\"\n",
			profile:  "q",
			want:     "gpt-reserve",
		},
		{
			name:    "an unreachable codex with no configuration establishes nothing",
			profile: "q",
			want:    "",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			set := probe(t, tc.document, tc.profile, tc.models, nil)

			if set.Default != tc.want {
				t.Errorf("Default = %q, want %q", set.Default, tc.want)
			}

			if set.DefaultEffort != tc.wantEffort {
				t.Errorf("DefaultEffort = %q, want %q", set.DefaultEffort, tc.wantEffort)
			}

			if set.ProbedAt.IsZero() {
				t.Error("ProbedAt is zero, so the board would report a set that was never probed")
			}
		})
	}
}

// TestProberFallback covers the degraded answer: codex cannot be reached, so the
// list comes from configuration and carries no effort levels, because which
// efforts a model accepts is knowable only from codex.
func TestProberFallback(t *testing.T) {
	cases := []struct {
		name     string
		document string
		// configured is agents.codex.models.
		configured []string
		wantValues []string
	}{
		{
			name: "models the configuration names",
			document: "model = \"my-tuned\"\n\n" +
				"[profiles.cheap]\nmodel = \"something-small\"\n",
			wantValues: []string{"my-tuned", "something-small"},
		},
		{
			name:       "the configured list is offered too",
			configured: []string{"only-this"},
			wantValues: []string{"only-this"},
		},
		{
			name:       "both, without duplicates",
			document:   "model = \"shared\"\n",
			configured: []string{"shared", "other"},
			wantValues: []string{"shared", "other"},
		},
		{
			name:       "nothing to go on means nothing offered",
			wantValues: nil,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()

			if tc.document != "" {
				if err := os.WriteFile(filepath.Join(dir, ConfigFile), []byte(tc.document), 0o600); err != nil {
					t.Fatal(err)
				}
			}

			prober := NewProber(ProberOptions{ConfigDir: dir, Profile: "q", Models: tc.configured})

			set, err := prober.Probe(context.Background())
			if err != nil {
				t.Fatalf("Probe: %v", err)
			}

			var got []string
			for _, opt := range set.Options {
				got = append(got, opt.Value)

				if len(opt.Efforts) > 0 {
					t.Errorf("%q carries efforts, which are unknowable without codex", opt.Value)
				}
			}

			if strings.Join(got, ",") != strings.Join(tc.wantValues, ",") {
				t.Errorf("options = %v, want %v", got, tc.wantValues)
			}

			// The board needs to distinguish "codex offers nothing" from "codex was
			// not reachable", so the reason is recorded either way.
			if set.Err == "" {
				t.Error("Err is empty, so nothing would say the list is partial")
			}
		})
	}
}

// TestProberReportsListFailure covers a codex that is present but refuses.
func TestProberReportsListFailure(t *testing.T) {
	set := probe(t, "model = \"configured\"\n", "q", nil, errors.New("app-server refused"))

	if !strings.Contains(set.Err, "app-server refused") {
		t.Errorf("Err = %q, want the refusal", set.Err)
	}

	if set.Default != "configured" {
		t.Errorf("Default = %q, want the configured model despite the failure", set.Default)
	}
}

// TestProberWithoutBinNeverRunsCodex pins that a prober with no binary asks
// nothing, rather than trying to start a process that is not there.
func TestProberWithoutBinNeverRunsCodex(t *testing.T) {
	if NewProber(ProberOptions{ConfigDir: t.TempDir()}).list != nil {
		t.Error("a prober with no binary has a lister, so it would try to run codex")
	}

	if NewProber(ProberOptions{Bin: "/bin/codex", ConfigDir: t.TempDir()}).list == nil {
		t.Error("a prober with a binary has no lister, so it could never ask codex")
	}
}

func TestProberTool(t *testing.T) {
	if got := NewProber(ProberOptions{}).Tool(); got != mission.ToolCodex {
		t.Errorf("Tool() = %q, want %q", got, mission.ToolCodex)
	}
}
