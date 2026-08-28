package runner

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"syscall"
	"time"

	"github.com/justinrush/q/internal/paths"
)

// DefaultTimeout bounds any Spec that does not arrive with a deadline of its
// own. git fetch against cloudlab is the slowest thing q runs.
const DefaultTimeout = 2 * time.Minute

// OS runs processes for real. It is the only type in q that calls
// os/exec, which is what keeps the gosec G204 suppression to a single site.
type OS struct {
	// Logger receives one debug line per invocation. Optional.
	Logger *slog.Logger
	// Timeout overrides DefaultTimeout when non-zero.
	Timeout time.Duration
}

// Stream is a running process whose stdin and stdout stay connected to its
// caller. It is used for long-lived machine protocols rather than commands that
// return one buffered result.
type Stream struct {
	Stdin  io.WriteCloser
	Stdout io.ReadCloser

	cmd    *exec.Cmd
	spec   Spec
	stderr bytes.Buffer
}

// StartStream starts a process with connected stdin and stdout pipes.
func (o OS) StartStream(ctx context.Context, s Spec) (*Stream, error) {
	if s.Name == "" {
		return nil, errors.New("runner: empty command name")
	}

	// #nosec G204 -- see the note in Run. This path has the same argv-only
	// boundary and exists so protocol clients do not add another exec site.
	cmd := exec.CommandContext(ctx, s.Name, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = s.Env

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("opening stdin for %s: %w", s, err)
	}

	stdout, err := cmd.StdoutPipe()
	if err != nil {
		_ = stdin.Close()

		return nil, fmt.Errorf("opening stdout for %s: %w", s, err)
	}

	stream := &Stream{Stdin: stdin, Stdout: stdout, cmd: cmd, spec: s}
	cmd.Stderr = &stream.stderr

	err = cmd.Start()
	if err != nil {
		_ = stdin.Close()
		_ = stdout.Close()

		return nil, fmt.Errorf("starting %s: %w", s, err)
	}

	return stream, nil
}

// Wait waits for the stream process and includes captured stderr in failures.
func (s *Stream) Wait() error {
	err := s.cmd.Wait()
	if err == nil {
		return nil
	}

	exitCode := -1
	if s.cmd.ProcessState != nil {
		exitCode = s.cmd.ProcessState.ExitCode()
	}

	result := Result{Stderr: s.stderr.Bytes(), ExitCode: exitCode}
	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return &ExitError{Spec: s.spec, Result: result}
	}

	return fmt.Errorf("waiting for %s: %w", s.spec, err)
}

// Stop closes the protocol pipes and stops the process.
func (s *Stream) Stop() error {
	_ = s.Stdin.Close()
	_ = s.Stdout.Close()

	if s.cmd.Process == nil {
		return nil
	}

	err := s.cmd.Process.Kill()
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}

	return err
}

// Run implements [Runner].
func (o OS) Run(ctx context.Context, s Spec) (Result, error) {
	if s.Name == "" {
		return Result{}, errors.New("runner: empty command name")
	}

	timeout := o.Timeout
	if timeout == 0 {
		timeout = DefaultTimeout
	}

	ctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	if o.Logger != nil {
		o.Logger.Debug("exec", "command", s)
	}

	// #nosec G204 -- Spec.Name and Args are built from validated internal
	// state (resolved binary paths, generated slugs, stored repo paths), never
	// from a shell string or unsanitized external input. This is the only
	// exec site in q precisely so this reasoning lives in one place.
	cmd := exec.CommandContext(ctx, s.Name, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = s.Env

	var stdout, stderr bytes.Buffer

	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	if len(s.Stdin) > 0 {
		cmd.Stdin = bytes.NewReader(s.Stdin)
	}

	err := cmd.Run()
	res := Result{Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: cmd.ProcessState.ExitCode()}

	return res, o.classify(ctx, s, res, err)
}

// classify turns exec's error into either a launch failure, a cancellation, or
// an [ExitError] that callers can inspect.
func (o OS) classify(ctx context.Context, s Spec, res Result, err error) error {
	if err == nil {
		return nil
	}

	if ctxErr := ctx.Err(); ctxErr != nil {
		return fmt.Errorf("%s: %w", s, ctxErr)
	}

	if _, ok := errors.AsType[*exec.ExitError](err); ok {
		return &ExitError{Spec: s, Result: res}
	}

	return fmt.Errorf("running %s: %w", s, err)
}

// StartDetached launches a process in its own session so it survives the death
// of the caller, and returns its pid. It is used only to bring up the daemon;
// every other process q starts either completes promptly or is owned by
// tmux.
//
// Output is appended to logPath rather than inherited, so a detached daemon
// cannot write into a TUI's terminal.
func StartDetached(s Spec, logPath string) (int, error) {
	if s.Name == "" {
		return 0, errors.New("runner: empty command name")
	}

	logFile, err := paths.OpenLog(logPath)
	if err != nil {
		return 0, err
	}
	defer func() { _ = logFile.Close() }()

	// #nosec G204 -- see the note in Run; argv here is q's own
	// executable path plus a fixed subcommand.
	cmd := exec.Command(s.Name, s.Args...)
	cmd.Dir = s.Dir
	cmd.Env = s.Env
	cmd.Stdin = nil
	cmd.Stdout = logFile
	cmd.Stderr = logFile
	cmd.SysProcAttr = &syscall.SysProcAttr{Setsid: true}

	if err := cmd.Start(); err != nil {
		return 0, fmt.Errorf("starting %s: %w", s, err)
	}

	// Reap the child so it does not linger as a zombie attached to us; the
	// daemon itself is now session leader and unaffected by our exit.
	go func() { _ = cmd.Wait() }()

	return cmd.Process.Pid, nil
}
