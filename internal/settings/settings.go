// Package settings holds Q's standing orders: the tunable assumptions every
// other package would otherwise hard-code.
//
// It is a leaf. Nothing here parses a file, reads an environment variable, or
// touches the network — those belong to cmd/q, which loads ~/.q-config.json and
// hands the resolved values down. Keeping the shape here and the serde there is
// what lets a package take exactly the sub-struct it needs, and lets tests build
// one in a literal.
package settings

import (
	"os"
	"runtime"
	"strings"
)

// Settings is the complete set of resolved options.
type Settings struct {
	Repos    Repos
	Git      Git
	Agents   Agents
	Editor   Editor
	Terminal Terminal
	Paths    Paths
	// Tools maps a tool name ("git", "tmux", "codex", …) to an absolute path,
	// overriding both PATH lookup and the built-in fallbacks.
	Tools map[string]string
	// LogLevel is the threshold for q's own logs: debug, info, warn, or error.
	LogLevel string
}

// Repos configures where q looks for the git checkouts an operation can span.
type Repos struct {
	// Roots are the directories searched, each of which may start with ~.
	Roots []string
	// MaxDepth is how many levels below a root are walked. The walk stops
	// descending as soon as it recognizes a checkout, so depth only bounds the
	// containers above one.
	MaxDepth int
	// Skip are directory names never descended into, on top of hidden ones.
	Skip []string
}

// Git configures how q names the branches it creates.
type Git struct {
	// BranchPrefix is the namespace mission branches are cut under, e.g. a
	// prefix of "jane" gives "jane/add-endpoint". Empty means use $USER.
	BranchPrefix string
}

// Agents configures the coding agents q can dispatch.
type Agents struct {
	// Default is the agent a new mission gets: "claude" or "codex".
	Default string
	Claude  Agent
	Codex   Codex
}

// Agent configures one coding agent.
type Agent struct {
	// Bin is an absolute path overriding PATH lookup.
	Bin string
	// Args are extra arguments appended to every invocation, before the prompt.
	Args []string
}

// Codex adds the settings that only apply to codex.
type Codex struct {
	Agent
	// ConfigDir is where codex keeps its configuration, normally ~/.codex. q
	// writes its own profile there and never touches config.toml.
	ConfigDir string
	// Profile is the codex profile name q writes and selects.
	Profile string
}

// Editor configures the command opened on each changed worktree in a debrief.
type Editor struct {
	// Command is the argv, e.g. ["nvim", "+Neotree"]. The worktree is the
	// process's working directory, so no path argument is needed.
	Command []string
}

// Terminal modes.
const (
	// TerminalGhostty drives Ghostty 1.3+ through its macOS AppleScript
	// interface, which keeps new windows inside the running application.
	TerminalGhostty = "ghostty"
	// TerminalCommand runs a user-supplied argv template.
	TerminalCommand = "command"
	// TerminalNone never opens a window. q reports the tmux command to run
	// instead, which is what a remote or headless setup wants.
	TerminalNone = "none"
)

// Terminal configures how a debrief window is opened.
type Terminal struct {
	// Mode is one of TerminalGhostty, TerminalCommand, or TerminalNone.
	Mode string
	// Command is the argv template used when Mode is TerminalCommand.
	//
	// Two placeholders are substituted: "{dir}" becomes the working directory,
	// and "{cmd}" becomes the shell-quoted command. An element that is exactly
	// "{argv}" is replaced by the command's arguments, spliced in place.
	Command []string
}

// Paths overrides where q keeps its files. Empty values fall back to the XDG
// directories.
type Paths struct {
	// Data holds durable content: state and mission worktrees.
	Data string
	// State holds runtime ephemera: the daemon handle, the hook spool, logs.
	State string
}

// Default returns the built-in settings, before any config file or environment
// override is applied.
func Default() Settings {
	return Settings{
		Repos: Repos{
			Roots:    []string{"~/dev", "~/src", "~/code"},
			MaxDepth: 5,
			Skip:     []string{"node_modules", "vendor", "target", "Library"},
		},
		Agents: Agents{
			Default: "claude",
			Codex:   Codex{ConfigDir: "~/.codex", Profile: "q"},
		},
		Editor:   Editor{Command: DefaultEditorCommand()},
		Terminal: Terminal{Mode: DefaultTerminalMode()},
		Tools:    map[string]string{},
		LogLevel: "info",
	}
}

// DefaultTerminalMode picks the mode that works out of the box on this OS.
//
// Ghostty's AppleScript interface exists only on macOS, and there is no terminal
// emulator every Linux desktop has, so anywhere else the honest default is to
// open nothing and tell the user the tmux command.
func DefaultTerminalMode() string {
	if runtime.GOOS == "darwin" {
		return TerminalGhostty
	}

	return TerminalNone
}

// DefaultEditorCommand honors $VISUAL, then $EDITOR, and falls back to vi,
// which POSIX requires to exist.
func DefaultEditorCommand() []string {
	for _, key := range []string{"VISUAL", "EDITOR"} {
		if v := strings.TrimSpace(os.Getenv(key)); v != "" {
			return strings.Fields(v)
		}
	}

	return []string{"vi"}
}
