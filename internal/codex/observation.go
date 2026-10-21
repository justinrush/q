package codex

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"io"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/justinrush/q/internal/mission"
)

// rolloutObserver reads only appended complete lines. It deliberately ignores
// oversized tool output; execution settings and plan messages are small records.
type rolloutObserver struct {
	mu    sync.Mutex
	files map[string]rolloutObservation
}

type rolloutObservation struct {
	offset     int64
	session    string
	settings   mission.TurnSettings
	proposed   bool
	approved   bool
	activeTurn string
	active     bool
}

func (o *rolloutObserver) read(path, session string) (rolloutObservation, error) {
	o.mu.Lock()
	defer o.mu.Unlock()
	f, err := os.Open(path)
	if err != nil {
		return rolloutObservation{}, err
	}
	defer func() { _ = f.Close() }()
	info, err := f.Stat()
	if err != nil {
		return rolloutObservation{}, err
	}
	if o.files == nil {
		o.files = make(map[string]rolloutObservation)
	}
	state := o.files[path]
	if info.Size() < state.offset || state.session != session {
		state = rolloutObservation{session: session}
	}
	if _, err = f.Seek(state.offset, io.SeekStart); err != nil {
		return state, err
	}
	reader := bufio.NewReader(f)
	for {
		line, size, err := observationLine(reader)
		if err != nil {
			if !errors.Is(err, io.EOF) {
				return state, err
			}
			break // A partial last line is read again on the next poll.
		}
		state.offset += int64(size)
		state.apply(line)
	}
	o.files[path] = state
	return state, nil
}

func observationLine(r *bufio.Reader) ([]byte, int, error) {
	const limit = 2 * 1024 * 1024
	var line []byte
	size := 0
	for {
		part, err := r.ReadSlice('\n')
		size += len(part)
		if size <= limit {
			line = append(line, part...)
		} else {
			line = nil
		}
		if errors.Is(err, bufio.ErrBufferFull) {
			continue
		}
		return line, size, err
	}
}

type observationRecord struct {
	Type      string             `json:"type"`
	Timestamp time.Time          `json:"timestamp"`
	Payload   observationPayload `json:"payload"`
}

type observationPayload struct {
	ID      string `json:"id"`
	TurnID  string `json:"turn_id"`
	Model   string `json:"model"`
	Effort  string `json:"effort"`
	Type    string `json:"type"`
	Role    string `json:"role"`
	Content []struct {
		Text string `json:"text"`
	} `json:"content"`
	Collaboration *struct {
		Mode     string `json:"mode"`
		Settings struct {
			Model  string `json:"model"`
			Effort string `json:"reasoning_effort"`
		} `json:"settings"`
	} `json:"collaboration_mode"`
}

func (o *rolloutObservation) apply(line []byte) {
	var record observationRecord
	// Avoid decoding large tool outputs at all.
	if !bytes.Contains(line, []byte(`"turn_context"`)) && !bytes.Contains(line, []byte(`"response_item"`)) && !bytes.Contains(line, []byte(`"session_meta"`)) && !bytes.Contains(line, []byte(`"event_msg"`)) {
		return
	}
	if json.Unmarshal(line, &record) != nil {
		return
	}
	p := record.Payload
	switch record.Type {
	case "session_meta":
		if p.ID != "" && p.ID != o.session {
			o.settings = mission.TurnSettings{}
			o.session = p.ID
		}
	case "event_msg":
		switch p.Type {
		case "task_started":
			o.activeTurn, o.active = p.TurnID, true
		case "task_complete", "turn_aborted":
			if p.TurnID == o.activeTurn {
				o.active = false
			}
		}
	case "turn_context":
		s := mission.TurnSettings{SessionID: o.session, TurnID: p.TurnID, Model: p.Model, Effort: p.Effort, At: record.Timestamp}
		if p.Collaboration != nil {
			s.Mode = p.Collaboration.Mode
			if p.Collaboration.Settings.Model != "" {
				s.Model = p.Collaboration.Settings.Model
			}
			if p.Collaboration.Settings.Effort != "" {
				s.Effort = p.Collaboration.Settings.Effort
			}
		}
		if o.proposed && o.settings.Mode == modePlan && s.Mode == modeDefault {
			o.approved = true
		}
		o.settings = s
	case "response_item":
		o.applyMessage(p)
	}
}

func (o *rolloutObservation) applyMessage(p observationPayload) {
	if p.Type != "message" {
		return
	}
	for _, content := range p.Content {
		if p.Role == "assistant" && o.settings.Mode == modePlan && strings.Contains(content.Text, "<proposed_plan>") && strings.Contains(content.Text, "</proposed_plan>") {
			o.proposed = true
		}
		if p.Role == "user" && (mission.HookEvent{Prompt: content.Text}).IsPlanGoAhead() {
			o.approved = true
		}
	}
}
