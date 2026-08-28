package runner

import (
	"context"
	"errors"
	"os/exec"
	"strings"
	"testing"
)

func requireBinary(t *testing.T, name string) string {
	t.Helper()

	path, err := exec.LookPath(name)
	if err != nil {
		t.Skipf("%s is not available in this environment", name)
	}

	return path
}

func TestSpecString(t *testing.T) {
	s := Spec{Name: "/usr/bin/git", Args: []string{"-C", "/repo", "rev-parse", "HEAD"}}

	want := "/usr/bin/git -C /repo rev-parse HEAD"
	if got := s.String(); got != want {
		t.Errorf("String() = %q, want %q", got, want)
	}
}

func TestResultOutAndLines(t *testing.T) {
	r := Result{Stdout: []byte("  first\nsecond  \n\n")}

	if got, want := r.Out(), "first\nsecond"; got != want {
		t.Errorf("Out() = %q, want %q", got, want)
	}

	// Out trims the whole string, so leading/trailing blank lines and the outer
	// padding are gone, but interior line content is preserved verbatim.
	lines := r.Lines()
	if len(lines) != 2 || lines[0] != "first" || lines[1] != "second" {
		t.Errorf("Lines() = %q, want [first second]", lines)
	}

	// Interior whitespace survives, which matters for tmux formats that pad
	// columns.
	interior := Result{Stdout: []byte("\n a \n  b  \n\n")}
	if got := interior.Lines(); len(got) != 2 || got[0] != "a " || got[1] != "  b" {
		t.Errorf("Lines() = %q, want [%q %q]", got, "a ", "  b")
	}

	if got := (Result{}).Lines(); got != nil {
		t.Errorf("Lines() on empty = %q, want nil", got)
	}
}

func TestFakeRecordsCallsAndReplies(t *testing.T) {
	f := NewFake()
	f.Expect("/bin/tmux has-session -t =q-x", "yes")

	res, err := f.Run(context.Background(), Spec{Name: "/bin/tmux", Args: []string{"has-session", "-t", "=q-x"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := res.Out(); got != "yes" {
		t.Errorf("Out() = %q, want %q", got, "yes")
	}

	if got := f.Argv(); len(got) != 1 || got[0] != "/bin/tmux has-session -t =q-x" {
		t.Errorf("Argv() = %q", got)
	}
}

func TestFakeUnregisteredCallReturnsDefault(t *testing.T) {
	f := NewFake()
	f.Default = Result{Stdout: []byte("fallback")}

	res, err := f.Run(context.Background(), Spec{Name: "/bin/true"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := res.Out(); got != "fallback" {
		t.Errorf("Out() = %q, want %q", got, "fallback")
	}
}

func TestFakeStrictModeRejectsUnregisteredCall(t *testing.T) {
	f := NewFake()
	f.StrictMode = true

	if _, err := f.Run(context.Background(), Spec{Name: "/bin/true"}); err == nil {
		t.Fatal("expected error for unregistered call in strict mode")
	}
}

// Callers distinguish "the command ran and said no" from "the command could not
// run", so a fake non-zero exit must surface as an ExitError just as the real
// runner's would.
func TestFakeExitErrorSatisfiesIsExit(t *testing.T) {
	f := NewFake()
	f.ExpectExit("/bin/tmux has-session -t =gone", 1, "can't find session")

	res, err := f.Run(context.Background(), Spec{Name: "/bin/tmux", Args: []string{"has-session", "-t", "=gone"}})
	if err == nil {
		t.Fatal("expected an error")
	}

	if !IsExit(err) {
		t.Errorf("IsExit(%v) = false, want true", err)
	}

	if res.ExitCode != 1 {
		t.Errorf("ExitCode = %d, want 1", res.ExitCode)
	}

	if !strings.Contains(err.Error(), "can't find session") {
		t.Errorf("error %q should include stderr", err)
	}
}

func TestFakeExplicitErrorIsNotAnExit(t *testing.T) {
	sentinel := errors.New("boom")

	f := NewFake()
	f.ExpectError("/bin/git status", sentinel)

	_, err := f.Run(context.Background(), Spec{Name: "/bin/git", Args: []string{"status"}})
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v, want %v", err, sentinel)
	}

	if IsExit(err) {
		t.Error("IsExit should be false for a launch failure")
	}
}

func TestFakeTranscript(t *testing.T) {
	f := NewFake()
	ctx := context.Background()

	_, _ = f.Run(ctx, Spec{Name: "/bin/git", Args: []string{"fetch"}})
	_, _ = f.Run(ctx, Spec{Name: "/bin/git", Args: []string{"status"}})

	want := "/bin/git fetch\n/bin/git status\n"
	if got := f.Transcript(); got != want {
		t.Errorf("Transcript() = %q, want %q", got, want)
	}
}

func TestFakeResetKeepsReplies(t *testing.T) {
	f := NewFake()
	f.Expect("/bin/echo hi", "hi")

	ctx := context.Background()
	_, _ = f.Run(ctx, Spec{Name: "/bin/echo", Args: []string{"hi"}})
	f.Reset()

	if got := f.Calls(); len(got) != 0 {
		t.Fatalf("Calls() after Reset = %d, want 0", len(got))
	}

	res, err := f.Run(ctx, Spec{Name: "/bin/echo", Args: []string{"hi"}})
	if err != nil || res.Out() != "hi" {
		t.Errorf("reply lost after Reset: %q, %v", res.Out(), err)
	}
}

func TestFakeHonorsCanceledContext(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()

	if _, err := NewFake().Run(ctx, Spec{Name: "/bin/true"}); !errors.Is(err, context.Canceled) {
		t.Errorf("err = %v, want context.Canceled", err)
	}
}

func TestOSRunCapturesStdout(t *testing.T) {
	echo := requireBinary(t, "echo")
	res, err := OS{}.Run(context.Background(), Spec{Name: echo, Args: []string{"hello"}})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := res.Out(); got != "hello" {
		t.Errorf("Out() = %q, want %q", got, "hello")
	}

	if res.ExitCode != 0 {
		t.Errorf("ExitCode = %d, want 0", res.ExitCode)
	}
}

func TestOSRunPassesStdin(t *testing.T) {
	cat := requireBinary(t, "cat")
	res, err := OS{}.Run(context.Background(), Spec{Name: cat, Stdin: []byte("piped")})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}

	if got := res.Out(); got != "piped" {
		t.Errorf("Out() = %q, want %q", got, "piped")
	}
}

func TestOSRunReportsNonZeroExitAsExitError(t *testing.T) {
	falseBin := requireBinary(t, "false")
	_, err := OS{}.Run(context.Background(), Spec{Name: falseBin})
	if err == nil {
		t.Fatal("expected an error")
	}

	if !IsExit(err) {
		t.Errorf("IsExit(%v) = false, want true", err)
	}
}

func TestOSRunReportsMissingBinaryAsLaunchFailure(t *testing.T) {
	_, err := OS{}.Run(context.Background(), Spec{Name: "/nonexistent/q-not-a-real-binary"})
	if err == nil {
		t.Fatal("expected an error")
	}

	if IsExit(err) {
		t.Error("a missing binary is a launch failure, not an exit")
	}
}

func TestOSRunRejectsEmptyName(t *testing.T) {
	if _, err := (OS{}).Run(context.Background(), Spec{}); err == nil {
		t.Fatal("expected an error for an empty command name")
	}
}
