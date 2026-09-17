// Package agy runs missions with Google Antigravity's terminal CLI.
package agy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/justinrush/q/internal/mission"
)

// Options configures the extra arguments passed to agy.
type Options struct {
	Args         []string
	SettingsPath string
}

// Agent implements mission.Agent for agy.
type Agent struct {
	bin          string
	args         []string
	settingsPath string
}

func New(bin string, opts Options) *Agent {
	return &Agent{bin: bin, args: opts.Args, settingsPath: opts.SettingsPath}
}
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
	// Mission selections win over legacy model flags in agents.agy.args.
	if inv.Model != "" {
		args = append(args, "--model", mission.ShellQuote(inv.Model))
	}
	if inv.Effort != "" {
		args = append(args, "--effort", mission.ShellQuote(inv.Effort))
	}
	return append(args, "--prompt-interactive", mission.PromptArg)
}

type handler struct {
	Type    string `json:"type"`
	Command string `json:"command"`
	Timeout int    `json:"timeout"`
}

func (a *Agent) resolveSettingsPath() (string, error) {
	if a.settingsPath != "" {
		return a.settingsPath, nil
	}
	home, err := os.UserHomeDir()
	if err != nil {
		return "", fmt.Errorf("locating the home directory: %w", err)
	}
	return filepath.Join(home, ".gemini", "antigravity-cli", "settings.json"), nil
}

// Artifacts installs workspace-local hooks and pre-trusts mission workspaces in
// agy's settings.json so agy does not block on interactive trust at startup.
// Pre-trusting the directory is also what allows agy to load .agents/hooks.json.
func (a *Agent) Artifacts(inv mission.Invocation) ([]mission.Artifact, error) {
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
	hooksArtifact := mission.Artifact{
		Path:  filepath.Join(inv.MissionDir, ".agents", "hooks.json"),
		Data:  append(data, '\n'),
		Merge: mergeHooks,
	}

	settingsPath, err := a.resolveSettingsPath()
	if err != nil {
		return nil, err
	}

	settingsData, err := renderSettings(inv)
	if err != nil {
		return nil, fmt.Errorf("rendering agy settings: %w", err)
	}

	settingsArtifact := mission.Artifact{
		Path: settingsPath,
		Data: settingsData,
		Merge: func(generated, existing []byte) ([]byte, bool) {
			return mergeSettings(inv, generated, existing)
		},
	}

	return []mission.Artifact{hooksArtifact, settingsArtifact}, nil
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

func targetWorkspaces(inv mission.Invocation) []string {
	var list []string
	seen := map[string]bool{}
	add := func(p string) {
		if p != "" && !seen[p] {
			seen[p] = true
			list = append(list, p)
		}
	}
	if inv.MissionDir != "" {
		add(inv.MissionDir)
	}
	for _, d := range inv.MissionDirs {
		add(d)
	}
	for _, w := range inv.Worktrees {
		add(w)
	}
	slices.Sort(list)
	return list
}

func renderSettings(inv mission.Invocation) ([]byte, error) {
	doc := map[string]any{
		"trustedWorkspaces": targetWorkspaces(inv),
	}
	data, err := json.MarshalIndent(doc, "", "  ")
	if err != nil {
		return nil, err
	}
	return append(data, '\n'), nil
}

func mergeSettings(inv mission.Invocation, generated, existing []byte) ([]byte, bool) {
	if len(existing) == 0 {
		return generated, true
	}

	dec := json.NewDecoder(bytes.NewReader(existing))
	tok, err := dec.Token()
	if err != nil || tok != json.Delim('{') {
		return existing, false
	}

	var keyOrder []string
	root := map[string]json.RawMessage{}
	for dec.More() {
		tok, err := dec.Token()
		if err != nil {
			return existing, false
		}
		key, ok := tok.(string)
		if !ok {
			return existing, false
		}
		keyOrder = append(keyOrder, key)
		var raw json.RawMessage
		if err := dec.Decode(&raw); err != nil {
			return existing, false
		}
		root[key] = raw
	}

	var existingWorkspaces []string
	hasTrustedWorkspaces := false
	if raw, ok := root["trustedWorkspaces"]; ok {
		hasTrustedWorkspaces = true
		_ = json.Unmarshal(raw, &existingWorkspaces)
	}

	targets := targetWorkspaces(inv)
	liveSet := map[string]bool{}
	for _, d := range targets {
		liveSet[d] = true
	}

	isMissionPath := func(p string) bool {
		if inv.MissionDir == "" {
			return false
		}
		base := filepath.Dir(inv.MissionDir)
		if base == "" || base == "/" || base == "." {
			return false
		}
		rel, err := filepath.Rel(base, p)
		if err != nil {
			return false
		}
		return rel != "." && !strings.HasPrefix(rel, "..")
	}

	var finalWorkspaces []string
	seen := map[string]bool{}
	for _, p := range existingWorkspaces {
		if isMissionPath(p) {
			keep := false
			for live := range liveSet {
				if p == live || strings.HasPrefix(p, live+string(filepath.Separator)) {
					keep = true
					break
				}
			}
			if !keep {
				continue
			}
		}
		if !seen[p] {
			seen[p] = true
			finalWorkspaces = append(finalWorkspaces, p)
		}
	}
	for _, d := range targets {
		if !seen[d] {
			seen[d] = true
			finalWorkspaces = append(finalWorkspaces, d)
		}
	}

	if hasTrustedWorkspaces && slices.Equal(existingWorkspaces, finalWorkspaces) {
		return existing, false
	}

	rawWorkspaces, err := json.Marshal(finalWorkspaces)
	if err != nil {
		return existing, false
	}
	root["trustedWorkspaces"] = rawWorkspaces
	if !hasTrustedWorkspaces {
		keyOrder = append(keyOrder, "trustedWorkspaces")
	}

	var b bytes.Buffer
	b.WriteString("{\n")
	for i, key := range keyOrder {
		val := root[key]
		var indented bytes.Buffer
		if err := json.Indent(&indented, val, "  ", "  "); err != nil {
			return existing, false
		}
		fmt.Fprintf(&b, "  %q: %s", key, indented.String())
		if i < len(keyOrder)-1 {
			b.WriteString(",")
		}
		b.WriteString("\n")
	}
	b.WriteString("}\n")

	return b.Bytes(), true
}
