package main

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/terminal"
	"github.com/spf13/cobra"
)

func buildAttachSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "attach <mission-id>",
		Short: "Attach this terminal to a mission's agent session",
		Long: "Attach to the tmux session a mission's agent is running in, and nothing " +
			"else: no editor panes, no window.\n\n" +
			"This is what a paired q runs over ssh to show an agent that is on this " +
			"machine. Use `q open` to debrief a mission yourself.",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := paths.Resolve(pathOverrides())
			if err != nil {
				return err
			}

			// Never starts a daemon, for the reason `q rpc` does not: one started
			// from an ssh session would hand its bare environment to every agent.
			c, err := api.Connect(cmd.Context(), dirs)
			if err != nil {
				return err
			}

			snap, err := c.State(cmd.Context())
			if err != nil {
				return err
			}

			session, err := attachTarget(snap, mission.MissionID(args[0]))
			if err != nil {
				return err
			}

			tmux, err := resolveTool(cfg, toolTmux)
			if err != nil {
				return err
			}

			return attachSession(cmd, tmux, session)
		},
	}
}

// attachSession attaches this terminal to a tmux session and waits for it to
// detach.
//
// It always attaches, and never switches an existing client the way `q open`
// does inside tmux. The terminal it runs in belongs to the other machine: it is
// a pane there, reached over ssh, and whatever TMUX names is that machine's
// server, not this one's. The variable is dropped for the same reason, since
// tmux refuses to attach when it believes it is already inside itself.
func attachSession(cmd *cobra.Command, tmux, session string) error {
	argv := terminal.NewTmux(tmux, nil).AttachArgs(session, false)

	// The binary is this machine's own tmux, resolved from configuration, and
	// the session name comes from this machine's daemon.
	child := exec.CommandContext(cmd.Context(), argv[0], argv[1:]...) // #nosec G204
	child.Stdin, child.Stdout, child.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
	child.Env = withoutEnv(os.Environ(), "TMUX")

	return child.Run()
}

// withoutEnv returns env with one variable removed.
func withoutEnv(env []string, name string) []string {
	out := make([]string, 0, len(env))

	for _, entry := range env {
		if !strings.HasPrefix(entry, name+"=") {
			out = append(out, entry)
		}
	}

	return out
}

// attachTarget returns the session to attach to for a mission, or an error that
// says why there is none.
//
// The explanations matter more than usual. This runs in a pane on another
// machine, so whatever it prints is the only thing the person there sees.
func attachTarget(snap mission.Snapshot, id mission.MissionID) (string, error) {
	ms, ok := snap.Mission(id)
	if !ok {
		return "", fmt.Errorf("no mission %s on %s", id, snap.HostName(snap.Self.ID))
	}

	if !snap.Holds(ms) {
		return "", fmt.Errorf("%s is not running on %s; it moved to %s",
			ms.Name, snap.HostName(snap.Self.ID), snap.HostName(ms.Lease.Holder))
	}

	if ms.TmuxSession == "" {
		return "", errors.New(ms.Name + " has no agent session right now; resume it to start one")
	}

	return ms.TmuxSession, nil
}
