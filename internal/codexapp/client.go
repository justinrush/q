// Package codexapp speaks the Codex app-server JSON-RPC protocol.
//
// The protocol deliberately lives behind a small standard-library-only client.
// Q reaches a running app-server through `codex app-server proxy`, whose
// stdin and stdout are newline-delimited JSON messages.
package codexapp

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"sync"
	"sync/atomic"
)

const maxMessageBytes = 16 * 1024 * 1024

// Client exchanges requests, responses, and notifications over one app-server
// connection. Run must be active while Request waits for a response.
type Client struct {
	reader io.Reader
	writer io.Writer

	nextID  atomic.Int64
	writes  sync.Mutex
	mu      sync.Mutex
	pending map[int64]chan response
	closed  error
}

type response struct {
	message message
	err     error
}

type message struct {
	ID     *int64          `json:"id,omitempty"`
	Method string          `json:"method,omitempty"`
	Params json.RawMessage `json:"params,omitempty"`
	Result json.RawMessage `json:"result,omitempty"`
	Error  *RPCError       `json:"error,omitempty"`
}

// RPCError is an error returned by app-server for a JSON-RPC request.
type RPCError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

// Error implements error.
func (e *RPCError) Error() string {
	return fmt.Sprintf("codex app-server error %d: %s", e.Code, e.Message)
}

// Notification is a server-initiated event.
type Notification struct {
	Method string
	Params json.RawMessage
}

// NewClient returns a client over a newline-delimited JSON stream.
func NewClient(reader io.Reader, writer io.Writer) *Client {
	return &Client{
		reader:  reader,
		writer:  writer,
		pending: make(map[int64]chan response),
	}
}

// Run reads the connection until it closes or ctx is canceled.
func (c *Client) Run(ctx context.Context, notifications chan<- Notification) error {
	scanner := bufio.NewScanner(c.reader)
	buffer := make([]byte, 64*1024)
	scanner.Buffer(buffer, maxMessageBytes)

	for scanner.Scan() {
		var incoming message
		err := json.Unmarshal(scanner.Bytes(), &incoming)
		if err != nil {
			c.fail(fmt.Errorf("decoding app-server message: %w", err))

			return c.closedError()
		}

		if incoming.ID != nil {
			c.deliver(*incoming.ID, incoming)

			continue
		}

		if incoming.Method == "" {
			continue
		}

		select {
		case notifications <- Notification{Method: incoming.Method, Params: incoming.Params}:
		case <-ctx.Done():
			c.fail(ctx.Err())

			return ctx.Err()
		}
	}

	err := scanner.Err()
	if err == nil {
		err = io.EOF
	}

	c.fail(err)

	return err
}

// Request sends one request and waits for its response.
func (c *Client) Request(ctx context.Context, method string, params any, result any) error {
	id := c.nextID.Add(1)
	replies := make(chan response, 1)

	c.mu.Lock()
	if c.closed != nil {
		err := c.closed
		c.mu.Unlock()

		return err
	}
	c.pending[id] = replies
	c.mu.Unlock()

	err := c.write(message{ID: &id, Method: method}, params)
	if err != nil {
		c.removePending(id)

		return err
	}

	select {
	case reply := <-replies:
		if reply.err != nil {
			return reply.err
		}

		if reply.message.Error != nil {
			return reply.message.Error
		}

		if result == nil || len(reply.message.Result) == 0 {
			return nil
		}

		err = json.Unmarshal(reply.message.Result, result)
		if err != nil {
			return fmt.Errorf("decoding %s response: %w", method, err)
		}

		return nil
	case <-ctx.Done():
		c.removePending(id)

		return ctx.Err()
	}
}

// Notify sends a notification, which has no response id.
func (c *Client) Notify(method string, params any) error {
	return c.write(message{Method: method}, params)
}

// Initialize performs the required app-server connection handshake.
func (c *Client) Initialize(ctx context.Context, name, title, version string) error {
	params := struct {
		ClientInfo struct {
			Name    string `json:"name"`
			Title   string `json:"title"`
			Version string `json:"version"`
		} `json:"clientInfo"`
	}{}
	params.ClientInfo.Name = name
	params.ClientInfo.Title = title
	params.ClientInfo.Version = version

	var result json.RawMessage
	err := c.Request(ctx, "initialize", params, &result)
	if err != nil {
		return err
	}

	err = c.Notify("initialized", struct{}{})
	if err != nil {
		return fmt.Errorf("acknowledging app-server initialization: %w", err)
	}

	return nil
}

func (c *Client) write(base message, params any) error {
	encoded, err := json.Marshal(params)
	if err != nil {
		return fmt.Errorf("encoding %s params: %w", base.Method, err)
	}
	base.Params = encoded

	c.writes.Lock()
	defer c.writes.Unlock()

	encoder := json.NewEncoder(c.writer)
	err = encoder.Encode(base)
	if err != nil {
		return fmt.Errorf("writing %s request: %w", base.Method, err)
	}

	return nil
}

func (c *Client) deliver(id int64, incoming message) {
	c.mu.Lock()
	replies, ok := c.pending[id]
	if ok {
		delete(c.pending, id)
	}
	c.mu.Unlock()

	if ok {
		replies <- response{message: incoming}
	}
}

func (c *Client) removePending(id int64) {
	c.mu.Lock()
	delete(c.pending, id)
	c.mu.Unlock()
}

func (c *Client) fail(err error) {
	if err == nil {
		err = errors.New("app-server connection closed")
	}

	c.mu.Lock()
	if c.closed != nil {
		c.mu.Unlock()

		return
	}

	c.closed = err
	pending := c.pending
	c.pending = make(map[int64]chan response)
	c.mu.Unlock()

	for _, replies := range pending {
		replies <- response{err: err}
	}
}

func (c *Client) closedError() error {
	c.mu.Lock()
	defer c.mu.Unlock()

	return c.closed
}
