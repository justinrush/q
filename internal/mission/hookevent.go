// Parsing the hook payloads agents write to a hook's standard input.
//
// claude and codex deliberately share event names and most field names, and
// internal/gemini translates gemini's names to the same set, so one parser
// serves them all. Where they still differ, the difference is recorded here
// rather than left for callers to rediscover:
//
//   - codex has no Notification, PermissionDenied, or StopFailure event.
//   - codex's transcript_path may be null; claude's is always a string.
//   - codex's session-end reason is always the literal "other" in 0.147.0.
//   - gemini names its events differently — BeforeTool, AfterAgent, and so on.
//     That mapping lives in internal/gemini, which emits q's canonical name in
//     the hook command, so nothing here knows about it. What gemini does change
//     is two field names: the closing message of a turn is prompt_response
//     rather than last_assistant_message, and a permission request names the
//     tool under details.type rather than tool_name.
//   - Fields such as permission_mode are absent on session-lifecycle events even
//     though the schema marks them optional, so every optional field is a pointer.

package mission

import (
	"encoding/json"
	"fmt"
	"io"
	"strings"
	"unicode"
)

// Canonical hook event names, shared by every agent except where noted.
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

// The tools whose permission request means a plan is ready for debrief, rather
// than that the agent is blocked on something routine. Each agent names its own.
const (
	// ExitPlanModeTool is claude's.
	ExitPlanModeTool = "ExitPlanMode"
	// ExitPlanModeToolGemini is gemini's, which arrives as the type of a
	// ToolPermission notification rather than as a tool name.
	ExitPlanModeToolGemini = "exit_plan_mode"
)

// planApprovalTools is every agent's name for that tool, so [HookEvent.IsPlanApproval]
// stays one check rather than a branch on which agent sent the event.
var planApprovalTools = map[string]bool{
	ExitPlanModeTool:       true,
	ExitPlanModeToolGemini: true,
}

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
	// NotificationToolPermission is gemini's only notification type, and unlike
	// claude's it is the primary blocked-on-the-human signal rather than a
	// backstop: gemini has no PermissionRequest event. internal/gemini therefore
	// reports it as EventPermissionRequest, and it never reaches
	// [applyNotification].
	NotificationToolPermission = "ToolPermission"
)

// SessionStart sources.
const (
	SourceStartup = "startup"
	SourceResume  = "resume"
	SourceClear   = "clear"
	SourceCompact = "compact"
	SourceFork    = "fork"
)

// HookEvent is a parsed hook event.
type HookEvent struct {
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
	// ToolName is set on the tool and permission events. For a gemini permission
	// request it is filled from details.type, which is the only place gemini
	// names what it is asking about.
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
	PromptResponse       *string           `json:"prompt_response"`
	Details              *details          `json:"details"`
}

// details is the alert metadata gemini attaches to a notification. Only the type
// is read: it is what distinguishes a plan approval from an ordinary tool
// confirmation.
type details struct {
	Type string `json:"type"`
}

// Parse decodes a hook payload.
//
// event is the canonical name the caller already knows from its own arguments; it
// wins over the payload's own hook_event_name, so a mismatch cannot silently
// reroute an event.
func ParseHookEvent(r io.Reader, event string) (HookEvent, error) {
	data, err := io.ReadAll(r)
	if err != nil {
		return HookEvent{}, fmt.Errorf("reading hook payload: %w", err)
	}

	return ParseHookEventBytes(data, event)
}

// ParseBytes decodes a hook payload from bytes.
func ParseHookEventBytes(data []byte, event string) (HookEvent, error) {
	var body raw
	if err := json.Unmarshal(data, &body); err != nil {
		return HookEvent{}, fmt.Errorf("decoding hook payload: %w", err)
	}

	if event == "" {
		event = body.HookEventName
	}

	if event == "" {
		return HookEvent{}, fmt.Errorf("hook payload names no event")
	}

	return HookEvent{
		Event:                event,
		SessionID:            body.SessionID,
		CWD:                  body.CWD,
		TranscriptPath:       deref(body.TranscriptPath),
		Source:               deref(body.Source),
		Reason:               deref(body.Reason),
		ToolName:             firstNonEmpty(deref(body.ToolName), detailType(body.Details)),
		NotificationType:     deref(body.NotificationType),
		Message:              deref(body.Message),
		Prompt:               deref(body.Prompt),
		LastAssistantMessage: firstNonEmpty(deref(body.LastAssistantMessage), deref(body.PromptResponse)),
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

// detailType returns a gemini notification's alert type, or empty.
func detailType(d *details) string {
	if d == nil {
		return ""
	}

	return d.Type
}

// firstNonEmpty returns the first value that is set. It resolves the two fields
// gemini names differently without giving either agent's spelling precedence
// over a value the other actually sent.
func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if v != "" {
			return v
		}
	}

	return ""
}

// IsPlanApproval reports whether the payload concerns an exit-from-plan-mode
// tool, whose permission request means a plan is ready for a human to read rather
// than that the agent is stuck.
func (p HookEvent) IsPlanApproval() bool { return planApprovalTools[p.ToolName] }

// CanonicalEvent converts a command-line event slug such as "session-start" to its
// canonical name.
func CanonicalHookEvent(slug string) (string, error) {
	want := strings.ReplaceAll(slug, "-", "")

	for _, event := range AllHookEvents {
		if strings.EqualFold(strings.ReplaceAll(event, "-", ""), want) {
			return event, nil
		}
	}

	return "", fmt.Errorf("unknown hook event %q", slug)
}

// EventSlug converts a canonical event name to its command-line slug.
func HookEventSlug(event string) string {
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
var AllHookEvents = []string{
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
