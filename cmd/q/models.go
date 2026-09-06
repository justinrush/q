package main

import (
	"context"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/spf13/cobra"
)

func buildModelsSubcommand() *cobra.Command {
	var (
		asJSON  bool
		refresh bool
	)

	cmd := &cobra.Command{
		Use:   "models",
		Short: "Show the models each agent offers",
		Long: "Report the models a mission can be launched on, per agent, and which one a " +
			"new mission gets by default.\n\n" +
			"The daemon learns this by asking each agent, and caches the answer. Use " +
			"--refresh to ask again now, which starts each agent and so takes a moment.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			sets, err := fetchModels(cmd.Context(), c, refresh)
			if err != nil {
				return err
			}

			if asJSON {
				return writeJSONOut(cmd.OutOrStdout(), sets)
			}

			return renderModels(cmd.OutOrStdout(), sets)
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit JSON instead of a table")
	cmd.Flags().BoolVar(&refresh, "refresh", false, "Ask each agent again rather than using the cache")

	return cmd
}

// modelClient is the part of the daemon client this command needs, declared here
// so the rendering can be tested without one.
type modelClient interface {
	Models(ctx context.Context) (map[mission.Tool]mission.ModelSet, error)
	RefreshModels(ctx context.Context) (map[mission.Tool]mission.ModelSet, error)
}

// fetchModels reads the catalog, re-probing first when asked.
func fetchModels(
	ctx context.Context,
	c modelClient,
	refresh bool,
) (map[mission.Tool]mission.ModelSet, error) {
	if refresh {
		return c.RefreshModels(ctx)
	}

	return c.Models(ctx)
}

// renderModels prints the catalog, one block per agent.
func renderModels(out io.Writer, sets map[mission.Tool]mission.ModelSet) error {
	rep := newReport()

	var known int

	// Iterated over the tool list rather than the map so the order is q's own and
	// an agent with nothing known is still reported as such.
	for _, tool := range mission.Tools {
		set, ok := sets[tool]
		if !ok || (len(set.Options) == 0 && set.Err == "") {
			rep.line("%s  not known — no %s on this machine, or it has not been asked yet",
				tool, tool)
			rep.line("")

			continue
		}

		known++

		rep.line("%s  %s", tool, probeAge(set))

		if set.Err != "" {
			rep.line("  last attempt failed: %s", firstLine(set.Err))
		}

		for _, opt := range set.Options {
			rep.row("  %s\t%s\t%s", opt.Value, modelMarks(set, opt), opt.Detail)
		}

		rep.line("")
	}

	if known == 0 {
		rep.line("Run q doctor to see which agents q can find.")
	}

	_, err := io.WriteString(out, rep.String())

	return err
}

// modelMarks describes one model in a word or two: whether it is the default,
// and what effort levels it takes.
func modelMarks(set mission.ModelSet, opt mission.ModelOption) string {
	var parts []string

	if opt.Value == set.Default {
		parts = append(parts, "default")
	}

	if len(opt.Efforts) > 0 {
		parts = append(parts, "effort: "+strings.Join(opt.Efforts, "|"))
	}

	return strings.Join(parts, "  ")
}

// probeAge says how current an agent's answer is, which is the thing to check
// first when the board offers a model that looks wrong.
func probeAge(set mission.ModelSet) string {
	if set.ProbedAt.IsZero() {
		return "never asked; showing what was cached"
	}

	return "asked " + humanAge(time.Since(set.ProbedAt)) + " ago"
}

// humanAge renders a duration at the coarsest useful unit.
func humanAge(d time.Duration) string {
	switch {
	case d < time.Minute:
		return "moments"
	case d < time.Hour:
		return fmt.Sprintf("%dm", int(d.Minutes()))
	case d < 24*time.Hour:
		return fmt.Sprintf("%dh", int(d.Hours()))
	default:
		return fmt.Sprintf("%dd", int(d.Hours()/24))
	}
}

// overrideProber applies q's own configuration on top of what an agent reports.
//
// It wraps rather than being folded into each prober because the override is a q
// setting, not an agent one: the probers answer "what does this agent offer",
// and this answers "what has the user told q to prefer". Keeping them apart is
// what lets a prober be tested against the agent's real output alone.
type overrideProber struct {
	mission.ModelProber
	model  string
	effort string
}

// withOverrides wraps a prober when the user configured a preference, and
// returns it untouched when they did not.
func withOverrides(p mission.ModelProber, s agentSettings) mission.ModelProber {
	if s.Model == "" && s.Effort == "" {
		return p
	}

	return &overrideProber{ModelProber: p, model: s.Model, effort: s.Effort}
}

// Probe asks the agent and then applies the configured preference.
//
// The override is added to the option list when the agent did not report it, so
// a user who names a model q has not heard of still sees it selected rather than
// silently dropped.
func (p *overrideProber) Probe(ctx context.Context) (mission.ModelSet, error) {
	set, err := p.ModelProber.Probe(ctx)
	if err != nil {
		return set, err
	}

	if p.model != "" {
		set.Default = p.model

		if _, known := set.Option(p.model); !known {
			set.Options = append(set.Options, mission.ModelOption{
				Value:  p.model,
				Label:  p.model,
				Detail: "from your q configuration",
			})
		}
	}

	if p.effort != "" {
		set.DefaultEffort = p.effort
	}

	return set, nil
}
