package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"strings"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/client"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/paths"
	"github.com/spf13/cobra"
)

func buildOperationSubcommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "operation",
		Aliases: []string{"operations"},
		Short:   "Manage investigation operations",
		Long: "An operation is an area of investigation: a high-level summary plus the git " +
			"repos it spans. Missions belong to operations, and a mission's agent is told its " +
			"operation's summary and repo list.\n\n" +
			"These commands exist for scripting; the TUI is the primary interface.",
		Args: cobra.NoArgs,
	}

	cmd.AddCommand(
		buildOperationListSubcommand(),
		buildOperationShowSubcommand(),
		buildOperationAddSubcommand(),
		buildOperationEditSubcommand(),
		buildOperationRemoveSubcommand(),
	)

	return cmd
}

// connectDaemon resolves the directories and returns a client, starting a daemon
// if none is running.
func connectDaemon(ctx context.Context) (*client.Client, error) {
	dirs, err := paths.Resolve(pathOverrides())
	if err != nil {
		return nil, err
	}

	return client.Ensure(ctx, dirs)
}

func buildOperationListSubcommand() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List operations",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			operations, err := c.Operations(cmd.Context())
			if err != nil {
				return err
			}

			if asJSON {
				return writeJSONOut(cmd.OutOrStdout(), operations)
			}

			return renderOperationTable(cmd.OutOrStdout(), operations)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit JSON instead of a table")

	return cmd
}

func buildOperationShowSubcommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "show <operation-id>",
		Short: "Show an operation and its missions",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			snap, err := c.State(cmd.Context())
			if err != nil {
				return err
			}

			operation, ok := snap.Operation(domain.OperationID(args[0]))
			if !ok {
				return fmt.Errorf("no such operation: %s", args[0])
			}

			return writeJSONOut(cmd.OutOrStdout(), struct {
				Operation domain.Operation `json:"operation"`
				Missions  []domain.Mission `json:"missions"`
			}{Operation: operation, Missions: snap.MissionsForOperation(operation.ID)})
		},
	}

	return cmd
}

func buildOperationAddSubcommand() *cobra.Command {
	var (
		summary string
		repos   []string
	)

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create an operation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			parsed, err := parseRepoFlags(repos)
			if err != nil {
				return err
			}

			operation, err := c.CreateOperation(cmd.Context(), api.CreateOperationRequest{
				Name:    args[0],
				Summary: summary,
				Repos:   parsed,
			})
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", operation.ID)

			return err
		},
	}

	cmd.Flags().StringVar(&summary, "summary", "", "High-level description handed to every agent on this operation")
	cmd.Flags().StringArrayVar(&repos, "repo", nil,
		"Path to a related git checkout; repeatable. Accepts a bare path or name=path")

	return cmd
}

func buildOperationEditSubcommand() *cobra.Command {
	var (
		name    string
		summary string
		repos   []string
	)

	cmd := &cobra.Command{
		Use:   "edit <operation-id>",
		Short: "Update an operation",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			var req api.UpdateOperationRequest

			if cmd.Flags().Changed("name") {
				req.Name = &name
			}

			if cmd.Flags().Changed("summary") {
				req.Summary = &summary
			}

			if cmd.Flags().Changed("repo") {
				parsed, err := parseRepoFlags(repos)
				if err != nil {
					return err
				}

				req.Repos = &parsed
			}

			operation, err := c.UpdateOperation(cmd.Context(), domain.OperationID(args[0]), req)
			if err != nil {
				return err
			}

			return writeJSONOut(cmd.OutOrStdout(), operation)
		},
	}

	cmd.Flags().StringVar(&name, "name", "", "New name")
	cmd.Flags().StringVar(&summary, "summary", "", "New summary")
	cmd.Flags().StringArrayVar(&repos, "repo", nil, "Replace the repo list; repeatable")

	return cmd
}

func buildOperationRemoveSubcommand() *cobra.Command {
	var force bool

	cmd := &cobra.Command{
		Use:     "rm <operation-id>",
		Aliases: []string{"remove", "delete"},
		Short:   "Delete an operation and its missions",
		Args:    cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			return c.DeleteOperation(cmd.Context(), domain.OperationID(args[0]), force)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Delete even if the operation has unfinished missions")

	return cmd
}

// parseRepoFlags turns --repo values into repo records.
//
// Only the name and path are set here. The default branch and common directory
// require running git, which the daemon does when it provisions worktrees.
func parseRepoFlags(values []string) ([]domain.Repo, error) {
	out := make([]domain.Repo, 0, len(values))

	for _, v := range values {
		name, path, hasName := strings.Cut(v, "=")
		if !hasName {
			path = name
			name = ""
		}

		path = strings.TrimSpace(path)
		if path == "" {
			return nil, fmt.Errorf("--repo %q has no path", v)
		}

		abs, err := absPath(path)
		if err != nil {
			return nil, err
		}

		if name == "" {
			name = baseName(abs)
		}

		out = append(out, domain.Repo{Name: name, Path: abs})
	}

	return out, nil
}

// renderOperationTable prints operations as an aligned table.
func renderOperationTable(out io.Writer, operations []domain.Operation) error {
	if len(operations) == 0 {
		_, err := fmt.Fprintln(out, "no operations yet")

		return err
	}

	rep := newReport()
	rep.row("ID\tNAME\tREPOS\tSUMMARY")

	for _, t := range operations {
		names := make([]string, 0, len(t.Repos))
		for _, r := range t.Repos {
			names = append(names, r.Name)
		}

		rep.row("%s\t%s\t%s\t%s", t.ID, t.Name, strings.Join(names, ","), firstLine(t.Summary))
	}

	_, err := io.WriteString(out, rep.String())

	return err
}

// firstLine returns the first line of s, ellipsized if there is more.
func firstLine(s string) string {
	s = strings.TrimSpace(s)
	if s == "" {
		return ""
	}

	if head, _, found := strings.Cut(s, "\n"); found {
		return strings.TrimSpace(head) + "…"
	}

	return s
}

// writeJSONOut prints a value as indented JSON.
func writeJSONOut(out io.Writer, v any) error {
	enc := json.NewEncoder(out)
	enc.SetIndent("", "  ")

	return enc.Encode(v)
}
