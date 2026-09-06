// Package agy runs missions with Google Antigravity's terminal CLI.
package agy

import (
	"encoding/json"
	"fmt"
	"path/filepath"

	"github.com/justinrush/q/internal/mission"
)

// Options configures the extra arguments passed to agy.
type Options struct{ Args []string }

// Agent implements mission.Agent for agy.
type Agent struct {
	bin  string
	args []string
}

func New(bin string, opts Options) *Agent         { return &Agent{bin: bin, args: opts.Args} }
func (*Agent) Tool() mission.Tool                 { return mission.ToolAgy }
func (a *Agent) Bin() string                      { return a.bin }
func (*Agent) Prologue(mission.Invocation) string { return "" }

// PreInvocation supplies the conversation ID and confirms that work has started.
// Agy has no session lifecycle or permission-wait hooks.
func (*Agent) HookEvents() []string {
	return []string{"PreInvocation", "PreToolUse", "PostToolUse", "Stop"}
}

// Args uses the interactive prompt flag; a positional prompt is not supported.
// Never resume the globally most recent conversation when the ID is unknown:
// another mission may have run since this one.
func (a *Agent) Args(inv mission.Invocation) []string {
	var args []string
	if inv.Resume && inv.SessionID != "" {
		args = append(args, "--conversation", mission.ShellQuote(inv.SessionID))
	}
	for _, path := range inv.Worktrees {
		args = append(args, "--add-dir", mission.ShellQuote(path))
	}
	args = append(args, mission.ShellQuoteAll(a.args)...)
	return append(args, "--prompt-interactive", mission.PromptArg)
}

type handler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

// Artifacts installs workspace-local hooks, leaving global customizations alone.
// Schema: https://antigravity.google/docs/hooks/. Non-tool events use direct
// handlers; tool events require matcher groups. Explicit routing also works if
// agy's backend does not inherit the terminal's launch environment.
func (*Agent) Artifacts(inv mission.Invocation) ([]mission.Artifact, error) {
	events := map[string]any{}
	for _, event := range []string{"PreInvocation", "PreToolUse", "PostToolUse", "Stop"} {
		target := event
		if event == "PreInvocation" {
			target = mission.EventSessionStart
		}
		command := fmt.Sprintf("%s=%s %s=%s %s=%s %s hook agy %s",
			mission.EnvMissionID, mission.ShellQuote(string(inv.MissionID)),
			mission.EnvHookEpoch, mission.ShellQuote(fmt.Sprint(inv.HookEpoch)),
			mission.EnvDaemonFile, mission.ShellQuote(inv.DaemonFile),
			mission.ShellQuote(inv.QBin), mission.HookSlug(target))
		h := []handler{{Type: "command", Command: command, Timeout: mission.HookTimeoutSeconds}}
		if event == "PreToolUse" || event == "PostToolUse" {
			events[event] = []any{map[string]any{"matcher": "*", "hooks": h}}
		} else {
			events[event] = h
		}
	}
	data, err := json.MarshalIndent(map[string]any{"q-mission": events}, "", "  ")
	if err != nil {
		return nil, fmt.Errorf("encoding agy hooks: %w", err)
	}
	return []mission.Artifact{{Path: filepath.Join(inv.MissionDir, ".agents", "hooks.json"), Data: append(data, '\n'), Merge: mergeHooks}}, nil
}

// Preserve custom hooks added to the mission directory between launches. An
// invalid document is left untouched rather than destroying the user's edits.
func mergeHooks(generated, existing []byte) ([]byte, bool) {
	if len(existing) == 0 {
		return generated, true
	}
	var old, fresh map[string]json.RawMessage
	if json.Unmarshal(existing, &old) != nil || old == nil {
		return existing, false
	}
	if json.Unmarshal(generated, &fresh) != nil {
		return existing, false
	}
	old["q-mission"] = fresh["q-mission"]
	data, err := json.MarshalIndent(old, "", "  ")
	if err != nil {
		return existing, false
	}
	return append(data, '\n'), true
}
