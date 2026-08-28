package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/settings"
)

// cfg is the resolved configuration for this process. It is populated once by
// the root command's PersistentPreRunE, before any subcommand runs, so every
// command sees the same values and a malformed config file fails immediately
// rather than halfway through provisioning something.
var cfg = settings.Default()

// configFlag holds the --config value, when one was given.
var configFlag string

// DefaultConfigName is the file q looks for in the user's home directory.
const DefaultConfigName = ".q-config.json"

// EnvConfig overrides the config file location entirely.
const EnvConfig = "Q_CONFIG"

// Environment variables that override individual settings. They exist because
// the daemon is started as a subprocess and a wrapper script is often the
// easiest place to point one run at a different checkout or state directory.
const (
	EnvRepoRoots   = "Q_REPO_ROOTS"
	EnvDataDir     = "Q_DATA_DIR"
	EnvStateDir    = "Q_STATE_DIR"
	EnvEditor      = "Q_EDITOR"
	EnvTerminal    = "Q_TERMINAL"
	EnvBranchPfx   = "Q_BRANCH_PREFIX"
	EnvDefaultTool = "Q_DEFAULT_AGENT"
	EnvLogLevel    = "Q_LOG_LEVEL"
)

// fileConfig is the JSON shape of ~/.q-config.json.
//
// It exists separately from settings.Settings so that the marshaling concerns —
// tags, pointers for "was this key present", tolerant shapes — stay in the cmd
// package, and the internal packages take plain Go values.
type fileConfig struct {
	Repos    *reposConfig    `json:"repos,omitempty"`
	Git      *gitConfig      `json:"git,omitempty"`
	Agents   *agentsConfig   `json:"agents,omitempty"`
	Editor   *editorConfig   `json:"editor,omitempty"`
	Terminal *terminalConfig `json:"terminal,omitempty"`
	Paths    *pathsConfig    `json:"paths,omitempty"`
	// Tools maps a tool name to an absolute path, e.g. {"tmux": "/usr/bin/tmux"}.
	Tools    map[string]string `json:"tools,omitempty"`
	LogLevel string            `json:"logLevel,omitempty"`
}

type reposConfig struct {
	Roots    []string `json:"roots,omitempty"`
	MaxDepth int      `json:"maxDepth,omitempty"`
	Skip     []string `json:"skip,omitempty"`
}

type gitConfig struct {
	BranchPrefix string `json:"branchPrefix,omitempty"`
}

type agentsConfig struct {
	Default string       `json:"default,omitempty"`
	Claude  *agentConfig `json:"claude,omitempty"`
	Codex   *codexConfig `json:"codex,omitempty"`
}

type agentConfig struct {
	Bin  string   `json:"bin,omitempty"`
	Args []string `json:"args,omitempty"`
}

type codexConfig struct {
	Bin       string   `json:"bin,omitempty"`
	Args      []string `json:"args,omitempty"`
	ConfigDir string   `json:"configDir,omitempty"`
	Profile   string   `json:"profile,omitempty"`
}

type editorConfig struct {
	Command []string `json:"command,omitempty"`
}

type terminalConfig struct {
	Mode    string   `json:"mode,omitempty"`
	Command []string `json:"command,omitempty"`
}

type pathsConfig struct {
	Data  string `json:"dataDir,omitempty"`
	State string `json:"stateDir,omitempty"`
}

// loadConfig resolves the effective settings: built-in defaults, then the config
// file, then environment overrides.
//
// A missing config file is not an error — q is expected to work with no
// configuration at all — but a malformed one is, because silently falling back
// to defaults would hide the typo that caused it.
func loadConfig(path string) (settings.Settings, error) {
	out := settings.Default()

	file, err := readConfigFile(path)
	if err != nil {
		return out, err
	}

	applyFile(&out, file)
	applyEnv(&out)
	expandSettings(&out)

	return out, nil
}

// readConfigFile reads and decodes the config file, returning the zero value
// when there is none to read.
func readConfigFile(path string) (fileConfig, error) {
	var file fileConfig

	if path == "" {
		return file, nil
	}

	data, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return file, nil
	}

	if err != nil {
		return file, fmt.Errorf("reading %s: %w", path, err)
	}

	dec := json.NewDecoder(strings.NewReader(string(data)))
	dec.DisallowUnknownFields()

	if err := dec.Decode(&file); err != nil {
		return file, fmt.Errorf("parsing %s: %w", path, err)
	}

	return file, nil
}

// configPath returns the config file q should read, or "" when there is none.
//
// The search order is the --config flag, then $Q_CONFIG, then ~/.q-config.json,
// then the XDG location. The first path that exists wins; when none does, the
// home-directory path is returned anyway so `q config path` can say where to
// create one.
func configPath() string {
	if configFlag != "" {
		return expandHome(configFlag)
	}

	if env := strings.TrimSpace(os.Getenv(EnvConfig)); env != "" {
		return expandHome(env)
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return ""
	}

	primary := filepath.Join(home, DefaultConfigName)
	if _, err := os.Stat(primary); err == nil {
		return primary
	}

	if dirs, err := paths.Resolve(paths.Overrides{}); err == nil {
		if _, err := os.Stat(dirs.ConfigFile()); err == nil {
			return dirs.ConfigFile()
		}
	}

	return primary
}

// applyFile layers the config file over the defaults. Only keys the file
// actually set are applied, which is why the sections are pointers.
func applyFile(out *settings.Settings, file fileConfig) {
	if r := file.Repos; r != nil {
		if len(r.Roots) > 0 {
			out.Repos.Roots = r.Roots
		}

		if r.MaxDepth > 0 {
			out.Repos.MaxDepth = r.MaxDepth
		}

		if len(r.Skip) > 0 {
			out.Repos.Skip = r.Skip
		}
	}

	if g := file.Git; g != nil && g.BranchPrefix != "" {
		out.Git.BranchPrefix = g.BranchPrefix
	}

	applyAgents(out, file.Agents)

	if e := file.Editor; e != nil && len(e.Command) > 0 {
		out.Editor.Command = e.Command
	}

	if t := file.Terminal; t != nil {
		if t.Mode != "" {
			out.Terminal.Mode = t.Mode
		}

		if len(t.Command) > 0 {
			out.Terminal.Command = t.Command
		}
	}

	if p := file.Paths; p != nil {
		out.Paths.Data = firstNonEmpty(p.Data, out.Paths.Data)
		out.Paths.State = firstNonEmpty(p.State, out.Paths.State)
	}

	for name, path := range file.Tools {
		out.Tools[name] = path
	}

	if file.LogLevel != "" {
		out.LogLevel = file.LogLevel
	}
}

// applyAgents layers the agents section.
func applyAgents(out *settings.Settings, agents *agentsConfig) {
	if agents == nil {
		return
	}

	if agents.Default != "" {
		out.Agents.Default = agents.Default
	}

	if c := agents.Claude; c != nil {
		out.Agents.Claude.Bin = firstNonEmpty(c.Bin, out.Agents.Claude.Bin)
		if len(c.Args) > 0 {
			out.Agents.Claude.Args = c.Args
		}
	}

	if c := agents.Codex; c != nil {
		out.Agents.Codex.Bin = firstNonEmpty(c.Bin, out.Agents.Codex.Bin)
		out.Agents.Codex.ConfigDir = firstNonEmpty(c.ConfigDir, out.Agents.Codex.ConfigDir)
		out.Agents.Codex.Profile = firstNonEmpty(c.Profile, out.Agents.Codex.Profile)

		if len(c.Args) > 0 {
			out.Agents.Codex.Args = c.Args
		}
	}
}

// applyEnv layers environment variables over the file.
func applyEnv(out *settings.Settings) {
	if v := strings.TrimSpace(os.Getenv(EnvRepoRoots)); v != "" {
		out.Repos.Roots = splitList(v)
	}

	if v := strings.TrimSpace(os.Getenv(EnvDataDir)); v != "" {
		out.Paths.Data = v
	}

	if v := strings.TrimSpace(os.Getenv(EnvStateDir)); v != "" {
		out.Paths.State = v
	}

	if v := strings.TrimSpace(os.Getenv(EnvEditor)); v != "" {
		out.Editor.Command = strings.Fields(v)
	}

	if v := strings.TrimSpace(os.Getenv(EnvTerminal)); v != "" {
		out.Terminal.Mode = v
	}

	if v := strings.TrimSpace(os.Getenv(EnvBranchPfx)); v != "" {
		out.Git.BranchPrefix = v
	}

	if v := strings.TrimSpace(os.Getenv(EnvDefaultTool)); v != "" {
		out.Agents.Default = v
	}

	if v := strings.TrimSpace(os.Getenv(EnvLogLevel)); v != "" {
		out.LogLevel = v
	}
}

// expandSettings resolves ~ in every path-shaped value, so the rest of q never
// has to wonder whether a path has been expanded yet.
func expandSettings(out *settings.Settings) {
	for i, root := range out.Repos.Roots {
		out.Repos.Roots[i] = expandHome(root)
	}

	out.Paths.Data = expandHome(out.Paths.Data)
	out.Paths.State = expandHome(out.Paths.State)
	out.Agents.Claude.Bin = expandHome(out.Agents.Claude.Bin)
	out.Agents.Codex.Bin = expandHome(out.Agents.Codex.Bin)
	out.Agents.Codex.ConfigDir = expandHome(out.Agents.Codex.ConfigDir)

	for name, path := range out.Tools {
		out.Tools[name] = expandHome(path)
	}
}

// expandHome resolves a leading ~ against the user's home directory.
func expandHome(p string) string {
	if p == "" || !strings.HasPrefix(p, "~") {
		return p
	}

	rest, _ := strings.CutPrefix(p, "~")
	if rest != "" && !strings.HasPrefix(rest, "/") {
		return p
	}

	home, err := os.UserHomeDir()
	if err != nil {
		return p
	}

	return filepath.Join(home, rest)
}

// splitList splits an OS-path-list-separated value, e.g. Q_REPO_ROOTS.
func splitList(v string) []string {
	var out []string

	for _, entry := range strings.Split(v, string(os.PathListSeparator)) {
		if entry = strings.TrimSpace(entry); entry != "" {
			out = append(out, entry)
		}
	}

	return out
}

// firstNonEmpty returns the first non-empty string.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// pathOverrides converts the configured directories into the form paths takes.
func pathOverrides() paths.Overrides {
	return paths.Overrides{Data: cfg.Paths.Data, State: cfg.Paths.State}
}

// writeSampleConfig writes a fully populated config file to w, so `q config init`
// produces something a user can edit rather than an empty object.
//
// Every key is written with its current effective value, which makes the file
// double as documentation of what q resolved on this machine.
func writeSampleConfig(w io.Writer, s settings.Settings) error {
	file := fileConfig{
		Repos: &reposConfig{Roots: s.Repos.Roots, MaxDepth: s.Repos.MaxDepth, Skip: s.Repos.Skip},
		Git:   &gitConfig{BranchPrefix: branchPrefixOrUser(s)},
		Agents: &agentsConfig{
			Default: s.Agents.Default,
			Claude:  &agentConfig{Bin: s.Agents.Claude.Bin, Args: s.Agents.Claude.Args},
			Codex: &codexConfig{
				Bin:       s.Agents.Codex.Bin,
				Args:      s.Agents.Codex.Args,
				ConfigDir: s.Agents.Codex.ConfigDir,
				Profile:   s.Agents.Codex.Profile,
			},
		},
		Editor:   &editorConfig{Command: s.Editor.Command},
		Terminal: &terminalConfig{Mode: s.Terminal.Mode, Command: s.Terminal.Command},
		Paths:    &pathsConfig{Data: s.Paths.Data, State: s.Paths.State},
		Tools:    s.Tools,
		LogLevel: s.LogLevel,
	}

	enc := json.NewEncoder(w)
	enc.SetIndent("", "  ")

	return enc.Encode(file)
}

// branchPrefixOrUser resolves the branch prefix the way the launcher will, so a
// generated config shows the real value rather than an empty string.
func branchPrefixOrUser(s settings.Settings) string {
	if s.Git.BranchPrefix != "" {
		return s.Git.BranchPrefix
	}

	for _, key := range []string{"USER", "LOGNAME"} {
		if v := os.Getenv(key); v != "" {
			return v
		}
	}

	return "q"
}

// createConfigFile writes the effective settings to path.
//
// It refuses to clobber an existing file without --force, because the file it
// would replace is hand-edited by definition.
func createConfigFile(path string, force bool) (string, error) {
	if path == "" {
		return "", errors.New("no config path could be resolved; set Q_CONFIG or pass --config")
	}

	if _, err := os.Stat(path); err == nil && !force {
		return "", fmt.Errorf("%s already exists (pass --force to overwrite)", path)
	}

	if err := os.MkdirAll(filepath.Dir(path), paths.DirMode); err != nil {
		return "", fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	var buf strings.Builder
	if err := writeSampleConfig(&buf, cfg); err != nil {
		return "", err
	}

	if err := os.WriteFile(path, []byte(buf.String()), paths.FileMode); err != nil {
		return "", fmt.Errorf("writing %s: %w", path, err)
	}

	return path, nil
}
