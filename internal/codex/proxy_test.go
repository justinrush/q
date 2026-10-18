package codex

import (
	"context"
	"encoding/json"
	"errors"
	"net"
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

	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	finished := make(chan error, 1)
	go func() {
		_, err := startAppServer(ctx, bin, "test", runner.OS{}, nil)
		finished <- err
	}()

	var data []byte
	deadline := time.Now().Add(5 * time.Second)
	for time.Now().Before(deadline) {
		data, _ = os.ReadFile(bin + ".pid")
		if len(data) > 0 {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	if len(data) == 0 {
		t.Fatal("proxy did not report its PID")
	}
	cancel()
	err := <-finished
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("startAppServer() error = %v, want cancellation", err)
	}

	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil {
		t.Fatalf("parsing child PID: %v", err)
	}

	deadline = time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if errors.Is(syscall.Kill(pid, 0), syscall.ESRCH) {
			return
		}

		time.Sleep(10 * time.Millisecond)
	}

	t.Errorf("proxy child PID %d was not reaped", pid)
}

func TestManagerPausesAfterConnectionFailure(t *testing.T) {
	bin := filepath.Join(t.TempDir(), "failing-codex")
	if err := os.WriteFile(bin, []byte("#!/bin/sh\nprintf x >> \"$0.calls\"\nexit 1\n"), 0o700); err != nil {
		t.Fatal(err)
	}

	manager := NewManager(t.Context(), bin, "test", runner.OS{})
	if _, err := manager.connect(); err == nil {
		t.Fatal("first connection unexpectedly succeeded")
	}
	if _, err := manager.connect(); err == nil {
		t.Fatal("connection during retry pause unexpectedly succeeded")
	}

	calls, err := os.ReadFile(bin + ".calls")
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "x" {
		t.Errorf("launches during retry pause = %d, want 1", len(calls))
	}

	manager.mu.Lock()
	manager.nextAttempt = time.Now().Add(-time.Second)
	manager.mu.Unlock()
	if _, err := manager.connect(); err == nil {
		t.Fatal("connection after retry pause unexpectedly succeeded")
	}

	calls, err = os.ReadFile(bin + ".calls")
	if err != nil {
		t.Fatal(err)
	}
	if string(calls) != "xx" {
		t.Errorf("launches after retry pause = %d, want 2", len(calls))
	}
}

func TestProxyReadThread(t *testing.T) {
	stream := newScriptedStream("{\"id\":1,\"result\":{\"thread\":{\"id\":\"thr-1\",\"path\":\"/rollout.jsonl\",\"status\":{\"type\":\"active\",\"activeFlags\":[\"waitingOnApproval\"]}}}}\n")
	proxy := &Proxy{client: NewClient(stream, stream)}
	notifications := make(chan Notification)

	go func() {
		_ = proxy.client.Run(t.Context(), notifications)
	}()

	status, err := proxy.ReadThread(t.Context(), "thr-1")
	if err != nil {
		t.Fatalf("ReadThread() error = %v", err)
	}

	if status.Path != "/rollout.jsonl" {
		t.Fatalf("rollout path = %q", status.Path)
	}
	if got := status.Status.Classify(); got != mission.ActivityWaitingApproval {
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

func TestFindThreadSkipsSubagentsAndPaginates(t *testing.T) {
	conn, server := net.Pipe()
	defer conn.Close()
	defer server.Close()
	go func() {
		decoder := json.NewDecoder(server)
		for _, response := range [...]string{
			`{"id":1,"result":{"data":[{"id":"child","cwd":"/mission","parentThreadId":"root","status":{"type":"active","activeFlags":[]}}],"nextCursor":"next"}}`,
			`{"id":2,"result":{"data":[{"id":"root","cwd":"/mission","status":{"type":"idle"}}]}}`,
		} {
			var request json.RawMessage
			err := decoder.Decode(&request)
			if err != nil {
				return
			}
			_, err = server.Write([]byte(response + "\n"))
			if err != nil {
				return
			}
		}
	}()
	client := NewClient(conn, conn)
	go func() { _ = client.Run(t.Context(), make(chan Notification)) }()
	thread, found, err := (&Proxy{client: client}).FindThread(t.Context(), "/mission")
	if err != nil || !found || thread.ID != "root" {
		t.Fatalf("thread=%+v found=%v err=%v", thread, found, err)
	}
}
