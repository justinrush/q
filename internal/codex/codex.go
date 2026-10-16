// Package codex runs missions with OpenAI's codex CLI.
//
// It is one implementation of [mission.Agent] and holds everything specific to
// that agent: how it is invoked, the shared profile it is configured through,
// its app-server client, and the runtime status q polls from it. Nothing outside
// this package branches on whether a mission's tool is codex.
package codex

import (
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/justinrush/q/internal/mission"
)

// DefaultProfile is the codex profile q configures when the user has not named
// another one.
const DefaultProfile = "q"

// hooks are the events codex supports that q needs.
//
// codex has no Notification, PermissionDenied, or StopFailure event, and its
// SessionStart hook is the only way to learn its session id, since it has no
// equivalent of claude's --session-id.
//
// The per-tool hooks are the ones most likely to be dropped later: codex runs hook
// commands through a login shell, which on this machine sources nvm, so each
// invocation carries real startup cost on every tool call.
var hooks = []subscription{
	{Event: "SessionStart"},
	{Event: "SessionEnd"},
	{Event: "UserPromptSubmit"},
	{Event: "PreToolUse", Matcher: "*"},
	{Event: "PostToolUse", Matcher: "*"},
	{Event: "PermissionRequest", Matcher: "*"},
	{Event: "Stop"},
}

// subscription is one event q wants told about.
type subscription struct {
	Event   string
	Matcher string
}

// Options configure an [Agent]. The cmd package builds them from the user's
// configuration.
type Options struct {
	// Args are the user's configured extra arguments. They are appended after
	// everything q needs and before the prompt, so a user flag cannot displace a
	// flag the board depends on.
	Args []string
	// Profile is the codex profile q writes and selects. Empty means
	// [DefaultProfile].
	Profile string
	// ConfigDir is where codex keeps its configuration. Empty means ~/.codex.
	ConfigDir string
}

// Agent runs missions with codex.
type Agent struct {
	bin       string
	args      []string
	profile   string
	configDir string
}

// New returns an agent invoking the codex binary at bin.
func New(bin string, opts Options) *Agent {
	profile := opts.Profile
	if profile == "" {
		profile = DefaultProfile
	}

	return &Agent{bin: bin, args: opts.Args, profile: profile, configDir: opts.ConfigDir}
}

// Tool reports that this is the codex agent.
func (*Agent) Tool() mission.Tool { return mission.ToolCodex }

// Bin is the absolute path to the codex executable.
func (a *Agent) Bin() string { return a.bin }

// Profile is the codex profile q writes and selects.
func (a *Agent) Profile() string { return a.profile }

// HookEvents returns the codex events q subscribes to.
func (*Agent) HookEvents() []string {
	out := make([]string, 0, len(hooks))
	for _, sub := range hooks {
		out = append(out, sub.Event)
	}

	return out
}

// Prologue starts Codex's managed app-server and makes the terminal UI its
// client. [Agent.Args] remains the fallback for an older Codex release or a
// local app-server startup failure.
//
// The daemon is started without --profile: codex rejects it on anything but
// a runtime command, and that failure silently sent every mission down the
// fallback path, where q cannot observe the session. The profile still applies
// to the terminal UI, which is the runtime command.
func (a *Agent) Prologue(inv mission.Invocation) string {
	var b strings.Builder

	b.WriteString("\nif ")
	b.WriteString(mission.ShellQuote(a.bin))
	b.WriteString(" app-server daemon start >/dev/null 2>&1; then\n")
	if inv.QBin != "" {
		b.WriteString("  ")
		b.WriteString(mission.ShellQuote(inv.QBin))
		b.WriteString(" codex-prepare ")
		b.WriteString(mission.ShellQuote(a.bin))
		b.WriteString(" ")
		b.WriteString(mission.ShellQuote(inv.MissionDir))
		b.WriteString(" || exit $?\n")
	}
	b.WriteString("  exec ")
	b.WriteString(mission.ShellQuote(a.bin))

	for _, arg := range a.remoteArgs(inv) {
		b.WriteString(" \\\n    ")
		b.WriteString(arg)
	}

	b.WriteString("\nfi\n")

	return b.String()
}

// Args is the compatibility invocation used when app-server is not available.
// It intentionally remains equivalent to the pre-integration argv.
//
// The user's approval policy and sandbox mode are deliberately left alone: no -s,
// no -a, and no bypass flag of any kind. Everything q needs to configure travels
// in the profile, which is also where directory trust lives because codex will
// not accept it on the command line.
func (a *Agent) Args(inv mission.Invocation) []string {
	return a.launchArgs(inv, outsideRoots(inv))
}

// launchArgs is a fresh launch or a resume, granting each of roots with
// --add-dir.
func (a *Agent) launchArgs(inv mission.Invocation, roots []string) []string {
	if inv.Resume && inv.SessionID != "" {
		return a.resumeArgs(inv)
	}

	args := a.commonArgs(inv)

	for _, root := range roots {
		args = append(args, "--add-dir", mission.ShellQuote(root))
	}

	return a.withPrompt(args, inv)
}

// outsideRoots are the worktrees that do not live beneath the mission
// directory.
//
// A mission's own worktrees are created inside it, and --cd already makes it
// the workspace root, so granting them again adds nothing. Only a review has
// any: it is handed the worktrees of the mission it reviews, where they are.
func outsideRoots(inv mission.Invocation) []string {
	var out []string

	for _, wt := range inv.Worktrees {
		rel, err := filepath.Rel(inv.MissionDir, wt)
		if err != nil || rel == ".." || strings.HasPrefix(rel, ".."+string(filepath.Separator)) {
			out = append(out, wt)
		}
	}

	return out
}

// withPrompt ends an argument list with the composed prompt.
//
// A planning session is given none. codex enters plan mode only through its
// /plan command, and a prompt passed here is submitted as written, slash and
// all, so the brief is delivered by [Agent.Opening] instead.
func (*Agent) withPrompt(args []string, inv mission.Invocation) []string {
	if inv.PlanMode {
		return args
	}

	return append(args, mission.PromptArg)
}

// Opening is the /plan command that starts a planning session.
//
// The brief is named rather than included. codex folds a long paste into a
// placeholder, and a command line that begins with one is no longer read as a
// command, so the whole brief would arrive as an ordinary prompt and the
// session would never enter plan mode.
func (*Agent) Opening(inv mission.Invocation) (string, bool) {
	if !inv.PlanMode {
		return "", false
	}

	brief := filepath.Join(mission.ArtifactDir, mission.PromptFile)

	return "/plan Read " + brief + " in this directory for your instructions, then plan the work.", true
}

// Ready reports whether codex is showing its composer.
//
// Before that it may be asking whether to trust its hooks or the directory,
// and text sent then would answer the question instead of reaching the
// composer.
func (*Agent) Ready(pane string) bool {
	composer := false

	for line := range strings.SplitSeq(pane, "\n") {
		line = strings.TrimSpace(line)

		if isDialogHint(line) {
			return false
		}

		if rest, ok := strings.CutPrefix(line, composerMarker); ok && !isMenuChoice(rest) {
			composer = true
		}
	}

	return composer
}

// Pending reports whether codex's composer still holds the opening, unsent.
//
// codex draws its composer well before it can take a command: the app-server
// is still starting the thread, which includes a login-shell snapshot that
// takes seconds, and nothing on screen marks the end of that. A /plan
// submitted in that window is refused as "disabled while a task is in
// progress" and left in the composer, and an Enter that arrives on the heels of
// a paste is taken as part of the paste and submits nothing. Either way the
// text is still there, and pressing Enter again once codex is listening sends
// it.
//
// A long opening wraps, so the composer line need only begin the opening.
func (*Agent) Pending(pane, text string) bool {
	held := ""

	for line := range strings.SplitSeq(pane, "\n") {
		if rest, ok := strings.CutPrefix(strings.TrimSpace(line), composerMarker); ok && !isMenuChoice(rest) {
			held = strings.TrimSpace(rest)
		}
	}

	return held != "" && strings.HasPrefix(text, held)
}

// isDialogHint reports whether a line is the key hint codex draws under a
// question, e.g. "enter confirm · esc skip" or "enter continue · esc quit".
func isDialogHint(line string) bool {
	return strings.HasPrefix(line, "enter ") && strings.Contains(line, "esc ")
}

// composerMarker begins codex's input line. It also begins the highlighted
// choice of a menu, which is what [isMenuChoice] tells apart.
const composerMarker = "›"

// isMenuChoice reports whether the text after a marker is a numbered option,
// e.g. "1. Review hooks".
func isMenuChoice(rest string) bool {
	rest = strings.TrimSpace(rest)
	digits := strings.TrimLeft(rest, "0123456789")

	return len(digits) < len(rest) && strings.HasPrefix(digits, ".")
}

// remoteArgs points codex at the managed app-server.
//
// No --add-dir is passed: codex refuses it alongside --remote, and exits
// before drawing anything, which left every multi-repo mission a dead pane.
// The mission's own worktrees are already inside the workspace root, and a
// review only reads the worktrees it is given, which the sandbox allows
// anywhere.
func (a *Agent) remoteArgs(inv mission.Invocation) []string {
	args := a.launchArgs(inv, nil)

	// The empty Unix URL selects Codex's managed app-server control socket.
	// Keeping the transport local avoids a TCP listener and any credential.
	insertAt := 0
	if inv.Resume && inv.SessionID != "" {
		insertAt = 1
	}

	remote := []string{"--remote", "unix://"}
	args = append(args, remote...)
	copy(args[insertAt+len(remote):], args[insertAt:len(args)-len(remote)])
	copy(args[insertAt:], remote)

	return args
}

// commonArgs are the flags shared by a fresh launch and a resume.
//
// The mission directory is written out in full rather than as "$d/..": codex matches
// project trust on the exact path it is given, so an unresolved path containing ".."
// would not match the trust entry written for it.
//
// Trust itself is not passed here. codex ignores it as a -c override and reads it from
// a config file only, so it lives in the profile instead.
func (a *Agent) commonArgs(inv mission.Invocation) []string {
	args := []string{
		"--cd", mission.ShellQuote(inv.MissionDir),
		"--profile", mission.ShellQuote(a.profile),
	}

	// The model is a flag while the effort is a -c override, because codex exposes
	// the first on the command line and keeps the second in configuration only.
	// Both are layered over the profile, so a mission that names neither still
	// gets whatever the profile and config.toml resolve to.
	if inv.Model != "" {
		args = append(args, "--model", mission.ShellQuote(inv.Model))
	}

	if inv.Effort != "" {
		args = append(args, "-c", mission.ShellQuote(keyEffort+"="+inv.Effort))
	}

	return append(args, mission.ShellQuoteAll(a.args)...)
}

// resumeArgs resumes a codex session by id.
//
// codex has no --session-id, so the id here was learned from the session's own
// SessionStart hook. When it is unknown, callers fall back to `codex resume --last`
// run from the mission directory, which works only because codex filters resume
// candidates by working directory and mission directories are unique per mission.
func (a *Agent) resumeArgs(inv mission.Invocation) []string {
	// The subcommand leads; its flags follow it.
	args := append([]string{"resume"}, a.commonArgs(inv)...)
	args = append(args, mission.ShellQuote(inv.SessionID))

	return a.withPrompt(args, inv)
}

// Artifacts refreshes the shared codex profile.
//
// It is shared rather than per-mission because codex keys hook trust on the file
// and a hash of each handler, so a per-mission hooks section would require
// re-approving hooks on every launch. Codex stores those approvals in a
// [hooks.state] section in this file, which [Artifact.Merge] preserves.
//
// The profile also pre-trusts every live mission directory, which is what stops
// codex stopping to ask about them in a detached session where nobody would see
// the question. Directories that no longer exist are dropped, so the file does
// not grow forever.
func (a *Agent) Artifacts(inv mission.Invocation) ([]mission.Artifact, error) {
	dir := a.configDir
	if dir == "" {
		home, err := os.UserHomeDir()
		if err != nil {
			return nil, fmt.Errorf("locating the home directory: %w", err)
		}

		dir = filepath.Join(home, ".codex")
	}

	return []mission.Artifact{{
		Path:  filepath.Join(dir, a.profile+".config.toml"),
		Data:  a.renderProfile(inv),
		Merge: mergeProfile,
	}}, nil
}

// renderProfile writes the shared codex configuration q layers with --profile.
//
// It carries two things codex will not accept any other way.
//
// Project trust, one entry per mission directory. codex refuses to start in an untrusted
// directory and instead asks interactively, which in a detached tmux session blocks
// forever with nothing on screen. Passing the same setting with -c does not work:
// codex reads project trust from a config file only. Trust is also matched on the
// exact path with no inheritance, so a parent entry would not cover the mission
// directories beneath it.
//
// The hooks that report status back to q. These still require a one-time
// interactive approval, which is why the generated [hooks] section is emitted last and
// byte-stably.
//
// Only command handlers are emitted. codex 0.147.0 parses prompt, agent, and async
// handlers and then silently skips them, so writing one would look configured and do
// nothing.
func (a *Agent) renderProfile(inv mission.Invocation) []byte {
	var b strings.Builder

	b.WriteString("# Generated by q. Do not edit.\n")
	b.WriteString("#\n")
	b.WriteString("# The project entries pre-trust q mission directories, so codex does not\n")
	b.WriteString("# stop to ask about them in a detached session where nobody would see it.\n")
	b.WriteString("#\n")
	b.WriteString("# The [hooks] section reports agent status back to q. It is kept last\n")
	b.WriteString("# and byte-stable because codex keys hook trust on a hash of each handler:\n")
	b.WriteString("# approve it once and it holds, even as mission directories change above.\n")

	// Sorted so the file is identical for the same set of directories, rather than
	// reordering with map iteration and looking like a change.
	dirs := slices.Clone(inv.MissionDirs)
	slices.Sort(dirs)
	dirs = slices.Compact(dirs)

	for _, dir := range dirs {
		if dir == "" {
			continue
		}

		fmt.Fprintf(&b, "\n[projects.%q]\ntrust_level = \"trusted\"\n", dir)
	}

	b.WriteString("\n[hooks]\n")

	for _, sub := range hooks {
		b.WriteString(sub.Event)
		b.WriteString(" = [{ ")

		if sub.Matcher != "" {
			fmt.Fprintf(&b, "matcher = %q, ", sub.Matcher)
		}

		fmt.Fprintf(&b, "hooks = [{ type = \"command\", command = %q, timeout = %d }] }]\n",
			mission.HookCommand(inv.QBin, mission.ToolCodex, sub.Event), mission.HookTimeoutSeconds)
	}

	return []byte(b.String())
}
