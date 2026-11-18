package main

import (
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/terminal"
	"github.com/spf13/cobra"
)

func buildOpenSubcommand() *cobra.Command {
	var (
		steal   bool
		raise   bool
		prepare bool
	)

	cmd := &cobra.Command{
		Use:   "open <mission-id>",
		Short: "Open a mission's debrief session",
		Long: "Attach to a mission's live agent session and add an editor pane for each repo " +
			"it changed.\n\n" +
			"The session is the one the agent is already running in, so this joins the " +
			"live conversation rather than starting anything. Repos with no changes get " +
			"no pane.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			mode := api.DebriefAttach

			switch {
			case prepare:
				mode = api.DebriefPrepare
			case raise:
				mode = api.DebriefRaise
			case steal:
				mode = api.DebriefSteal
			}

			bin := currentTerminalBin(cfg)
			attach := bin != "" && !prepare && !raise
			if attach {
				mode = api.DebriefPrepare
			}

			result, err := c.OpenDebrief(cmd.Context(), mission.MissionID(args[0]), mode)
			if err != nil {
				return err
			}

			if attach && !result.NeedsRelaunch && result.Remote != nil {
				return attachRemote(cmd, *result.Remote, steal)
			}

			if attach && !result.NeedsRelaunch {
				child := terminal.CurrentCommand(cmd.Context(), bin, result.Session, steal)
				child.Stdin, child.Stdout, child.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()
				return child.Run()
			}
			return renderOpenResult(cmd.OutOrStdout(), result)
		},
	}

	cmd.Flags().BoolVar(&steal, "steal", false, "Detach any other client first")
	cmd.Flags().BoolVar(&raise, "raise", false, "Bring an already-attached window forward")
	cmd.Flags().BoolVar(&prepare, "prepare", false, "Arrange the panes without attaching")

	return cmd
}

// attachRemote connects this terminal to a mission's session on the paired
// machine, and waits for it to detach.
//
// Inside tmux this cannot switch the client the way a local open does: the
// session is in another machine's tmux server, so it runs nested in this pane,
// and this machine's tmux sees the prefix key first.
func attachRemote(cmd *cobra.Command, target api.AttachCommand, steal bool) error {
	argv := target.ArgvFor(steal)

	if os.Getenv("TMUX") != "" {
		_, _ = fmt.Fprintf(cmd.ErrOrStderr(),
			"q: the agent runs on %s; its tmux is nested in this one, so press the prefix twice to reach it\n",
			target.Host)
	}

	// The argv comes from this machine's daemon, which built it from the
	// configured remote command. It is passed literally, never through a shell.
	child := exec.CommandContext(cmd.Context(), argv[0], argv[1:]...) // #nosec G204
	child.Stdin, child.Stdout, child.Stderr = cmd.InOrStdin(), cmd.OutOrStdout(), cmd.ErrOrStderr()

	return child.Run()
}

// renderOpenResult explains what opening the debrief did.
func renderOpenResult(out io.Writer, result api.Result) error {
	rep := newReport()

	if result.Remote != nil {
		rep.line("session  %s on %s", result.Session, result.Remote.Host)
	} else {
		rep.line("session  %s", result.Session)
	}

	if result.NeedsRelaunch {
		rep.line("")
		rep.line("The tmux session is gone, so there is nothing to attach to.")
		rep.line("Move the mission back to active to revive the agent against its existing worktrees:")
		rep.line("  q mission move <mission-id> active --message \"continue\"")

		_, err := io.WriteString(out, rep.String())

		return err
	}

	if len(result.Touched) == 0 {
		rep.line("changes  none yet")
	} else {
		rep.line("changes")

		for _, item := range result.Touched {
			rep.row("  %s\t%s\t%s", item.Repo, item.Branch, describeTouched(item))
		}
	}

	rep.line("panes    %d added", result.PanesAdded)

	switch {
	case result.Attached:
		rep.line("attach   opened a terminal window")
	case result.AttachCommand != "":
		rep.line("attach   run this to join the session:")
		rep.line("  %s", result.AttachCommand)
	case result.Remote != nil:
		rep.line("attach   run this to join the session on %s:", result.Remote.Host)
		rep.line("  %s", terminal.ShellJoin(result.Remote.Argv))
	default:
		rep.line("attach   not attached")
	}

	_, err := io.WriteString(out, rep.String())

	return err
}

// describeTouched summarizes one repo's changes.
func describeTouched(item api.Touched) string {
	parts := make([]string, 0, 3)

	if item.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("%d commit(s)", item.Ahead))
	}

	if item.Dirty {
		parts = append(parts, "uncommitted changes")
	}

	if item.ShortStat != "" {
		parts = append(parts, strings.TrimSpace(item.ShortStat))
	}

	return strings.Join(parts, ", ")
}
