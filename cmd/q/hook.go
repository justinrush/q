package main

import (
	"context"
	"encoding/json"
	"io"
	"os"
	"strconv"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/client"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/hookspec"
	"github.com/justinrush/q/internal/loadout"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/spool"
	"github.com/spf13/cobra"
)

// hookBudget bounds the total time a hook may take.
//
// A hook runs inside the agent's turn, so every millisecond here is latency the user
// feels. Exceeding this is treated as a delivery failure and the event is spooled.
const hookBudget = 2 * time.Second

// maxHookPayload bounds the payload read from standard input.
const maxHookPayload = 1 << 20

func buildHookSubcommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "hook <tool> <event>",
		Short: "Report an agent hook event to the q daemon",
		Long: "Read a hook payload on standard input and report it to the daemon.\n\n" +
			"This is invoked by claude and codex, not by hand. It is written to be " +
			"incapable of harming the agent that calls it: it always exits zero, never " +
			"writes to standard output, and records events to disk when the daemon is " +
			"unreachable rather than failing.",
		Args:   cobra.ExactArgs(2),
		Hidden: true,
		// The agent parses this command's stdout on several events and treats a
		// non-zero exit as a signal, so cobra must never print anything itself.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, args []string) error {
			runHook(cmd.Context(), args[0], args[1])

			// Always succeed. In claude a PreToolUse hook exiting non-zero blocks
			// the tool call, so a q problem must never become the agent's
			// problem.
			return nil
		},
	}

	return cmd
}

// runHook delivers one hook event, swallowing every failure.
func runHook(ctx context.Context, toolArg, eventArg string) {
	// A panic here would surface as a crash inside the agent's turn.
	defer func() { _ = recover() }()

	ctx, cancel := context.WithTimeout(ctx, hookBudget)
	defer cancel()

	tool, err := domain.ParseTool(toolArg)
	if err != nil {
		return
	}

	event, err := hookspec.CanonicalEvent(eventArg)
	if err != nil {
		return
	}

	payload, err := io.ReadAll(io.LimitReader(os.Stdin, maxHookPayload))
	if err != nil {
		return
	}

	req := api.HookRequest{
		Tool:      tool,
		Event:     event,
		MissionID: domain.MissionID(os.Getenv(loadout.EnvMissionID)),
		HookEpoch: hookEpoch(),
		Payload:   json.RawMessage(payload),
	}

	dirs, err := paths.Resolve(pathOverrides())
	if err != nil {
		return
	}

	deliver(ctx, dirs, req)
}

// deliver posts the event, spooling it if the daemon cannot be reached.
//
// It deliberately never starts a daemon. Hooks fire on every tool call, so a hook
// that spawned one could start a stampede, and starting a daemon takes far longer
// than a hook's budget allows.
func deliver(ctx context.Context, dirs paths.Dirs, req api.HookRequest) {
	c, err := client.Connect(ctx, dirs)
	if err == nil {
		if err := c.PostHook(ctx, req); err == nil {
			return
		}
	}

	_ = spool.Write(dirs.SpoolDir(), spool.Entry{ObservedAt: time.Now(), Hook: req})
}

// hookEpoch reads the launch generation from the environment.
func hookEpoch() int {
	epoch, err := strconv.Atoi(os.Getenv(loadout.EnvHookEpoch))
	if err != nil {
		return 0
	}

	return epoch
}
