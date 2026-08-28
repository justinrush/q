package main

import (
	"fmt"
	"io"
	"strings"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/launch"
	"github.com/justinrush/q/internal/state"
	"github.com/spf13/cobra"
)

func buildMissionSubcommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:     "mission",
		Aliases: []string{"missions"},
		Short:   "Manage agent missions",
		Long: "A mission is one unit of agent work within an operation. New missions start in the " +
			"briefing lane; moving one to active launches its agent.\n\n" +
			"These commands exist for scripting; the TUI is the primary interface.",
		Args: cobra.NoArgs,
	}

	cmd.AddCommand(
		buildMissionListSubcommand(),
		buildMissionAddSubcommand(),
		buildMissionMoveSubcommand(),
		buildMissionRemoveSubcommand(),
	)

	return cmd
}

func buildMissionListSubcommand() *cobra.Command {
	var (
		asJSON bool
		lane   string
	)

	cmd := &cobra.Command{
		Use:   "list",
		Short: "List missions grouped by lane",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			snap, err := c.State(cmd.Context())
			if err != nil {
				return err
			}

			if asJSON {
				return writeJSONOut(cmd.OutOrStdout(), snap.Missions)
			}

			lanes := domain.Lanes

			if lane != "" {
				status, err := domain.ParseStatus(lane)
				if err != nil {
					return err
				}

				lanes = []domain.Status{status}
			}

			return renderBoard(cmd.OutOrStdout(), snap, lanes)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit JSON instead of a table")
	cmd.Flags().StringVar(&lane, "lane", "", "Show only one lane (briefing, active, awaiting, debrief, closed)")

	return cmd
}

func buildMissionAddSubcommand() *cobra.Command {
	var (
		operation string
		prompt    string
		tool      string
		planMode  bool
		repos     []string
	)

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a mission in the briefing lane",
		Args:  cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			parsedTool, err := domain.ParseTool(tool)
			if err != nil {
				return err
			}

			parsedRepos, err := parseRepoFlags(repos)
			if err != nil {
				return err
			}

			mission, err := c.CreateMission(cmd.Context(), api.CreateMissionRequest{
				OperationID: domain.OperationID(operation),
				Name:        args[0],
				Prompt:      prompt,
				Tool:        parsedTool,
				PlanMode:    planMode,
				ExtraRepos:  parsedRepos,
			})
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", mission.ID)

			return err
		},
	}

	cmd.Flags().StringVar(&operation, "operation", "", "Operation id this mission belongs to (required)")
	cmd.Flags().StringVar(&prompt, "prompt", "", "What the agent should do (required)")
	cmd.Flags().StringVar(&tool, "tool", string(domain.ToolClaude), "Agent to run: claude or codex")
	cmd.Flags().BoolVar(&planMode, "plan", false, "Start in plan mode and stop for approval (claude only)")
	cmd.Flags().StringArrayVar(&repos, "repo", nil, "Add a repo to this mission; repeatable, accepts name=path")

	if err := cmd.MarkFlagRequired("operation"); err != nil {
		panic(err)
	}

	if err := cmd.MarkFlagRequired("prompt"); err != nil {
		panic(err)
	}

	return cmd
}

func buildMissionMoveSubcommand() *cobra.Command {
	var (
		message string
		force   bool
	)

	cmd := &cobra.Command{
		Use:   "move <mission-id> <lane>",
		Short: "Move a mission to another lane",
		Long: "Move a mission between lanes. Moving out of briefing launches its agent; moving " +
			"into the active lane from waiting or debrief delivers --message to the live " +
			"session. Moving to closed stops the agent and reclaims its worktrees.",
		Args: cobra.ExactArgs(2),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			status, err := domain.ParseStatus(args[1])
			if err != nil {
				return err
			}

			mission, err := c.SetStatus(cmd.Context(), domain.MissionID(args[0]), api.SetStatusRequest{
				To:      status,
				Message: message,
				Force:   force,
			})
			if err != nil {
				return err
			}

			if mission.Status == domain.StatusClosed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is done; resources reclaimed\n", mission.Name)
			} else {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s\n", mission.Name, mission.Status.Label())
			}

			return err
		},
	}

	cmd.Flags().StringVar(&message, "message", "", "Text to send to the live agent session")
	cmd.Flags().BoolVar(&force, "force", false, "Discard uncommitted changes when moving to closed")

	return cmd
}

func buildMissionRemoveSubcommand() *cobra.Command {
	var (
		force  bool
		dryRun bool
	)

	cmd := &cobra.Command{
		Use:     "rm <mission-id>",
		Aliases: []string{"remove", "delete"},
		Short:   "Delete a mission and reclaim its worktrees",
		Long: "Delete a mission, stop its agent, and remove the git worktrees it was given.\n\n" +
			"A branch is deleted only when nothing would be lost with it. One carrying " +
			"commits that are not pushed anywhere is kept and reported.\n\n" +
			"A worktree holding uncommitted changes is refused, because git refuses it " +
			"and that refusal is the last thing between a keystroke and lost work. Use " +
			"--force to discard it anyway.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			id := domain.MissionID(args[0])

			if dryRun {
				plan, err := c.DeletePlan(cmd.Context(), id)
				if err != nil {
					return err
				}

				return renderDeletePlan(cmd.OutOrStdout(), plan)
			}

			report, err := c.DeleteMission(cmd.Context(), id, force)
			if err != nil {
				return err
			}

			return renderDeleteReport(cmd.OutOrStdout(), report)
		},
	}

	cmd.Flags().BoolVar(&force, "force", false, "Discard uncommitted changes in the mission's worktrees")
	cmd.Flags().BoolVar(&dryRun, "dry-run", false, "Report what would be discarded without deleting anything")

	return cmd
}

// renderDeletePlan prints what deleting a mission would discard.
func renderDeletePlan(out io.Writer, plan launch.Plan) error {
	rep := newReport()

	if plan.SessionAlive {
		rep.line("session  %s (running, would be stopped)", plan.TmuxSession)
	}

	if len(plan.Repos) == 0 {
		rep.line("nothing was provisioned for this mission")
	} else {
		rep.line("worktrees")

		for _, repo := range plan.Repos {
			rep.row("  %s\t%s\t%s", repo.Repo, repo.Action, planDetail(repo))
		}
	}

	if plan.NeedsForce {
		rep.line("")
		rep.line("Uncommitted changes would be lost. Re-run with --force to discard them.")
	}

	_, err := io.WriteString(out, rep.String())

	return err
}

// planDetail explains one repo's disposition.
func planDetail(repo launch.RepoDisposition) string {
	parts := make([]string, 0, 3)

	if repo.Dirty {
		parts = append(parts, "uncommitted changes")
	}

	if repo.Ahead > 0 {
		parts = append(parts, fmt.Sprintf("%d commit(s)", repo.Ahead))
	}

	if repo.Pushed {
		parts = append(parts, "pushed")
	}

	if repo.Reason != "" {
		parts = append(parts, repo.Reason)
	}

	if len(parts) == 0 {
		return "clean"
	}

	return strings.Join(parts, ", ")
}

// renderDeleteReport prints what a delete actually did.
func renderDeleteReport(out io.Writer, report launch.Report) error {
	rep := newReport()

	for _, path := range report.Removed {
		rep.line("removed  %s", path)
	}

	for _, branch := range report.DeletedBranches {
		rep.line("deleted  branch %s", branch)
	}

	// A kept branch is the one outcome nothing else will mention again.
	for _, branch := range report.KeptBranches {
		rep.line("kept     branch %s (it holds work)", branch)
	}

	for _, failure := range report.Failures {
		rep.line("failed   %s", failure)
	}

	if rep.String() == "" {
		rep.line("deleted")
	}

	_, err := io.WriteString(out, rep.String())

	return err
}

// renderBoard prints missions grouped by lane, which is the terminal equivalent of
// the board and the quickest way to watch status transitions land.
func renderBoard(out io.Writer, snap state.Snapshot, lanes []domain.Status) error {
	operations := make(map[domain.OperationID]string, len(snap.Operations))
	for _, t := range snap.Operations {
		operations[t.ID] = t.Name
	}

	rep := newReport()

	var total int

	for _, lane := range lanes {
		missions := snap.MissionsInLane(lane)
		total += len(missions)

		rep.line("%s (%d)", strings.ToUpper(lane.Label()), len(missions))

		for _, mission := range missions {
			rep.row("  %s\t%s\t%s\t%s\t%s",
				mission.ID, mission.Name, mission.Tool, operations[mission.OperationID], missionDetail(mission))
		}

		rep.line("")
	}

	if total == 0 {
		rep.line("no missions yet")
	}

	_, err := io.WriteString(out, rep.String())

	return err
}

// missionDetail summarizes what a card would show beneath its title.
func missionDetail(mission domain.Mission) string {
	parts := make([]string, 0, 4)

	if mission.PlanMode {
		parts = append(parts, "plan")
	}

	if mission.AgentState != "" && mission.AgentState != domain.AgentUnknown {
		parts = append(parts, mission.AgentState.String())
	}

	if mission.WaitingFor != "" {
		parts = append(parts, mission.WaitingFor)
	}

	for _, b := range mission.Badges {
		if b.Detail != "" {
			parts = append(parts, b.Kind+":"+b.Detail)

			continue
		}

		parts = append(parts, b.Kind)
	}

	if mission.LaunchError != "" {
		parts = append(parts, "launch failed: "+firstLine(mission.LaunchError))
	}

	return strings.Join(parts, " · ")
}
