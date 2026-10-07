package main

import (
	"bufio"
	"context"
	"fmt"
	"io"
	"os/exec"
	"strings"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
	"github.com/spf13/cobra"
)

func buildViewSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "view <mission-id>",
		Short: "Show a mission's agent that is running on the paired machine",
		Long: "Attach this terminal to an agent running on the paired machine, and stay " +
			"open when the connection drops so it can be picked up again.\n\n" +
			"This is what runs in the agent pane of a mission this machine is only " +
			"mirroring. It is started by `q open`, not by hand.",
		Args:   cobra.ExactArgs(1),
		Hidden: true,
		RunE: func(cmd *cobra.Command, args []string) error {
			dirs, err := paths.Resolve(pathOverrides())
			if err != nil {
				return err
			}

			return runView(cmd.Context(), viewIO{
				in: cmd.InOrStdin(), out: cmd.OutOrStdout(), err: cmd.ErrOrStderr(),
			}, mission.MissionID(args[0]), func(ctx context.Context) (api.AttachCommand, error) {
				c, err := api.Connect(ctx, dirs)
				if err != nil {
					return api.AttachCommand{}, err
				}

				return c.AttachCommand(ctx, mission.MissionID(args[0]))
			}, runAttached)
		},
	}
}

// viewIO is the terminal a view runs in.
type viewIO struct {
	in       io.Reader
	out, err io.Writer
}

// runAttached runs the attach command with this terminal as its own.
func runAttached(ctx context.Context, argv []string, streams viewIO) error {
	// The argv comes from this machine's own daemon, which built it from the
	// user's configured command. It is passed literally, never through a shell.
	child := exec.CommandContext(ctx, argv[0], argv[1:]...) // #nosec G204
	child.Stdin, child.Stdout, child.Stderr = streams.in, streams.out, streams.err

	return child.Run()
}

// runView attaches to the agent, and when the attachment ends says why and
// waits to be told to try again.
//
// The waiting is the point. The pane this runs in is the agent's place in a
// debrief session, with the mirrored code open in editors beside it. If it
// exited when a laptop slept and its connection dropped, tmux would close the
// pane, and with it the only sign of what had happened.
func runView(
	ctx context.Context,
	streams viewIO,
	id mission.MissionID,
	command func(context.Context) (api.AttachCommand, error),
	attach func(context.Context, []string, viewIO) error,
) error {
	lines := bufio.NewReader(streams.in)

	for {
		target, err := command(ctx)

		switch {
		case err != nil:
			_, _ = fmt.Fprintf(streams.out, "\nq: cannot show the agent for %s: %v\n", id, err)
		default:
			if err := attach(ctx, target.Argv, streams); err != nil {
				_, _ = fmt.Fprintf(streams.out, "\nq: the connection to %s ended: %v\n", target.Host, err)
			} else {
				_, _ = fmt.Fprintf(streams.out, "\nq: detached from the agent on %s\n", target.Host)
			}
		}

		_, _ = fmt.Fprint(streams.out, "q: press enter to reconnect, or q then enter to close this pane: ")

		answer, readErr := lines.ReadString('\n')
		if readErr != nil || strings.EqualFold(strings.TrimSpace(answer), "q") {
			return nil
		}

		if ctx.Err() != nil {
			return nil
		}
	}
}
