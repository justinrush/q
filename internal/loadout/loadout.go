// Package agentcli builds everything needed to invoke claude or codex for a mission:
// the composed prompt, the per-mission hook configuration, and the launch script.
//
// It runs no processes. Everything here is a pure function from mission state to
// bytes or argv, which is what lets the whole invocation be asserted in tests
// without launching an agent.
package loadout

import (
	"strings"
	"unicode"
)

// Environment variables q exports into an agent's session.
//
// The prefix is Q_ rather than the more obvious TZ_-style abbreviation of the
// binary name: TZ is the POSIX timezone variable, and a near-miss like
// TZ_MISSION_ID=ms_abc123 would corrupt time handling in every child process the
// agent spawns, git included.
const (
	// EnvMissionID tells a hook which mission it belongs to. This is the primary
	// identity channel, ahead of matching on session id or working directory.
	EnvMissionID = "Q_MISSION_ID"
	// EnvHookEpoch is the launch generation the hook was configured for, so
	// events from a session q has already abandoned can be discarded
	// instead of moving a live card.
	EnvHookEpoch = "Q_HOOK_EPOCH"
	// EnvDaemonFile is the path to the daemon handle. The path is passed rather
	// than the token itself, because `tmux show-environment` prints a session's
	// environment in plaintext.
	EnvDaemonFile = "Q_DAEMON_FILE"
	// EnvBin overrides the q binary path used in generated hook commands.
	EnvBin = "Q_BIN"
)

// Artifact file names written into a mission's .q directory.
const (
	// ArtifactDir is the subdirectory of a mission directory holding generated files.
	ArtifactDir = ".q"
	// PromptFile holds the composed prompt.
	PromptFile = "prompt.md"
	// LaunchScript starts the agent.
	LaunchScript = "launch.sh"
	// ClaudeSettingsFile holds the per-mission claude settings.
	ClaudeSettingsFile = "claude-settings.json"
	// MetaFile records what was generated, as a debugging aid.
	MetaFile = "meta.json"
)

// hookTimeoutSeconds bounds each hook invocation.
//
// Hooks must never make the agent wait: in claude a PreToolUse hook that exits
// non-zero can block the tool outright, so the bridge is built to be fast and to
// fail open.
const hookTimeoutSeconds = 5

// hookSlug converts a hook event name to the subcommand form q uses, e.g.
// "SessionStart" to "session-start".
func hookSlug(event string) string {
	var b strings.Builder

	for i, r := range event {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('-')
			}

			b.WriteRune(unicode.ToLower(r))

			continue
		}

		b.WriteRune(r)
	}

	return b.String()
}

// hookCommand renders the shell command a hook runs.
//
// The q binary is named by absolute path because hooks execute in the
// agent's environment, which for codex is a login shell and for claude is
// whatever the tmux session inherited; neither reliably has q on PATH.
func hookCommand(bin, tool, event string) string {
	return bin + " hook " + tool + " " + hookSlug(event)
}
