package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/azure"
	"github.com/justinrush/q/internal/daemon"
	"github.com/justinrush/q/internal/k8s"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
	"github.com/spf13/cobra"
)

// useStatus is the name every `status` subcommand goes by.
const useStatus = "status"

// witnessProbeTimeout bounds the check a witness command makes before saving.
const witnessProbeTimeout = 20 * time.Second

func buildRemoteWitnessSubcommand() *cobra.Command {
	cmd := &cobra.Command{
		Use:   "witness",
		Short: "Give the pair a third place to check before one acts on the other's silence",
		Long: "Without a witness, the always-on machine takes over when it has not heard " +
			"from the laptop for remote.takeoverAfter, and it cannot tell a closed lid " +
			"from a laptop that is awake on a network it is not on. On a split network " +
			"both then run the same missions.\n\n" +
			"A witness is somewhere both machines can reach when they cannot reach each " +
			"other: a Kubernetes Lease, or a blob in an Azure storage account. It holds " +
			"one claim. The laptop renews it while awake, and the always-on machine takes " +
			"over only once that claim has lapsed and it has put its own there. A laptop " +
			"that wakes to find the other machine's claim, or can reach neither the " +
			"witness nor the other machine, stands by: it stops the agents the other " +
			"machine would have taken over and starts them again, or accepts the " +
			"takeover, once the two have spoken.\n\n" +
			"Each machine reaches the witness with its own credentials, so it is set up " +
			"on both, naming the same record:\n\n" +
			"  q remote witness kubernetes --namespace q        # on each machine\n" +
			"  q remote witness azure mystorageaccount          # or\n\n" +
			"Until both name the same witness, nothing is taken over at all, and " +
			"`q remote status` says so.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error { return cmd.Help() },
	}

	cmd.AddCommand(
		buildWitnessKubernetesSubcommand(),
		buildWitnessAzureSubcommand(),
		buildWitnessStatusSubcommand(),
		buildWitnessTakeSubcommand(),
		buildWitnessClearSubcommand(),
	)

	return cmd
}

func buildWitnessKubernetesSubcommand() *cobra.Command {
	var k kubernetesWitnessConfig

	cmd := &cobra.Command{
		Use:   "kubernetes",
		Short: "Keep the pair's claim in a Kubernetes Lease",
		Long: "Use a coordination.k8s.io Lease as the witness, reached with this machine's " +
			"kubeconfig. The Lease is read and written back once to prove the " +
			"credentials can do both, and nothing is saved if they cannot.\n\n" +
			"The namespace and lease name must be the same on both machines. The " +
			"kubeconfig and context are this machine's own and need not be.\n\n" +
			"The credentials need get, create and update on leases in that namespace " +
			"and nothing else.",
		Example: "  q remote witness kubernetes\n" +
			"  q remote witness kubernetes --context home --namespace q --lease laptop-and-mini",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return setWitness(cmd, witnessConfig{Kubernetes: &k})
		},
	}

	cmd.Flags().StringVar(&k.Kubeconfig, "kubeconfig", "",
		"Kubeconfig file (default $KUBECONFIG, then ~/.kube/config, then the pod's service account)")
	cmd.Flags().StringVar(&k.Context, "context", "", "Kubeconfig context (default its current one)")
	cmd.Flags().StringVar(&k.Namespace, "namespace", k8s.DefaultNamespace, "Namespace of the Lease")
	cmd.Flags().StringVar(&k.Lease, "lease", k8s.DefaultName, "Name of the Lease")

	return cmd
}

func buildWitnessAzureSubcommand() *cobra.Command {
	var a azureWitnessConfig

	cmd := &cobra.Command{
		Use:   "azure <storage-account>",
		Short: "Keep the pair's claim in a blob in an Azure storage account",
		Long: "Use a blob as the witness, reached with this machine's Azure identity: " +
			"`az login` on a laptop, a managed identity on a VM, or the AZURE_* " +
			"environment variables. The blob is read and written back once to prove " +
			"the identity can do both, and nothing is saved if it cannot.\n\n" +
			"The account, container and blob must be the same on both machines. The " +
			"container has to exist already, and the identity needs the Storage Blob " +
			"Data Contributor role on it.",
		Example: "  q remote witness azure mystorageaccount\n" +
			"  q remote witness azure mystorageaccount --container q --blob laptop-and-vm.json",
		Args: cobra.ExactArgs(1),
		RunE: func(cmd *cobra.Command, args []string) error {
			a.Account = args[0]

			return setWitness(cmd, witnessConfig{Azure: &a})
		},
	}

	cmd.Flags().StringVar(&a.Container, "container", azure.DefaultContainer, "Container holding the blob")
	cmd.Flags().StringVar(&a.Blob, "blob", azure.DefaultBlob, "Name of the blob")
	cmd.Flags().StringVar(&a.Endpoint, "endpoint", "",
		"Blob service URL (default https://<storage-account>.blob.core.windows.net)")

	return cmd
}

// setWitness proves a witness works from this machine, saves it, and restarts
// the daemon to consult it.
func setWitness(cmd *cobra.Command, w witnessConfig) error {
	path := configPath()

	probe := cfg
	probe.Remote.Witness = witnessSettings{}
	applyRemote(&probe, &remoteConfig{Witness: &w})

	witness, err := witnessFor(probe)
	if err != nil {
		return err
	}

	// Checked before anything is written, for the reason setup asks the other
	// machine who it is first: a saved witness that cannot be reached leaves
	// this machine standing by, or never taking over, until someone notices.
	claim, err := probeWitness(cmd.Context(), witness)
	if err != nil {
		return fmt.Errorf("%s cannot be used from this machine, so nothing was saved: %w", witness.Name(), err)
	}

	if err := writeWitnessConfig(path, &w); err != nil {
		return err
	}

	rep := newReport()
	rep.line("wrote remote.witness to %s", path)

	if err := restartDaemon(cmd); err != nil {
		_, _ = io.WriteString(cmd.OutOrStdout(), rep.String())

		return fmt.Errorf("the configuration is saved, but the daemon did not restart: %w", err)
	}

	status, err := remoteStatus(cmd.Context())
	if err != nil {
		return err
	}

	rep.row("witness\t%s", witness.Name())
	rep.row("claim\t%s", claimLabel(claim, status, time.Now()))

	if peer := peerWitness(status); peer != witness.Name() {
		rep.line("")

		if peer == "" {
			rep.line("The other machine has not said it uses this witness. Run the same command there,")
		} else {
			rep.line("The other machine uses %s. Point both at the same record,", peer)
		}

		rep.line("naming the same record; until both do, neither takes over from the other.")
	}

	_, err = io.WriteString(cmd.OutOrStdout(), rep.String())

	return err
}

// probeWitness reads a witness's record and writes it back unchanged, which is
// the least that proves both are permitted. Losing a race to the other machine
// proves the same thing and is not a failure.
func probeWitness(ctx context.Context, w daemon.Witness) (mission.Claim, error) {
	ctx, cancel := context.WithTimeout(ctx, witnessProbeTimeout)
	defer cancel()

	claim, err := w.Read(ctx)
	if err != nil {
		return mission.Claim{}, err
	}

	if err := w.Write(ctx, claim); err != nil && !errors.Is(err, mission.ErrClaimRaced) {
		return mission.Claim{}, err
	}

	return claim, nil
}

// writeWitnessConfig records the witness, or removes it when w is nil, leaving
// every other setting in the file as it was.
func writeWitnessConfig(path string, w *witnessConfig) error {
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

	file.Remote.Witness = w

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

func buildWitnessStatusSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   useStatus,
		Short: "Ask the witness who it names, and show what the daemon makes of it",
		Long: "Read the witness now, from this command and with this machine's " +
			"credentials, and show the claim beside what the daemon last learned. The " +
			"two differ when the daemon has not been restarted since the witness was " +
			"configured, or cannot reach it from where it runs.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			status, err := remoteStatus(cmd.Context())
			if err != nil {
				return err
			}

			rep := newReport()

			witness, err := witnessFor(cfg)
			if err != nil {
				return err
			}

			if witness == nil {
				rep.line("No witness is configured on this machine.")

				if peer := peerWitness(status); peer != "" {
					rep.line("The other machine uses %s; until this one does too, neither takes over.", peer)
				} else {
					rep.line("Run `q remote witness kubernetes` or `q remote witness azure <account>` on both machines.")
				}

				_, err = io.WriteString(cmd.OutOrStdout(), rep.String())

				return err
			}

			rep.row("witness\t%s", witness.Name())

			ctx, cancel := context.WithTimeout(cmd.Context(), witnessProbeTimeout)
			defer cancel()

			if claim, err := witness.Read(ctx); err != nil {
				rep.row("claim\tunreachable: %v", err)
			} else {
				rep.row("claim\t%s", claimLabel(claim, status, time.Now()))
			}

			if status.Witness != nil {
				describeWitness(rep, status)
			} else if status.Role == mission.RoleStandalone {
				rep.row("daemon\tnot paired, so the witness is not consulted")
			}

			_, err = io.WriteString(cmd.OutOrStdout(), rep.String())

			return err
		},
	}
}

func buildWitnessTakeSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "take",
		Short: "Claim the witness for this machine without the other's agreement",
		Long: "Put this machine's claim on the witness, replacing the other's even if it " +
			"still stands. The daemon here notices within remote.interval and acts on " +
			"it: a laptop that was standing by starts its agents again, and an " +
			"always-on machine takes over.\n\n" +
			"This is the override for when you know better than the witness: the other " +
			"machine is gone for good, or you are deliberately off its network and " +
			"want the work here. The other machine is not told. If it is in fact " +
			"running the same missions, both now are, and what each did is kept side " +
			"by side when they next speak.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			witness, err := witnessFor(cfg)
			if err != nil {
				return err
			}

			if witness == nil {
				return errors.New("no witness is configured on this machine")
			}

			status, err := remoteStatus(cmd.Context())
			if err != nil {
				return err
			}

			// The always-on machine's claim does not lapse; see primaryAway in
			// the daemon. The laptop's lasts one takeover window and is renewed.
			var ttl time.Duration
			if status.Role != mission.RoleSecondary {
				ttl = cfg.Remote.TakeoverAfter
			}

			ctx, cancel := context.WithTimeout(cmd.Context(), witnessProbeTimeout)
			defer cancel()

			claim, _, err := daemon.Claim(ctx, witness, status.Self.ID, time.Now, ttl, true)
			if err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"%s now names %s\nthe daemon will act on it within %s\n",
				witness.Name(), claimLabel(claim, status, time.Now()), cfg.Remote.Interval)

			return err
		},
	}
}

func buildWitnessClearSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "clear",
		Short: "Stop consulting a witness on this machine",
		Long: "Remove remote.witness from the config file and restart the daemon. Run it " +
			"on both machines: while only one consults a witness, neither takes over " +
			"from the other. The record itself is left where it is.",
		Args: cobra.NoArgs,
		RunE: func(cmd *cobra.Command, _ []string) error {
			path := configPath()

			file, err := readConfigFile(path)
			if err != nil {
				return err
			}

			if file.Remote == nil || file.Remote.Witness == nil {
				_, err = fmt.Fprintln(cmd.OutOrStdout(), "no witness was configured")

				return err
			}

			if err := writeWitnessConfig(path, nil); err != nil {
				return err
			}

			if err := restartDaemon(cmd); err != nil {
				return err
			}

			_, err = fmt.Fprintf(cmd.OutOrStdout(),
				"removed remote.witness from %s and restarted the daemon\n"+
					"run `q remote witness clear` on the other machine too\n", path)

			return err
		},
	}
}

// remoteStatus asks the daemon about its pairing.
func remoteStatus(ctx context.Context) (api.RemoteStatus, error) {
	c, err := connectDaemon(ctx)
	if err != nil {
		return api.RemoteStatus{}, err
	}

	return c.RemoteStatus(ctx)
}

// peerWitness is the witness the paired machine said it uses, empty when it
// has none or has not said.
func peerWitness(status api.RemoteStatus) string {
	if status.Witness == nil {
		return ""
	}

	return status.Witness.PeerName
}

// hostName names a host a claim mentions in terms of the pair.
func hostName(id mission.HostID, status api.RemoteStatus) string {
	switch {
	case id == "":
		return "nobody"
	case id == status.Self.ID:
		return "this machine"
	case status.Peer != nil && id == status.Peer.ID:
		return hostLabel(*status.Peer)
	default:
		return fmt.Sprintf("%s, which is neither machine of this pair", id)
	}
}

// claimLabel renders a claim for a person.
func claimLabel(claim mission.Claim, status api.RemoteStatus, now time.Time) string {
	if claim.Holder == "" {
		return "nobody yet"
	}

	who := hostName(claim.Holder, status)
	age := now.Sub(claim.RenewedAt).Round(time.Second)

	switch {
	case claim.TTL == 0:
		return fmt.Sprintf("%s, since %s ago, until the two next speak", who, age)
	case claim.Expired(now):
		return fmt.Sprintf("%s, lapsed %s ago", who, (age - claim.TTL).Round(time.Second))
	default:
		return fmt.Sprintf("%s, renewed %s ago, good for another %s", who, age, (claim.TTL - age).Round(time.Second))
	}
}

// describeWitness renders what the daemon knows of the witness.
func describeWitness(rep *report, status api.RemoteStatus) {
	w := status.Witness

	switch {
	case w.Name == "":
		rep.row("witness\tnone here, but the other machine uses %s", w.PeerName)
		rep.row("\tnothing is taken over until both do: run `q remote witness` here")

		return
	case w.PeerName == "":
		rep.row("witness\t%s, which the other machine has not said it uses", w.Name)
		rep.row("\tnothing is taken over until both do: run `q remote witness` there")

		return
	case w.Mismatched():
		rep.row("witness\t%s, but the other machine uses %s", w.Name, w.PeerName)
		rep.row("\tnothing is taken over until both name the same one")

		return
	}

	rep.row("witness\t%s", w.Name)

	switch {
	case w.Error != "":
		rep.row("\tunreachable: %s", w.Error)
	case w.AskedAt.IsZero():
		rep.row("\tnot asked yet")
	default:
		rep.row("\tnames %s", hostName(w.Holder, status))
	}

	if w.Standby {
		rep.row("\tstanding by: unpinned missions are stopped here until this machine reaches")
		rep.row("\tthe other one, or `q remote witness take` says to run them here anyway")
	}
}
