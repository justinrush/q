package mission

import (
	"encoding/json"
	"fmt"
)

// ParseToolHookEventBytes translates agent-specific payloads (such as agy's
// or opencode's camelCase or plugin formats) at the ingestion boundary so
// persisted and spooled events use the same state machine.
func ParseToolHookEventBytes(tool Tool, data []byte, event string) (HookEvent, error) {
	switch tool {
	case ToolAgy:
		return parseAgyHook(data, event)
	case ToolOpencode:
		return parseOpencodeHook(data, event)
	default:
		return ParseHookEventBytes(data, event)
	}
}

func parseAgyHook(data []byte, event string) (HookEvent, error) {
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

func parseOpencodeHook(data []byte, event string) (HookEvent, error) {
	var body struct {
		SessionID      string  `json:"session_id"`
		SessionIDCamel string  `json:"sessionID"`
		SessionIDAlt   string  `json:"sessionId"`
		CWD            string  `json:"cwd"`
		Directory      string  `json:"directory"`
		TranscriptPath *string `json:"transcript_path"`
		HookEventName  string  `json:"hook_event_name"`
		Source         *string `json:"source"`
		Reason         *string `json:"reason"`
		Error          *string `json:"error"`
		ToolName       *string `json:"tool_name"`
		ToolNameCamel  *string `json:"toolName"`
		Tool           *string `json:"tool"`
		Prompt         *string `json:"prompt"`
		LastAssistant  *string `json:"last_assistant_message"`
		PermissionMode *string `json:"permission_mode"`
	}
	if err := json.Unmarshal(data, &body); err != nil {
		return HookEvent{}, fmt.Errorf("decoding opencode hook: %w", err)
	}

	if event == "" {
		event = body.HookEventName
	}
	if event == "" {
		return HookEvent{}, fmt.Errorf("opencode hook payload names no event")
	}

	sessionID := body.SessionID
	if sessionID == "" {
		sessionID = body.SessionIDCamel
	}
	if sessionID == "" {
		sessionID = body.SessionIDAlt
	}

	cwd := body.CWD
	if cwd == "" {
		cwd = body.Directory
	}

	toolName := deref(body.ToolName)
	if toolName == "" {
		toolName = deref(body.ToolNameCamel)
	}
	if toolName == "" {
		toolName = deref(body.Tool)
	}

	reason := deref(body.Reason)
	if reason == "" {
		reason = deref(body.Error)
	}

	source := deref(body.Source)
	if source == "" && event == EventSessionStart {
		source = SourceStartup
	}

	return HookEvent{
		Event:                event,
		SessionID:            sessionID,
		CWD:                  cwd,
		TranscriptPath:       deref(body.TranscriptPath),
		Source:               source,
		Reason:               reason,
		ToolName:             toolName,
		Prompt:               deref(body.Prompt),
		LastAssistantMessage: deref(body.LastAssistant),
		PermissionMode:       deref(body.PermissionMode),
	}, nil
}
