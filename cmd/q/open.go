package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/debrief"
	"github.com/justinrush/q/internal/domain"
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

			result, err := c.OpenDebrief(cmd.Context(), domain.MissionID(args[0]), mode)
			if err != nil {
				return err
			}

			return renderOpenResult(cmd.OutOrStdout(), result)
		},
	}

	cmd.Flags().BoolVar(&steal, "steal", false, "Detach any other client first")
	cmd.Flags().BoolVar(&raise, "raise", false, "Bring an already-attached window forward")
	cmd.Flags().BoolVar(&prepare, "prepare", false, "Arrange the panes without attaching")

	return cmd
}

// renderOpenResult explains what opening the debrief did.
func renderOpenResult(out io.Writer, result debrief.Result) error {
	rep := newReport()

	rep.line("session  %s", result.Session)

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
	default:
		rep.line("attach   not attached")
	}

	_, err := io.WriteString(out, rep.String())

	return err
}

// describeTouched summarizes one repo's changes.
func describeTouched(item debrief.Touched) string {
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
