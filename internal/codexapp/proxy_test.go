package codexapp

import "testing"

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

	if got := status.Classify(); got != ActivityWaitingApproval {
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
