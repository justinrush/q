package main

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
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
		buildMissionQueueSubcommand(),
		buildMissionUnqueueSubcommand(),
		buildMissionTakeSubcommand(),
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

			lanes := mission.Lanes

			if lane != "" {
				status, err := mission.ParseStatus(lane)
				if err != nil {
					return err
				}

				lanes = []mission.Status{status}
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
		model     string
		effort    string
		planMode  bool
		repos     []string
		bases     []string
		fromFlag  string
		where     placement
	)

	cmd := &cobra.Command{
		Use:   "add <name>",
		Short: "Create a mission in the briefing lane",
		Long: "Create a mission in the briefing lane. Nothing launches until it is moved to active, " +
			"unless --queue asks q to start it once a slot is free.\n\n" +
			"Give --from a mission id to copy its operation, agent, model, effort, repositories, " +
			"and base branches, so a mission that has just designed follow-up work can queue it " +
			"without restating its own context. Anything passed here still wins.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			parsedTool, err := parseToolFlag(tool)
			if err != nil {
				return err
			}

			parsedRepos, err := parseRepoFlags(repos)
			if err != nil {
				return err
			}

			parsedBases, err := parseBaseFlags(bases)
			if err != nil {
				return err
			}

			// Q_MISSION_ID is set only by the launch script q writes, so its
			// presence means an agent is running inside a mission and is exactly
			// the mission whose setup a follow-up should match.
			from := inheritTarget(fromFlag)

			// An unnamed model resolves to whatever the agent itself reports, so a
			// scripted mission gets the same default the board would have offered
			// rather than silently differing from it. Inheriting is different: the
			// parent's model is the answer, and a parent that had none should leave
			// its children on the agent's own default rather than being quietly
			// upgraded out of step with it.
			if wantsModelDefault(model, from) {
				model, effort = defaultModelFor(cmd.Context(), c, orDefaultTool(parsedTool), effort)
			}

			ms, err := c.CreateMission(cmd.Context(), api.CreateMissionRequest{
				OperationID:  mission.OperationID(operation),
				InheritFrom:  from,
				Name:         args[0],
				Prompt:       prompt,
				Tool:         parsedTool,
				Model:        model,
				Effort:       effort,
				PlanMode:     planMode,
				ExtraRepos:   parsedRepos,
				BaseBranches: parsedBases,
				Queued:       where.queued,
				Pin:          where.pin,
			})
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s\n", ms.ID)

			return err
		},
	}

	cmd.Flags().StringVar(&operation, "operation", "",
		"Operation id this mission belongs to; not required with --from")
	cmd.Flags().StringVar(&fromFlag, "from", "",
		"Copy another mission's operation, agent, model, effort, repos, and base branches "+
			"(default: $Q_MISSION_ID when set)")
	cmd.Flags().StringVar(&prompt, "prompt", "", "What the agent should do (required)")
	cmd.Flags().StringVar(&tool, "tool", "",
		"Agent to run: claude, codex, agy, or opencode. Defaults to claude, or to the agent "+
			"this mission inherits when --from is used")
	cmd.Flags().StringVar(&model, "model", "",
		"Model to run on; defaults to the agent's own (see q models)")
	cmd.Flags().StringVar(&effort, "effort", "",
		"Reasoning effort, for a model that takes one (see q models)")
	cmd.Flags().BoolVar(&planMode, "plan", false,
		"Start in plan mode and stop for approval (claude and opencode); never inherited")
	cmd.Flags().StringArrayVar(&repos, "repo", nil,
		"Add a repo to this mission; repeatable, accepts name=path. Replaces the inherited list")
	cmd.Flags().StringArrayVar(&bases, "base", nil,
		"Base a repo's worktree on a branch instead of its default; repeatable, accepts repo=branch. "+
			"Replaces the inherited map")
	where.register(cmd)

	// --operation is deliberately not marked required. The daemon is the only
	// place mission rules live, and "required unless --from names a mission that
	// has one" is not a rule the client can express without a second copy of it
	// waiting to drift.
	if err := cmd.MarkFlagRequired("prompt"); err != nil {
		panic(err)
	}

	return cmd
}

// placement is when and where a new mission starts.
type placement struct {
	queued bool
	pin    string
}

// register adds the placement flags to a command.
func (p *placement) register(cmd *cobra.Command) {
	cmd.Flags().BoolVar(&p.queued, "queue", false,
		"Start the mission automatically once a slot is free, instead of waiting in briefing")
	cmd.Flags().StringVar(&p.pin, "on", "",
		"Run only on this host: local, remote, or a host name. Default: q decides")
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

			status, err := mission.ParseStatus(args[1])
			if err != nil {
				return err
			}

			ms, err := c.SetStatus(cmd.Context(), mission.MissionID(args[0]), api.SetStatusRequest{
				To:      status,
				Message: message,
				Force:   force,
			})
			if err != nil {
				return err
			}

			if ms.Status == mission.StatusClosed {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is done; resources reclaimed\n", ms.Name)
			} else {
				_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s is now %s\n", ms.Name, ms.Status.Label())
			}

			return err
		},
	}

	cmd.Flags().StringVar(&message, "message", "", "Text to send to the live agent session")
	cmd.Flags().BoolVar(&force, "force", false, "Discard uncommitted changes when moving to closed")

	return cmd
}

func buildMissionQueueSubcommand() *cobra.Command {
	var pin string

	cmd := &cobra.Command{
		Use:   "queue <mission-id>...",
		Short: "Start briefed missions automatically as slots free up",
		Long: "Mark missions in the briefing lane to be started by q rather than by hand. " +
			"They start in board order, as many at once as queue.maxConcurrent allows.\n\n" +
			"With two paired machines, a queued mission runs on the primary when it is " +
			"around and on the secondary when it is not. --on overrides that.",
		Args: cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			req := api.UpdateMissionRequest{Queued: new(true)}
			if cmd.Flags().Changed("on") {
				req.Pin = &pin
			}

			return patchMissions(cmd, args, req, "queued")
		},
	}

	cmd.Flags().StringVar(&pin, "on", "",
		"Run only on this host: local, remote, or a host name. Empty clears a pin")

	return cmd
}

func buildMissionUnqueueSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "unqueue <mission-id>...",
		Short: "Leave briefed missions to be started by hand",
		Args:  cobra.MinimumNArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			return patchMissions(cmd, args, api.UpdateMissionRequest{Queued: new(false)}, "not queued")
		},
	}
}

func buildMissionTakeSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "take <mission-id>",
		Short: "Bring a mission the paired machine is running to this one",
		Long: "Move a mission's agent to this machine. The other machine is asked to stop " +
			"its agent first, and its work arrives here before this returns. If it cannot " +
			"be reached, the mission is taken anyway and the other machine stands down when " +
			"the two next speak; anything it did in the meantime is kept beside what " +
			"happens here.\n\n" +
			"q moves missions by itself: to the always-on machine when this one goes " +
			"quiet, and back when an agent there finishes a turn. This is for when you " +
			"do not want to wait for that.",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			ms, err := c.TakeMission(cmd.Context(), mission.MissionID(args[0]))
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(), "%s now runs here (%s)\n", ms.Name, ms.Status.Label())

			return err
		},
	}
}

// patchMissions applies one patch to several missions, reporting each.
func patchMissions(cmd *cobra.Command, ids []string, req api.UpdateMissionRequest, verb string) error {
	c, err := connectDaemon(cmd.Context())
	if err != nil {
		return err
	}

	for _, id := range ids {
		ms, err := c.UpdateMission(cmd.Context(), mission.MissionID(id), req)
		if err != nil {
			return err
		}

		if _, err := fmt.Fprintf(cmd.OutOrStdout(), "%s is %s\n", ms.Name, verb); err != nil {
			return err
		}
	}

	return nil
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

			id := mission.MissionID(args[0])

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
func renderDeletePlan(out io.Writer, plan mission.Plan) error {
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
func planDetail(repo mission.RepoDisposition) string {
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
func renderDeleteReport(out io.Writer, report mission.Report) error {
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
func renderBoard(out io.Writer, snap mission.Snapshot, lanes []mission.Status) error {
	operations := make(map[mission.OperationID]string, len(snap.Operations))
	for _, t := range snap.Operations {
		operations[t.ID] = t.Name
	}

	rep := newReport()

	var total int

	for _, lane := range lanes {
		missions := snap.MissionsInLane(lane)
		total += len(missions)

		rep.line("%s (%d)", strings.ToUpper(lane.Label()), len(missions))

		for _, ms := range missions {
			detail := missionDetail(ms)
			if host := snap.Elsewhere(ms); host != "" {
				detail = strings.TrimPrefix("@"+host+" · "+detail, " · ")
			}

			rep.row("  %s\t%s\t%s\t%s\t%s",
				ms.ID, ms.Name, ms.Tool, operations[ms.OperationID], strings.TrimSuffix(detail, " · "))
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
func missionDetail(ms mission.Mission) string {
	parts := make([]string, 0, 4)

	if ms.Model != "" {
		parts = append(parts, modelLabel(ms))
	}

	if ms.PlanMode {
		parts = append(parts, "plan")
	}

	if ms.Queued {
		parts = append(parts, mission.BadgeQueued)
	}

	if ms.AgentState != "" && ms.AgentState != mission.AgentUnknown {
		parts = append(parts, ms.AgentState.String())
	}

	if ms.WaitingFor != "" {
		parts = append(parts, ms.WaitingFor)
	}

	for _, b := range ms.AllBadges() {
		if b.Detail != "" {
			parts = append(parts, b.Kind+":"+b.Detail)

			continue
		}

		parts = append(parts, b.Kind)
	}

	if ms.LaunchError != "" {
		parts = append(parts, "launch failed: "+firstLine(ms.LaunchError))
	}

	return strings.Join(parts, " · ")
}

// inheritTarget resolves which mission a new one copies its setup from.
//
// An explicit --from wins. Otherwise Q_MISSION_ID, which only the launch script q
// writes, names the mission the agent is currently running inside -- so the
// default is the mission the agent can see the context of, which is the one thing
// a follow-up is most likely to need copied.
func inheritTarget(flag string) mission.MissionID {
	if flag != "" {
		return mission.MissionID(flag)
	}

	return mission.MissionID(os.Getenv(mission.EnvMissionID))
}

// wantsModelDefault reports whether an unnamed model should be resolved from the
// daemon's catalog before the mission is created.
//
// Not when inheriting. The parent's model is the answer, and a parent that named
// none should leave its children on the agent's own default rather than being
// quietly upgraded out of step with the mission they were copied from.
func wantsModelDefault(model string, from mission.MissionID) bool {
	return model == "" && from == ""
}

// parseToolFlag turns --tool into a Tool, leaving it empty when the flag was not
// given.
//
// Empty is a real answer here, not a missing one: it is what lets the daemon
// either apply its own default or inherit the parent's agent, and a flag that
// defaulted to "claude" would report claude on every request and make inheriting
// one impossible.
func parseToolFlag(value string) (mission.Tool, error) {
	if strings.TrimSpace(value) == "" {
		return "", nil
	}

	return mission.ParseTool(value)
}

// orDefaultTool names a concrete agent for the lookups that need one, matching
// the fallback the daemon applies to an empty tool.
func orDefaultTool(t mission.Tool) mission.Tool {
	if t == "" {
		return mission.DefaultTool
	}

	return t
}

// defaultModelFor asks the daemon what a new mission on this agent should run
// on, so an unflagged `q mission add` matches what the board would have offered.
//
// A daemon that cannot answer is not an error: the mission is created with no
// model, which leaves the agent on its own default. That is strictly better than
// refusing to create it over a catalog that is only ever advisory.
func defaultModelFor(
	ctx context.Context,
	c modelClient,
	tool mission.Tool,
	effort string,
) (string, string) {
	sets, err := c.Models(ctx)
	if err != nil {
		return "", effort
	}

	set := sets[tool]

	if effort == "" {
		effort = set.DefaultEffort
	}

	// An effort the chosen model does not accept is dropped rather than sent: it
	// would be rejected by the agent at launch, in a detached pane.
	if !set.ValidEffort(set.Default, effort) {
		effort = ""
	}

	return set.Default, effort
}

// modelLabel renders a mission's model, with its effort appended when it has
// one, e.g. "opus/high".
func modelLabel(ms mission.Mission) string {
	if ms.Effort == "" {
		return ms.Model
	}

	return ms.Model + "/" + ms.Effort
}
