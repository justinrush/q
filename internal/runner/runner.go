// Package runner is the single seam through which q executes external
// programs.
//
// Every git, tmux, and terminal-launch invocation goes through a [Runner], for
// three reasons: tests can assert exact argv without running anything, there is
// one place to apply timeouts and trace logging, and gosec's G204 warning is
// suppressed once rather than at dozens of call sites.
//
// A [Spec] is always argv, never a shell string. Nothing here interpolates into
// a shell.
package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
)

// Spec describes one process to run. Name must be an absolute path; resolve it
// at process start rather than relying on the inherited PATH.
type Spec struct {
	// Name is the absolute path to the executable.
	Name string
	// Args are the arguments after argv[0].
	Args []string
	// Dir is the working directory. Empty means inherit.
	Dir string
	// Env replaces the environment entirely when non-nil. Nil means inherit.
	Env []string
	// Stdin is written to the process's standard input, if non-empty.
	Stdin []byte
}

// String renders the spec as a shell-ish line. It is for logs and test
// failures only and is never executed.
func (s Spec) String() string {
	var b strings.Builder

	b.WriteString(s.Name)

	for _, a := range s.Args {
		b.WriteByte(' ')
		b.WriteString(a)
	}

	return b.String()
}

// Result is the outcome of a completed process.
type Result struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

// Out returns standard output with surrounding whitespace removed, which is
// what nearly every git and tmux query wants.
func (r Result) Out() string { return strings.TrimSpace(string(r.Stdout)) }

// Lines returns standard output split into non-empty trimmed lines.
func (r Result) Lines() []string {
	out := r.Out()
	if out == "" {
		return nil
	}

	return strings.Split(out, "\n")
}

// Runner executes a Spec.
//
// Run returns a non-nil error when the process could not be started, was
// canceled, or exited non-zero. Callers that treat a non-zero exit as
// meaningful data rather than a failure (git's "is this ref present" probes,
// tmux has-session) should inspect Result.ExitCode and use [IsExit] to tell the
// two cases apart.
type Runner interface {
	Run(ctx context.Context, s Spec) (Result, error)
}

// ExitError reports a process that started and then exited non-zero.
type ExitError struct {
	Spec   Spec
	Result Result
}

// Error implements error.
func (e *ExitError) Error() string {
	msg := strings.TrimSpace(string(e.Result.Stderr))
	if msg == "" {
		msg = strings.TrimSpace(string(e.Result.Stdout))
	}

	if msg == "" {
		return fmt.Sprintf("%s: exit status %d", e.Spec, e.Result.ExitCode)
	}

	return fmt.Sprintf("%s: exit status %d: %s", e.Spec, e.Result.ExitCode, msg)
}

// IsExit reports whether err is a non-zero exit from a process that did start,
// as opposed to a failure to launch it or a canceled context.
func IsExit(err error) bool {
	_, ok := errors.AsType[*ExitError](err)

	return ok
}
