package codex

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

func TestFailedProxyInitializationReapsProcess(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "silent-proxy")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf '%s' \"$$\" > \"$0.pid\"\nexec sleep 30\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithTimeout(t.Context(), 100*time.Millisecond)
	defer cancel()

	_, err := startAppServer(ctx, bin, "test", runner.OS{}, nil)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("startAppServer() error = %v, want deadline exceeded", err)
	}

	data, err := os.ReadFile(bin + ".pid")
	if err != nil {
		t.Fatalf("reading child PID: %v", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parsing child PID: %v", err)
	}

	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Errorf("proxy child PID %d was not reaped", pid)
}

func TestProxyReadThread(t *testing.T) {
	stream := newScriptedStream("{\"id\":1,\"result\":{\"thread\":{\"id\":\"thr-1\",\"status\":{\"type\":\"active\",\"activeFlags\":[\"waitingOnApproval\"]}}}}\n")
	proxy := &Proxy{client: NewClient(stream, stream)}
	notifications := make(chan Notification)

	go func() {
		_ = proxy.client.Run(t.Context(), notifications)
	}()

	status, err := proxy.ReadThread(t.Context(), "thr-1")
	if err != nil {
		t.Fatalf("ReadThread() error = %v", err)
	}

	if got := status.Classify(); got != mission.ActivityWaitingApproval {
		t.Errorf("Classify() = %q", got)
	}
}

func TestProxyFindThreadPrefersLoadedThread(t *testing.T) {
	stream := newScriptedStream("{\"id\":1,\"result\":{\"data\":[{\"id\":\"old\",\"status\":{\"type\":\"notLoaded\"}},{\"id\":\"live\",\"status\":{\"type\":\"active\",\"activeFlags\":[]}}]}}\n")
	proxy := &Proxy{client: NewClient(stream, stream)}
	notifications := make(chan Notification)

	go func() {
		_ = proxy.client.Run(t.Context(), notifications)
	}()

	thread, found, err := proxy.FindThread(t.Context(), "/tasks/one")
	if err != nil {
		t.Fatalf("FindThread() error = %v", err)
	}

	if !found {
		t.Fatal("FindThread() found = false")
	}

	if thread.ID != "live" {
		t.Errorf("thread ID = %q, want live", thread.ID)
	}
}
