package codex

import (
	"encoding/json"
	"net"
	"net/http/httptest"
	"strings"
	"testing"

	"golang.org/x/net/websocket"
)

// Exercise an actual HTTP upgrade and masked WebSocket frames, including
// responses without trailing newlines, as emitted by the managed daemon.
func TestProxyWebSocketProtocol(t *testing.T) {
	server := httptest.NewServer(websocket.Handler(func(conn *websocket.Conn) {
		defer conn.Close()
		for {
			var request struct {
				ID     int    `json:"id"`
				Method string `json:"method"`
			}
			err := websocket.JSON.Receive(conn, &request)
			if err != nil {
				return
			}
			if request.Method == "initialized" {
				continue
			}
			result := json.RawMessage(`{}`)
			if request.Method == "thread/read" {
				result = json.RawMessage(`{"thread":{"id":"live","status":{"type":"active","activeFlags":["waitingOnUserInput"]}}}`)
			}
			err = websocket.JSON.Send(conn, struct {
				ID     int             `json:"id"`
				Result json.RawMessage `json:"result"`
			}{request.ID, result})
			if err != nil {
				return
			}
		}
	}))
	defer server.Close()

	conn, err := net.Dial("tcp", strings.TrimPrefix(server.URL, "http://"))
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	transport, err := newProxyTransport(conn, conn)
	if err != nil {
		t.Fatal(err)
	}
	client := NewClient(transport, transport.conn)
	notifications := make(chan Notification, 1)
	go func() { _ = client.Run(t.Context(), notifications) }()
	err = client.Initialize(t.Context(), "test", "Test", "1")
	if err != nil {
		t.Fatal(err)
	}
	proxy := &Proxy{client: client}
	status, err := proxy.ReadThread(t.Context(), "live")
	if err != nil {
		t.Fatal(err)
	}
	if status.Type != "active" || len(status.ActiveFlags) != 1 || status.ActiveFlags[0] != flagWaitingInput {
		t.Fatalf("unexpected status: %+v", status)
	}
}
