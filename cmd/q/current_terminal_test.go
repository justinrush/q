package main

import (
	"runtime"
	"testing"
)

func TestCurrentTerminalDefaults(t *testing.T) {
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "")
	want := terminalCurrent
	if runtime.GOOS == "darwin" {
		want = terminalGhostty
	}
	if got := defaultTerminalMode(); got != want {
		t.Fatalf("local default = %q, want %q", got, want)
	}
	t.Setenv("SSH_CONNECTION", "host 123 server 22")
	if got := defaultTerminalMode(); got != terminalCurrent {
		t.Fatalf("SSH default = %q", got)
	}
	t.Setenv("SSH_CONNECTION", "")
	t.Setenv("SSH_TTY", "/dev/pts/1")
	if got := defaultTerminalMode(); got != terminalCurrent {
		t.Fatalf("SSH TTY default = %q", got)
	}
}

func TestCurrentTerminalBin(t *testing.T) {
	s := settings{Terminal: terminalSettings{Mode: terminalCurrent}}
	if got := currentTerminalBin(s); got != "tmux" {
		t.Fatalf("default binary = %q", got)
	}
	s.Tools = map[string]string{"tmux": "/custom/tmux"}
	if got := currentTerminalBin(s); got != "/custom/tmux" {
		t.Fatalf("override = %q", got)
	}
	s.Terminal.Mode = terminalNone
	if got := currentTerminalBin(s); got != "" {
		t.Fatalf("manual mode must not attach: %q", got)
	}
}
