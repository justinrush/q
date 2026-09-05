// Asking claude which models it offers.
//
// claude has no "list models" subcommand, but its stream-json control protocol
// answers an initialize request with the full set, and that answer is the
// account's real entitlements rather than a generic list. Reading it is the only
// way to learn what a model is called, what it costs, and which effort levels it
// takes without hardcoding a table that goes stale with every release.

package claude

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// probeRequestID identifies q's control request in the response stream.
const probeRequestID = "q-models"

// probeTimeout bounds the probe. The measured cost is under a second; anything
// far beyond that means claude is waiting on something it will not get from a
// closed stdin, and a hung probe must not hold up the daemon's reconcile loop.
const probeTimeout = 30 * time.Second

// EnvModel is claude's own environment override for the session model. It is
// read here because claude honors it, so ignoring it would make q report a
// default that claude would not actually use.
const EnvModel = "ANTHROPIC_MODEL"

// SettingsKeyModel is the settings key holding the user's preferred model.
const SettingsKeyModel = "model"

// Prober asks claude which models it offers.
type Prober struct {
	bin  string
	run  runner.Runner
	home string
	// managed is the enterprise managed-settings path, overridable for tests.
	managed string
	// lookupEnv is os.LookupEnv, overridable for tests.
	lookupEnv func(string) (string, bool)
}

// ProberOptions configure a [Prober]. The zero value probes the real machine.
type ProberOptions struct {
	// Home overrides the home directory the user settings file is read from.
	Home string
	// Managed overrides the enterprise managed-settings path.
	Managed string
	// LookupEnv overrides environment lookup.
	LookupEnv func(string) (string, bool)
}

// NewProber returns a prober invoking the claude binary at bin.
func NewProber(bin string, run runner.Runner, opts ProberOptions) *Prober {
	lookup := opts.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}

	managed := opts.Managed
	if managed == "" {
		managed = defaultManagedSettings()
	}

	return &Prober{bin: bin, run: run, home: opts.Home, managed: managed, lookupEnv: lookup}
}

// Tool reports that this prober asks claude.
func (*Prober) Tool() mission.Tool { return mission.ToolClaude }

// probeArgs is the invocation that asks claude what it offers.
//
// Three flags are load-bearing rather than incidental:
//
//   - --no-session-persistence keeps the probe out of claude's session directory.
//     [Registry] reads that same directory to heal cards, and a probe session
//     appearing there would be a session q cannot account for.
//   - --print with the stream-json formats is what exposes the control protocol
//     at all; --verbose is required by claude alongside them.
//   - --bare is deliberately absent. It would make the probe faster and skip the
//     user's hooks, but it also stops claude reading OAuth credentials, and an
//     unauthenticated probe answers with a generic model list rather than the
//     models this account may actually use.
func probeArgs() []string {
	return []string{
		"--print",
		"--verbose",
		"--no-session-persistence",
		"--input-format", "stream-json",
		"--output-format", "stream-json",
	}
}

// probeRequest is the single control request written to claude's standard input.
// Closing the stream after it is what makes claude answer and exit rather than
// wait for a turn.
func probeRequest() []byte {
	req := controlRequest{Type: "control_request", RequestID: probeRequestID}
	req.Request.Subtype = "initialize"

	data, err := json.Marshal(req)
	if err != nil {
		// The value is a fixed struct of string literals; marshaling it cannot fail.
		panic("claude: encoding the probe request: " + err.Error())
	}

	return append(data, '\n')
}

// controlRequest is the stream-json control frame q writes.
type controlRequest struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id"`
	Request   struct {
		Subtype string `json:"subtype"`
	} `json:"request"`
}

// controlResponse is the frame claude answers with. Everything before it in the
// stream is hook and system chatter that must be skipped rather than parsed.
type controlResponse struct {
	Type     string `json:"type"`
	Response struct {
		Subtype   string `json:"subtype"`
		RequestID string `json:"request_id"`
		Error     string `json:"error"`
		Response  struct {
			Models []probedModel `json:"models"`
		} `json:"response"`
	} `json:"response"`
}

// probedModel is one entry of the initialize response's model list.
type probedModel struct {
	// Value is what --model takes.
	Value string `json:"value"`
	// ResolvedModel is the concrete model Value names, which for the "default"
	// entry is how the account's real default becomes knowable.
	ResolvedModel         string   `json:"resolvedModel"`
	DisplayName           string   `json:"displayName"`
	Description           string   `json:"description"`
	SupportsEffort        bool     `json:"supportsEffort"`
	SupportedEffortLevels []string `json:"supportedEffortLevels"`
}

// DefaultValue is the model value claude treats as "whatever is recommended".
const DefaultValue = "default"

// Probe runs claude and reports the models it offers.
func (p *Prober) Probe(ctx context.Context) (mission.ModelSet, error) {
	ctx, cancel := context.WithTimeout(ctx, probeTimeout)
	defer cancel()

	res, err := p.run.Run(ctx, runner.Spec{
		Name:  p.bin,
		Args:  probeArgs(),
		Stdin: probeRequest(),
	})
	if err != nil {
		return mission.ModelSet{}, fmt.Errorf("asking claude for its models: %w", err)
	}

	models, err := parseProbe(res.Stdout)
	if err != nil {
		return mission.ModelSet{}, err
	}

	set := mission.ModelSet{Options: toOptions(models), ProbedAt: time.Now()}
	set.Default = p.resolveDefault(models)

	return set, nil
}

// parseProbe pulls the model list out of claude's response stream.
func parseProbe(stdout []byte) ([]probedModel, error) {
	for _, line := range strings.Split(string(stdout), "\n") {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}

		var frame controlResponse

		// A frame q does not recognize is skipped rather than treated as a failure:
		// the stream carries hook and system events ahead of the answer, and claude
		// is free to add more.
		if err := json.Unmarshal([]byte(line), &frame); err != nil {
			continue
		}

		if frame.Type != "control_response" || frame.Response.RequestID != probeRequestID {
			continue
		}

		if frame.Response.Subtype != "success" {
			return nil, fmt.Errorf("claude refused the models request: %s", frame.Response.Error)
		}

		if len(frame.Response.Response.Models) == 0 {
			return nil, errors.New("claude answered the models request with no models")
		}

		return frame.Response.Response.Models, nil
	}

	return nil, errors.New("claude did not answer the models request")
}

// toOptions converts claude's model list to the board's form.
func toOptions(models []probedModel) []mission.ModelOption {
	out := make([]mission.ModelOption, 0, len(models))

	for _, m := range models {
		if m.Value == "" {
			continue
		}

		opt := mission.ModelOption{
			Value:  m.Value,
			Label:  m.DisplayName,
			Detail: m.Description,
		}

		// The two are reported separately, and a model can list levels while
		// declining to support them. Trusting the flag keeps q from offering an
		// effort claude would reject.
		if m.SupportsEffort {
			opt.Efforts = m.SupportedEffortLevels
		}

		if opt.Label == "" {
			opt.Label = m.Value
		}

		out = append(out, opt)
	}

	return out
}

// resolveDefault reports the model a mission gets when the human picks none.
//
// The probe's own "default" entry is the last word rather than the first,
// because it reports what the account defaults to, not what this machine is set
// up to use. A user whose settings say opus expects a new mission to be opus
// even when their account's recommended default is sonnet, so their own
// configuration wins — in claude's own precedence order, which q mirrors rather
// than invents.
func (p *Prober) resolveDefault(models []probedModel) string {
	for _, candidate := range []func() string{
		func() string { v, _ := p.lookupEnv(EnvModel); return v },
		func() string { return settingsModel(p.managed) },
		func() string { return settingsModel(p.userSettingsPath()) },
	} {
		if v := strings.TrimSpace(candidate()); v != "" {
			return v
		}
	}

	// Nothing configured, so claude's own answer stands. The resolved name is
	// preferred over the literal "default" so the board and the launch script say
	// which model actually ran.
	for _, m := range models {
		if m.Value != DefaultValue {
			continue
		}

		if m.ResolvedModel != "" {
			// A resolved name claude also offers as a selectable value is the one to
			// record; otherwise "default" is the only value claude will accept back.
			if hasValue(models, m.ResolvedModel) {
				return m.ResolvedModel
			}

			if v := valueResolvingTo(models, m.ResolvedModel); v != "" {
				return v
			}
		}

		return m.Value
	}

	return ""
}

// hasValue reports whether the list offers a model by that exact value.
func hasValue(models []probedModel, value string) bool {
	for _, m := range models {
		if m.Value == value {
			return true
		}
	}

	return false
}

// valueResolvingTo finds the selectable alias for a concrete model name, e.g.
// "sonnet" for "claude-sonnet-5". The "default" entry is skipped, since naming
// it would defeat the point of resolving it.
func valueResolvingTo(models []probedModel, resolved string) string {
	for _, m := range models {
		if m.Value != DefaultValue && m.ResolvedModel == resolved {
			return m.Value
		}
	}

	return ""
}

// userSettingsPath is the user-level settings file claude reads.
func (p *Prober) userSettingsPath() string {
	home := p.home
	if home == "" {
		var err error

		home, err = os.UserHomeDir()
		if err != nil {
			return ""
		}
	}

	return filepath.Join(home, ".claude", "settings.json")
}

// defaultManagedSettings is where an enterprise deployment puts settings that
// override the user's own.
func defaultManagedSettings() string {
	if runtime.GOOS == "darwin" {
		return "/Library/Application Support/ClaudeCode/managed-settings.json"
	}

	return "/etc/claude-code/managed-settings.json"
}

// settingsModel reads the model key from one settings file.
//
// Every failure is silent and reports no preference. These files belong to
// claude, not to q: one may be absent, unreadable, or newer than q understands,
// and none of that is a reason to fail a probe that has already succeeded.
func settingsModel(path string) string {
	if path == "" {
		return ""
	}

	data, err := os.ReadFile(path) // #nosec G304 -- a fixed claude settings path.
	if err != nil {
		return ""
	}

	var doc map[string]json.RawMessage
	if err := json.Unmarshal(data, &doc); err != nil {
		return ""
	}

	var model string
	if err := json.Unmarshal(doc[SettingsKeyModel], &model); err != nil {
		return ""
	}

	return model
}
