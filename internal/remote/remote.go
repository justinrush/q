// Package remote reaches the q daemon on another machine.
//
// Two q installations pair as a primary and a secondary, and the primary does
// all the dialing. That asymmetry is the design: the primary is a laptop that
// sleeps and moves between networks, so nothing can depend on reaching it,
// while the secondary is always on and always at the same address.
//
// The peer's daemon never listens on the network. A request is carried by
// running a command on the peer — ssh, in practice — whose far end is `q rpc`,
// which relays the one request to that machine's own loopback daemon and
// prints the answer. So the daemon's promise to accept connections only from
// its own host holds on both machines, and the authentication q relies on is
// the shell access the user already has to both.
package remote

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"path/filepath"
	"strings"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// ErrUnreachable reports that the peer could not be asked at all, as distinct
// from having been asked and refusing. Callers queue work on the first and
// report the second.
var ErrUnreachable = errors.New("the paired q is unreachable")

// Request is one call to the peer's daemon, as `q rpc` reads it.
type Request struct {
	Method string          `json:"method"`
	Path   string          `json:"path"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// Response is the peer daemon's answer, as `q rpc` writes it.
type Response struct {
	Status int             `json:"status"`
	Body   json.RawMessage `json:"body,omitempty"`
}

// ResponseMarker prefixes the one line of `q rpc` output that is the response.
//
// A remote shell is free to print before the command runs: a login banner, a
// message from a profile script. Anything not carrying the marker is ignored,
// so a chatty shell cannot be mistaken for a malformed answer.
const ResponseMarker = "q-rpc:"

// Transport carries one request to the peer and returns its answer.
type Transport interface {
	Call(ctx context.Context, req Request) (Response, error)
}

// Command is a Transport that reaches the peer by running a command.
type Command struct {
	// Argv is the command that gets a shell on the peer, e.g.
	// ["/usr/bin/ssh", "mini.local"]. Its first element must be an absolute
	// path, like every program q runs.
	Argv []string
	// Bin is the q binary on the peer, as its shell would resolve it.
	Bin string
	// ControlDir is where ssh keeps the socket of the connection it reuses.
	ControlDir string
	Run        runner.Runner
}

// DefaultBin is where q's installer puts the binary. A non-interactive ssh
// session does not read the profile that puts it on PATH, so the default has
// to be a path rather than a name.
const DefaultBin = "~/.local/bin/q"

// sshOptions are added when the command is ssh.
//
// BatchMode makes a missing key fail at once instead of prompting a daemon
// that has no terminal. ConnectTimeout bounds the wait for a peer that is
// asleep or off the network, which is the normal case for half of every day.
// The control options keep one connection open and reuse it, so an exchange
// every few seconds costs a round trip rather than a handshake.
func (c Command) sshOptions() []string {
	opts := []string{"-o", "BatchMode=yes", "-o", "ConnectTimeout=5"}

	if c.ControlDir != "" {
		opts = append(opts,
			"-o", "ControlMaster=auto",
			"-o", "ControlPath="+filepath.Join(c.ControlDir, "ssh-%C"),
			"-o", "ControlPersist=10m",
		)
	}

	return opts
}

// shellArgv returns the command up to, but not including, what runs on the
// peer.
func (c Command) shellArgv() []string {
	if len(c.Argv) == 0 {
		return nil
	}

	if filepath.Base(c.Argv[0]) != "ssh" {
		return c.Argv
	}

	out := append([]string{c.Argv[0]}, c.sshOptions()...)

	return append(out, c.Argv[1:]...)
}

// bin returns the peer's q binary.
func (c Command) bin() string {
	if c.Bin == "" {
		return DefaultBin
	}

	return c.Bin
}

// Call runs `q rpc` on the peer with the request on its standard input.
func (c Command) Call(ctx context.Context, req Request) (Response, error) {
	argv := c.shellArgv()
	if len(argv) == 0 {
		return Response{}, fmt.Errorf("%w: no command configured to reach it", ErrUnreachable)
	}

	body, err := json.Marshal(req)
	if err != nil {
		return Response{}, fmt.Errorf("encoding the request: %w", err)
	}

	res, err := c.Run.Run(ctx, runner.Spec{
		Name:  argv[0],
		Args:  append(argv[1:], c.bin(), "rpc"),
		Stdin: body,
	})
	if err != nil {
		return Response{}, fmt.Errorf("%w: %s", ErrUnreachable, failure(res, err))
	}

	return parseResponse(res.Stdout)
}

// failure describes why the command did not produce an answer, preferring what
// it printed over the bare exit status.
func failure(res runner.Result, err error) string {
	if msg := strings.TrimSpace(string(res.Stderr)); msg != "" {
		lines := strings.Split(msg, "\n")

		return lines[len(lines)-1]
	}

	return err.Error()
}

// parseResponse finds and decodes the marked response line.
func parseResponse(stdout []byte) (Response, error) {
	var resp Response

	for line := range strings.SplitSeq(string(stdout), "\n") {
		payload, ok := strings.CutPrefix(strings.TrimSpace(line), ResponseMarker)
		if !ok {
			continue
		}

		if err := json.Unmarshal([]byte(payload), &resp); err != nil {
			return Response{}, fmt.Errorf("%w: decoding its answer: %w", ErrUnreachable, err)
		}

		return resp, nil
	}

	return Response{}, fmt.Errorf("%w: it ran but did not answer; is q installed there?", ErrUnreachable)
}

// GitURL returns the remote git should use to reach a repository on the peer
// through this same command.
//
// It uses git's ext transport, which runs a command and speaks the git protocol
// over its pipes. That keeps one definition of "how to reach the peer": whatever
// the user configured for q — a jump host, a bastion wrapper, a tailnet name —
// is what git uses too, with no second place to keep in step.
//
// ext splits its command on spaces and treats % as an escape, so each element
// is escaped for that. The path is quoted for the peer's shell, which is what
// ssh hands the joined command line to.
func (c Command) GitURL(path string) string {
	parts := make([]string, 0, len(c.Argv)+2)

	for _, arg := range c.shellArgv() {
		parts = append(parts, extEscape(arg))
	}

	parts = append(parts, "%S", extEscape(shellQuote(path)))

	return "ext::" + strings.Join(parts, " ")
}

// extEscape escapes one argument for git's ext transport.
func extEscape(arg string) string {
	return strings.NewReplacer("%", "%%", " ", "% ").Replace(arg)
}

// shellQuote quotes a string for a POSIX shell.
func shellQuote(s string) string {
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// Peer is the paired daemon, spoken to through a Transport.
type Peer struct {
	transport Transport
}

// NewPeer returns a client for the daemon reached by transport.
func NewPeer(transport Transport) *Peer { return &Peer{transport: transport} }

// Sync performs one exchange of shared state.
func (p *Peer) Sync(ctx context.Context, req api.SyncRequest) (api.SyncResponse, error) {
	return call[api.SyncResponse](ctx, p, http.MethodPost, "/v1/sync", req)
}

// SetStatus moves a mission the peer runs to another lane, which is how it is
// resumed, messaged, or finished from here.
func (p *Peer) SetStatus(
	ctx context.Context,
	id mission.MissionID,
	req api.SetStatusRequest,
) (mission.Mission, error) {
	return call[mission.Mission](ctx, p, http.MethodPost, "/v1/missions/"+string(id)+"/status", req)
}

// call sends one request and decodes the answer, turning a refusal from the
// peer's daemon into the same error a local client would see.
func call[T any](ctx context.Context, p *Peer, method, path string, body any) (T, error) {
	var zero T

	encoded, err := json.Marshal(body)
	if err != nil {
		return zero, fmt.Errorf("encoding %s %s: %w", method, path, err)
	}

	resp, err := p.transport.Call(ctx, Request{Method: method, Path: path, Body: encoded})
	if err != nil {
		return zero, err
	}

	if resp.Status >= http.StatusBadRequest {
		var payload api.Error
		if err := json.Unmarshal(resp.Body, &payload); err != nil || payload.Error == "" {
			payload.Error = fmt.Sprintf("%s %s: status %d", method, path, resp.Status)
		}

		return zero, &api.StatusError{Code: resp.Status, Message: payload.Error}
	}

	if resp.Status == http.StatusNoContent || len(resp.Body) == 0 {
		return zero, nil
	}

	var out T
	if err := json.Unmarshal(resp.Body, &out); err != nil {
		return zero, fmt.Errorf("decoding %s %s response: %w", method, path, err)
	}

	return out, nil
}
