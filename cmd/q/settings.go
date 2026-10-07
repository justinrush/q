package main

import (
	"os"
	"runtime"
	"strings"
	"time"

	"github.com/justinrush/q/internal/daemon"
	"github.com/justinrush/q/internal/remote"
)

// settings is the complete set of resolved options.
//
// It is the shape every internal package's constructor is fed from, and it holds
// no JSON tags: the file format lives in config.go, so a change to either can be
// made without touching the other.
type settings struct {
	Repos    reposSettings
	Git      gitSettings
	Agents   agentsSettings
	Editor   editorSettings
	Terminal terminalSettings
	Paths    pathsSettings
	Cost     costSettings
	TUI      tuiSettings
	Queue    queueSettings
	Remote   remoteSettings
	// Tools maps a tool name ("git", "tmux", "codex", …) to an absolute path,
	// overriding both PATH lookup and the built-in fallbacks.
	Tools    map[string]string
	LogLevel string
}

// queueSettings configures how the daemon works through queued missions.
type queueSettings struct {
	// MaxConcurrent is how many queued missions run at once on this machine. A
	// mission started by hand is not held back by it, but does count against it.
	MaxConcurrent int
}

// remoteSettings pairs this q with one on another machine.
type remoteSettings struct {
	// SSH is the argv that gets a shell on the peer. Setting it is what makes
	// this machine the primary of the pair: it dials, it is preferred for
	// running agents, and it wins ties. Empty means q is not a primary.
	SSH []string
	// Bin is the q binary on the peer, as the peer's shell would resolve it. It
	// is deliberately not expanded here: a leading ~ means the peer's home.
	Bin string
	// Name is what this machine is called on cards and to its peer.
	Name string
	// Interval is how often the primary exchanges state with its peer.
	Interval time.Duration
	// TakeoverAfter is how long the peer waits, having not heard from this
	// machine, before running its missions.
	TakeoverAfter time.Duration
}

// tuiSettings configures the terminal UI.
type tuiSettings struct {
	// Mouse enables mouse reporting in the TUI (clicking cards, tabs, and scrolling).
	Mouse bool
}

// reposSettings configures where q looks for the git checkouts an operation can span.
type reposSettings struct {
	// Roots are the directories searched, each of which may start with ~.
	Roots []string
	// MaxDepth is how many levels below a root are walked. The walk stops
	// descending as soon as it recognizes a checkout, so depth only bounds the
	// containers above one.
	MaxDepth int
	// Skip are directory names never descended into, on top of hidden ones.
	Skip []string
}

// gitSettings configures how q names the branches it creates.
type gitSettings struct {
	// BranchPrefix is the namespace mission branches are cut under, e.g. a
	// prefix of "jane" gives "jane/add-endpoint". Empty means use $USER.
	BranchPrefix string
}

// agentsSettings configures the coding agents q can dispatch.
type agentsSettings struct {
	// Default is the agent a new mission gets: "claude", "codex", "agy", or "opencode".
	Default  string
	Claude   agentSettings
	Codex    codexSettings
	Agy      agentSettings
	Opencode agentSettings
	// ModelRefresh is how often the daemon re-asks each agent which models it
	// offers. Zero means the daemon's own default.
	ModelRefresh time.Duration
}

// agentSettings configures one coding agent.
type agentSettings struct {
	// Bin is an absolute path overriding PATH lookup.
	Bin string
	// Args are extra arguments appended to every invocation, before the prompt.
	Args []string
	// Model overrides the default q discovers by asking the agent. It exists for
	// a user who wants q missions on a different model from their own interactive
	// sessions, and as the escape hatch when discovery is wrong.
	Model string
	// Effort overrides the discovered default reasoning effort.
	Effort string
	// Models replaces the model list offered on the board, for an agent q cannot
	// ask. It is ignored for an agent that answers with its own list.
	Models []string
}

// codexSettings adds the settings that only apply to codex.
type codexSettings struct {
	agentSettings
	// ConfigDir is where codex keeps its configuration, normally ~/.codex. q
	// writes its own profile there and never touches config.toml.
	ConfigDir string
	// Profile is the codex profile name q writes and selects.
	Profile string
}

// costSettings configures the running total shown on a mission card.
//
// The rates are carried as plain numbers rather than as the usage package's own
// type so that settings stays free of internal imports, and are converted where
// the meter is built.
type costSettings struct {
	// Disabled turns metering off entirely: no transcripts are read and no card
	// carries a cost.
	Disabled bool
	// Models overrides or extends the built-in price table, keyed by model id,
	// in dollars per million tokens.
	Models map[string]modelPriceSettings
}

// modelPriceSettings is one model's rates, in dollars per million tokens.
type modelPriceSettings struct {
	Input  float64
	Output float64
	// CacheRead overrides the usual fraction-of-input read rate, for a model
	// priced differently. Zero means use the default.
	CacheRead float64
}

// editorSettings configures the command opened on each changed worktree in a debrief.
type editorSettings struct {
	// Command is the argv, e.g. ["nvim", "+Neotree"]. The worktree is the
	// process's working directory, so no path argument is needed.
	Command []string
}

// Terminal modes.
const (
	// terminalGhostty drives Ghostty 1.3+ through its macOS AppleScript
	// interface, which keeps new windows inside the running application.
	terminalGhostty = "ghostty"
	// terminalCommand runs a user-supplied argv template.
	terminalCommand = "command"
	// terminalNone never opens a window. q reports the tmux command to run
	// instead, for callers that want to attach manually.
	terminalNone = "none"
	// terminalCurrent attaches using the foreground client terminal.
	terminalCurrent = "current"
)

// terminalSettings configures how a debrief window is opened.
type terminalSettings struct {
	// Mode is one of terminalGhostty, terminalCommand, terminalCurrent, or terminalNone.
	Mode string
	// Command is the argv template used when Mode is terminalCommand.
	//
	// Two placeholders are substituted: "{dir}" becomes the working directory,
	// and "{cmd}" becomes the shell-quoted command. An element that is exactly
	// "{argv}" is replaced by the command's arguments, spliced in place.
	Command []string
}

// pathsSettings overrides where q keeps its files. Empty values fall back to the XDG
// directories.
type pathsSettings struct {
	// Data holds durable content: state and mission worktrees.
	Data string
	// State holds runtime ephemera: the daemon handle, the hook spool, logs.
	State string
}

// defaultSettings returns the built-in settings, before any config file or environment
// override is applied.
func defaultSettings() settings {
	return settings{
		Repos: reposSettings{
			Roots:    []string{"~/dev", "~/src", "~/code"},
			MaxDepth: 5,
			Skip:     []string{"node_modules", "vendor", "target", "Library"},
		},
		Agents: agentsSettings{
			Default: "claude",
			Codex:   codexSettings{ConfigDir: "~/.codex", Profile: "q"},
		},
		Editor:   editorSettings{Command: defaultEditorCommand()},
		Terminal: terminalSettings{Mode: defaultTerminalMode()},
		TUI:      tuiSettings{Mouse: true},
		Queue:    queueSettings{MaxConcurrent: daemon.DefaultMaxConcurrent},
		Remote: remoteSettings{
			Bin:           remote.DefaultBin,
			Name:          defaultHostName(),
			Interval:      daemon.DefaultSyncInterval,
			TakeoverAfter: daemon.DefaultTakeoverAfter,
		},
		Tools:    map[string]string{},
		LogLevel: "info",
	}
}

// defaultHostName is the machine's own name without its domain, which is how a
// person refers to it: "mini", not "mini.local".
func defaultHostName() string {
	name, err := os.Hostname()
	if err != nil {
		return ""
	}

	short, _, _ := strings.Cut(name, ".")

	return short
}

// defaultTerminalMode picks the mode that works out of the box on this OS.
//
// Remote shells and non-macOS hosts attach in the terminal running q.
func defaultTerminalMode() string {
	if runtime.GOOS == "darwin" && os.Getenv("SSH_CONNECTION") == "" && os.Getenv("SSH_TTY") == "" {
		return terminalGhostty
	}

	return terminalCurrent
}

// defaultEditorCommand honors $VISUAL, then $EDITOR, and falls back to vi,
// which POSIX requires to exist.
func defaultEditorCommand() []string {
	for _, key := range []string{"VISUAL", "EDITOR"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return strings.Fields(v)
		}
	}

	return []string{"vi"}
}
