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
			"This rewrites the config file, keeping every setting and losing only its " +
			"formatting.",
		Example: "  q remote setup mini.local\n" +
			"  q remote setup -J bastion.example.com dev-vm",
		Args:               cobra.MinimumNArgs(1),
		DisableFlagParsing: false,
		RunE: func(cmd *cobra.Command, args []string) error {
			path := configPath()

			if err := writeRemoteConfig(path, append([]string{"ssh"}, args...), bin); err != nil {
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

// restartAndSync replaces the daemon, which reads its configuration once at
// start, and runs an exchange with the peer it now knows how to reach.
func restartAndSync(cmd *cobra.Command) (api.RemoteStatus, error) {
	ctx := cmd.Context()

	dirs, err := paths.Resolve(pathOverrides())
	if err != nil {
		return api.RemoteStatus{}, err
	}

	if _, err := api.Connect(ctx, dirs); err == nil {
		if err := api.Stop(dirs); err != nil {
			return api.RemoteStatus{}, err
		}

		if err := waitForStop(ctx, dirs); err != nil {
			return api.RemoteStatus{}, err
		}
	}

	c, err := api.Ensure(ctx, dirs)
	if err != nil {
		return api.RemoteStatus{}, err
	}

	return c.RemoteSync(ctx)
}

func buildRemoteStatusSubcommand() *cobra.Command {
	var asJSON bool

	cmd := &cobra.Command{
		Use:   "status",
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
			"This changes only this machine. Run it on both to end a pairing, and on the " +
			"primary also remove remote.ssh from the config file, or the next exchange " +
			"will pair the two again.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			c, err := connectDaemon(cmd.Context())
			if err != nil {
				return err
			}

			if _, err := c.RemoteForget(cmd.Context()); err != nil {
				return err
			}

			_, err = fmt.Fprintln(cmd.OutOrStdout(), "forgotten")

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
}

// hostLabel renders a host as its name and id.
func hostLabel(h mission.HostInfo) string {
	if h.Name == "" {
		return string(h.ID)
	}

	return fmt.Sprintf("%s (%s)", h.Name, h.ID)
}
