// Asking codex which models it offers.
//
// codex answers a model/list request on its app-server with its full catalog:
// every model, which one is the default, and the reasoning efforts each accepts.
// That is asked for rather than hardcoded, because the catalog changes with
// releases and entitlements and a table baked into q would be wrong within weeks.
//
// The request goes to a private app-server this process starts and stops, not to
// the managed daemon [StartProxy] uses. The managed daemon needs codex's
// standalone installer, and a machine with codex from npm has none — but its
// codex can still answer the question perfectly well.
//
// When codex cannot be reached at all, the configuration file is the fallback:
// it names the model codex would use, and every model the user has configured a
// profile for. What it cannot name is which reasoning efforts those models
// accept, so the fallback reports none rather than guessing at values codex
// would reject at launch.

package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// listTimeout bounds the catalog request, including starting the app-server.
const listTimeout = 30 * time.Second

// ConfigFile is codex's own configuration, which q reads and never writes. The
// file q does write is the profile beside it, and it deliberately carries no
// model: a profile is shared by every mission, so a model set there could not be
// chosen per mission.
const ConfigFile = "config.toml"

// Config keys q reads.
const (
	keyModel  = "model"
	keyEffort = "model_reasoning_effort"
)

// Prober reports which models a codex mission can run.
type Prober struct {
	configDir string
	profile   string
	// models is the user's configured option list, used only when codex itself
	// cannot be asked.
	models []string
	// list fetches codex's own catalog. It is nil when q could not find codex, in
	// which case the configuration file is the only source.
	list func(context.Context) ([]Model, error)
}

// ProberOptions configure a [Prober].
type ProberOptions struct {
	// Bin is the codex executable. When empty, codex is never run and the
	// configuration file is the only source.
	Bin string
	// Version is q's version, reported to codex when the app-server connection is
	// initialized.
	Version string
	// Run starts the app-server process.
	Run runner.OS
	// ConfigDir is where codex keeps its configuration. Empty means ~/.codex.
	ConfigDir string
	// Profile is the codex profile q launches with, whose model key outranks the
	// top-level one.
	Profile string
	// Models is the option list to offer when codex cannot be asked.
	Models []string
}

// NewProber returns a prober for the given codex installation.
func NewProber(opts ProberOptions) *Prober {
	profile := opts.Profile
	if profile == "" {
		profile = DefaultProfile
	}

	p := &Prober{configDir: opts.ConfigDir, profile: profile, models: opts.Models}

	if opts.Bin != "" {
		p.list = func(ctx context.Context) ([]Model, error) {
			return listModels(ctx, opts.Bin, opts.Version, opts.Run)
		}
	}

	return p
}

// listModels asks a private app-server for codex's catalog and stops it again.
func listModels(ctx context.Context, bin, version string, run runner.OS) ([]Model, error) {
	ctx, cancel := context.WithTimeout(ctx, listTimeout)
	defer cancel()

	server, err := StartAppServer(ctx, bin, version, run)
	if err != nil {
		return nil, err
	}
	defer func() { _ = server.Close() }()

	return server.ListModels(ctx)
}

// Tool reports that this prober asks codex.
func (*Prober) Tool() mission.Tool { return mission.ToolCodex }

// Probe reports the models codex offers.
//
// Being unable to reach codex is not an error: the configuration file still says
// which model codex would use, and a board that offers that one model is more
// useful than a board that reports a failure. The reduced answer is marked, so
// the form and q doctor can say the list is partial rather than complete.
func (p *Prober) Probe(ctx context.Context) (mission.ModelSet, error) {
	settings := readConfig(p.configPath())

	set := mission.ModelSet{ProbedAt: time.Now()}

	models, err := p.catalog(ctx)
	if err != nil {
		set.Err = err.Error()
		set.Options = p.fallbackOptions(settings)
	} else {
		set.Options = toOptions(models)
	}

	// The configuration file outranks the catalog's own default, because a model
	// named there is the one codex would actually start with.
	set.Default = firstNonEmpty(settings.model(p.profile), defaultModel(models))
	set.DefaultEffort = firstNonEmpty(
		settings.effort(p.profile), defaultEffort(models, set.Default))

	// An effort the chosen model does not accept would be rejected at launch, in
	// a detached pane where nobody would see the refusal.
	if !set.ValidEffort(set.Default, set.DefaultEffort) {
		set.DefaultEffort = ""
	}

	return set, nil
}

// catalog fetches codex's own model list, or reports why it could not.
func (p *Prober) catalog(ctx context.Context) ([]Model, error) {
	if p.list == nil {
		return nil, errNoCodex
	}

	return p.list(ctx)
}

// errNoCodex reports that there is no codex on this machine to ask.
var errNoCodex = errors.New("no codex binary to ask")

// toOptions converts codex's catalog to the board's form.
func toOptions(models []Model) []mission.ModelOption {
	out := make([]mission.ModelOption, 0, len(models))

	for _, m := range models {
		// Model is what -m takes; ID is the catalog key, and stands in on the rare
		// entry that carries only one of the two.
		value := firstNonEmpty(m.Model, m.ID)
		if value == "" {
			continue
		}

		opt := mission.ModelOption{
			Value:  value,
			Label:  firstNonEmpty(m.DisplayName, value),
			Detail: m.Description,
		}

		for _, effort := range m.SupportedReasoningEfforts {
			if effort.ReasoningEffort != "" {
				opt.Efforts = append(opt.Efforts, effort.ReasoningEffort)
			}
		}

		out = append(out, opt)
	}

	return out
}

// defaultModel is the model codex marks as its own default.
func defaultModel(models []Model) string {
	for _, m := range models {
		if m.IsDefault {
			return firstNonEmpty(m.Model, m.ID)
		}
	}

	return ""
}

// defaultEffort is the effort codex pairs with a model by default.
func defaultEffort(models []Model, value string) string {
	for _, m := range models {
		if firstNonEmpty(m.Model, m.ID) == value {
			return m.DefaultReasoningEffort
		}
	}

	return ""
}

// fallbackOptions is what to offer when codex could not be asked: every model
// the user's own configuration names, plus whatever they listed in q's.
//
// None carries effort levels. Which efforts a model accepts is knowable only
// from codex, and offering a guess would produce a launch codex refuses.
func (p *Prober) fallbackOptions(settings config) []mission.ModelOption {
	var out []mission.ModelOption

	for _, value := range slices.Concat(settings.models, p.models) {
		if value == "" || slices.ContainsFunc(out, func(o mission.ModelOption) bool {
			return o.Value == value
		}) {
			continue
		}

		out = append(out, mission.ModelOption{Value: value, Label: value})
	}

	return out
}

// firstNonEmpty returns the first value that is not empty.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// configPath is the codex configuration file this prober reads.
func (p *Prober) configPath() string {
	dir := p.configDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}

		dir = filepath.Join(home, ".codex")
	}

	return filepath.Join(dir, ConfigFile)
}

// config is what q understands of codex's configuration file.
type config struct {
	// top holds the keys before any table header.
	top table
	// profiles holds each [profiles.<name>] table.
	profiles map[string]table
	// models is every model value the file mentions, in the order it mentions
	// them, so a user who has profiles for three models sees all three.
	models []string
}

// table is one table's keys.
type table map[string]string

// model reports the model codex would use under a profile, which outranks the
// top-level setting.
func (c config) model(profile string) string { return c.lookup(profile, keyModel) }

// effort reports the reasoning effort codex would use under a profile.
func (c config) effort(profile string) string { return c.lookup(profile, keyEffort) }

// lookup reads one key, preferring the profile q launches with over the
// top-level table, which is codex's own precedence.
func (c config) lookup(profile, key string) string {
	if v := c.profiles[profile][key]; v != "" {
		return v
	}

	return c.top[key]
}

// readConfig parses the subset of codex's configuration q needs.
//
// It is scanned line-wise rather than parsed as TOML because q has no TOML
// dependency and this file is already handled at the byte level here: the
// profile q writes is rendered by hand in codex.go and merged by hand in
// profile.go. Only bare `key = "value"` assignments are recognized; anything
// this misses simply leaves q with no opinion, which is the same position it
// occupies when the file is absent.
func readConfig(path string) config {
	out := config{top: table{}, profiles: map[string]table{}}

	if path == "" {
		return out
	}

	data, err := os.ReadFile(path) // #nosec G304 -- a fixed codex configuration path.
	if err != nil {
		return out
	}

	current := out.top

	for _, line := range strings.Split(string(data), "\n") {
		line = strings.TrimSpace(line)

		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}

		if name, ok := tableHeader(line); ok {
			// Every table other than a profile is one q has no keys in, so it is
			// given a scratch table rather than being allowed to write into the
			// previous one.
			current = table{}

			if profile, isProfile := strings.CutPrefix(name, "profiles."); isProfile && profile != "" {
				out.profiles[profile] = current
			}

			continue
		}

		key, value, ok := assignment(line)
		if !ok {
			continue
		}

		current[key] = value

		if key == keyModel && value != "" && !slices.Contains(out.models, value) {
			out.models = append(out.models, value)
		}
	}

	return out
}

// tableHeader reports the name of a `[table.name]` line.
func tableHeader(line string) (string, bool) {
	if !strings.HasPrefix(line, "[") {
		return "", false
	}

	name, ok := strings.CutSuffix(line, "]")
	if !ok {
		return "", false
	}

	// A [[array]] header names something q reads no keys from, but it still ends
	// the previous table, so it is reported as a header with an unusable name.
	return unquoteKey(strings.TrimPrefix(name, "[")), true
}

// assignment splits a `key = "value"` line. A line whose value is not a quoted
// string is not one q reads.
func assignment(line string) (string, string, bool) {
	key, rest, ok := strings.Cut(line, "=")
	if !ok {
		return "", "", false
	}

	key = strings.TrimSpace(key)

	// A trailing comment is only stripped after the closing quote, so a value
	// containing a # survives.
	value := strings.TrimSpace(rest)

	if !strings.HasPrefix(value, `"`) {
		return "", "", false
	}

	end := strings.Index(value[1:], `"`)
	if end < 0 {
		return "", "", false
	}

	return key, value[1 : 1+end], true
}

// unquoteKey strips the quotes codex puts around a table name that needs them.
func unquoteKey(name string) string {
	return strings.ReplaceAll(strings.TrimSpace(name), `"`, "")
}
