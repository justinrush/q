package daemon

import (
	"encoding/json"
	"fmt"
	"github.com/justinrush/q/internal/mission"
	"sync"
)

// SSE event names.
// subBuffer is how many frames a subscriber may fall behind before it is dropped.
//
// Dropping a slow subscriber is the right trade: the reducer must never block on
// a client, and a dropped client reconnects and receives a fresh snapshot, so no
// state is lost by disconnecting it.
const subBuffer = 64

// Hub fans state changes out to connected clients. It is safe for concurrent use.
type Hub struct {
	mu     sync.Mutex
	subs   map[int]chan []byte
	nextID int
	closed bool
}

// NewHub returns an empty Hub.
func NewHub() *Hub {
	return &Hub{subs: map[int]chan []byte{}}
}

// Subscribe registers a client, returning its id and frame channel. The channel
// is closed when the subscriber is dropped or the hub shuts down.
func (h *Hub) Subscribe() (int, <-chan []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()

	ch := make(chan []byte, subBuffer)

	if h.closed {
		close(ch)

		return 0, ch
	}

	h.nextID++
	id := h.nextID
	h.subs[id] = ch

	return id, ch
}

// Unsubscribe removes a client and closes its channel.
func (h *Hub) Unsubscribe(id int) {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.drop(id)
}

// drop removes a subscriber. The caller must hold the mutex.
func (h *Hub) drop(id int) {
	if ch, ok := h.subs[id]; ok {
		delete(h.subs, id)
		close(ch)
	}
}

// Broadcast sends an event to every subscriber.
//
// It never blocks: a subscriber whose buffer is full is dropped rather than
// waited on, because this runs on the path that applies hook events and stalling
// it would let one wedged client freeze the whole board.
func (h *Hub) Broadcast(event string, payload any) {
	// Encoding a mission is most of the cost of a broadcast, and with no board
	// open, which is most of the time, nobody would read it.
	if h.Subscribers() == 0 {
		return
	}

	frame, err := encodeFrame(event, payload)
	if err != nil {
		return
	}

	h.mu.Lock()
	defer h.mu.Unlock()

	for id, ch := range h.subs {
		select {
		case ch <- frame:
		default:
			h.drop(id)
		}
	}
}

// Subscribers reports the current subscriber count.
func (h *Hub) Subscribers() int {
	h.mu.Lock()
	defer h.mu.Unlock()

	return len(h.subs)
}

// Close drops every subscriber and refuses new ones.
func (h *Hub) Close() {
	h.mu.Lock()
	defer h.mu.Unlock()

	h.closed = true

	for id := range h.subs {
		h.drop(id)
	}
}

// encodeFrame renders one server-sent event.
//
// The JSON payload is emitted on a single line. That is required rather than
// cosmetic: the SSE wire format is line-oriented, so an embedded newline would
// be parsed as a field boundary and split one event into two malformed ones.
func encodeFrame(event string, payload any) ([]byte, error) {
	data, err := json.Marshal(boardPayload(payload))
	if err != nil {
		return nil, fmt.Errorf("encoding %s event: %w", event, err)
	}

	frame := make([]byte, 0, len(data)+len(event)+16)
	frame = append(frame, "event: "...)
	frame = append(frame, event...)
	frame = append(frame, "\ndata: "...)
	frame = append(frame, data...)
	frame = append(frame, "\n\n"...)

	return frame, nil
}

// boardPayload is what a client is sent of the board: by the stream, and in
// every JSON reply.
//
// Conversation artifacts travel in durable state and peer exchange, not on
// every board refresh. Large tool outputs must not exhaust the SSE frame limit.
//
// The Codex usage checkpoint stays home too. It is one record per response the
// agent ever made, which on a long Codex mission runs to megabytes, and only
// the daemon's meter reads it: a client is shown Usage, its sum.
func boardPayload(payload any) any {
	switch value := payload.(type) {
	case mission.Mission:
		return clientMission(value)
	case []mission.Mission:
		return clientMissions(value)
	case mission.Snapshot:
		value.Missions = clientMissions(value.Missions)
		return value
	default:
		return payload
	}
}

func clientMission(m mission.Mission) mission.Mission {
	m.ToolHandoff.Plan = ""
	m.ToolHandoff.Conversation = ""
	m.CodexUsage = nil

	return m
}

func clientMissions(missions []mission.Mission) []mission.Mission {
	out := make([]mission.Mission, len(missions))
	for i, m := range missions {
		out[i] = clientMission(m)
	}

	return out
}
