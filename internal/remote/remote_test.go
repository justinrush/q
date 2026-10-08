package remote

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

const sshBin = "/usr/bin/ssh"

// sshCommand returns a Command over a fake runner.
func sshCommand(fake *runner.Fake) Command {
	return Command{Argv: []string{sshBin, "mini.local"}, ControlDir: "/state", Run: fake}
}

// The argv is the contract with ssh: these options are what keep a daemon with
// no terminal from hanging on a prompt or on a sleeping peer, and reusing one
// connection is what makes an exchange every few seconds affordable.
func TestCallRunsTheRelayOverSSHWithBatchOptions(t *testing.T) {
	fake := runner.NewFake()
	fake.Default = runner.Result{Stdout: []byte(ResponseMarker + `{"status":200,"body":{"ok":true}}` + "\n")}

	resp, err := sshCommand(fake).Call(t.Context(), Request{Method: "POST", Path: "/v1/sync", Body: json.RawMessage(`{}`)})
	if err != nil {
		t.Fatalf("Call: %v", err)
	}

	if resp.Status != 200 || string(resp.Body) != `{"ok":true}` {
		t.Errorf("response = %d %s", resp.Status, resp.Body)
	}

	calls := fake.Calls()
	if len(calls) != 1 {
		t.Fatalf("ran %d commands, want 1", len(calls))
	}

	want := sshBin + " -o BatchMode=yes -o ConnectTimeout=5" +
		" -o ControlMaster=auto -o ControlPath=/state/ssh-%C -o ControlPersist=10m" +
		" mini.local ~/.local/bin/q rpc"

	if got := calls[0].String(); got != want {
		t.Errorf("argv:\n got %s\nwant %s", got, want)
	}

	var sent Request
	if err := json.Unmarshal(calls[0].Stdin, &sent); err != nil || sent.Path != "/v1/sync" {
		t.Errorf("stdin = %s, want the request", calls[0].Stdin)
	}
}

// A command that is not ssh is run exactly as configured. Whatever it is, the
// user chose it, and ssh's options would mean nothing to it.
func TestCallLeavesANonSSHCommandAlone(t *testing.T) {
	fake := runner.NewFake()
	fake.Default = runner.Result{Stdout: []byte(ResponseMarker + `{"status":204}`)}

	cmd := Command{Argv: []string{"/usr/local/bin/az-bastion", "dev-vm"}, Bin: "/opt/q/bin/q", ControlDir: "/state", Run: fake}

	if _, err := cmd.Call(t.Context(), Request{Method: "GET", Path: "/v1/remote"}); err != nil {
		t.Fatalf("Call: %v", err)
	}

	if got, want := fake.Calls()[0].String(), "/usr/local/bin/az-bastion dev-vm /opt/q/bin/q rpc"; got != want {
		t.Errorf("argv = %s, want %s", got, want)
	}
}

// A remote shell may print before the command runs. Only the marked line is
// the answer.
func TestCallIgnoresShellNoiseAroundTheResponse(t *testing.T) {
	fake := runner.NewFake()
	fake.Default = runner.Result{Stdout: []byte(
		"Last login: Tue Oct  6 from 10.0.0.2\n" +
			"{\"not\":\"ours\"}\n" +
			ResponseMarker + `{"status":200,"body":[1,2]}` + "\n")}

	resp, err := sshCommand(fake).Call(t.Context(), Request{Method: "GET", Path: "/v1/state"})
	if err != nil || string(resp.Body) != "[1,2]" {
		t.Errorf("resp = %s, err = %v; want the marked line", resp.Body, err)
	}
}

func TestCallReportsAFailedCommandAsUnreachable(t *testing.T) {
	fake := runner.NewFake()
	fake.StrictMode = true

	_, err := sshCommand(fake).Call(t.Context(), Request{Method: "GET", Path: "/v1/remote"})
	if !errors.Is(err, ErrUnreachable) {
		t.Errorf("err = %v, want ErrUnreachable so the caller can queue instead of failing", err)
	}
}

// ssh connected, but nothing answered: q is missing on the peer, or its shell
// printed an error instead.
func TestCallReportsASilentPeerAsUnreachable(t *testing.T) {
	fake := runner.NewFake()
	fake.Default = runner.Result{Stdout: []byte("zsh: command not found: q\n")}

	_, err := sshCommand(fake).Call(t.Context(), Request{Method: "GET", Path: "/v1/remote"})
	if !errors.Is(err, ErrUnreachable) || !strings.Contains(err.Error(), "installed") {
		t.Errorf("err = %v, want an unreachable error that suggests why", err)
	}
}

func TestCallWithoutACommandIsUnreachable(t *testing.T) {
	if _, err := (Command{}).Call(t.Context(), Request{}); !errors.Is(err, ErrUnreachable) {
		t.Errorf("err = %v, want ErrUnreachable", err)
	}
}

// git's ext transport splits on spaces and reads % as an escape, and ssh hands
// the rest to a shell. Each layer needs its own quoting or a path with a space
// in it reaches the peer as two arguments.
func TestGitURLQuotesForExtAndForTheRemoteShell(t *testing.T) {
	cmd := Command{Argv: []string{sshBin, "-J", "jump host", "dev-vm"}}

	got := cmd.GitURL("/home/me/my repos/it's")
	want := "ext::" + sshBin + " -o BatchMode=yes -o ConnectTimeout=5 -J jump% host dev-vm %S " +
		`'/home/me/my% repos/it'\''s'`

	if got != want {
		t.Errorf("GitURL:\n got %s\nwant %s", got, want)
	}
}

// staticTransport answers every call with one response.
type staticTransport struct {
	resp Response
	err  error
	got  Request
}

func (s *staticTransport) Call(_ context.Context, req Request) (Response, error) {
	s.got = req

	return s.resp, s.err
}

func TestPeerDecodesASuccessfulAnswer(t *testing.T) {
	transport := &staticTransport{resp: Response{Status: 200, Body: json.RawMessage(`{"id":"ms_aabbccddeeff","name":"x"}`)}}

	ms, err := NewPeer(transport).SetStatus(t.Context(), "ms_aabbccddeeff", api.SetStatusRequest{To: mission.StatusActive, Message: "go"})
	if err != nil || ms.Name != "x" {
		t.Fatalf("ms = %+v, err = %v", ms, err)
	}

	if transport.got.Method != "POST" || transport.got.Path != "/v1/missions/ms_aabbccddeeff/status" {
		t.Errorf("request = %s %s", transport.got.Method, transport.got.Path)
	}

	var sent api.SetStatusRequest
	if err := json.Unmarshal(transport.got.Body, &sent); err != nil || sent.Message != "go" {
		t.Errorf("body = %s", transport.got.Body)
	}
}

// A refusal from the peer's daemon must arrive as the same error a local
// client would see, carrying the daemon's own words.
func TestPeerTurnsARefusalIntoAStatusError(t *testing.T) {
	transport := &staticTransport{resp: Response{Status: 409, Body: json.RawMessage(`{"error":"already paired with laptop"}`)}}

	_, err := NewPeer(transport).Sync(t.Context(), api.SyncRequest{})

	status, ok := errors.AsType[*api.StatusError](err)
	if !ok || status.Code != 409 || status.Message != "already paired with laptop" {
		t.Errorf("err = %v, want a 409 with the daemon's message", err)
	}

	if errors.Is(err, ErrUnreachable) {
		t.Error("a refusal was reported as unreachable; the caller would queue instead of reporting it")
	}
}

func TestPeerPassesUnreachableThrough(t *testing.T) {
	transport := &staticTransport{err: ErrUnreachable}

	if _, err := NewPeer(transport).Sync(t.Context(), api.SyncRequest{}); !errors.Is(err, ErrUnreachable) {
		t.Errorf("err = %v, want ErrUnreachable", err)
	}
}

// The far end is q, not tmux: a non-interactive ssh session has no PATH to
// find tmux on, and only the peer's q knows which session the mission is in.
func TestAttachArgvAsksForATerminalAndRunsQOnThePeer(t *testing.T) {
	cmd := Command{Argv: []string{sshBin, "mini.local"}, ControlDir: "/state"}

	got := strings.Join(cmd.AttachArgv("ms_aabbccddeeff"), " ")
	want := sshBin + " -o BatchMode=yes -o ConnectTimeout=5" +
		" -o ControlMaster=auto -o ControlPath=/state/ssh-%C -o ControlPersist=10m" +
		" -t mini.local ~/.local/bin/q attach ms_aabbccddeeff"

	if got != want {
		t.Errorf("AttachArgv:\n got %s\nwant %s", got, want)
	}

	if (Command{}).AttachArgv("ms_aabbccddeeff") != nil {
		t.Error("an unconfigured command produced something to run")
	}
}

// A config change on the peer is a command there, not a request to its daemon.
// Its arguments reach a shell, so each is quoted; the binary is not, or its ~
// would not expand.
func TestExecRunsQOnThePeer(t *testing.T) {
	const prefix = sshBin + " -o BatchMode=yes -o ConnectTimeout=5" +
		" -o ControlMaster=auto -o ControlPath=/state/ssh-%C -o ControlPersist=10m" +
		" mini.local ~/.local/bin/q "

	cases := []struct {
		name        string
		args        []string
		exit        int
		stderr      string
		wantArgv    string
		wantErr     string
		unreachable bool
	}{
		{
			name:     "arguments are quoted for the peer's shell",
			args:     []string{"remote", "witness", "azure", "my account", "--local"},
			wantArgv: prefix + "'remote' 'witness' 'azure' 'my account' '--local'",
		},
		{
			name:     "q refusing on the peer is its error, not an unreachable peer",
			args:     []string{"remote", "witness", "clear"},
			exit:     1,
			stderr:   "Error: the lease cannot be written",
			wantArgv: prefix + "'remote' 'witness' 'clear'",
			wantErr:  "Error: the lease cannot be written",
		},
		{
			name:        "ssh failing to connect is an unreachable peer",
			args:        []string{"remote", "witness", "clear"},
			exit:        255,
			stderr:      "ssh: connect to host mini.local port 22: Operation timed out",
			wantArgv:    prefix + "'remote' 'witness' 'clear'",
			wantErr:     "Operation timed out",
			unreachable: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			fake := runner.NewFake()
			fake.Default = runner.Result{Stdout: []byte("done\n")}

			if tc.exit != 0 {
				fake.ExpectExit(tc.wantArgv, tc.exit, tc.stderr)
			}

			out, err := sshCommand(fake).Exec(t.Context(), tc.args...)

			if got := fake.Calls()[0].String(); got != tc.wantArgv {
				t.Errorf("argv:\n got %s\nwant %s", got, tc.wantArgv)
			}

			if tc.wantErr == "" {
				if err != nil || out != "done\n" {
					t.Fatalf("out = %q, err = %v", out, err)
				}

				return
			}

			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("err = %v, want it to carry %q", err, tc.wantErr)
			}

			if errors.Is(err, ErrUnreachable) != tc.unreachable {
				t.Errorf("unreachable = %v, want %v", errors.Is(err, ErrUnreachable), tc.unreachable)
			}
		})
	}
}
