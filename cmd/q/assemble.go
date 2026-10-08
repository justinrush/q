package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os/exec"
	"path/filepath"
	"slices"
	"time"

	"github.com/justinrush/q/internal/agy"
	"github.com/justinrush/q/internal/azure"
	"github.com/justinrush/q/internal/claude"
	"github.com/justinrush/q/internal/codex"
	"github.com/justinrush/q/internal/daemon"
	"github.com/justinrush/q/internal/debrief"
	"github.com/justinrush/q/internal/git"
	"github.com/justinrush/q/internal/k8s"
	"github.com/justinrush/q/internal/launch"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/opencode"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/remote"
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
		daemon.WithMaxConcurrent(s.Queue.MaxConcurrent),
		daemon.WithHostName(s.Remote.Name),
		daemon.WithVersion(version),
		daemon.WithSyncInterval(s.Remote.Interval),
		daemon.WithTakeoverAfter(s.Remote.TakeoverAfter),
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

	workspace := git.NewProvisioner(dirs, gitc, tmux,
		git.WithLogger(logger),
		git.WithBranchPrefix(s.Git.BranchPrefix),
	)

	pairing, err := pairingOptions(s, dirs, run, gitc, workspace)
	if err != nil {
		return nil, nil, err
	}

	opts = append(append(opts, daemon.WithBrancher(gitc)), pairing...)

	launchOpts := []launch.Option{launch.WithLogger(logger), launch.WithHostSnapshot(store.Snapshot)}
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

// pairingOptions wires what sharing missions with another machine needs.
//
// All of it is harmless on a q that is never paired. Locating repositories and
// snapshotting worktrees are only ever asked for by an exchange, and an
// exchange only happens when one side has been told how to reach the other.
func pairingOptions(
	s settings,
	dirs paths.Dirs,
	run runner.Runner,
	gitc *git.Client,
	workspace *git.Provisioner,
) ([]daemon.Option, error) {
	opts := []daemon.Option{
		daemon.WithLocator(git.NewLocator(gitc, git.ScanOptions{
			Roots:    s.Repos.Roots,
			MaxDepth: s.Repos.MaxDepth,
			Skip:     s.Repos.Skip,
		})),
		daemon.WithWorktrees(git.NewMirrors(gitc, workspace)),
	}

	link, err := remoteFor(s, dirs, run)
	if err != nil {
		return nil, err
	}

	if link != nil {
		opts = append(opts, daemon.WithRemote(remote.NewPeer(*link)))
	}

	witness, err := witnessFor(s)
	if err != nil {
		return nil, err
	}

	if witness != nil {
		opts = append(opts, daemon.WithWitness(witness))
	}

	return opts, nil
}

// witnessFor builds the pair's witness, or nil when none is configured.
//
// A witness that cannot be built is an error and not a warning. A daemon that
// started without the one it was told to consult would take over, or carry on,
// in exactly the case the witness was configured to prevent.
func witnessFor(s settings) (daemon.Witness, error) {
	w := s.Remote.Witness

	switch {
	case w.Kubernetes != nil && w.Azure != nil:
		return nil, errors.New("remote.witness names both kubernetes and azure; a pair has one witness")
	case w.Kubernetes != nil:
		cfg := *w.Kubernetes

		if cfg.Kubeconfig != "" {
			path, err := absPath(cfg.Kubeconfig)
			if err != nil {
				return nil, fmt.Errorf("remote.witness.kubernetes.kubeconfig: %w", err)
			}

			cfg.Kubeconfig = path
		}

		lease, err := k8s.New(cfg)
		if err != nil {
			return nil, fmt.Errorf("remote.witness.kubernetes: %w", err)
		}

		return lease, nil
	case w.Azure != nil:
		blob, err := azure.New(*w.Azure)
		if err != nil {
			return nil, fmt.Errorf("remote.witness.azure: %w", err)
		}

		return blob, nil
	default:
		return nil, nil
	}
}

// remoteFor builds the connection to the paired q, or nil when this machine
// does not dial one.
//
// The command's first element is resolved here, once, for the same reason every
// other tool is: the daemon may not have the PATH an interactive shell does,
// and a pairing that works from a terminal and fails from the daemon is the
// hardest kind of broken to notice.
func remoteFor(s settings, dirs paths.Dirs, run runner.Runner) (*remote.Command, error) {
	if len(s.Remote.SSH) == 0 {
		return nil, nil
	}

	argv := slices.Clone(s.Remote.SSH)

	if !filepath.IsAbs(argv[0]) {
		resolved, err := exec.LookPath(argv[0])
		if err != nil {
			return nil, fmt.Errorf("remote.ssh: %q was not found on PATH: %w", argv[0], err)
		}

		argv[0] = resolved
	}

	return &remote.Command{Argv: argv, Bin: s.Remote.Bin, ControlDir: dirs.State, Run: run}, nil
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

	if bin, err := resolveTool(s, toolOpencode); err == nil {
		agents = append(agents, opencode.New(bin, opencode.Options{Args: s.Agents.Opencode.Args}))
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

	if bin, err := resolveTool(s, toolOpencode); err == nil {
		fallback := append([]string(nil), s.Agents.Opencode.Models...)
		if s.Agents.Opencode.Model != "" {
			fallback = append(fallback, s.Agents.Opencode.Model)
		}
		probers = append(probers, withOverrides(opencode.NewProber(bin, run, fallback), s.Agents.Opencode))
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
	case terminalNone, terminalCurrent:
		return terminal.NewManual(), nil
	case terminalCommand:
		return terminal.NewCommand(s.Terminal.Command, run)
	case terminalGhostty, "":
		return terminal.NewGhostty(scriptBin, run), nil
	default:
		return nil, fmt.Errorf("unknown terminal.mode %q", s.Terminal.Mode)
	}
}
