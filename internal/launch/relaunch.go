package launch

import (
	"context"
	"fmt"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/terminal"
	"os"
	"path/filepath"
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
	operation mission.Operation,
	ms mission.Mission,
	message string,
) (mission.Mission, error) {
	if ms.MissionDir == "" {
		return ms, fmt.Errorf("mission %s has never been launched", ms.ID)
	}

	if _, err := os.Stat(ms.MissionDir); err != nil {
		return ms, fmt.Errorf("mission directory %s is gone: %w", ms.MissionDir, err)
	}

	ms.HookEpoch++

	if err := l.writeRelaunchArtifacts(ms, message); err != nil {
		return ms, err
	}

	// A session left behind with a dead pane would block creating the new one.
	if l.tmux.HasSession(ctx, ms.TmuxSession) {
		if err := l.tmux.KillSession(ctx, ms.TmuxSession); err != nil {
			return ms, err
		}
	}

	if err := l.startSession(ctx, operation, &ms); err != nil {
		return ms, err
	}

	ms.AgentState = mission.AgentUnknown
	ms.LaunchError = ""
	ms.Badges = ms.WithoutBadge(mission.BadgeTmuxGone)
	ms.Badges = ms.WithoutBadge(mission.BadgeEnded)

	return ms, nil
}

// writeRelaunchArtifacts regenerates the prompt and script for a resumed session.
//
// The prompt is rewritten because a resumed agent is given the follow-up message
// rather than the original mission, which it has already seen. The agent's own
// configuration is rewritten too, so a moved q binary is corrected on relaunch
// rather than leaving the revived session unable to report status.
func (l *Launcher) writeRelaunchArtifacts(ms mission.Mission, message string) error {
	prompt := message
	if prompt == "" {
		prompt = defaultResumePrompt
	}

	if err := l.writePrompt(ms, prompt); err != nil {
		return err
	}

	// An agent with no way to be told a session id up front has nothing to resume
	// by id when its SessionStart hook never arrived. The session then starts
	// fresh rather than silently resuming whatever was most recent in this
	// directory.
	return l.writeScript(ms, ms.AgentSessionID != "")
}

// defaultResumePrompt is sent when a session is revived with nothing specific to say.
//
// Something has to be sent, because both agents take their prompt positionally and
// an empty one would leave the session sitting at an empty input box with no
// indication of what happened.
const defaultResumePrompt = "Continue where you left off. " +
	"Your session was restarted by q, so re-check the state of the working tree before acting."

// SendMessage delivers text to a mission's live agent session.
//
// The pane guard is a safety requirement rather than a nicety. If the agent has
// exited and its pane fell back to a shell, pasting a message would type it into
// that shell and run it. q sets remain-on-exit so a finished agent leaves a
// dead pane instead, and this refuses to send unless the pane is both alive and
// running an agent.
func (l *Launcher) SendMessage(ctx context.Context, ms mission.Mission, text string) error {
	if text == "" {
		return nil
	}

	if ms.TmuxSession == "" || ms.AgentPaneID == "" {
		return fmt.Errorf("mission %s has no live session", ms.ID)
	}

	if err := l.verifyAgentPane(ctx, ms); err != nil {
		return err
	}

	// Delivered via a file and a tmux buffer rather than as keystrokes, so a
	// multi-line message with quotes survives intact.
	path := filepath.Join(l.dirs.State, "msg-"+string(ms.ID)+".txt")
	if err := os.WriteFile(path, []byte(text), paths.FileMode); err != nil {
		return fmt.Errorf("staging the message: %w", err)
	}

	bufferName := "q-msg-" + string(ms.ID)

	if err := l.tmux.LoadBuffer(ctx, bufferName, path); err != nil {
		return err
	}

	if err := l.tmux.PasteBuffer(ctx, terminal.Pane(ms.AgentPaneID), bufferName); err != nil {
		return err
	}

	return l.tmux.SendKeys(ctx, terminal.Pane(ms.AgentPaneID), "Enter")
}

// verifyAgentPane confirms the target pane is still running an agent.
func (l *Launcher) verifyAgentPane(ctx context.Context, ms mission.Mission) error {
	panes, err := l.tmux.ListPanes(ctx, terminal.Session(ms.TmuxSession))
	if err != nil {
		return err
	}

	for _, pane := range panes {
		if pane.ID != ms.AgentPaneID {
			continue
		}

		if pane.Dead {
			return fmt.Errorf("the agent pane for %s has exited; relaunch instead of messaging it", ms.ID)
		}

		if !AgentCommands[pane.Command] {
			return fmt.Errorf(
				"refusing to send to pane %s: it is running %q rather than an agent, "+
					"so the message would be executed as a command",
				pane.ID, pane.Command)
		}

		return nil
	}

	return fmt.Errorf("the agent pane for %s is gone; relaunch instead of messaging it", ms.ID)
}

// AgentCommands are the process names a live agent pane runs.
//
// node appears because codex and gemini both ship as node wrappers, so a pane
// running either may report the interpreter rather than the agent.
var AgentCommands = map[string]bool{
	"claude": true,
	"codex":  true,
	"gemini": true,
	"node":   true,
}
