package mission

import (
	"regexp"
	"testing"
	"time"
)

func TestStatusLabels(t *testing.T) {
	for _, tc := range []struct {
		status Status
		want   string
	}{
		{StatusBriefing, "briefing"},
		{StatusActive, "active"},
		{StatusAwaiting, "awaiting orders"},
		{StatusDebrief, "debrief"},
		{StatusClosed, "closed"},
	} {
		if got := tc.status.Label(); got != tc.want {
			t.Errorf("%s.Label() = %q, want %q", tc.status, got, tc.want)
		}
	}
}

func TestLanesCoversEveryValidStatus(t *testing.T) {
	if len(Lanes) != 5 {
		t.Fatalf("len(Lanes) = %d, want 5", len(Lanes))
	}

	for _, s := range Lanes {
		if !s.Valid() {
			t.Errorf("lane %q is not Valid", s)
		}
	}
}

// The reducer resolves competing transitions by precedence rather than arrival
// order, so the ordering itself is load-bearing: waiting must outrank debrief,
// which must outrank active.
func TestStatusPrecedenceOrdering(t *testing.T) {
	if StatusAwaiting.Precedence() <= StatusDebrief.Precedence() {
		t.Error("waiting must outrank debrief so a card never wrongly reads as finished")
	}

	if StatusDebrief.Precedence() <= StatusActive.Precedence() {
		t.Error("debrief must outrank active")
	}

	if StatusActive.Precedence() <= StatusBriefing.Precedence() {
		t.Error("active must outrank draft")
	}
}

func TestOnlyDoneIsTerminal(t *testing.T) {
	for _, s := range Lanes {
		want := s == StatusClosed
		if got := s.Terminal(); got != want {
			t.Errorf("%s.Terminal() = %v, want %v", s, got, want)
		}
	}
}

// The CLI accepts whichever spelling the user types.
func TestParseStatusAcceptsWireValueAndLabel(t *testing.T) {
	for _, in := range []string{"awaiting", "awaiting orders"} {
		got, err := ParseStatus(in)
		if err != nil {
			t.Fatalf("ParseStatus(%q): %v", in, err)
		}

		if got != StatusAwaiting {
			t.Errorf("ParseStatus(%q) = %q, want %q", in, got, StatusAwaiting)
		}
	}

	if _, err := ParseStatus("nonsense"); err == nil {
		t.Error("expected an error for an unknown status")
	}
}

func TestAgentStateValid(t *testing.T) {
	for _, a := range []AgentState{AgentUnknown, AgentBusy, AgentWaiting, AgentIdle, AgentDead} {
		if !a.Valid() {
			t.Errorf("%q should be valid", a)
		}
	}

	if AgentState("sleepy").Valid() {
		t.Error("unknown agent state should not be valid")
	}
}

// Plan mode is claude-only: codex 0.147.0 exposes no --permission-mode flag, so
// offering the toggle for codex would silently do nothing.
func TestPlanModeIsClaudeOnly(t *testing.T) {
	if !ToolClaude.SupportsPlanMode() {
		t.Error("claude should support plan mode")
	}

	if ToolCodex.SupportsPlanMode() {
		t.Error("codex must not advertise plan mode support")
	}
}

// claude takes --session-id; codex generates its own UUIDv7 and must be learned
// from its SessionStart hook instead.
func TestPresetSessionIDIsClaudeOnly(t *testing.T) {
	if !ToolClaude.SupportsPresetSessionID() {
		t.Error("claude should accept a preset session id")
	}

	if ToolCodex.SupportsPresetSessionID() {
		t.Error("codex must not advertise preset session id support")
	}
}

func TestParseTool(t *testing.T) {
	for _, tc := range []struct {
		in   string
		want Tool
	}{{"claude", ToolClaude}, {"codex", ToolCodex}} {
		got, err := ParseTool(tc.in)
		if err != nil || got != tc.want {
			t.Errorf("ParseTool(%q) = %q, %v", tc.in, got, err)
		}
	}

	if _, err := ParseTool("cursor"); err == nil {
		t.Error("expected an error for an unsupported tool")
	}
}

func TestToolGlyphsAreDistinct(t *testing.T) {
	if ToolClaude.Glyph() == ToolCodex.Glyph() {
		t.Error("tool glyphs must differ to be useful on a card")
	}
}

func TestNewIDsAreValidAndDistinct(t *testing.T) {
	operationA, err := NewOperationID()
	if err != nil {
		t.Fatalf("NewOperationID: %v", err)
	}

	operationB, err := NewOperationID()
	if err != nil {
		t.Fatalf("NewOperationID: %v", err)
	}

	if !operationA.Valid() || !operationB.Valid() {
		t.Errorf("generated operation ids invalid: %q %q", operationA, operationB)
	}

	if operationA == operationB {
		t.Error("operation ids collided")
	}

	missionID, err := NewMissionID()
	if err != nil {
		t.Fatalf("NewMissionID: %v", err)
	}

	if !missionID.Valid() {
		t.Errorf("generated mission id invalid: %q", missionID)
	}

	if got := missionID.Short(); len(got) != idBytes*2 {
		t.Errorf("Short() = %q, want %d hex chars", got, idBytes*2)
	}
}

func TestIDValidityRejectsMalformed(t *testing.T) {
	for _, id := range []OperationID{"", "op_", "op_xyz", "ms_aabbccddeeff", "aabbccddeeff", "op_AABBCCDDEEFF"} {
		if id.Valid() {
			t.Errorf("OperationID(%q).Valid() = true, want false", id)
		}
	}

	for _, id := range []MissionID{"", "ms_short", "op_aabbccddeeff"} {
		if id.Valid() {
			t.Errorf("MissionID(%q).Valid() = true, want false", id)
		}
	}
}

// claude validates --session-id against this exact shape and refuses anything
// else, so a malformed UUID would fail every launch.
func TestNewSessionUUIDMatchesClaudeExpectedShape(t *testing.T) {
	pattern := regexp.MustCompile(`^[0-9a-f]{8}-[0-9a-f]{4}-4[0-9a-f]{3}-[89ab][0-9a-f]{3}-[0-9a-f]{12}$`)

	seen := map[string]bool{}

	for range 50 {
		got, err := NewSessionUUID()
		if err != nil {
			t.Fatalf("NewSessionUUID: %v", err)
		}

		if !pattern.MatchString(got) {
			t.Fatalf("NewSessionUUID() = %q, which is not a v4 UUID", got)
		}

		if seen[got] {
			t.Fatalf("NewSessionUUID() repeated %q", got)
		}

		seen[got] = true
	}
}

func TestBadgeHelpers(t *testing.T) {
	ms := Mission{}

	ms.Badges = ms.WithBadge(BadgeStale, "")
	if !ms.HasBadge(BadgeStale) {
		t.Fatal("badge not added")
	}

	// Adding the same kind replaces rather than duplicates, so a repeatedly
	// firing reconciler cannot grow the badge list without bound.
	ms.Badges = ms.WithBadge(BadgeStale, "5m")
	if len(ms.Badges) != 1 {
		t.Fatalf("len(Badges) = %d, want 1", len(ms.Badges))
	}

	if ms.Badges[0].Detail != "5m" {
		t.Errorf("Detail = %q, want %q", ms.Badges[0].Detail, "5m")
	}

	ms.Badges = ms.WithBadge(BadgeAPIError, "rate_limit")
	ms.Badges = ms.WithoutBadge(BadgeStale)

	if ms.HasBadge(BadgeStale) {
		t.Error("badge not removed")
	}

	if !ms.HasBadge(BadgeAPIError) {
		t.Error("removing one badge dropped another")
	}
}

func TestWithoutBadgeIsANoopWhenAbsent(t *testing.T) {
	ms := Mission{Badges: []Badge{{Kind: BadgeAPIError}}}

	got := ms.WithoutBadge(BadgeStale)
	if len(got) != 1 {
		t.Errorf("len = %d, want 1", len(got))
	}
}

func TestLaunchedAndResumable(t *testing.T) {
	var ms Mission

	if ms.Launched() {
		t.Error("a fresh mission has not launched")
	}

	if ms.Resumable() {
		t.Error("a mission with no session id is not resumable")
	}

	now := time.Now()
	ms.StartedAt = &now
	ms.AgentSessionID = "abc"

	if !ms.Launched() || !ms.Resumable() {
		t.Error("mission should report launched and resumable")
	}
}

// Work is a map, so anything that renders or iterates it needs a deterministic
// order or generated prompts and pane layouts would shuffle between runs.
func TestWorktreesSortedByRepoName(t *testing.T) {
	ms := Mission{Work: map[string]RepoWork{
		"weave":                {RepoName: "weave"},
		"azure-tf":             {RepoName: "azure-tf"},
		"change-management-ui": {RepoName: "change-management-ui"},
	}}

	want := []string{"azure-tf", "change-management-ui", "weave"}

	got := ms.Worktrees()
	if len(got) != len(want) {
		t.Fatalf("len = %d, want %d", len(got), len(want))
	}

	for i, w := range want {
		if got[i].RepoName != w {
			t.Errorf("Worktrees()[%d] = %q, want %q", i, got[i].RepoName, w)
		}
	}
}

func TestWorktreesEmpty(t *testing.T) {
	if got := (Mission{}).Worktrees(); len(got) != 0 {
		t.Errorf("Worktrees() = %v, want empty", got)
	}
}
