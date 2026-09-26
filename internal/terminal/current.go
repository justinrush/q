package terminal

import (
	"context"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
)

// CurrentAttachment runs in the foreground terminal, with I/O supplied by the
// CLI or Bubble Tea after releasing its terminal state.
type CurrentAttachment struct {
	*exec.Cmd
	ctx     context.Context
	bin     string
	session string
	steal   bool
	inside  bool
}

// CurrentCommand switches the calling client inside tmux, or attaches until
// detach outside tmux.
func CurrentCommand(ctx context.Context, bin, session string, steal bool) *CurrentAttachment {
	inside := os.Getenv("TMUX") != ""
	argv := NewTmux(bin, nil).AttachArgs(session, steal)[1:]
	if inside {
		argv = []string{"switch-client", "-t", Session(session).String()}
	}
	return &CurrentAttachment{
		Cmd: currentExec(ctx, bin, argv...), ctx: ctx, bin: bin,
		session: session, steal: steal, inside: inside,
	}
}

func currentExec(ctx context.Context, bin string, args ...string) *exec.Cmd {
	// The executable comes from user configuration. Arguments are passed
	// literally, never interpolated into a shell command.
	return exec.CommandContext(ctx, bin, args...) // #nosec G204
}

// Run switches first, then detaches only other clients of the destination
// session when stealing. Clients viewing unrelated sessions are left alone.
func (c *CurrentAttachment) Run() error {
	if !c.inside || !c.steal {
		return c.Cmd.Run()
	}
	tty, err := currentExec(c.ctx, c.bin, "display-message", "-p", "#{client_tty}").Output()
	if err != nil {
		return fmt.Errorf("identifying current tmux client: %w", err)
	}
	client := strings.TrimSpace(string(tty))
	if client == "" {
		return fmt.Errorf("no current tmux client")
	}
	c.Args = append(c.Args, "-c", client)
	if err := c.Cmd.Run(); err != nil {
		return err
	}
	out, err := currentExec(c.ctx, c.bin, "list-clients", "-t", Session(c.session).String(), "-F", "#{client_tty}").Output()
	if err != nil {
		return fmt.Errorf("listing mission clients: %w", err)
	}
	for _, other := range strings.Fields(string(out)) {
		if other != client {
			if err := currentExec(c.ctx, c.bin, "detach-client", "-t", other).Run(); err != nil {
				return fmt.Errorf("detaching client %s: %w", other, err)
			}
		}
	}
	return nil
}

// These methods implement Bubble Tea's ExecCommand interface.
func (c *CurrentAttachment) SetStdin(in io.Reader)   { c.Stdin = in }
func (c *CurrentAttachment) SetStdout(out io.Writer) { c.Stdout = out }
func (c *CurrentAttachment) SetStderr(out io.Writer) { c.Stderr = out }
