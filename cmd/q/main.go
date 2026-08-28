// q is a terminal UI for running coding agents across several git repos at once.
//
// Q runs the operations board: operations are areas of investigation, missions
// are the units of agent work inside them, and each mission gets its own git
// worktree per repo with claude or codex running in a detached tmux session.
package main

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/signal"
	"syscall"
)

func main() {
	if err := run(); err != nil {
		fmt.Fprintf(os.Stderr, "error: %v\n", err)
		os.Exit(1)
	}
}

// run wires the signal handling and the shared logger, then executes the command
// tree.
//
// The first interrupt cancels the context so in-flight work can unwind; a second
// one is left to the default handler, which is what makes a wedged process still
// killable with a second ctrl-c.
func run() error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	ctx = withLogger(ctx, bootstrapLogger())

	err := BuildRootCommand().ExecuteContext(ctx)

	// A canceled context means the user interrupted us, which is not a failure
	// worth an error line.
	if errors.Is(err, context.Canceled) {
		return nil
	}

	return err
}
