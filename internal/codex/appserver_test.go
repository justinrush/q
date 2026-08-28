package codex

import (
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"testing"
)

type scriptedStream struct {
	input  *strings.Reader
	output bytes.Buffer
	ready  chan struct{}
	once   sync.Once
}

func newScriptedStream(input string) *scriptedStream {
	return &scriptedStream{
		input: strings.NewReader(input),
		ready: make(chan struct{}),
	}
}

func (s *scriptedStream) Read(p []byte) (int, error) {
	<-s.ready

	return s.input.Read(p)
}

func (s *scriptedStream) Write(p []byte) (int, error) {
	n, err := s.output.Write(p)
	s.once.Do(func() { close(s.ready) })

	return n, err
}

func TestClientRequestAndNotification(t *testing.T) {
	stream := newScriptedStream("{\"method\":\"thread/status/changed\",\"params\":{\"threadId\":\"thr-1\",\"status\":{\"type\":\"idle\"}}}\n{\"id\":1,\"result\":{\"data\":[\"thr-1\"]}}\n")
	client := NewClient(stream, stream)
	notifications := make(chan Notification, 1)
	errCh := make(chan error, 1)

	go func() {
		errCh <- client.Run(t.Context(), notifications)
	}()

	var result struct {
		Data []string `json:"data"`
	}
	err := client.Request(t.Context(), "thread/loaded/list", struct{}{}, &result)
	if err != nil {
		t.Fatalf("Request() error = %v", err)
	}

	if len(result.Data) != 1 || result.Data[0] != "thr-1" {
		t.Errorf("Request() result = %#v", result)
	}

	notification := <-notifications
	if notification.Method != "thread/status/changed" {
		t.Errorf("notification method = %q", notification.Method)
	}

	var sent map[string]any
	err = json.Unmarshal(stream.output.Bytes(), &sent)
	if err != nil {
		t.Fatalf("decoding sent request: %v", err)
	}

	if sent["method"] != "thread/loaded/list" {
		t.Errorf("sent method = %v", sent["method"])
	}

	if err = <-errCh; !errors.Is(err, io.EOF) {
		t.Errorf("Run() error = %v, want EOF", err)
	}
}

func TestClientReturnsRPCError(t *testing.T) {
	stream := newScriptedStream("{\"id\":1,\"error\":{\"code\":-32600,\"message\":\"bad request\"}}\n")
	client := NewClient(stream, stream)
	notifications := make(chan Notification)

	go func() {
		_ = client.Run(t.Context(), notifications)
	}()

	err := client.Request(t.Context(), "broken", struct{}{}, nil)
	var rpcErr *RPCError
	if !errors.As(err, &rpcErr) {
		t.Fatalf("Request() error = %v, want RPCError", err)
	}

	if rpcErr.Code != -32600 {
		t.Errorf("RPCError.Code = %d", rpcErr.Code)
	}
}

func TestClientInitialize(t *testing.T) {
	stream := newScriptedStream("{\"id\":1,\"result\":{\"userAgent\":\"codex\"}}\n")
	client := NewClient(stream, stream)
	notifications := make(chan Notification)

	go func() {
		_ = client.Run(t.Context(), notifications)
	}()

	err := client.Initialize(t.Context(), "q", "Q", "test")
	if err != nil {
		t.Fatalf("Initialize() error = %v", err)
	}

	lines := strings.Split(strings.TrimSpace(stream.output.String()), "\n")
	if len(lines) != 2 {
		t.Fatalf("sent %d messages, want 2: %s", len(lines), stream.output.String())
	}

	if !strings.Contains(lines[0], `"method":"initialize"`) {
		t.Errorf("first message = %s", lines[0])
	}

	if !strings.Contains(lines[1], `"method":"initialized"`) {
		t.Errorf("second message = %s", lines[1])
	}
}
