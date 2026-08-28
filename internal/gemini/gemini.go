// Package gemini runs missions with Google's gemini CLI.
//
// It is one implementation of [mission.Agent] and holds everything specific to
// that agent: how it is invoked, the per-mission workspace settings document it
// is configured through, and the events it reports back. Nothing outside this
// package branches on whether a mission's tool is gemini.
//
// # Event names
//
// gemini is the first agent whose hook events are not named the way claude and
// codex name theirs. The translation lives here and nowhere else: each
// [subscription] pairs gemini's name, which goes in the settings file, with the
// canonical name q understands, which goes in the hook command. By the time an
// event reaches the daemon it is indistinguishable from claude's.
package gemini

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/justinrush/q/internal/mission"
)

// SettingsDir is the directory, relative to the mission directory, that gemini
// reads workspace settings from.
const SettingsDir = ".gemini"

// SettingsFile is the workspace settings document q writes for a mission.
//
// gemini has no --settings flag, so unlike claude this is placed where the agent
// will find it rather than named on the command line. Workspace scope is the
// right one: it is per-mission, it disappears with the mission directory, and it
// never touches the user's own ~/.gemini.
const SettingsFile = "settings.json"

// TrustEnvVar makes gemini treat its working directory as trusted.
//
// Folder trust defaults to on in gemini 0.40.1, and an untrusted directory is
// doubly fatal here. Interactively gemini stops to ask, which in a detached tmux
// session blocks forever with nobody watching; and an untrusted workspace has
// its .gemini/settings.json discarded outright, which would silently drop the
// hooks the board depends on. Every mission directory is freshly created by q,
// so none is ever in the user's trust file.
const TrustEnvVar = "GEMINI_CLI_TRUST_WORKSPACE"

// Approval modes gemini accepts for --approval-mode.
const (
	// approvalAutoEdit auto-approves edits and still asks about everything else,
	// which is the closest match to the mode claude missions run in.
	approvalAutoEdit = "auto_edit"
	// approvalPlan is the read-only mode a plan mission starts in.
	approvalPlan = "plan"
)

// hooks are the events q subscribes to, paired with the canonical event each
// stands for.
//
// The set is chosen from what each event actually tells us:
//
//   - SessionStart confirms the agent came up and carries its session id, which
//     for gemini is the only way q ever learns it.
//   - Notification is the blocked-on-the-human signal. gemini has no separate
//     permission event, and its one notification type is ToolPermission, so this
//     is reported as PermissionRequest rather than as a backstop the way
//     claude's same-named event is.
//   - AfterTool is what lets a card heal itself when the human answers a prompt
//     in the pane and never touches q, which is the common case.
//   - AfterAgent ends a turn. gemini has no equivalent of claude's StopFailure.
//
// BeforeModel, AfterModel, BeforeToolSelection, and PreCompress are deliberately
// not subscribed to. The model hooks fire on every streamed chunk, which would
// mean thousands of processes per turn for a signal the board never reads.
var hooks = []subscription{
	{Event: "SessionStart", Canonical: mission.EventSessionStart},
	{Event: "SessionEnd", Canonical: mission.EventSessionEnd},
	{Event: "BeforeAgent", Canonical: mission.EventUserPromptSubmit},
	{Event: "BeforeTool", Canonical: mission.EventPreToolUse, Matcher: ".*"},
	{Event: "AfterTool", Canonical: mission.EventPostToolUse, Matcher: ".*"},
	{Event: "Notification", Canonical: mission.EventPermissionRequest},
	{Event: "AfterAgent", Canonical: mission.EventStop},
}

// subscription is one event q wants told about.
type subscription struct {
	// Event is gemini's name for it, which is the settings file's key.
	Event string
	// Canonical is q's name for the same thing, which is what the hook command
	// reports and what the state machine switches on.
	Canonical string
	// Matcher filters which tools fire a tool event. gemini matches it as a
	// regular expression, so the catch-all is ".*" rather than claude's "*".
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

// Agent runs missions with gemini.
type Agent struct {
	bin  string
	args []string
}

// New returns an agent invoking the gemini binary at bin.
func New(bin string, opts Options) *Agent {
	return &Agent{bin: bin, args: opts.Args}
}

// Tool reports that this is the gemini agent.
func (*Agent) Tool() mission.Tool { return mission.ToolGemini }

// Bin is the absolute path to the gemini executable.
func (a *Agent) Bin() string { return a.bin }

// HookEvents returns the canonical events q subscribes to, not gemini's names
// for them, so a caller comparing agents is comparing like with like.
func (*Agent) HookEvents() []string {
	out := make([]string, 0, len(hooks))
	for _, sub := range hooks {
		out = append(out, sub.Canonical)
	}

	return out
}

// Prologue trusts the mission directory.
//
// This is exported into the environment rather than passed as --skip-trust
// because gemini applies that flag only after argv is parsed, by which point its
// settings may already have been loaded and cached as untrusted. The environment
// variable is read at the trust check itself and so cannot lose that race.
func (*Agent) Prologue(mission.Invocation) string {
	return fmt.Sprintf("\nexport %s=true\n", TrustEnvVar)
}

// Args builds gemini's arguments.
//
// No variadic flag is ever emitted. gemini's --include-directories is a greedy
// list flag and would consume the positional prompt that follows it, so worktree
// access is granted through the settings file instead — the same hazard claude's
// --add-dir poses, and the same answer.
//
// There is no display-name flag to pass: gemini has no equivalent of claude's
// --name, so a mission is identified in its own UI only by its working directory.
func (a *Agent) Args(inv mission.Invocation) []string {
	var args []string

	// gemini has no --session-id, so the id here was learned from the session's
	// own SessionStart hook. --resume also accepts "latest", but q does not use
	// it: gemini scopes sessions by working directory and mission directories are
	// unique, so "latest" would usually be right and silently wrong the one time
	// a second session had been started by hand.
	if inv.Resume && inv.SessionID != "" {
		args = append(args, "--resume", mission.ShellQuote(inv.SessionID))
	}

	mode := approvalAutoEdit
	if inv.PlanMode {
		mode = approvalPlan
	}

	args = append(args, "--approval-mode", mode)
	args = append(args, mission.ShellQuoteAll(a.args)...)

	return append(args, mission.PromptArg)
}

// settings is the workspace settings document gemini reads from the mission
// directory.
type settings struct {
	Context contextSettings `json:"context"`
	Hooks   hookSpec        `json:"hooks"`
}

// contextSettings grants the agent access to each repo worktree.
//
// These directories are granted here rather than with --include-directories
// because that flag is variadic and would swallow the positional prompt that
// follows it, silently turning the mission's instructions into a list of
// directories.
type contextSettings struct {
	IncludeDirectories []string `json:"includeDirectories,omitempty"`
}

// hookSpec is the hooks section, keyed by gemini's own event names.
type hookSpec map[string][]matcher

// matcher is one matcher group within a hook event.
type matcher struct {
	Matcher string  `json:"matcher,omitempty"`
	Hooks   []entry `json:"hooks"`
}

// entry is a single hook command.
type entry struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	// Timeout is milliseconds, unlike claude's and codex's seconds. Sending
	// seconds here would give every hook a five-millisecond budget and fail all
	// of them.
	Timeout int `json:"timeout,omitempty"`
}

// hookTimeoutMillis is q's hook budget in the unit gemini expects.
const hookTimeoutMillis = mission.HookTimeoutSeconds * 1000

// Artifacts renders the per-mission workspace settings file.
//
// It merges with the user's own settings rather than replacing them: gemini
// declares every hook event's merge strategy as concat, so hook lists combine
// across scopes and the user's existing ~/.gemini hooks keep running alongside
// q's.
//
// The path is absolute rather than relative because it must land in the
// mission's .gemini directory, which is where gemini looks, rather than in q's
// own .q artifact directory.
func (*Agent) Artifacts(inv mission.Invocation) ([]mission.Artifact, error) {
	doc := settings{
		Context: contextSettings{IncludeDirectories: inv.Worktrees},
		Hooks:   hookSpec{},
	}

	for _, sub := range hooks {
		doc.Hooks[sub.Event] = []matcher{{
			Matcher: sub.Matcher,
			Hooks: []entry{{
				Type:    "command",
				Command: mission.HookCommand(inv.QBin, mission.ToolGemini, sub.Canonical),
				Timeout: hookTimeoutMillis,
			}},
		}}
	}

	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding gemini settings: %w", err)
	}

	return []mission.Artifact{{
		Path: filepath.Join(inv.MissionDir, SettingsDir, SettingsFile),
		Data: append(data, '\n'),
	}}, nil
}
