package main

import (
	"context"
	"time"

	"github.com/justinrush/q/internal/codex"
	"github.com/justinrush/q/internal/runner"
	"github.com/spf13/cobra"
)

// Called by the generated launch script after the managed daemon is available.
// Keep preparation explicit and bounded so a broken observer cannot silently
// leave the terminal waiting at a directory-trust dialog.
func buildCodexPrepareSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:    "codex-prepare <codex-binary> <mission-directory>",
		Hidden: true,
		Args:   cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			ctx, cancel := context.WithTimeout(cmd.Context(), 15*time.Second)
			defer cancel()
			proxy, err := codex.StartProxy(ctx, args[0], versionString(), runner.OS{})
			if err != nil {
				return err
			}
			defer proxy.Close()
			return proxy.TrustProject(ctx, args[1])
		},
	}
}
