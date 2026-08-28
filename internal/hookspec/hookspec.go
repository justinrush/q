// Package hookspec parses the hook payloads claude and codex write to a hook's
// standard input.
//
// The two agents deliberately share event names and most field names, so one
// parser serves both. Where they differ, the difference is recorded here rather
// than left for callers to rediscover:
//
//   - codex has no Notification, PermissionDenied, or StopFailure event.
//   - codex's transcript_path may be null; claude's is always a string.
//   - codex's session-end reason is always the literal "other" in 0.147.0.
//   - Fields such as permission_mode are absent on session-lifecycle events even
//     though the schema marks them optional, so every optional field is a pointer.
package hookspec

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Canonical hook event names, shared by both agents except where noted.
const (
	EventSessionStart      = "SessionStart"
	EventSessionEnd        = "SessionEnd"
	EventUserPromptSubmit  = "UserPromptSubmit"
	EventPreToolUse        = "PreToolUse"
	EventPostToolUse       = "PostToolUse"
	EventPermissionRequest = "PermissionRequest"
	// EventPermissionDenied is claude-only.
	EventPermissionDenied = "PermissionDenied"
	// EventNotification is claude-only.
	EventNotification = "Notification"
	EventStop         = "Stop"
	// EventStopFailure is claude-only, and fires instead of Stop when an API error
	// ended the turn.
	EventStopFailure   = "StopFailure"
	EventPreCompact    = "PreCompact"
	EventPostCompact   = "PostCompact"
	EventSubagentStart = "SubagentStart"
	EventSubagentStop  = "SubagentStop"
)

// ExitPlanModeTool is the tool whose permission request means a plan is ready for
// debrief, rather than that the agent is blocked on something routine.
const ExitPlanModeTool = "ExitPlanMode"

// Notification types claude uses. Notification is really eleven distinct events
// sharing one name, so the type is what carries the meaning.
const (
	// NotificationPermissionPrompt fires six seconds after a permission dialog
	// appears, so it is a backstop rather than a primary signal.
	NotificationPermissionPrompt = "permission_prompt"
	// NotificationIdlePrompt fires after sixty seconds of inactivity. Sixty
	// seconds of thinking is not the same as being blocked, so this only marks a
	// mission as stale.
	NotificationIdlePrompt = "idle_prompt"
	// NotificationAgentNeedsInput reports another tracked session being blocked.
	NotificationAgentNeedsInput = "agent_needs_input"
	// NotificationWorkerPermissionPrompt reports a worker awaiting permission.
	NotificationWorkerPermissionPrompt = "worker_permission_prompt"
)

// SessionStart sources.
const (
	SourceStartup = "startup"
	SourceResume  = "resume"
	SourceClear   = "clear"
	SourceCompact = "compact"
	SourceFork    = "fork"
)

// Payload is a parsed hook event.
type Payload struct {
	// Event is the canonical event name.
	Event string
	// SessionID is the agent's own session identifier. For codex this is the only
	// way q ever learns it.
	SessionID string
	// CWD is the agent's working directory, used as a last-resort way to identify
	// which mission an event belongs to.
	CWD string
	// TranscriptPath is the conversation log, absent when codex runs ephemerally.
	TranscriptPath string

	// Source is set on SessionStart.
	Source string
	// Reason is set on SessionEnd and on StopFailure, where it names the API
	// failure.
	Reason string
	// ToolName is set on the tool and permission events.
	ToolName string
	// NotificationType is set on Notification and carries its actual meaning.
	NotificationType string
	// Message is set on Notification.
	Message string
	// Prompt is set on UserPromptSubmit.
	Prompt string
	// LastAssistantMessage is the agent's closing message on Stop.
	LastAssistantMessage string
	// StopHookActive reports that a Stop hook already ran for this turn.
	StopHookActive bool
	// BackgroundTasks counts in-flight background work reported on Stop. A
	// non-zero count means the session is paused, not finished.
	BackgroundTasks int
	// PermissionMode is the agent's mode, absent on session-lifecycle events.
	PermissionMode string
}

// raw mirrors the on-the-wire payload. Every optional field is a pointer because
// several are documented as optional yet absent in practice, and because codex
// sends an explicit null for transcript_path.
type raw struct {
	SessionID            string            `json:"session_id"`
	CWD                  string            `json:"cwd"`
	TranscriptPath       *string           `json:"transcript_path"`
	HookEventName        string            `json:"hook_event_name"`
	Source               *string           `json:"source"`
	Reason               *string           `json:"reason"`
	ToolName             *string           `json:"tool_name"`
	NotificationType     *string           `json:"notification_type"`
	Message              *string           `json:"message"`
	Prompt               *string           `json:"prompt"`
	LastAssistantMessage *string           `json:"last_assistant_message"`
	StopHookActive       *bool             `json:"stop_hook_active"`
	BackgroundTasks      []json.RawMessage `json:"background_tasks"`
	PermissionMode       *string           `json:"permission_mode"`
}

// Parse decodes a hook payload.
//
// event is the canonical name the caller already knows from its own arguments; it
// wins over the payload's own hook_event_name, so a mismatch cannot silently
// reroute an event.
func Parse(r io.Reader, event string) (Payload, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return Payload{}, fmt.Errorf("reading hook payload: %w", err)
	}

	return ParseBytes(data, event)
}

// ParseBytes decodes a hook payload from bytes.
func ParseBytes(data []byte, event string) (Payload, error) {
	var body raw
	if err := json.Unmarshal(data, &body); err != nil {
		return Payload{}, fmt.Errorf("decoding hook payload: %w", err)
	}

	if event == "" {
		event = body.HookEventName
	}

	if event == "" {
		return Payload{}, fmt.Errorf("hook payload names no event")
	}

	return Payload{
		Event:                event,
		SessionID:            body.SessionID,
		CWD:                  body.CWD,
		TranscriptPath:       deref(body.TranscriptPath),
		Source:               deref(body.Source),
		Reason:               deref(body.Reason),
		ToolName:             deref(body.ToolName),
		NotificationType:     deref(body.NotificationType),
		Message:              deref(body.Message),
		Prompt:               deref(body.Prompt),
		LastAssistantMessage: deref(body.LastAssistantMessage),
		StopHookActive:       body.StopHookActive != nil && *body.StopHookActive,
		BackgroundTasks:      len(body.BackgroundTasks),
		PermissionMode:       deref(body.PermissionMode),
	}, nil
}

// deref returns the pointed-to string, or empty.
func deref(p *string) string {
	if p == nil {
		return ""
	}

	return *p
}

// IsPlanApproval reports whether the payload concerns claude's exit-from-plan-mode
// tool, whose permission request means a plan is ready for a human to read rather
// than that the agent is stuck.
func (p Payload) IsPlanApproval() bool { return p.ToolName == ExitPlanModeTool }

// CanonicalEvent converts a command-line event slug such as "session-start" to its
// canonical name.
func CanonicalEvent(slug string) (string, error) {
	want := strings.ReplaceAll(slug, "-", "")

	for _, event := range AllEvents {
		if strings.EqualFold(strings.ReplaceAll(event, "-", ""), want) {
			return event, nil
		}
	}

	return "", fmt.Errorf("unknown hook event %q", slug)
}

// EventSlug converts a canonical event name to its command-line slug.
func EventSlug(event string) string {
	var b strings.Builder

	for i, r := range event {
		if unicode.IsUpper(r) {
			if i > 0 {
				b.WriteByte('-')
			}

			b.WriteRune(unicode.ToLower(r))

			continue
		}

		b.WriteRune(r)
	}

	return b.String()
}

// AllEvents lists every event q understands from either agent.
var AllEvents = []string{
	EventSessionStart,
	EventSessionEnd,
	EventUserPromptSubmit,
	EventPreToolUse,
	EventPostToolUse,
	EventPermissionRequest,
	EventPermissionDenied,
	EventNotification,
	EventStop,
	EventStopFailure,
	EventPreCompact,
	EventPostCompact,
	EventSubagentStart,
	EventSubagentStop,
}
