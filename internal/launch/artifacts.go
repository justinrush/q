package launch

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/justinrush/q/internal/git"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/terminal"
)

// meta records what a launch generated, purely as a debugging aid: when a mission
// misbehaves, this says exactly which binary, session, and arguments produced it.
//
// Args is what the script's final exec runs. An agent whose prologue prefers a
// different invocation records the fallback here, which is the one that survives
// when the preferred path is unavailable.
type meta struct {
	MissionID   mission.MissionID `json:"missionId"`
	Tool        mission.Tool      `json:"tool"`
	HookEpoch   int               `json:"hookEpoch"`
	SessionID   string            `json:"sessionId,omitempty"`
	TmuxSession string            `json:"tmuxSession"`
	Bin         string            `json:"bin"`
	Args        []string          `json:"args"`
	GeneratedAt time.Time         `json:"generatedAt"`
}

// agentFor returns the agent configured for a mission's tool.
func (l *Launcher) agentFor(tool mission.Tool) (mission.Agent, error) {
	agent, ok := l.agents[tool]
	if !ok {
		return nil, fmt.Errorf("%w: no %s agent is configured", errNoAgentBinary, tool)
	}

	return agent, nil
}

// writeArtifacts generates the prompt, the agent's own configuration, and the
// launch script.
func (l *Launcher) writeArtifacts(operation mission.Operation, ms mission.Mission) error {
	prompt, err := mission.ComposePrompt(operation, ms)
	if err != nil {
		return err
	}

	if err := l.writePrompt(ms, prompt); err != nil {
		return err
	}

	return l.writeScript(ms, false)
}

// writePrompt writes the composed prompt into the mission's artifact directory.
func (l *Launcher) writePrompt(ms mission.Mission, prompt string) error {
	dir := filepath.Join(ms.MissionDir, mission.ArtifactDir)
	if err := os.MkdirAll(dir, paths.DirMode); err != nil {
		return fmt.Errorf("creating the artifact directory: %w", err)
	}

	path := filepath.Join(dir, mission.PromptFile)
	if err := os.WriteFile(path, []byte(prompt), paths.FileMode); err != nil {
		return fmt.Errorf("writing prompt: %w", err)
	}

	return nil
}

// writeScript renders the agent's configuration, the launch script, and the
// launch metadata.
func (l *Launcher) writeScript(ms mission.Mission, resume bool) error {
	agent, err := l.agentFor(ms.Tool)
	if err != nil {
		return err
	}

	inv, err := l.invocation(ms, resume)
	if err != nil {
		return err
	}

	artifacts, err := agent.Artifacts(inv)
	if err != nil {
		return err
	}

	for _, artifact := range artifacts {
		if err := writeArtifact(inv, artifact); err != nil {
			return err
		}
	}

	script := mission.RenderLaunchScript(agent, inv)

	path := filepath.Join(ms.MissionDir, mission.ArtifactDir, mission.LaunchScript)
	if err := os.WriteFile(path, []byte(script), paths.ExecMode); err != nil {
		return fmt.Errorf("writing the launch script: %w", err)
	}

	return l.writeMeta(ms, agent, inv)
}

// writeArtifact writes one of an agent's generated files, merging with what is
// already on disk when the agent asked for it.
func writeArtifact(inv mission.Invocation, artifact mission.Artifact) error {
	path := inv.ArtifactPath(artifact)
	data := artifact.Data

	if artifact.Merge != nil {
		existing, err := os.ReadFile(path)
		if err != nil && !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("reading %s: %w", path, err)
		}

		merged, changed := artifact.Merge(data, existing)
		if !changed {
			return nil
		}

		data = merged
	}

	mode := artifact.Mode
	if mode == 0 {
		mode = paths.FileMode
	}

	if err := os.MkdirAll(filepath.Dir(path), paths.DirMode); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, data, mode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

// writeMeta records the launch inputs alongside the script.
func (l *Launcher) writeMeta(ms mission.Mission, agent mission.Agent, inv mission.Invocation) error {
	data, err := json.MarshalIndent(meta{
		MissionID:   ms.ID,
		Tool:        ms.Tool,
		HookEpoch:   ms.HookEpoch,
		SessionID:   inv.SessionID,
		TmuxSession: ms.TmuxSession,
		Bin:         agent.Bin(),
		Args:        agent.Args(inv),
		GeneratedAt: l.now(),
	}, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding launch metadata: %w", err)
	}

	path := filepath.Join(ms.MissionDir, mission.ArtifactDir, mission.MetaFile)
	if err := os.WriteFile(path, append(data, '\n'), paths.FileMode); err != nil {
		return fmt.Errorf("writing launch metadata: %w", err)
	}

	return nil
}

// invocation assembles the inputs an agent needs to run a mission.
func (l *Launcher) invocation(ms mission.Mission, resume bool) (mission.Invocation, error) {
	qBin, err := l.selfPath()
	if err != nil {
		return mission.Invocation{}, err
	}

	return mission.Invocation{
		MissionID:   ms.ID,
		HookEpoch:   ms.HookEpoch,
		MissionDir:  ms.MissionDir,
		MissionDirs: l.liveMissionDirs(ms.MissionDir),
		DaemonFile:  l.dirs.DaemonFile(),
		SessionID:   ms.AgentSessionID,
		Resume:      resume,
		PlanMode:    ms.PlanMode,
		DisplayName: "q: " + ms.Name,
		Worktrees:   git.WorktreePaths(ms),
		PathEnv:     os.Getenv("PATH"),
		QBin:        qBin,
	}, nil
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

// selfPath returns the absolute path of the running q binary, which is
// embedded in generated hook commands.
//
// A moved or rebuilt binary silently breaks status reporting for sessions already
// running, so Q_BIN exists as an override and q doctor reports the value in
// use.
func (l *Launcher) selfPath() (string, error) {
	if override := os.Getenv(mission.EnvBin); override != "" {
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
func (l *Launcher) startSession(ctx context.Context, operation mission.Operation, ms *mission.Mission) error {
	ms.TmuxSession = mission.TmuxSessionName(operation.Slug, ms.Slug, ms.ID)

	script := filepath.Join(ms.MissionDir, mission.ArtifactDir, mission.LaunchScript)

	paneID, err := l.tmux.NewSession(ctx, terminal.NewSessionOptions{
		Name:   ms.TmuxSession,
		Window: agentWindow,
		Dir:    ms.MissionDir,
		Env: map[string]string{
			mission.EnvMissionID:  string(ms.ID),
			mission.EnvHookEpoch:  fmt.Sprint(ms.HookEpoch),
			mission.EnvDaemonFile: l.dirs.DaemonFile(),
		},
		Command: []string{script},
	})
	if err != nil {
		return err
	}

	ms.AgentPaneID = paneID

	l.applySessionOptions(ctx, ms)

	return nil
}

// applySessionOptions pins the window and pane behavior q depends on.
//
// Failures are logged rather than fatal: the agent is already running by this
// point, and losing it over a cosmetic option would be the wrong trade.
func (l *Launcher) applySessionOptions(ctx context.Context, ms *mission.Mission) {
	window := terminal.Window(ms.TmuxSession, agentWindow)

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
			l.logger.Warn("setting a tmux option", "option", name, "session", ms.TmuxSession, "error", err)
		}
	}

	// remain-on-exit keeps a finished agent's pane visible with its scrollback,
	// and crucially prevents a shell from taking over the pane. A pane that fell
	// back to a shell would execute any follow-up message typed into it.
	if err := l.tmux.SetPaneOption(ctx, terminal.Pane(ms.AgentPaneID), "remain-on-exit", "on"); err != nil {
		l.logger.Warn("setting remain-on-exit", "pane", ms.AgentPaneID, "error", err)
	}
}
