package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"strings"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/remote"
	"github.com/spf13/cobra"
)

// Relay limits.
const (
	// rpcTimeout bounds one relayed request. It is generous because a relayed
	// request can launch an agent, which fetches and provisions worktrees.
	rpcTimeout = 90 * time.Second
	// maxRPCRequest bounds what is read from standard input. An exchange
	// carries every mission's brief, so this is far above an ordinary request.
	maxRPCRequest = 32 << 20
)

// rpcPathPrefix is the only part of the daemon's API a relayed request may
// reach.
const rpcPathPrefix = "/v1/"

func buildRPCSubcommand() *cobra.Command {
	return &cobra.Command{
		Use:   "rpc",
		Short: "Relay one request from a paired q to this machine's daemon",
		Long: "Read one request on standard input, send it to the daemon running on this " +
			"machine, and print the answer.\n\n" +
			"This is the far end of the connection a paired q makes over ssh. It is run " +
			"by q, not by hand.",
		Args:   cobra.NoArgs,
		Hidden: true,
		// The caller parses standard output, so cobra must add nothing to it.
		SilenceErrors: true,
		SilenceUsage:  true,
		RunE: func(cmd *cobra.Command, _ []string) error {
			return runRPC(cmd.Context(), cmd.InOrStdin(), cmd.OutOrStdout())
		},
	}
}

// runRPC relays one request.
//
// It never starts a daemon. A daemon started from here would inherit the bare
// environment of a non-interactive ssh session, and the daemon's environment is
// what every agent it launches inherits in turn: the agents would start with no
// PATH worth the name. A machine meant to be reached this way runs its daemon
// as a service, and the error says so.
func runRPC(ctx context.Context, in io.Reader, out io.Writer) error {
	data, err := io.ReadAll(io.LimitReader(in, maxRPCRequest))
	if err != nil {
		return fmt.Errorf("reading the request: %w", err)
	}

	var req remote.Request
	if err := json.Unmarshal(data, &req); err != nil {
		return fmt.Errorf("decoding the request: %w", err)
	}

	if !strings.HasPrefix(req.Path, rpcPathPrefix) {
		return fmt.Errorf("refusing to relay %q: not a daemon API path", req.Path)
	}

	dirs, err := paths.Resolve(pathOverrides())
	if err != nil {
		return err
	}

	client, err := api.Connect(ctx, dirs)
	if err != nil {
		if errors.Is(err, api.ErrNoDaemon) {
			return errors.New("no q daemon is running on this machine; " +
				"start it with `q daemon run`, or install it as a service so it stays up")
		}

		return err
	}

	status, body, err := client.Raw(ctx, req.Method, req.Path, req.Body, rpcTimeout)
	if err != nil {
		return err
	}

	encoded, err := json.Marshal(remote.Response{Status: status, Body: compactJSON(body)})
	if err != nil {
		return fmt.Errorf("encoding the response: %w", err)
	}

	_, err = fmt.Fprintf(out, "%s%s\n", remote.ResponseMarker, encoded)

	return err
}

// compactJSON returns body as a raw message, or nil when there is none.
//
// The response travels as one line, and the daemon ends its JSON with a
// newline. Embedding that verbatim would be legal JSON and would still split
// the line the caller is looking for.
func compactJSON(body []byte) json.RawMessage {
	trimmed := strings.TrimSpace(string(body))
	if trimmed == "" || !json.Valid([]byte(trimmed)) {
		return nil
	}

	return json.RawMessage(trimmed)
}
