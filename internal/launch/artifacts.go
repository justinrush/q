package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/gadgets"
	"github.com/justinrush/q/internal/loadout"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/tmuxc"
)

// meta records what a launch generated, purely as a debugging aid: when a mission
// misbehaves, this says exactly which binary, session, and arguments produced it.
type meta struct {
	MissionID   domain.MissionID `json:"missionId"`
	Tool        domain.Tool      `json:"tool"`
	HookEpoch   int              `json:"hookEpoch"`
	SessionID   string           `json:"sessionId,omitempty"`
	TmuxSession string           `json:"tmuxSession"`
	Bin         string           `json:"bin"`
	Args        []string         `json:"args"`
	GeneratedAt time.Time        `json:"generatedAt"`
}

// writeArtifacts generates the prompt, hook configuration, and launch script.
func (l *Launcher) writeArtifacts(operation domain.Operation, mission domain.Mission) error {
	prompt, err := loadout.ComposePrompt(operation, mission)
	if err != nil {
		return err
	}

	dir := filepath.Join(mission.MissionDir, loadout.ArtifactDir)

	if err := os.WriteFile(filepath.Join(dir, loadout.PromptFile), []byte(prompt), paths.FileMode); err != nil {
		return fmt.Errorf("writing prompt: %w", err)
	}

	qBin, err := l.selfPath()
	if err != nil {
		return err
	}

	switch mission.Tool {
	case domain.ToolClaude:
		settings, err := loadout.ClaudeSettings(qBin, worktreePaths(mission))
		if err != nil {
			return err
		}

		path := filepath.Join(dir, loadout.ClaudeSettingsFile)
		if err := os.WriteFile(path, settings, paths.FileMode); err != nil {
			return fmt.Errorf("writing claude settings: %w", err)
		}
	case domain.ToolCodex:
		if err := l.writeCodexProfile(qBin, mission.MissionDir); err != nil {
			return err
		}
	}

	return l.writeLaunchScript(mission)
}

// writeCodexProfile refreshes the shared codex profile.
//
// It is shared rather than per-mission because codex keys hook trust on the file and a
// hash of each handler, so a per-mission hooks section would require re-approving hooks on
// every launch. Codex stores those approvals in a [hooks.state] section in this file;
// refreshing q's generated entries preserves that Codex-owned section.
//
// The profile also pre-trusts every live mission directory, which is what stops codex
// stopping to ask about them in a detached session where nobody would see the question.
// Directories that no longer exist are dropped, so the file does not grow forever.
func (l *Launcher) writeCodexProfile(qBin, currentMissionDir string) error {
	home, err := os.UserHomeDir()
	if err != nil {
		return fmt.Errorf("locating the home directory: %w", err)
	}

	dir := l.agents.Codex.ConfigDir
	if dir == "" {
		dir = filepath.Join(home, ".codex")
	}

	path := filepath.Join(dir, l.codexProfile()+".config.toml")
	want := loadout.CodexProfile(qBin, l.liveMissionDirs(currentMissionDir))

	existing, err := os.ReadFile(path)
	if err == nil {
		want = preserveCodexHookState(want, existing)
		if string(existing) == string(want) {
			return nil
		}
	}

	if err := os.MkdirAll(filepath.Dir(path), paths.DirMode); err != nil {
		return fmt.Errorf("creating the codex config directory: %w", err)
	}

	if err := os.WriteFile(path, want, paths.FileMode); err != nil {
		return fmt.Errorf("writing the codex profile: %w", err)
	}

	return nil
}

// liveMissionDirs lists the mission directories that currently exist, plus the one being
// launched, which may not be on disk yet when this runs.
func (l *Launcher) liveMissionDirs(currentMissionDir string) []string {
	dirs := []string{}
	if currentMissionDir != "" {
		dirs = append(dirs, currentMissionDir)
	}

	entries, err := os.ReadDir(l.dirs.MissionsDir())
	if err != nil {
		// A missing missions directory just means there is nothing else to trust.
		return dirs
	}

	for _, entry := range entries {
		if entry.IsDir() {
			dirs = append(dirs, filepath.Join(l.dirs.MissionsDir(), entry.Name()))
		}
	}

	return dirs
}

// writeLaunchScript renders and writes the mission's launch script.
func (l *Launcher) writeLaunchScript(mission domain.Mission) error {
	spec, err := l.launchSpec(mission)
	if err != nil {
		return err
	}

	script, err := loadout.RenderLaunchScript(spec)
	if err != nil {
		return err
	}

	path := filepath.Join(mission.MissionDir, loadout.ArtifactDir, loadout.LaunchScript)
	if err := os.WriteFile(path, []byte(script), paths.ExecMode); err != nil {
		return fmt.Errorf("writing the launch script: %w", err)
	}

	args, err := loadout.AgentArgs(spec)
	if err != nil {
		return err
	}

	return l.writeMeta(mission, spec, args)
}

// writeMeta records the launch inputs alongside the script.
func (l *Launcher) writeMeta(mission domain.Mission, spec loadout.LaunchSpec, args []string) error {
	data, err := json.MarshalIndent(meta{
		MissionID:   mission.ID,
		Tool:        mission.Tool,
		HookEpoch:   mission.HookEpoch,
		SessionID:   spec.SessionID,
		TmuxSession: mission.TmuxSession,
		Bin:         spec.Bin,
		Args:        args,
		GeneratedAt: l.now(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding launch metadata: %w", err)
	}

	path := filepath.Join(mission.MissionDir, loadout.ArtifactDir, loadout.MetaFile)
	if err := os.WriteFile(path, append(data, '\n'), paths.FileMode); err != nil {
		return fmt.Errorf("writing launch metadata: %w", err)
	}

	return nil
}

// launchSpec assembles the inputs for the launch script.
func (l *Launcher) launchSpec(mission domain.Mission) (loadout.LaunchSpec, error) {
	agentBin, err := l.agentBin(mission.Tool)
	if err != nil {
		return loadout.LaunchSpec{}, err
	}

	return loadout.LaunchSpec{
		Tool:         mission.Tool,
		ExtraArgs:    l.agentArgs(mission.Tool),
		CodexProfile: l.codexProfile(),
		Bin:          agentBin,
		MissionDir:   mission.MissionDir,
		MissionID:    mission.ID,
		HookEpoch:    mission.HookEpoch,
		DaemonFile:   l.dirs.DaemonFile(),
		SessionID:    mission.AgentSessionID,
		PlanMode:     mission.PlanMode,
		DisplayName:  "q: " + mission.Name,
		Worktrees:    worktreePaths(mission),
		PathEnv:      os.Getenv("PATH"),
	}, nil
}

// codexProfile is the codex profile name q writes and selects.
func (l *Launcher) codexProfile() string {
	if l.agents.Codex.Profile != "" {
		return l.agents.Codex.Profile
	}

	return loadout.DefaultCodexProfile
}

// agentArgs returns the user's configured extra arguments for a tool.
func (l *Launcher) agentArgs(tool domain.Tool) []string {
	if tool == domain.ToolCodex {
		return l.agents.Codex.Args
	}

	return l.agents.Claude.Args
}

// agentBin resolves the binary for a tool.
func (l *Launcher) agentBin(tool domain.Tool) (string, error) {
	var which gadgets.Tool

	switch tool {
	case domain.ToolClaude:
		which = gadgets.Claude
	case domain.ToolCodex:
		which = gadgets.Codex
	default:
		return "", fmt.Errorf("unknown tool %q", tool)
	}

	bin, err := l.bins.Path(which)
	if err != nil {
		return "", fmt.Errorf("%w: %w", errNoAgentBinary, err)
	}

	return bin, nil
}

// selfPath returns the absolute path of the running q binary, which is
// embedded in generated hook commands.
//
// A moved or rebuilt binary silently breaks status reporting for sessions already
// running, so Q_BIN exists as an override and q doctor reports the value in
// use.
func (l *Launcher) selfPath() (string, error) {
	if override := os.Getenv(loadout.EnvBin); override != "" {
		return override, nil
	}

	self, err := os.Executable()
	if err != nil {
		return "", fmt.Errorf("locating the q binary: %w", err)
	}

	return self, nil
}

// startSession creates the detached tmux session that hosts the agent.
//
// It does not check whether the session already exists. Callers own that decision:
// an initial launch refuses to adopt an existing session, while a relaunch has just
// killed the stale one on purpose.
func (l *Launcher) startSession(ctx context.Context, operation domain.Operation, mission *domain.Mission) error {
	mission.TmuxSession = domain.TmuxSessionName(operation.Slug, mission.Slug, mission.ID)

	script := filepath.Join(mission.MissionDir, loadout.ArtifactDir, loadout.LaunchScript)

	paneID, err := l.tmux.NewSession(ctx, tmuxc.NewSessionOptions{
		Name:   mission.TmuxSession,
		Window: agentWindow,
		Dir:    mission.MissionDir,
		Env: map[string]string{
			loadout.EnvMissionID:  string(mission.ID),
			loadout.EnvHookEpoch:  fmt.Sprint(mission.HookEpoch),
			loadout.EnvDaemonFile: l.dirs.DaemonFile(),
		},
		Command: []string{script},
	})
	if err != nil {
		return err
	}

	mission.AgentPaneID = paneID

	l.applySessionOptions(ctx, mission)

	return nil
}

// applySessionOptions pins the window and pane behavior q depends on.
//
// Failures are logged rather than fatal: the agent is already running by this
// point, and losing it over a cosmetic option would be the wrong trade.
func (l *Launcher) applySessionOptions(ctx context.Context, mission *domain.Mission) {
	window := tmuxc.Window(mission.TmuxSession, agentWindow)

	// The user's config enables automatic-rename and set-titles globally, which
	// would rewrite the window name q uses to find things.
	for name, value := range map[string]string{
		"automatic-rename": "off",
		"allow-rename":     "off",
		// Without this, a second attached client shrinks the window to the
		// smallest client's size.
		"window-size": "latest",
	} {
		if err := l.tmux.SetWindowOption(ctx, window, name, value); err != nil {
			l.logger.Warn("setting a tmux option", "option", name, "session", mission.TmuxSession, "error", err)
		}
	}

	// remain-on-exit keeps a finished agent's pane visible with its scrollback,
	// and crucially prevents a shell from taking over the pane. A pane that fell
	// back to a shell would execute any follow-up message typed into it.
	if err := l.tmux.SetPaneOption(ctx, tmuxc.Pane(mission.AgentPaneID), "remain-on-exit", "on"); err != nil {
		l.logger.Warn("setting remain-on-exit", "pane", mission.AgentPaneID, "error", err)
	}
}
