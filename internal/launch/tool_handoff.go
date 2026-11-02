package launch

import (
	"context"
	"encoding/json"
	"fmt"
	"github.com/justinrush/q/internal/terminal"
	"os"
	"os/exec"
	"path/filepath"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
)

// PreflightTool checks the destination before the planner is stopped.
func (l *Launcher) PreflightTool(tool mission.Tool) error {
	agent, err := l.agentFor(tool)
	if err != nil {
		return err
	}
	if _, err := exec.LookPath(agent.Bin()); err != nil {
		return fmt.Errorf("destination %s is unavailable: %w", tool, err)
	}
	return nil
}

func writeHandoffArtifacts(ms mission.Mission) error {
	if ms.ToolHandoff.Plan == "" {
		return nil
	}
	dir := filepath.Join(ms.MissionDir, mission.ArtifactDir)
	if err := os.MkdirAll(dir, paths.DirMode); err != nil {
		return err
	}
	for name, body := range map[string]string{"approved-plan.md": ms.ToolHandoff.Plan, "planning-conversation.md": ms.ToolHandoff.Conversation} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), paths.FileMode); err != nil {
			return err
		}
	}
	return nil
}

// RecoverToolSession recovers the launch metadata written before exec, including
// when the daemon died before it could persist the new pane or Claude session.
func (l *Launcher) RecoverToolSession(ctx context.Context, ms mission.Mission) (mission.Mission, error) {
	data, err := os.ReadFile(filepath.Join(ms.MissionDir, mission.ArtifactDir, mission.MetaFile))
	if os.IsNotExist(err) {
		return ms, nil
	}
	if err != nil {
		return ms, err
	}
	var saved meta
	if err := json.Unmarshal(data, &saved); err != nil {
		return ms, err
	}
	if saved.MissionID != ms.ID || saved.Tool != ms.Tool || saved.HookEpoch != ms.HookEpoch {
		return ms, nil
	}
	if ms.AgentSessionID == "" {
		ms.AgentSessionID = saved.SessionID
	}
	if !l.tmux.HasSession(ctx, ms.TmuxSession) {
		return ms, nil
	}
	panes, err := l.tmux.ListPanes(ctx, terminal.Window(ms.TmuxSession, agentWindow))
	if err != nil {
		return ms, err
	}
	for _, pane := range panes {
		if !pane.Dead && PaneRunsAgent(pane.Command) {
			ms.AgentPaneID = pane.ID
			break
		}
	}
	return ms, nil
}
