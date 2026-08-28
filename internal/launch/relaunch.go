package launch

import (
	"context"
	"fmt"
	"os"
	"path/filepath"

	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/loadout"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/tmuxc"
)

// Relaunch restarts a mission's agent against its existing worktrees, resuming the
// previous session where possible.
//
// This is what recovers a mission whose tmux session died, which happens whenever the
// tmux server restarts or the machine reboots. The worktrees survive that, so only
// the agent needs restarting, and the conversation is picked up rather than started
// over.
//
// The hook epoch is bumped so that any late hook from the previous incarnation is
// recognized as stale and discarded rather than moving the revived card.
func (l *Launcher) Relaunch(
	ctx context.Context,
	operation domain.Operation,
	mission domain.Mission,
	message string,
) (domain.Mission, error) {
	if mission.MissionDir == "" {
		return mission, fmt.Errorf("mission %s has never been launched", mission.ID)
	}

	if _, err := os.Stat(mission.MissionDir); err != nil {
		return mission, fmt.Errorf("mission directory %s is gone: %w", mission.MissionDir, err)
	}

	mission.HookEpoch++

	if err := l.writeRelaunchArtifacts(mission, message); err != nil {
		return mission, err
	}

	// A session left behind with a dead pane would block creating the new one.
	if l.tmux.HasSession(ctx, mission.TmuxSession) {
		if err := l.tmux.KillSession(ctx, mission.TmuxSession); err != nil {
			return mission, err
		}
	}

	if err := l.startSession(ctx, operation, &mission); err != nil {
		return mission, err
	}

	mission.AgentState = domain.AgentUnknown
	mission.LaunchError = ""
	mission.Badges = mission.WithoutBadge(domain.BadgeTmuxGone)
	mission.Badges = mission.WithoutBadge(domain.BadgeEnded)

	return mission, nil
}

// writeRelaunchArtifacts regenerates the prompt and script for a resumed session.
//
// The prompt is rewritten because a resumed agent is given the follow-up message
// rather than the original mission, which it has already seen.
func (l *Launcher) writeRelaunchArtifacts(mission domain.Mission, message string) error {
	dir := filepath.Join(mission.MissionDir, loadout.ArtifactDir)
	if err := os.MkdirAll(dir, paths.DirMode); err != nil {
		return fmt.Errorf("creating the artifact directory: %w", err)
	}

	prompt := message
	if prompt == "" {
		prompt = defaultResumePrompt
	}

	if err := os.WriteFile(filepath.Join(dir, loadout.PromptFile), []byte(prompt), paths.FileMode); err != nil {
		return fmt.Errorf("writing the resume prompt: %w", err)
	}

	qBin, err := l.selfPath()
	if err != nil {
		return err
	}

	// The settings file is rewritten so a moved q binary is corrected on
	// relaunch rather than leaving the revived session unable to report status.
	if mission.Tool == domain.ToolClaude {
		settings, err := loadout.ClaudeSettings(qBin, worktreePaths(mission))
		if err != nil {
			return err
		}

		path := filepath.Join(dir, loadout.ClaudeSettingsFile)
		if err := os.WriteFile(path, settings, paths.FileMode); err != nil {
			return fmt.Errorf("writing claude settings: %w", err)
		}
	} else if err := l.writeCodexProfile(qBin, mission.MissionDir); err != nil {
		return err
	}

	return l.writeResumeScript(mission)
}

// defaultResumePrompt is sent when a session is revived with nothing specific to say.
//
// Something has to be sent, because both agents take their prompt positionally and
// an empty one would leave the session sitting at an empty input box with no
// indication of what happened.
const defaultResumePrompt = "Continue where you left off. " +
	"Your session was restarted by q, so re-check the state of the working tree before acting."

// writeResumeScript renders the launch script in resume mode.
func (l *Launcher) writeResumeScript(mission domain.Mission) error {
	spec, err := l.launchSpec(mission)
	if err != nil {
		return err
	}

	// codex has no way to be told a session id up front, so if its SessionStart hook
	// never arrived there is nothing to resume by id. The session then starts fresh
	// rather than silently resuming whatever was most recent in this directory.
	spec.Resume = mission.AgentSessionID != ""

	script, err := loadout.RenderLaunchScript(spec)
	if err != nil {
		return err
	}

	path := filepath.Join(mission.MissionDir, loadout.ArtifactDir, loadout.LaunchScript)
	if err := os.WriteFile(path, []byte(script), paths.ExecMode); err != nil {
		return fmt.Errorf("writing the relaunch script: %w", err)
	}

	args, err := loadout.AgentArgs(spec)
	if err != nil {
		return err
	}

	return l.writeMeta(mission, spec, args)
}

// SendMessage delivers text to a mission's live agent session.
//
// The pane guard is a safety requirement rather than a nicety. If the agent has
// exited and its pane fell back to a shell, pasting a message would type it into
// that shell and run it. q sets remain-on-exit so a finished agent leaves a
// dead pane instead, and this refuses to send unless the pane is both alive and
// running an agent.
func (l *Launcher) SendMessage(ctx context.Context, mission domain.Mission, text string) error {
	if text == "" {
		return nil
	}

	if mission.TmuxSession == "" || mission.AgentPaneID == "" {
		return fmt.Errorf("mission %s has no live session", mission.ID)
	}

	if err := l.verifyAgentPane(ctx, mission); err != nil {
		return err
	}

	// Delivered via a file and a tmux buffer rather than as keystrokes, so a
	// multi-line message with quotes survives intact.
	path := filepath.Join(l.dirs.State, "msg-"+string(mission.ID)+".txt")
	if err := os.WriteFile(path, []byte(text), paths.FileMode); err != nil {
		return fmt.Errorf("staging the message: %w", err)
	}

	bufferName := "q-msg-" + string(mission.ID)

	if err := l.tmux.LoadBuffer(ctx, bufferName, path); err != nil {
		return err
	}

	if err := l.tmux.PasteBuffer(ctx, tmuxc.Pane(mission.AgentPaneID), bufferName); err != nil {
		return err
	}

	return l.tmux.SendKeys(ctx, tmuxc.Pane(mission.AgentPaneID), "Enter")
}

// verifyAgentPane confirms the target pane is still running an agent.
func (l *Launcher) verifyAgentPane(ctx context.Context, mission domain.Mission) error {
	panes, err := l.tmux.ListPanes(ctx, tmuxc.Session(mission.TmuxSession))
	if err != nil {
		return err
	}

	for _, pane := range panes {
		if pane.ID != mission.AgentPaneID {
			continue
		}

		if pane.Dead {
			return fmt.Errorf("the agent pane for %s has exited; relaunch instead of messaging it", mission.ID)
		}

		if !AgentCommands[pane.Command] {
			return fmt.Errorf(
				"refusing to send to pane %s: it is running %q rather than an agent, "+
					"so the message would be executed as a command",
				pane.ID, pane.Command)
		}

		return nil
	}

	return fmt.Errorf("the agent pane for %s is gone; relaunch instead of messaging it", mission.ID)
}

// AgentCommands are the process names a live agent pane runs.
//
// node appears because codex ships as a node wrapper.
var AgentCommands = map[string]bool{
	"claude": true,
	"codex":  true,
	"node":   true,
}
