package runner

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
)

// Fake is a Runner that records invocations and answers from a canned table
// instead of executing anything. It is safe for concurrent use.
//
// Responses are keyed by the full argv line, exactly as [Spec.String] renders
// it. Use [Fake.Expect] to register one:
//
//	f := runner.NewFake()
//	f.Expect("/usr/bin/git -C /repo rev-parse HEAD", "abc123")
type Fake struct {
	mu       sync.Mutex
	calls    []Spec
	replies  map[string]Result
	failures map[string]error

	// Default answers any call with no registered reply. The zero value is a
	// successful, empty result.
	Default Result
	// StrictMode makes unregistered calls fail instead of returning Default,
	// which is useful when a test wants to enumerate every expected command.
	StrictMode bool
}

// NewFake returns an empty Fake.
func NewFake() *Fake {
	return &Fake{
		replies:  map[string]Result{},
		failures: map[string]error{},
	}
}

// Expect registers stdout to be returned for the given argv line.
//
// Re-registering a call that was previously expected to fail clears that failure,
// so a test can override a shared fixture's expectation.
func (f *Fake) Expect(argv, stdout string) *Fake {
	return f.ExpectResult(argv, Result{Stdout: []byte(stdout)})
}

// ExpectResult registers a full Result for the given argv line, clearing any
// previously registered failure.
func (f *Fake) ExpectResult(argv string, res Result) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.replies[argv] = res
	delete(f.failures, argv)

	return f
}

// ExpectError registers a failure for the given argv line.
func (f *Fake) ExpectError(argv string, err error) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.failures[argv] = err

	return f
}

// ExpectExit registers a non-zero exit for the given argv line, reported as an
// [ExitError] so callers using [IsExit] behave as they would in production.
func (f *Fake) ExpectExit(argv string, code int, stderr string) *Fake {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.replies[argv] = Result{Stderr: []byte(stderr), ExitCode: code}
	f.failures[argv] = errExit

	return f
}

// errExit marks a reply that should surface as an ExitError. Run rewrites it
// into a properly populated *ExitError once it knows the Spec.
var errExit = errors.New("exit")

// Run implements [Runner].
func (f *Fake) Run(ctx context.Context, s Spec) (Result, error) {
	if err := ctx.Err(); err != nil {
		return Result{}, err
	}

	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = append(f.calls, s)

	key := s.String()

	res, ok := f.replies[key]
	if !ok {
		res = f.Default
	}

	if err, bad := f.failures[key]; bad {
		if errors.Is(err, errExit) {
			return res, &ExitError{Spec: s, Result: res}
		}

		return res, err
	}

	if !ok && f.StrictMode {
		return Result{}, fmt.Errorf("fake runner: unexpected call %q", key)
	}

	return res, nil
}

// Calls returns the recorded invocations in order.
func (f *Fake) Calls() []Spec {
	f.mu.Lock()
	defer f.mu.Unlock()

	return append([]Spec(nil), f.calls...)
}

// Argv returns the recorded invocations as argv lines, one per call, which is
// the convenient form for golden-file comparison.
func (f *Fake) Argv() []string {
	out := make([]string, 0, len(f.Calls()))
	for _, c := range f.Calls() {
		out = append(out, c.String())
	}

	return out
}

// Transcript renders the recorded invocations as a newline-terminated block,
// suitable for comparing against a testdata golden file.
func (f *Fake) Transcript() string {
	var b strings.Builder

	for _, line := range f.Argv() {
		b.WriteString(line)
		b.WriteByte('\n')
	}

	return b.String()
}

// Reset clears recorded calls, keeping registered replies.
func (f *Fake) Reset() {
	f.mu.Lock()
	defer f.mu.Unlock()

	f.calls = nil
}
