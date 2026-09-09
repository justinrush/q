package main

import (
	"context"
	"fmt"
	"log/slog"
	"time"

	"github.com/justinrush/q/internal/agy"
	"github.com/justinrush/q/internal/claude"
	"github.com/justinrush/q/internal/codex"
	"github.com/justinrush/q/internal/daemon"
	"github.com/justinrush/q/internal/debrief"
	"github.com/justinrush/q/internal/git"
	"github.com/justinrush/q/internal/launch"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/runner"
	"github.com/justinrush/q/internal/terminal"
	"github.com/justinrush/q/internal/usage"
)

// assembleService builds the daemon's object graph from the resolved settings.
//
// This is the one place that knows which concrete implementation satisfies each
// of the daemon's interfaces. Every internal package below takes only what it
// needs and never sees the configuration itself, so adding an agent or swapping
// a terminal is a change here rather than in any of them.
//
// Missing tooling is a warning rather than a failure: browsing and editing
// operations still works without git or tmux, and the actions that need them are
// refused with an explanation instead of failing halfway through provisioning a
// mission.
func assembleService(
	ctx context.Context,
	dirs paths.Dirs,
	s settings,
	logger *slog.Logger,
	version string,
) (*daemon.Service, func(), error) {
	store, err := mission.Open(dirs)
	if err != nil {
		return nil, nil, err
	}

	hub := daemon.NewHub()

	opts := []daemon.Option{
		daemon.WithLogger(logger),
		daemon.WithClock(time.Now),
		daemon.WithHealer(claude.NewRegistry("")),
		daemon.WithModelRefresh(s.Agents.ModelRefresh),
	}

	// Metering runs off files the agent has already written, so it is wired
	// before the tooling check below: a board that cannot find git or tmux can
	// still tell you what the missions it is showing have cost.
	for _, meter := range metersFor(s) {
		opts = append(opts, daemon.WithMeter(meter))
	}

	tools, err := requiredTools(s)
	if err != nil {
		logger.Warn("agent tooling is unavailable", "error", err)

		return daemon.NewService(store, hub, dirs, opts...), func() {}, nil
	}

	run := runner.OS{Logger: logger}
	gitc := git.New(tools[toolGit], run)
	tmux := terminal.NewTmux(tools[toolTmux], run)

	window, err := openerFor(s, tools[toolOsaScript], run)
	if err != nil {
		return nil, nil, err
	}

	opts = append(opts, daemon.WithBrancher(gitc))

	workspace := git.NewProvisioner(dirs, gitc, tmux,
		git.WithLogger(logger),
		git.WithBranchPrefix(s.Git.BranchPrefix),
	)

	launchOpts := []launch.Option{launch.WithLogger(logger)}
	for _, agent := range agentsFor(s) {
		launchOpts = append(launchOpts, launch.WithAgent(agent))
	}

	for _, prober := range probersFor(s, run, version) {
		opts = append(opts, daemon.WithProber(prober))
	}

	launcher := launch.New(dirs, workspace, tmux, launchOpts...)

	stop := func() {}

	// The codex runtime is lazy: it starts no external process until a live codex
	// mission is polled, so configuring it costs nothing on a claude-only board.
	if bin, err := resolveTool(s, toolCodex); err == nil {
		manager := codex.NewManager(ctx, bin, version, run)
		opts = append(opts, daemon.WithRuntime(mission.ToolCodex, codex.NewRuntime(manager)))
		stop = func() { _ = manager.Close() }
	}

	opts = append(opts,
		daemon.WithLauncher(launcher),
		daemon.WithMessenger(launcher),
		daemon.WithReclaimer(launcher),
		daemon.WithProbe(tmux),
		daemon.WithDebriefer(debrief.New(gitc, tmux, window,
			debrief.WithLogger(logger),
			debrief.WithEditor(s.Editor.Command),
		)),
	)

	return daemon.NewService(store, hub, dirs, opts...), stop, nil
}

// agentsFor builds an agent for every tool whose binary this machine has.
//
// An agent q cannot find is simply absent: a board with only claude installed
// refuses a codex mission with an explanation rather than failing to start.
func agentsFor(s settings) []mission.Agent {
	var agents []mission.Agent

	if bin, err := resolveTool(s, toolClaude); err == nil {
		agents = append(agents, claude.New(bin, claude.Options{Args: s.Agents.Claude.Args}))
	}

	if bin, err := resolveTool(s, toolCodex); err == nil {
		agents = append(agents, codex.New(bin, codex.Options{
			Args:      s.Agents.Codex.Args,
			Profile:   s.Agents.Codex.Profile,
			ConfigDir: s.Agents.Codex.ConfigDir,
		}))
	}

	if bin, err := resolveTool(s, toolAgy); err == nil {
		agents = append(agents, agy.New(bin, agy.Options{Args: s.Agents.Agy.Args}))
	}

	return agents
}

// metersFor builds a meter for every agent whose consumption q can read.
//
// Claude and codex write their usage down and both hand q the path on their hooks,
// so both are metered. What differs is pricing: q ships rates for the claude
// models and none for the ones codex runs, so a codex card reports its token
// count until rates for its model are added under "cost" in the config.
func metersFor(s settings) []mission.Meter {
	if s.Cost.Disabled {
		return nil
	}

	pricing := pricingFor(s)

	// Agy 1.1.27 transcripts contain steps and text, but no usage or quota
	// records. Do not reuse another agent's parser and report fictional totals.
	return []mission.Meter{usage.NewClaude(pricing), usage.NewCodex(pricing)}
}

// pricingFor layers the user's rates over the built-in table, so a model q does
// not ship a price for is one config key rather than one release away.
func pricingFor(s settings) usage.Pricing {
	pricing := usage.DefaultPricing()

	for model, price := range s.Cost.Models {
		pricing.Models[model] = usage.ModelPrice{
			Input:     price.Input,
			Output:    price.Output,
			CacheRead: price.CacheRead,
		}
	}

	return pricing
}

// probersFor builds a model prober for every agent this machine has.
//
// The two are asked differently, and the difference is not incidental. claude
// answers a control request with its own model list, so its prober runs the
// binary. codex has no interface q can ask, so its prober reads the
// configuration file codex documents. An agent whose binary is missing gets no
// prober at all, which leaves its models unknown rather than guessed at.
func probersFor(s settings, run runner.OS, version string) []mission.ModelProber {
	var probers []mission.ModelProber

	if bin, err := resolveTool(s, toolClaude); err == nil {
		probers = append(probers, withOverrides(
			claude.NewProber(bin, run, claude.ProberOptions{}), s.Agents.Claude))
	}

	if bin, err := resolveTool(s, toolCodex); err == nil {
		probers = append(probers, withOverrides(codex.NewProber(codex.ProberOptions{
			Bin:       bin,
			Version:   version,
			Run:       run,
			ConfigDir: s.Agents.Codex.ConfigDir,
			Profile:   s.Agents.Codex.Profile,
			Models:    s.Agents.Codex.Models,
		}), s.Agents.Codex.agentSettings))
	}

	if bin, err := resolveTool(s, toolAgy); err == nil {
		fallback := append([]string(nil), s.Agents.Agy.Models...)
		if s.Agents.Agy.Model != "" {
			fallback = append(fallback, s.Agents.Agy.Model)
		}
		probers = append(probers, withOverrides(agy.NewProber(bin, run, fallback), s.Agents.Agy))
	}
	return probers
}

// requiredTools resolves everything the configuration cannot start without.
func requiredTools(s settings) (map[toolName]string, error) {
	resolver := newToolResolver(toolOptionsFor(s))
	if err := resolver.Check(); err != nil {
		return nil, err
	}

	out := map[toolName]string{}

	for _, tool := range requiredToolsFor(s) {
		path, err := resolver.Path(tool)
		if err != nil {
			return nil, err
		}

		out[tool] = path
	}

	return out, nil
}

// resolveTool locates one optional tool.
func resolveTool(s settings, tool toolName) (string, error) {
	return newToolResolver(toolOptionsFor(s)).Path(tool)
}

// openerFor picks the window opener the user's configuration asks for.
func openerFor(s settings, scriptBin string, run runner.Runner) (terminal.Opener, error) {
	switch s.Terminal.Mode {
	case terminalNone:
		return terminal.NewManual(), nil
	case terminalCommand:
		return terminal.NewCommand(s.Terminal.Command, run)
	case terminalGhostty, "":
		return terminal.NewGhostty(scriptBin, run), nil
	default:
		return nil, fmt.Errorf("unknown terminal.mode %q", s.Terminal.Mode)
	}
}
