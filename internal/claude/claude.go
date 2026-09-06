// Package claude runs missions with Anthropic's claude CLI.
//
// It is one implementation of [mission.Agent] and holds everything specific to
// that agent: how it is invoked, the per-mission settings document it is given,
// and the events it reports back. Nothing outside this package branches on
// whether a mission's tool is claude.
//
// # Safety
//
// [Registry] reads claude's live session directory, which also contains
// per-session key files holding messaging credentials. It reads only *.json and
// must never be widened to read anything else. A test feeds it a decoy key file
// to prove it stays closed.
package claude

import (
	"encoding/json"
	"fmt"

	"github.com/justinrush/q/internal/mission"
)

// SettingsFile is the per-mission settings document passed with --settings.
const SettingsFile = "claude-settings.json"

// EnvSandboxed tells claude its working directory is already vouched for.
const EnvSandboxed = "CLAUDE_CODE_SANDBOXED"

// hooks are the events q subscribes to, with the matcher each needs.
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
var hooks = []subscription{
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
}

// Agent runs missions with claude.
type Agent struct {
	bin  string
	args []string
}

// New returns an agent invoking the claude binary at bin.
func New(bin string, opts Options) *Agent {
	return &Agent{bin: bin, args: opts.Args}
}

// Tool reports that this is the claude agent.
func (*Agent) Tool() mission.Tool { return mission.ToolClaude }

// Bin is the absolute path to the claude executable.
func (a *Agent) Bin() string { return a.bin }

// HookEvents returns the claude events q subscribes to.
func (*Agent) HookEvents() []string {
	out := make([]string, 0, len(hooks))
	for _, sub := range hooks {
		out = append(out, sub.Event)
	}

	return out
}

// Prologue exports the one variable that answers claude's workspace trust gate.
//
// Every mission runs in a directory that has never existed before, so claude's
// per-project trust record never matches it and the launch stops on "Accessing
// workspace" inside a detached tmux pane where nobody is looking. Setting this is
// what q is asserting anyway: the directory holds worktrees of the user's own
// repositories, provisioned moments earlier by q itself.
//
// It is an environment variable rather than a pre-seeded trust record because
// claude keeps trust in ~/.claude.json, which is its own live global config,
// rewritten by every running session. codex's trust lives in a file q owns
// outright, so that agent is served by a generated profile instead.
//
// It grants trust and nothing else. Permission prompts still apply, and because
// the variable is read per launch, no trust is left recorded on disk afterwards.
func (*Agent) Prologue(mission.Invocation) string {
	return "export " + EnvSandboxed + "=1\n"
}

// Args builds claude's arguments.
//
// No variadic flag is ever emitted. claude's --add-dir accepts multiple values
// and would consume the positional prompt that follows it, so worktree access is
// granted through the settings file instead.
func (a *Agent) Args(inv mission.Invocation) []string {
	var args []string

	switch {
	case inv.Resume && inv.SessionID != "":
		args = append(args, "--resume", mission.ShellQuote(inv.SessionID))
	case inv.SessionID != "":
		// A session id chosen up front makes the session resumable from the
		// instant it starts, with no need for the agent to report it back.
		args = append(args, "--session-id", mission.ShellQuote(inv.SessionID))
	}

	if inv.DisplayName != "" {
		args = append(args, "--name", mission.ShellQuote(inv.DisplayName))
	}

	mode := "auto"
	if inv.PlanMode {
		mode = "plan"
	}

	args = append(args, "--permission-mode", mode)

	// Both are omitted entirely when unset, so a mission that names no model runs
	// on whatever claude's own configuration says, exactly as it did before
	// missions could carry one.
	if inv.Model != "" {
		args = append(args, "--model", mission.ShellQuote(inv.Model))
	}

	if inv.Effort != "" {
		args = append(args, "--effort", mission.ShellQuote(inv.Effort))
	}

	args = append(args, "--settings", mission.ArtifactArg(SettingsFile))
	args = append(args, mission.ShellQuoteAll(a.args)...)

	return append(args, mission.PromptArg)
}

// settings is the per-mission settings document passed with --settings.
type settings struct {
	Permissions permissions          `json:"permissions"`
	Hooks       map[string][]matcher `json:"hooks"`
}

// permissions grants the agent access to each repo worktree.
//
// These directories are granted here rather than with --add-dir because that flag
// is variadic and would swallow the positional prompt that follows it, silently
// turning the mission's instructions into a list of directories.
type permissions struct {
	AdditionalDirectories []string `json:"additionalDirectories,omitempty"`
}

// matcher is one matcher group within a hook event.
type matcher struct {
	Matcher string  `json:"matcher,omitempty"`
	Hooks   []entry `json:"hooks"`
}

// entry is a single hook command.
type entry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout,omitempty"`
}

// Artifacts renders the per-mission settings file.
//
// It is written as a file rather than passed inline so the invocation needs no
// argv quoting and so the configuration remains inspectable after the fact.
//
// It merges with the user's own settings rather than replacing them: claude merges
// settings sources by concatenating arrays, so hook lists combine and the user's
// existing PreToolUse guards keep running alongside q's.
func (*Agent) Artifacts(inv mission.Invocation) ([]mission.Artifact, error) {
	doc := settings{
		Permissions: permissions{AdditionalDirectories: inv.Worktrees},
		Hooks:       map[string][]matcher{},
	}

	for _, sub := range hooks {
		doc.Hooks[sub.Event] = []matcher{{
			Matcher: sub.Matcher,
			Hooks: []entry{{
				Type:    "command",
				Command: mission.HookCommand(inv.QBin, mission.ToolClaude, sub.Event),
				Timeout: mission.HookTimeoutSeconds,
			}},
		}}
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding claude settings: %w", err)
	}

	return []mission.Artifact{{Path: SettingsFile, Data: append(data, '\n')}}, nil
}
