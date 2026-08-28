package loadout

import (
	"encoding/json"
	"fmt"
	"slices"
	"strings"
)

// claudeHooks are the events q subscribes to, with the matcher each needs.
//
// The set is chosen from what each event actually tells us:
//
//   - SessionStart confirms the agent came up and carries its session id.
//   - PermissionRequest is the immediate "blocked on the human" signal.
//     Notification's permission_prompt says the same thing six seconds later, and
//     idle_prompt only after sixty, so those are a backstop rather than the source.
//   - PostToolUse is what lets a card heal itself when the human answers a prompt
//     in the pane and never touches q, which is the common case.
//   - Stop ends a turn; StopFailure fires instead of Stop when an API error did.
//
// PermissionDenied is subscribed to only so an ExitPlanMode rejection can be
// distinguished; ordinary denials come from the user's own PreToolUse guard hooks
// and need no human attention.
var claudeHooks = []hookSubscription{
	{Event: "SessionStart"},
	{Event: "SessionEnd"},
	{Event: "UserPromptSubmit"},
	{Event: "PreToolUse", Matcher: "*"},
	{Event: "PostToolUse", Matcher: "*"},
	{Event: "PermissionRequest", Matcher: "*"},
	{Event: "PermissionDenied", Matcher: "*"},
	{Event: "Notification", Matcher: "permission_prompt|agent_needs_input|worker_permission_prompt|idle_prompt"},
	{Event: "Stop"},
	{Event: "StopFailure"},
}

// codexHooks are the events codex supports that q needs.
//
// codex has no Notification, PermissionDenied, or StopFailure event, and its
// SessionStart hook is the only way to learn its session id, since it has no
// equivalent of claude's --session-id.
//
// The per-tool hooks are the ones most likely to be dropped later: codex runs hook
// commands through a login shell, which on this machine sources nvm, so each
// invocation carries real startup cost on every tool call.
var codexHooks = []hookSubscription{
	{Event: "SessionStart"},
	{Event: "SessionEnd"},
	{Event: "UserPromptSubmit"},
	{Event: "PreToolUse", Matcher: "*"},
	{Event: "PostToolUse", Matcher: "*"},
	{Event: "PermissionRequest", Matcher: "*"},
	{Event: "Stop"},
}

// hookSubscription is one event q wants told about.
type hookSubscription struct {
	Event   string
	Matcher string
}

// claudeSettings is the per-mission settings document passed with --settings.
type claudeSettings struct {
	Permissions claudePermissions          `json:"permissions"`
	Hooks       map[string][]claudeMatcher `json:"hooks"`
}

// claudePermissions grants the agent access to each repo worktree.
//
// These directories are granted here rather than with --add-dir because that flag
// is variadic and would swallow the positional prompt that follows it, silently
// turning the mission's instructions into a list of directories.
type claudePermissions struct {
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
}

// claudeMatcher is one matcher group within a hook event.
type claudeMatcher struct {
	Matcher string        `json:"matcher,omitempty"`
	Hooks   []claudeEntry `json:"hooks"`
}

// claudeEntry is a single hook command.
type claudeEntry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// ClaudeSettings renders the per-mission settings file for claude.
//
// This is written as a file rather than passed inline so the invocation needs no
// argv quoting and so the configuration remains inspectable after the fact.
//
// It merges with the user's own settings rather than replacing them: claude merges
// settings sources by concatenating arrays, so hook lists combine and the user's
// existing PreToolUse guards keep running alongside q's.
func ClaudeSettings(qBin string, worktrees []string) ([]byte, error) {
	settings := claudeSettings{
		Permissions: claudePermissions{AdditionalDirectories: worktrees},
		Hooks:       map[string][]claudeMatcher{},
	}

	for _, sub := range claudeHooks {
		settings.Hooks[sub.Event] = []claudeMatcher{{
			Matcher: sub.Matcher,
			Hooks: []claudeEntry{{
				Type:    "command",
				Command: hookCommand(qBin, "claude", sub.Event),
				Timeout: hookTimeoutSeconds,
			}},
		}}
	}

	data, err := json.MarshalIndent(settings, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding claude settings: %w", err)
	}

	return append(data, '\n'), nil
}

// DefaultCodexProfile is the codex profile q configures when the user has not
// named another one.
const DefaultCodexProfile = "q"

// CodexProfile renders the shared codex configuration q layers with --profile.
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
// byte-stably. Codex appends its own [hooks.state] approval data afterward, and the
// launcher preserves that section when mission directories change.
//
// Only command handlers are emitted. codex 0.147.0 parses prompt, agent, and async
// handlers and then silently skips them, so writing one would look configured and do
// nothing.
func CodexProfile(qBin string, missionDirs []string) []byte {
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
	dirs := slices.Clone(missionDirs)
	slices.Sort(dirs)
	dirs = slices.Compact(dirs)

	for _, dir := range dirs {
		if dir == "" {
			continue
		}

		fmt.Fprintf(&b, "\n[projects.%q]\ntrust_level = \"trusted\"\n", dir)
	}

	b.WriteString("\n[hooks]\n")

	for _, sub := range codexHooks {
		b.WriteString(sub.Event)
		b.WriteString(" = [{ ")

		if sub.Matcher != "" {
			fmt.Fprintf(&b, "matcher = %q, ", sub.Matcher)
		}

		fmt.Fprintf(&b, "hooks = [{ type = \"command\", command = %q, timeout = %d }] }]\n",
			hookCommand(qBin, "codex", sub.Event), hookTimeoutSeconds)
	}

	return []byte(b.String())
}

// ClaudeHookEvents returns the claude events q subscribes to.
func ClaudeHookEvents() []string { return subscriptionEvents(claudeHooks) }

// CodexHookEvents returns the codex events q subscribes to.
func CodexHookEvents() []string { return subscriptionEvents(codexHooks) }

// subscriptionEvents extracts event names from a subscription list.
func subscriptionEvents(subs []hookSubscription) []string {
	out := make([]string, 0, len(subs))
	for _, sub := range subs {
		out = append(out, sub.Event)
	}

	return out
}
