package client

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"strings"

	"github.com/justinrush/q/internal/api"
)

// maxFrameBytes bounds a single event frame. The snapshot frame carries all
// operations and missions, so this is generous.
const maxFrameBytes = 8 << 20

// errStreamClosed reports that the daemon ended the stream. It is an error rather
// than a clean return so callers cannot forget to reconnect.
var errStreamClosed = errors.New("event stream closed by the daemon")

// Event is one server-sent event from the daemon.
type Event struct {
	// Name is the SSE event name, e.g. daemon.EventMission.
	Name string
	// Data is the raw JSON payload, decoded by the caller according to Name.
	Data json.RawMessage
}

// Decode unmarshals the event payload into dst.
func (e Event) Decode(dst any) error {
	if err := json.Unmarshal(e.Data, dst); err != nil {
		return fmt.Errorf("decoding %s event: %w", e.Name, err)
	}

	return nil
}

// Stream consumes the daemon's event stream, sending each event to out until ctx
// is canceled or the connection fails.
//
// It always returns a non-nil error, including on a clean server shutdown: the
// caller's job is to reconnect with backoff and re-synchronize from the snapshot
// frame the daemon sends on connect. That is what makes it safe for the daemon to
// drop a slow subscriber rather than block on it.
func (c *Client) Stream(ctx context.Context, out chan<- Event) error {
	req, err := c.newStreamRequest(ctx)
	if err != nil {
		return err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return fmt.Errorf("opening event stream: %w", err)
	}
	defer func() { _ = resp.Body.Close() }()

	if resp.StatusCode != http.StatusOK {
		return decodeError(resp, http.MethodGet, "/v1/events")
	}

	return readFrames(ctx, resp.Body, out)
}

// newStreamRequest builds the long-lived event-stream request. It deliberately
// does not apply the ordinary request timeout.
func (c *Client) newStreamRequest(ctx context.Context) (*http.Request, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, c.handle.BaseURL()+"/v1/events", nil)
	if err != nil {
		return nil, fmt.Errorf("building event stream request: %w", err)
	}

	req.Header.Set("Authorization", "Bearer "+c.handle.Token)
	req.Header.Set("Accept", "text/event-stream")
	req.Header.Set(api.ClientHeader, api.ClientHeaderValue)

	return req, nil
}

// readFrames parses the SSE wire format, emitting one Event per frame.
func readFrames(ctx context.Context, body io.Reader, out chan<- Event) error {
	scanner := bufio.NewScanner(body)
	scanner.Buffer(make([]byte, 0, 64<<10), maxFrameBytes)

	var name, data string

	for scanner.Scan() {
		line := scanner.Text()

		switch {
		case line == "":
			// A blank line terminates a frame.
			if data == "" {
				name, data = "", ""

				continue
			}

			event := Event{Name: name, Data: json.RawMessage(data)}
			name, data = "", ""

			select {
			case out <- event:
			case <-ctx.Done():
				return ctx.Err()
			}
		case strings.HasPrefix(line, "event:"):
			name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
		case strings.HasPrefix(line, "data:"):
			data += strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		default:
			// Comments and unknown fields are ignored, per the SSE spec.
		}
	}

	if err := scanner.Err(); err != nil {
		return fmt.Errorf("reading event stream: %w", err)
	}

	return errStreamClosed
}
