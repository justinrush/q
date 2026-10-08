package tui

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/x/ansi"
	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// Native copying avoids terminal clipboard size limits on a local Mac. Over
// SSH, OSC 52 targets the viewing terminal rather than the remote machine.
func (a *App) copyMissionLog(log *missionLog) tea.Cmd {
	result := func(err error) tea.Msg {
		text := "Log copied to clipboard"
		if err != nil {
			text = "Could not copy log: " + err.Error()
		}
		return missionLogResultMsg{Log: log, Text: text, Err: err != nil}
	}
	if runtime.GOOS == "darwin" && os.Getenv("SSH_CONNECTION") == "" && os.Getenv("SSH_TTY") == "" {
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			return result(copyLogNative(ctx, runner.OS{}, log.body))
		}
	}
	if os.Getenv("TMUX") != "" {
		return func() tea.Msg {
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			bin := a.opts.AttachBin
			if bin == "" {
				bin = "tmux"
			}
			resolved, err := exec.LookPath(bin)
			if err != nil {
				return result(err)
			}
			return result(copyLogTmux(ctx, runner.OS{}, resolved, log.body))
		}
	}
	return tea.Exec(&clipboardSequence{body: log.body}, func(err error) tea.Msg {
		if err == nil {
			return missionLogResultMsg{Log: log, Text: "Log sent to terminal clipboard (OSC 52)"}
		}
		return result(err)
	})
}

func copyLogNative(ctx context.Context, run runner.Runner, body string) error {
	_, err := run.Run(ctx, runner.Spec{Name: "/usr/bin/pbcopy", Stdin: []byte(body)})
	return err
}

func copyLogTmux(ctx context.Context, run runner.Runner, bin, body string) error {
	_, err := run.Run(ctx, runner.Spec{Name: bin, Args: []string{"load-buffer", "-w", "-"}, Stdin: []byte(body)})
	return err
}

// Bubble Tea pauses rendering while writing the escape sequence to its output.
// tmux with set-clipboard on forwards OSC 52 to the client terminal.
type clipboardSequence struct {
	body   string
	output io.Writer
}

func (c *clipboardSequence) SetStdin(io.Reader)    {}
func (c *clipboardSequence) SetStderr(io.Writer)   {}
func (c *clipboardSequence) SetStdout(w io.Writer) { c.output = w }
func (c *clipboardSequence) Run() error {
	_, err := io.WriteString(c.output, ansi.SetSystemClipboard(c.body))
	return err
}

func (a *App) troubleshootMission(log *missionLog) tea.Cmd {
	// Include the full snapshot so paths, host lease, session identifiers, repo
	// branches, timestamps, original brief, and diagnostics survive later edits.
	operation, _ := a.snapshot.Operation(log.mission.OperationID)
	details, _ := json.MarshalIndent(struct {
		Mission     mission.Mission   `json:"mission"`
		Operation   mission.Operation `json:"operation"`
		ViewingHost mission.HostInfo  `json:"viewingHost"`
		Peer        *mission.Peer     `json:"peer,omitempty"`
	}{log.mission, operation, a.snapshot.Self, a.snapshot.Peer}, "", "  ")
	prompt := fmt.Sprintf("Troubleshoot mission %q (%s). Investigate the launch failure, identify its root cause, and implement and verify the appropriate fix. Use the diagnostics and source mission snapshot below as context. Local paths and sessions belong to the source host and may be unavailable here.\n\nLaunch log:\n%s\n\nSource mission snapshot (JSON):\n%s", log.mission.Name, log.mission.ID, log.body, details)
	return func() tea.Msg {
		ms, err := a.client.CreateMission(a.ctx(), api.CreateMissionRequest{
			InheritFrom: log.mission.ID,
			Name:        "Troubleshoot " + log.mission.Name,
			Prompt:      prompt,
			Queued:      true,
		})
		if err != nil {
			return missionLogResultMsg{Log: log, Text: "Could not queue troubleshooting mission: " + err.Error(), Troubleshoot: true, Err: true}
		}
		return missionLogResultMsg{Log: log, Text: "Queued " + ms.Name, Troubleshoot: true}
	}
}
