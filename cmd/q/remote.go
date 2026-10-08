package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/remote"
	"github.com/justinrush/q/internal/runner"
	"github.com/spf13/cobra"
)

func buildRemoteSubcommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "remote",
		Short: "Pair this q with one on another machine",
		Long: "Share missions between two machines: the one you sit at, and one that is " +
			"always on.\n\n" +
			"The machine you sit at is the primary. It dials the other over ssh, runs " +
			"agents whenever it is awake, and has the final say when the two disagree. " +
			"The always-on machine is the secondary: it runs queued missions while the " +
			"primary is away and hands them back when it returns. Both keep a worktree " +
			"for every running mission, so the code is local wherever you look at it.\n\n" +
			"Set it up once, from the primary:\n\n" +
			"  q remote setup mini.local\n\n" +
			"The secondary needs no configuration, only q installed and its daemon running.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	cmd.AddCommand(
		buildRemoteSetupSubcommand(),
		buildRemoteStatusSubcommand(),
		buildRemoteSyncSubcommand(),
		buildRemoteForgetSubcommand(),
		buildRemoteWitnessSubcommand(),
	)

	return cmd
}

func buildRemoteSetupSubcommand() *cobra.Command {
	var bin string

	cmd := &cobra.Command{
		Use:   "setup <ssh-destination> [ssh-argument...]",
		Short: "Make this machine the primary and pair it with another",
		Long: "Record how to reach the other machine, restart the daemon so it takes " +
			"effect, and run the first exchange.\n\n" +
			"The arguments are what you would give ssh to get a shell there: a host from " +
			"your ssh config, user@host, or options followed by a host. To reach it some " +
			"other way, set remote.ssh in the config file to any command that does.\n\n" +
			"The other machine is asked who it is before anything is saved. Setup refuses " +
			"if either machine is already paired with a third, and says which `q remote " +
			"forget` clears the way. A machine has one peer.\n\n" +
			"q's own flags go before the destination. Everything from the destination on " +
			"is handed to ssh.\n\n" +
			"This rewrites the config file, keeping every setting and losing only its " +
			"formatting.",
		Example: "  q remote setup mini.local\n" +
			"  q remote setup -J bastion.example.com dev-vm\n" +
			"  q remote setup --bin /opt/q/bin/q mini.local",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := configPath()
			ssh := append([]string{"ssh"}, args...)

			// Asked before anything is written. A setup that saved the address
			// and then failed would leave this machine dialing a host it must
			// not pair with, every few seconds, until someone noticed.
			if err := probePairing(cmd, ssh, bin); err != nil {
				return err
			}

			if err := writeRemoteConfig(path, ssh, bin); err != nil {
				return err
			}

			rep := newReport()
			rep.line("wrote remote.ssh to %s", path)

			status, err := restartAndSync(cmd)
			if err != nil {
				_, _ = io.WriteString(cmd.OutOrStdout(), rep.String())

				return fmt.Errorf("the configuration is saved, but the first exchange failed: %w", err)
			}

			describeRemote(rep, status)

			_, err = io.WriteString(cmd.OutOrStdout(), rep.String())

			return err
		},
	}

	cmd.Flags().StringVar(&bin, "bin", "",
		"Path to q on the other machine, as its shell resolves it (default ~/.local/bin/q)")
	// Everything after the destination belongs to ssh, including its own flags.
	cmd.Flags().SetInterspersed(false)

	return cmd
}

// probePairing asks the machine at the far end of ssh who it is, and refuses
// unless pairing this machine with it would leave both with exactly one peer.
func probePairing(cmd *cobra.Command, ssh []string, bin string) error {
	ctx := cmd.Context()

	dirs, err := paths.Resolve(pathOverrides())
	if err != nil {
		return err
	}

	probe := cfg
	probe.Remote.SSH = ssh
	probe.Remote.Bin = firstNonEmpty(bin, cfg.Remote.Bin)

	link, err := remoteFor(probe, dirs, runner.OS{})
	if err != nil {
		return err
	}

	target, err := remote.NewPeer(*link).Status(ctx)
	if err != nil {
		return fmt.Errorf("could not ask the other machine who it is: %w", err)
	}

	c, err := connectDaemon(ctx)
	if err != nil {
		return err
	}

	local, err := c.RemoteStatus(ctx)
	if err != nil {
		return err
	}

	return checkPairable(local, target)
}

// checkPairable decides whether this machine may become the primary of target.
//
// A machine has one peer. Each refusal here is a way of ending up with two, or
// with half of one, and each names the command that clears the way.
func checkPairable(local, target api.RemoteStatus) error {
	switch {
	case target.Self.ID == local.Self.ID:
		return fmt.Errorf("that is this machine, or a copy of its q state: both report host id %s", local.Self.ID)
	case local.Peer != nil && local.Peer.ID != target.Self.ID:
		return fmt.Errorf(
			"this machine is already paired with %s; run `q remote forget` here first, then set up %s",
			hostLabel(*local.Peer), hostLabel(target.Self))
	case target.Role == mission.RolePrimary:
		return fmt.Errorf(
			"%s is itself set up to dial a peer, so it cannot be the always-on half of a pair; "+
				"run `q remote forget` there if that is no longer wanted", hostLabel(target.Self))
	case target.Peer != nil && target.Peer.ID != local.Self.ID:
		return fmt.Errorf(
			"%s is already paired with %s; run `q remote forget` on %s first",
			hostLabel(target.Self), hostLabel(*target.Peer), target.Self.Name)
	default:
		return nil
	}
}

// clearRemoteConfig removes the saved command that reaches the peer, reporting
// whether there was one. Without this a primary that had forgotten its peer
// would dial it again on the next tick and pair the two straight back.
func clearRemoteConfig(path string) (bool, error) {
	file, err := readConfigFile(path)
	if err != nil || file.Remote == nil || len(file.Remote.SSH) == 0 {
		return false, err
	}

	file.Remote.SSH = nil

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return false, fmt.Errorf("encoding %s: %w", path, err)
	}

	if err := os.WriteFile(path, append(data, '\n'), paths.FileMode); err != nil {
		return false, fmt.Errorf("writing %s: %w", path, err)
	}

	return true, nil
}

// writeRemoteConfig records the command that reaches the peer, leaving every
// other setting in the file as it was.
//
// The file is read and written in its own shape rather than through the
// resolved settings. Writing the resolved settings back would freeze every
// default into the file, and an environment override along with them.
func writeRemoteConfig(path string, ssh []string, bin string) error {
	if path == "" {
		return errors.New("no config path could be resolved; set Q_CONFIG or pass --config")
	}

	file, err := readConfigFile(path)
	if err != nil {
		return err
	}

	if file.Remote == nil {
		file.Remote = &remoteConfig{}
	}

	file.Remote.SSH = ssh

	if bin != "" {
		file.Remote.Bin = bin
	}

	data, err := json.MarshalIndent(file, "", "  ")
	if err != nil {
		return fmt.Errorf("encoding %s: %w", path, err)
	}

	if err := os.MkdirAll(filepath.Dir(path), paths.DirMode); err != nil {
		return fmt.Errorf("creating %s: %w", filepath.Dir(path), err)
	}

	if err := os.WriteFile(path, append(data, '\n'), paths.FileMode); err != nil {
		return fmt.Errorf("writing %s: %w", path, err)
	}

	return nil
}

// describeForgotten says what ending a pairing did and what, if anything, is
// still to do by hand.
func describeForgotten(forgotten api.Forgotten, cleared bool) string {
	rep := newReport()

	if forgotten.Peer == nil {
		rep.line("this machine was not paired")
	} else {
		rep.line("forgot %s", hostLabel(*forgotten.Peer))
	}

	if cleared {
		rep.line("removed remote.ssh from the config file and restarted the daemon")
	}

	if forgotten.Peer != nil && !forgotten.PeerTold {
		rep.line("")
		rep.line("%s was not told. Run `q remote forget` there too, or once it has gone",
			forgotten.Peer.Name)
		rep.line("without hearing from this machine it will start running this machine's missions.")
	}

	return rep.String()
}

// restartDaemon replaces the daemon, which reads its configuration once at
// start.
func restartDaemon(cmd *cobra.Command) error {
	ctx := cmd.Context()

	dirs, err := paths.Resolve(pathOverrides())
	if err != nil {
		return err
	}

	if _, err := api.Connect(ctx, dirs); err == nil {
		if err := api.Stop(dirs); err != nil {
			return err
		}

		if err := waitForStop(ctx, dirs); err != nil {
			return err
		}
	}

	_, err = api.Ensure(ctx, dirs)

	return err
}

// restartAndSync restarts the daemon and runs an exchange with the peer it now
// knows how to reach.
func restartAndSync(cmd *cobra.Command) (api.RemoteStatus, error) {
	if err := restartDaemon(cmd); err != nil {
		return api.RemoteStatus{}, err
	}

	c, err := connectDaemon(cmd.Context())
	if err != nil {
		return api.RemoteStatus{}, err
	}

	return c.RemoteSync(cmd.Context())
}

func buildRemoteStatusSubcommand() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   useStatus,
		Short: "Show which machine this q is paired with and when they last spoke",
		Args:  cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			status, err := c.RemoteStatus(cmd.Context())
			if err != nil {
				return err
			}

			if asJSON {
				return writeJSONOut(cmd.OutOrStdout(), status)
			}

			rep := newReport()
			describeRemote(rep, status)

			_, err = io.WriteString(cmd.OutOrStdout(), rep.String())

			return err
		},
	}

	cmd.Flags().BoolVar(&asJSON, "json", false, "Emit JSON instead of text")

	return cmd
}

func buildRemoteSyncSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "sync",
		Short: "Exchange state with the paired q now",
		Long: "Run an exchange now rather than waiting for the next one, and report " +
			"whether it worked. Only the primary dials, so this runs there.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			status, err := c.RemoteSync(cmd.Context())
			if err != nil {
				return err
			}

			rep := newReport()
			describeRemote(rep, status)

			_, err = io.WriteString(cmd.OutOrStdout(), rep.String())

			return err
		},
	}
}

func buildRemoteForgetSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "forget",
		Short: "End the pairing on this machine",
		Long: "Forget the paired q. Missions it was running are handed to this machine " +
			"and moved to debrief, since nothing will report on them again.\n\n" +
			"Run on the primary, this also tells the other machine to forget, and " +
			"removes remote.ssh from the config file so the two do not pair again on " +
			"the next exchange. If the other machine cannot be reached it says so, and " +
			"the command has to be run there too: a machine left believing in the " +
			"pairing will eventually start running this one's missions.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			forgotten, err := c.RemoteForget(cmd.Context())
			if err != nil {
				return err
			}

			cleared, err := clearRemoteConfig(configPath())
			if err != nil {
				return err
			}

			if cleared {
				// The daemon read the address at start and would go on dialing it.
				if err := restartDaemon(cmd); err != nil {
					return err
				}
			}

			_, err = io.WriteString(cmd.OutOrStdout(), describeForgotten(forgotten, cleared))

			return err
		},
	}
}

// describeRemote renders a pairing for a person.
func describeRemote(rep *report, status api.RemoteStatus) {
	rep.row("this host\t%s", hostLabel(status.Self))

	if status.Role == mission.RoleStandalone {
		rep.row("paired\tno")
		rep.line("")
		rep.line("Run `q remote setup <ssh-destination>` on the machine you sit at to pair two.")

		return
	}

	rep.row("role\t%s", status.Role)

	if status.Peer == nil {
		rep.row("paired with\tnobody yet")
	} else {
		rep.row("paired with\t%s", hostLabel(*status.Peer))
	}

	switch {
	case status.Error != "":
		rep.row("link\tdown: %s", status.Error)
	case status.LastSyncAt.IsZero():
		rep.row("link\tno exchange since this daemon started")
	default:
		rep.row("link\tup, last exchange %s ago", time.Since(status.LastSyncAt).Round(time.Second))
	}

	if status.Pending > 0 {
		rep.row("pending\t%d request(s) waiting to reach the other machine", status.Pending)
	}

	if status.Witness != nil {
		describeWitness(rep, status)
	}
}

// hostLabel renders a host as its name and id.
func hostLabel(h mission.HostInfo) string {
	if h.Name == "" {
		return string(h.ID)
	}

	return fmt.Sprintf("%s (%s)", h.Name, h.ID)
}
