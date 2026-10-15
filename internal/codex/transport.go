package codex

import (
	"io"
	"strings"

	"golang.org/x/net/websocket"
)

// proxyTransport adapts WebSocket message boundaries to the JSON-lines reader
// shared with the standalone stdio app-server. The proxy forwards raw Unix
// socket bytes; unlike `codex app-server`, it does not translate JSON lines.
type proxyTransport struct {
	conn    *websocket.Conn
	pending *strings.Reader
}

func newProxyTransport(reader io.ReadCloser, writer io.WriteCloser) (*proxyTransport, error) {
	config, err := websocket.NewConfig("ws://localhost/", "http://localhost")
	if err != nil {
		return nil, err
	}
	conn, err := websocket.NewClient(config, proxyPipes{reader, writer})
	if err != nil {
		return nil, err
	}
	conn.PayloadType = websocket.TextFrame
	conn.MaxPayloadBytes = maxMessageBytes
	return &proxyTransport{conn: conn}, nil
}

func (p *proxyTransport) Read(b []byte) (int, error) {
	if p.pending == nil || p.pending.Len() == 0 {
		var message string
		err := websocket.Message.Receive(p.conn, &message)
		if err != nil {
			return 0, err
		}
		p.pending = strings.NewReader(message + "\n")
	}
	return p.pending.Read(b)
}

type proxyPipes struct {
	io.ReadCloser
	io.WriteCloser
}

func (p proxyPipes) Close() error {
	readErr := p.ReadCloser.Close()
	writeErr := p.WriteCloser.Close()
	if readErr != nil {
		return readErr
	}
	return writeErr
}
