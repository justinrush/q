// Absolute paths to the external programs q drives.
//
// Absolute paths matter here more than usual. q launches processes from a
// daemon that may have been started with a minimal environment, and two of the
// tools it needs are not reachable that way: codex lives inside an
// nvm-managed node version directory, and Ghostty ships only as an app bundle
// with no binary on PATH. Relying on the inherited PATH would work from an
// interactive shell and fail from the daemon.
package main

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"sync"
)

// toolName names an external program q depends on.
type toolName string

// The tools q drives.
const (
	toolGit       toolName = "git"
	toolTmux      toolName = "tmux"
	toolClaude    toolName = "claude"
	toolCodex     toolName = "codex"
	toolOsaScript toolName = "osascript"
)

// allTools lists every tool, in the order q doctor reports them.
var allTools = []toolName{toolGit, toolTmux, toolClaude, toolCodex, toolOsaScript}

// defaultRequiredTools lists the tools q cannot function at all without.
//
// Claude and Codex are absent because a user may reasonably have only one of
// them, and osascript because it is only needed by the Ghostty terminal mode;
// callers that need it add it to toolOptions.Required.
var defaultRequiredTools = []toolName{toolGit, toolTmux}

// ghosttyApp is the app bundle q controls through its AppleScript API.
const ghosttyApp = "/Applications/Ghostty.app"

// toolOptionsFor derives resolver options from the user's settings.
//
// It lives here rather than in each caller because the daemon and q doctor must
// resolve tools identically; a doctor that looked somewhere else would report a
// setup the daemon cannot reproduce.
func toolOptionsFor(s settings) toolOptions {
	overrides := map[toolName]string{}

	for name, path := range s.Tools {
		overrides[toolName(name)] = path
	}

	// The agents section is the natural place to name an agent binary, so it is
	// accepted there as well as in the tools map.
	if bin := s.Agents.Claude.Bin; bin != "" {
		overrides[toolClaude] = bin
	}

	if bin := s.Agents.Codex.Bin; bin != "" {
		overrides[toolCodex] = bin
	}

	return toolOptions{Overrides: overrides, Required: requiredToolsFor(s)}
}

// requiredToolsFor lists what a configuration cannot start without.
//
// osascript is required only by the Ghostty terminal mode; demanding it
// elsewhere would make q refuse to start on Linux over a tool it never calls.
func requiredToolsFor(s settings) []toolName {
	tools := []toolName{toolGit, toolTmux}

	if s.Terminal.Mode == terminalGhostty {
		tools = append(tools, toolOsaScript)
	}

	return tools
}

// toolOptions configures a toolResolver.
type toolOptions struct {
	// Overrides maps a tool to an absolute path, from the config file. The
	// matching environment variable still wins over it.
	Overrides map[toolName]string
	// Required replaces defaultRequiredTools for Check.
	Required []toolName
}

// EnvVar returns the environment variable that overrides a tool's path, e.g.
// Q_CODEX_BIN.
func (t toolName) EnvVar() string {
	return "Q_" + strings.ToUpper(string(t)) + "_BIN"
}

// fallbacks lists absolute candidates to try when PATH lookup fails. Entries
// may contain a single glob, expanded at resolution time.
var fallbacks = map[toolName][]string{
	toolGit:       {"/opt/homebrew/bin/git", "/usr/bin/git"},
	toolTmux:      {"/opt/homebrew/bin/tmux", "/usr/local/bin/tmux"},
	toolClaude:    {"~/.local/bin/claude", "/opt/homebrew/bin/claude"},
	toolCodex:     {"~/.nvm/versions/node/*/bin/codex", "/opt/homebrew/bin/codex"},
	toolOsaScript: {"/usr/bin/osascript"},
}

// toolResolver caches tool paths for the lifetime of a process. It is safe for
// concurrent use.
type toolResolver struct {
	mu        sync.Mutex
	cache     map[toolName]string
	errors    map[toolName]error
	overrides map[toolName]string
	required  []toolName
}

// newToolResolver returns a resolver honoring the given options.
func newToolResolver(opts toolOptions) *toolResolver {
	required := opts.Required
	if len(required) == 0 {
		required = defaultRequiredTools
	}

	return &toolResolver{
		cache:     map[toolName]string{},
		errors:    map[toolName]error{},
		overrides: opts.Overrides,
		required:  required,
	}
}

// Path returns the absolute path to a tool, resolving and caching it on first
// use. Resolution order is: the Q_<TOOL>_BIN override, then PATH, then
// the known-location fallbacks.
func (r *toolResolver) Path(t toolName) (string, error) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if p, ok := r.cache[t]; ok {
		return p, nil
	}

	if err, ok := r.errors[t]; ok {
		return "", err
	}

	p, err := resolve(t, r.overrides[t])
	if err != nil {
		r.errors[t] = err

		return "", err
	}

	r.cache[t] = p

	return p, nil
}

// MustPath is Path for call sites that have already verified availability,
// typically after a Check at startup. It returns the empty string on failure.
func (r *toolResolver) MustPath(t toolName) string {
	p, err := r.Path(t)
	if err != nil {
		return ""
	}

	return p
}

// Check verifies that every required tool resolves, returning the first
// failure. Call it once at startup so a missing tmux is reported up front
// rather than in the middle of launching a mission.
func (r *toolResolver) Check() error {
	for _, t := range r.required {
		if _, err := r.Path(t); err != nil {
			return err
		}
	}

	return nil
}

// resolve performs the actual lookup for one tool.
//
// The environment variable wins over the configured path, so a one-off run can
// point q at a different build without editing the config file.
func resolve(t toolName, configured string) (string, error) {
	source := t.EnvVar()

	override := os.Getenv(t.EnvVar())
	if override == "" {
		override, source = configured, "the configured path for "+string(t)
	}

	if override != "" {
		if !filepath.IsAbs(override) {
			return "", fmt.Errorf("%s must be an absolute path, got %q", source, override)
		}

		if !executable(override) {
			return "", fmt.Errorf("%s points at %q, which is not an executable file", source, override)
		}

		return override, nil
	}

	if p, err := exec.LookPath(string(t)); err == nil {
		if abs, absErr := filepath.Abs(p); absErr == nil {
			return abs, nil
		}

		return p, nil
	}

	if p := firstFallback(t); p != "" {
		return p, nil
	}

	return "", fmt.Errorf("%s not found on PATH or in any known location (set %s to override)", t, t.EnvVar())
}

// firstFallback returns the first usable known-location candidate for a tool.
func firstFallback(t toolName) string {
	for _, cand := range fallbacks[t] {
		for _, p := range expand(cand) {
			if executable(p) {
				return p
			}
		}
	}

	return ""
}

// expand resolves a leading ~ and any glob in a candidate path. Glob matches
// come back newest-first, so the most recently installed node version wins for
// codex.
func expand(cand string) []string {
	if strings.HasPrefix(cand, "~/") {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil
		}

		cand = filepath.Join(home, cand[2:])
	}

	if !strings.ContainsRune(cand, '*') {
		return []string{cand}
	}

	matches, err := filepath.Glob(cand)
	if err != nil || len(matches) == 0 {
		return nil
	}

	sortNewestFirst(matches)

	return matches
}

// sortNewestFirst orders paths by descending modification time. Node version
// directories cannot be compared lexically (v9 would beat v26), and mtime is a
// good proxy for "most recently installed".
func sortNewestFirst(paths []string) {
	mtime := func(p string) int64 {
		fi, err := os.Stat(p)
		if err != nil {
			return 0
		}

		return fi.ModTime().UnixNano()
	}

	sort.SliceStable(paths, func(i, j int) bool {
		return mtime(paths[i]) > mtime(paths[j])
	})
}

// executable reports whether p is a regular file with an execute bit set.
func executable(p string) bool {
	fi, err := os.Stat(p)
	if err != nil || fi.IsDir() {
		return false
	}

	return fi.Mode().Perm()&0o111 != 0
}
