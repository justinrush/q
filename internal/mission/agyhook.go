package mission

import (
	"encoding/json"
	"fmt"
)

// ParseToolHookEventBytes translates agy's camelCase payloads at the ingestion
// boundary so persisted and spooled events use the same state machine.
func ParseToolHookEventBytes(tool Tool, data []byte, event string) (HookEvent, error) {
	if tool != ToolAgy {
		return ParseHookEventBytes(data, event)
	}
	var body struct {
		ConversationID string   `json:"conversationId"`
		WorkspacePaths []string `json:"workspacePaths"`
		TranscriptPath string   `json:"transcriptPath"`
		ToolCall       struct {
			Name string `json:"name"`
		} `json:"toolCall"`
		FullyIdle         *bool  `json:"fullyIdle"`
		TerminationReason string `json:"terminationReason"`
		Error             string `json:"error"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return HookEvent{}, fmt.Errorf("decoding agy hook: %w", err)
	}
	ev := HookEvent{Event: event, SessionID: body.ConversationID, TranscriptPath: body.TranscriptPath, ToolName: body.ToolCall.Name}
	if len(body.WorkspacePaths) > 0 {
		ev.CWD = body.WorkspacePaths[0]
	}
	if event == EventSessionStart {
		ev.Source = SourceStartup
	}
	if event == EventStop {
		// Missing idle information cannot establish that background work finished.
		if body.FullyIdle == nil || !*body.FullyIdle {
			ev.BackgroundTasks = 1
		}
		if body.TerminationReason == "error" || body.Error != "" {
			ev.Event = EventStopFailure
			ev.Reason = body.Error
			if ev.Reason == "" {
				ev.Reason = body.TerminationReason
			}
		}
	}
	return ev, nil
}
