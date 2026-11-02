// Package conversation exports visible planning context without importing one
// agent's private session format into another agent.
package conversation

import (
	"bufio"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"

	"github.com/justinrush/q/internal/mission"
)

const assistantRole = "assistant"

type Export struct{ Plan, Conversation string }
type record struct {
	Type    string          `json:"type"`
	Slug    string          `json:"slug"`
	Message json.RawMessage `json:"message"`
	Payload json.RawMessage `json:"payload"`
}
type message struct {
	Type      string          `json:"type"`
	Role      string          `json:"role"`
	Name      string          `json:"name"`
	Arguments string          `json:"arguments"`
	Input     string          `json:"input"`
	Output    json.RawMessage `json:"output"`
	Content   json.RawMessage `json:"content"`
}

// Read exports visible records only. Malformed records fail the handoff instead
// of silently dropping a clarification or handing over an incomplete plan.
func Read(r io.Reader, tool mission.Tool) (Export, string, error) {
	var out Export
	var history strings.Builder
	var slug string
	scan := bufio.NewScanner(r)
	scan.Buffer(make([]byte, 64*1024), 32<<20)
	for scan.Scan() {
		var rec record
		if err := json.Unmarshal(scan.Bytes(), &rec); err != nil {
			return out, slug, fmt.Errorf("reading planning transcript: %w", err)
		}
		if rec.Slug != "" {
			slug = rec.Slug
		}
		raw, err := rec.visibleMessage(tool)
		if err != nil {
			return out, slug, err
		}
		if len(raw) == 0 {
			continue
		}
		var msg message
		if err := json.Unmarshal(raw, &msg); err != nil {
			return out, slug, err
		}
		history.WriteString(msg.render())
		if plan := msg.plan(); plan != "" {
			out.Plan = plan
		}
	}
	if err := scan.Err(); err != nil {
		return out, slug, err
	}
	out.Conversation = history.String()
	return out, slug, nil
}

func (r record) visibleMessage(tool mission.Tool) (json.RawMessage, error) {
	switch tool {
	case mission.ToolClaude:
		if r.Type == "user" || r.Type == assistantRole {
			return r.Message, nil
		}
	case mission.ToolCodex:
		if r.Type == "response_item" {
			return r.Payload, nil
		}
	default:
		return nil, fmt.Errorf("no conversation exporter for %s", tool)
	}
	return nil, nil
}

func (m message) render() string {
	switch m.Type {
	case "function_call":
		return fmt.Sprintf("\n## Tool call: %s\n%s\n", m.Name, m.Arguments)
	case "custom_tool_call":
		return fmt.Sprintf("\n## Tool call: %s\n%s\n", m.Name, m.Input)
	case "function_call_output", "custom_tool_call_output":
		return "\n## Tool result\n" + visible(m.Output) + "\n"
	}
	if m.Role != assistantRole && m.Role != "user" {
		return ""
	}
	if text := visible(m.Content); text != "" {
		return fmt.Sprintf("\n## %s\n%s\n", m.Role, text)
	}
	return ""
}

func (m message) plan() string {
	if m.Role != assistantRole {
		return ""
	}
	text := visible(m.Content)
	if start := strings.LastIndex(text, "<proposed_plan>"); start >= 0 {
		if end := strings.Index(text[start:], "</proposed_plan>"); end >= 0 {
			return strings.TrimSpace(text[start+len("<proposed_plan>") : start+end])
		}
	}
	var blocks []struct {
		Type, Name string
		Input      struct {
			Plan string `json:"plan"`
		} `json:"input"`
	}
	if json.Unmarshal(m.Content, &blocks) != nil {
		return ""
	}
	var plan string
	for _, b := range blocks {
		if b.Type == "tool_use" && b.Name == "ExitPlanMode" && b.Input.Plan != "" {
			plan = b.Input.Plan
		}
	}
	return plan
}

func visible(raw json.RawMessage) string {
	var text string
	if json.Unmarshal(raw, &text) == nil {
		return text
	}
	var blocks []struct {
		Type    string          `json:"type"`
		Text    string          `json:"text"`
		Name    string          `json:"name"`
		Input   json.RawMessage `json:"input"`
		Content json.RawMessage `json:"content"`
	}
	if json.Unmarshal(raw, &blocks) != nil {
		return ""
	}
	var parts []string
	for _, b := range blocks {
		switch b.Type {
		case "text", "input_text", "output_text", "":
			parts = append(parts, b.Text)
		case "tool_use":
			parts = append(parts, "Tool call: "+b.Name+"\n"+string(b.Input))
		case "tool_result":
			parts = append(parts, "Tool result:\n"+visible(b.Content))
		}
	}
	return strings.Join(parts, "\n")
}

func FromMission(ms mission.Mission) (Export, error) {
	var out Export
	var slug string
	paths := []string{}
	if ms.RunningTool() == mission.ToolCodex {
		for path := range ms.CodexUsage {
			if path != ms.TranscriptPath {
				paths = append(paths, path)
			}
		}
		slices.Sort(paths)
	}
	paths = append(paths, ms.TranscriptPath)
	for _, path := range paths {
		part, sessionSlug, err := readFile(path, ms.RunningTool())
		if err != nil {
			return out, err
		}
		if part.Plan != "" {
			out.Plan = part.Plan
		}
		if sessionSlug != "" {
			slug = sessionSlug
		}
		out.Conversation += part.Conversation
	}
	// Claude stores its plan separately, keyed by the transcript's session slug.
	if ms.RunningTool() == mission.ToolClaude && slug != "" && filepath.Base(slug) == slug && slug != "." && slug != ".." {
		home, homeErr := os.UserHomeDir()
		if homeErr == nil {
			plan, readErr := os.ReadFile(filepath.Join(claudeConfigDir(home), "plans", slug+".md"))
			if readErr == nil {
				out.Plan = strings.TrimSpace(string(plan))
			}
		}
	}
	if strings.TrimSpace(out.Plan) == "" {
		return out, fmt.Errorf("no completed plan found in the planning session")
	}
	if strings.TrimSpace(out.Conversation) == "" {
		return out, fmt.Errorf("planning conversation is unavailable")
	}
	return out, nil
}

func readFile(path string, tool mission.Tool) (Export, string, error) {
	f, err := os.Open(path)
	if err != nil {
		return Export{}, "", fmt.Errorf("opening planning conversation: %w", err)
	}
	defer func() { _ = f.Close() }()
	return Read(f, tool)
}
func claudeConfigDir(home string) string {
	if dir := os.Getenv("CLAUDE_CONFIG_DIR"); dir != "" {
		return dir
	}
	return filepath.Join(home, ".claude")
}
