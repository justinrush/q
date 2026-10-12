package mission

import (
	"strings"
	"testing"
	"time"
)

var testNow = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

// missionIn returns a launched mission sitting in the given lane.
func missionIn(status Status) Mission {
	return Mission{
		ID:             "ms_aabbccddeeff",
		Tool:           ToolClaude,
		Status:         status,
		AgentState:     AgentBusy,
		AgentSessionID: "sess-1",
		HookEpoch:      1,
	}
}

// ev builds a payload for an event.
func ev(event string) HookEvent {
	return HookEvent{Event: event, SessionID: "sess-1"}
}

// The full event-to-lane table. Each row states the lane an event argues for from a
// given starting lane, with the empty status meaning "argues for none".
func TestReduceTransitionTable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from     Status
		payload  HookEvent
		want     Status
		wantMore func(*testing.T, Mission)
	}{
		{
			name:    "session start confirms the agent came up",
			from:    StatusBriefing,
			payload: HookEvent{Event: EventSessionStart, SessionID: "sess-1", Source: SourceStartup},
			want:    StatusActive,
			wantMore: func(t *testing.T, ms Mission) {
				if ms.WaitingFor != "" {
					t.Errorf("WaitingFor = %q, want cleared", ms.WaitingFor)
				}
			},
		},
		{
			name:    "session start on resume also reports active",
			from:    StatusDebrief,
			payload: HookEvent{Event: EventSessionStart, SessionID: "sess-1", Source: SourceResume},
			want:    StatusActive,
		},
		{
			name:    "user prompt clears a block",
			from:    StatusAwaiting,
			payload: ev(EventUserPromptSubmit),
			want:    StatusActive,
			wantMore: func(t *testing.T, ms Mission) {
				if ms.WaitingFor != "" {
					t.Errorf("WaitingFor = %q, want cleared", ms.WaitingFor)
				}
			},
		},
		{
			name:    "pre tool use argues for no lane",
			from:    StatusActive,
			payload: ev(EventPreToolUse),
			want:    "",
		},
		{
			name:    "permission request blocks on the human",
			from:    StatusActive,
			payload: HookEvent{Event: EventPermissionRequest, SessionID: "sess-1", ToolName: "Bash"},
			want:    StatusAwaiting,
			wantMore: func(t *testing.T, ms Mission) {
				if ms.WaitingFor != "Bash" {
					t.Errorf("WaitingFor = %q, want %q", ms.WaitingFor, "Bash")
				}

				if ms.AgentState != AgentWaiting {
					t.Errorf("AgentState = %q", ms.AgentState)
				}
			},
		},
		{
			name: "exit plan mode request goes to debrief, not waiting",
			from: StatusActive,
			payload: HookEvent{
				Event: EventPermissionRequest, SessionID: "sess-1",
				ToolName: ExitPlanModeTool,
			},
			want: StatusDebrief,
			wantMore: func(t *testing.T, ms Mission) {
				if !ms.PlanPending {
					t.Error("PlanPending should be set")
				}
			},
		},
		{
			name: "exit plan mode completing returns to active",
			from: StatusDebrief,
			payload: HookEvent{
				Event: EventPostToolUse, SessionID: "sess-1",
				ToolName: ExitPlanModeTool,
			},
			want: StatusActive,
			wantMore: func(t *testing.T, ms Mission) {
				if ms.PlanPending {
					t.Error("PlanPending should be cleared")
				}
			},
		},
		{
			name:    "stop ends the turn",
			from:    StatusActive,
			payload: HookEvent{Event: EventStop, SessionID: "sess-1", LastAssistantMessage: "all done"},
			want:    StatusDebrief,
			wantMore: func(t *testing.T, ms Mission) {
				if ms.LastMessage != "all done" {
					t.Errorf("LastMessage = %q", ms.LastMessage)
				}

				if ms.AgentState != AgentIdle {
					t.Errorf("AgentState = %q, want idle", ms.AgentState)
				}

				if ms.FinishedAt == nil {
					t.Error("FinishedAt should be set")
				}
			},
		},
		{
			name:    "api failure needs attention",
			from:    StatusActive,
			payload: HookEvent{Event: EventStopFailure, SessionID: "sess-1", Reason: "rate_limit"},
			want:    StatusAwaiting,
			wantMore: func(t *testing.T, ms Mission) {
				if !ms.HasBadge(BadgeAPIError) {
					t.Error("expected an api badge")
				}

				if !strings.Contains(ms.WaitingFor, "rate_limit") {
					t.Errorf("WaitingFor = %q, want it to name the reason", ms.WaitingFor)
				}
			},
		},
		{
			name:    "session end reports debrief and a dead agent",
			from:    StatusActive,
			payload: HookEvent{Event: EventSessionEnd, SessionID: "sess-1", Reason: "other"},
			want:    StatusDebrief,
			wantMore: func(t *testing.T, ms Mission) {
				if ms.AgentState != AgentDead {
					t.Errorf("AgentState = %q, want dead", ms.AgentState)
				}
			},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			res := Reduce(missionIn(tc.from), tc.payload, testNow)

			if res.ProposedStatus != tc.want {
				t.Errorf("ProposedStatus = %q, want %q", res.ProposedStatus, tc.want)
			}

			if tc.wantMore != nil {
				tc.wantMore(t, res.Mission)
			}
		})
	}
}

// This is the self-healing transition, and the most load-bearing row in the table:
// the common real flow is answering a prompt in the pane and never touching q.
func TestPostToolUseHealsAWaitingCard(t *testing.T) {
	ms := missionIn(StatusAwaiting)
	ms.WaitingFor = "Bash"

	res := Reduce(ms, HookEvent{
		Event: EventPostToolUse, SessionID: "sess-1", ToolName: "Bash",
	}, testNow)

	if res.ProposedStatus != StatusActive {
		t.Errorf("ProposedStatus = %q, want active", res.ProposedStatus)
	}

	if res.Mission.WaitingFor != "" {
		t.Errorf("WaitingFor = %q, want cleared", res.Mission.WaitingFor)
	}
}

// A completed tool call on a card that was never blocked must not reshuffle it.
func TestPostToolUseDoesNotDisturbAnInProgressCard(t *testing.T) {
	res := Reduce(missionIn(StatusActive), HookEvent{
		Event: EventPostToolUse, SessionID: "sess-1", ToolName: "Read",
	}, testNow)

	if res.ProposedStatus != "" {
		t.Errorf("ProposedStatus = %q, want no proposal", res.ProposedStatus)
	}
}

// Ordinary denials come from the user's own guard hooks, which the agent handles
// itself. Treating them as blocked would park cards in waiting for no reason.
func TestOrdinaryPermissionDenialDoesNotBlock(t *testing.T) {
	res := Reduce(missionIn(StatusActive), HookEvent{
		Event: EventPermissionDenied, SessionID: "sess-1", ToolName: "Bash",
	}, testNow)

	if res.ProposedStatus != "" {
		t.Errorf("ProposedStatus = %q, want no proposal", res.ProposedStatus)
	}
}

// A rejected plan does need a human.
func TestRejectedPlanNeedsDirection(t *testing.T) {
	ms := missionIn(StatusDebrief)
	ms.PlanPending = true

	res := Reduce(ms, HookEvent{
		Event: EventPermissionDenied, SessionID: "sess-1", ToolName: ExitPlanModeTool,
	}, testNow)

	if res.ProposedStatus != StatusAwaiting {
		t.Errorf("ProposedStatus = %q, want waiting", res.ProposedStatus)
	}

	if res.Mission.PlanPending {
		t.Error("PlanPending should be cleared")
	}
}

// A pending plan debrief outranks a Stop; downgrading it would strip the card of its
// "needs your approval" meaning.
func TestStopDoesNotDowngradeAPendingPlan(t *testing.T) {
	ms := missionIn(StatusDebrief)
	ms.PlanPending = true

	res := Reduce(ms, ev(EventStop), testNow)

	if res.ProposedStatus != "" {
		t.Errorf("ProposedStatus = %q, want no proposal", res.ProposedStatus)
	}

	if res.Mission.FinishedAt != nil {
		t.Error("a pending plan is not finished")
	}
}

// In-flight background work means paused, not finished.
func TestStopWithBackgroundWorkStaysInProgress(t *testing.T) {
	res := Reduce(missionIn(StatusActive), HookEvent{
		Event: EventStop, SessionID: "sess-1", BackgroundTasks: 2,
	}, testNow)

	if res.ProposedStatus != StatusActive {
		t.Errorf("ProposedStatus = %q, want active", res.ProposedStatus)
	}

	if !res.Mission.HasBadge(BadgeBackground) {
		t.Error("expected a background badge")
	}

	if res.Mission.FinishedAt != nil {
		t.Error("a paused mission is not finished")
	}
}

// Sixty seconds of quiet is neither blocked nor finished.
func TestIdleNotificationOnlyMarksStale(t *testing.T) {
	res := Reduce(missionIn(StatusActive), HookEvent{
		Event: EventNotification, SessionID: "sess-1",
		NotificationType: NotificationIdlePrompt,
	}, testNow)

	if res.ProposedStatus != "" {
		t.Errorf("ProposedStatus = %q, want no lane change for idle", res.ProposedStatus)
	}

	if !res.Mission.HasBadge(BadgeStale) {
		t.Error("expected a stale badge")
	}
}

// The permission notification arrives six seconds after PermissionRequest already
// reported the same thing, so it may only promote a card that still looks busy.
func TestPermissionNotificationOnlyPromotesABusyCard(t *testing.T) {
	busy := Reduce(missionIn(StatusActive), HookEvent{
		Event: EventNotification, SessionID: "sess-1",
		NotificationType: NotificationPermissionPrompt,
		Message:          "Claude needs your permission to use Bash",
	}, testNow)

	if busy.ProposedStatus != StatusAwaiting {
		t.Errorf("ProposedStatus = %q, want waiting", busy.ProposedStatus)
	}

	debriefed := Reduce(missionIn(StatusDebrief), HookEvent{
		Event: EventNotification, SessionID: "sess-1",
		NotificationType: NotificationPermissionPrompt,
	}, testNow)

	if debriefed.ProposedStatus != "" {
		t.Errorf("ProposedStatus = %q, want no proposal from a debriefed card", debriefed.ProposedStatus)
	}
}

// A clear, compact, or fork continues the same work and must not drag a card out of
// debrief.
func TestSessionRestartSourcesDoNotReopenADebriefedCard(t *testing.T) {
	for _, source := range []string{SourceClear, SourceCompact, SourceFork} {
		res := Reduce(missionIn(StatusDebrief), HookEvent{
			Event: EventSessionStart, SessionID: "sess-1", Source: source,
		}, testNow)

		if res.ProposedStatus != "" {
			t.Errorf("source %q proposed %q, want no lane change", source, res.ProposedStatus)
		}
	}
}

// Only a human files a card, and only a human takes it back.
func TestDoneIsTerminal(t *testing.T) {
	for _, event := range AllHookEvents {
		res := Reduce(missionIn(StatusClosed), ev(event), testNow)

		if res.Changed {
			t.Errorf("event %q changed a done mission", event)
		}

		if res.ProposedStatus != "" {
			t.Errorf("event %q proposed %q for a done mission", event, res.ProposedStatus)
		}
	}
}

// A draft mission has no session, so anything but SessionStart is a stray event from a
// previous incarnation.
func TestDraftIgnoresEverythingButSessionStart(t *testing.T) {
	for _, event := range AllHookEvents {
		res := Reduce(missionIn(StatusBriefing), ev(event), testNow)

		if event == EventSessionStart {
			if res.ProposedStatus == "" {
				t.Error("SessionStart should still be accepted in draft")
			}

			continue
		}

		if res.Changed || res.ProposedStatus != "" {
			t.Errorf("event %q was accepted by a draft mission", event)
		}
	}
}

// An abandoned session must not be able to move a live card.
func TestMismatchedSessionIsIgnored(t *testing.T) {
	res := Reduce(missionIn(StatusActive), HookEvent{
		Event: EventStop, SessionID: "stale-session",
	}, testNow)

	if res.Changed || res.ProposedStatus != "" {
		t.Errorf("a stale session moved the card to %q", res.ProposedStatus)
	}
}

// codex has no --session-id, so its mission has no recorded id until SessionStart
// arrives; events must be accepted in that window.
func TestEventsAreAcceptedBeforeTheSessionIDIsKnown(t *testing.T) {
	ms := missionIn(StatusActive)
	ms.AgentSessionID = ""
	ms.Tool = ToolCodex

	res := Reduce(ms, HookEvent{
		Event: EventSessionStart, SessionID: "codex-uuid-v7", Source: SourceStartup,
	}, testNow)

	if res.Mission.AgentSessionID != "codex-uuid-v7" {
		t.Errorf("AgentSessionID = %q, want it learned from the hook", res.Mission.AgentSessionID)
	}
}

// Whichever of the two arrives first, precedence resolves the window to waiting,
// because a card that wrongly reads as finished is worse than one that wrongly asks
// for attention.
func TestPermissionRequestOutranksStopInEitherOrder(t *testing.T) {
	permission := HookEvent{
		Event: EventPermissionRequest, SessionID: "sess-1", ToolName: "Bash",
	}
	stop := HookEvent{Event: EventStop, SessionID: "sess-1"}

	for _, order := range [][]HookEvent{
		{permission, stop},
		{stop, permission},
	} {
		ms := missionIn(StatusActive)

		var winner Status

		for _, payload := range order {
			res := Reduce(ms, payload, testNow)
			ms = res.Mission

			if res.ProposedStatus.Precedence() > winner.Precedence() {
				winner = res.ProposedStatus
			}
		}

		if winner != StatusAwaiting {
			t.Errorf("window resolved to %q, want waiting", winner)
		}
	}
}

func TestSessionStartClearsHooksSilentBadge(t *testing.T) {
	ms := missionIn(StatusActive)
	ms.Badges = ms.WithBadge(BadgeHooksSilent, "")

	res := Reduce(ms, HookEvent{
		Event: EventSessionStart, SessionID: "sess-1", Source: SourceStartup,
	}, testNow)

	if res.Mission.HasBadge(BadgeHooksSilent) {
		t.Error("SessionStart proves hooks are working, so the badge must clear")
	}
}

func TestCompactionBadgeIsPairedOnAndOff(t *testing.T) {
	ms := missionIn(StatusActive)

	res := Reduce(ms, ev(EventPreCompact), testNow)
	if !res.Mission.HasBadge(BadgeCompacting) {
		t.Error("expected a compacting badge")
	}

	res = Reduce(res.Mission, ev(EventPostCompact), testNow)
	if res.Mission.HasBadge(BadgeCompacting) {
		t.Error("compacting badge should clear")
	}
}

func TestLastMessageIsTruncatedToOneLine(t *testing.T) {
	long := strings.Repeat("x", maxLastMessage+50)

	res := Reduce(missionIn(StatusActive), HookEvent{
		Event: EventStop, SessionID: "sess-1",
		LastAssistantMessage: "first line\nsecond line",
	}, testNow)

	if res.Mission.LastMessage != "first line" {
		t.Errorf("LastMessage = %q, want just the first line", res.Mission.LastMessage)
	}

	res = Reduce(missionIn(StatusActive), HookEvent{
		Event: EventStop, SessionID: "sess-1", LastAssistantMessage: long,
	}, testNow)

	if len(res.Mission.LastMessage) > maxLastMessage+len("…") {
		t.Errorf("LastMessage length = %d, want it bounded", len(res.Mission.LastMessage))
	}
}

func TestReduceStampsEventTime(t *testing.T) {
	res := Reduce(missionIn(StatusActive), ev(EventPreToolUse), testNow)

	if !res.Mission.LastEventAt.Equal(testNow) {
		t.Errorf("LastEventAt = %v, want %v", res.Mission.LastEventAt, testNow)
	}
}

func TestUnknownEventIsIgnored(t *testing.T) {
	res := Reduce(missionIn(StatusActive), ev("SomethingNew"), testNow)

	if res.ProposedStatus != "" {
		t.Errorf("ProposedStatus = %q, want none", res.ProposedStatus)
	}
}

// A plan-mode mission's phase decides which settings its agent runs on, and a
// mission that names no planning model does not distinguish the two.
func TestMissionPlanPhase(t *testing.T) {
	cases := []struct {
		name       string
		ms         Mission
		planning   bool
		splits     bool
		wantModel  string
		wantEffort string
	}{
		{
			name:       "an ordinary mission runs on its model",
			ms:         Mission{Model: "opus", Effort: "low", PlanModel: "fable"},
			wantModel:  "opus",
			wantEffort: "low",
		},
		{
			name:       "planning with no planning model uses the implementing pair",
			ms:         Mission{PlanMode: true, Model: "opus", Effort: "low"},
			planning:   true,
			wantModel:  "opus",
			wantEffort: "low",
		},
		{
			name:       "planning takes the planning pair whole",
			ms:         Mission{PlanMode: true, Model: "opus", Effort: "low", PlanModel: "fable"},
			planning:   true,
			splits:     true,
			wantModel:  "fable",
			wantEffort: "",
		},
		{
			name:       "the same model at another effort is still a split",
			ms:         Mission{PlanMode: true, Model: "opus", Effort: "low", PlanModel: "opus", PlanEffort: "high"},
			planning:   true,
			splits:     true,
			wantModel:  "opus",
			wantEffort: "high",
		},
		{
			name:       "the same pair is no split",
			ms:         Mission{PlanMode: true, Model: "opus", Effort: "low", PlanModel: "opus", PlanEffort: "low"},
			planning:   true,
			wantModel:  "opus",
			wantEffort: "low",
		},
		{
			name: "an approved plan implements",
			ms: Mission{
				PlanMode: true, PlanApproved: true,
				Model: "opus", Effort: "low", PlanModel: "fable", PlanEffort: "high",
			},
			splits:     true,
			wantModel:  "opus",
			wantEffort: "low",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.ms.Planning(); got != tc.planning {
				t.Errorf("Planning() = %v, want %v", got, tc.planning)
			}

			if got := tc.ms.SplitsPlan(); got != tc.splits {
				t.Errorf("SplitsPlan() = %v, want %v", got, tc.splits)
			}

			model, effort := tc.ms.PhaseModel()
			if model != tc.wantModel || effort != tc.wantEffort {
				t.Errorf("PhaseModel() = %q/%q, want %q/%q", model, effort, tc.wantModel, tc.wantEffort)
			}
		})
	}
}

// codex raises no permission request around a plan: it ends its turn on one, and
// takes approval as a prompt it writes itself. These are the payloads codex
// 0.162.1 actually sends at each of those moments.
func TestReducePlanThatEndsTheTurn(t *testing.T) {
	planning := func(mut func(*Mission)) Mission {
		ms := missionIn(StatusActive)
		ms.Tool = ToolCodex
		ms.PlanMode = true

		if mut != nil {
			mut(&ms)
		}

		return ms
	}

	pending := func(ms *Mission) {
		ms.Status = StatusDebrief
		ms.PlanPending = true
	}

	cases := []struct {
		name         string
		ms           Mission
		payload      HookEvent
		want         Status
		wantPending  bool
		wantApproved bool
		wantSession  string
	}{
		{
			name:        "a planning turn that ends on no message proposed a plan",
			ms:          planning(nil),
			payload:     ev(EventStop),
			want:        StatusDebrief,
			wantPending: true,
		},
		{
			name: "a planning turn that ends on a question is waiting, not proposing",
			ms:   planning(nil),
			payload: HookEvent{
				Event: EventStop, SessionID: "sess-1",
				LastAssistantMessage: "Should the flag default to on?",
			},
			want: StatusAwaiting,
		},
		{
			name:    "an ordinary turn that ends on no message is just finished",
			ms:      planning(func(ms *Mission) { ms.PlanApproved = true }),
			payload: ev(EventStop),
			want:    StatusDebrief,
			// Approval is carried through untouched.
			wantApproved: true,
		},
		{
			name:    "an agent that asks permission is not read this way",
			ms:      planning(func(ms *Mission) { ms.Tool = ToolClaude }),
			payload: ev(EventStop),
			want:    StatusDebrief,
		},
		{
			name: "accepting in the same thread approves",
			ms:   planning(pending),
			payload: HookEvent{
				Event: EventUserPromptSubmit, SessionID: "sess-1", Prompt: "Implement the plan.",
			},
			want:         StatusActive,
			wantApproved: true,
		},
		{
			name: "accepting into a fresh thread approves",
			ms:   planning(pending),
			payload: HookEvent{
				Event: EventUserPromptSubmit, SessionID: "sess-1",
				Prompt: "A previous agent produced the plan below to accomplish the user's task. Implement the plan in a fresh context.\n\n- step",
			},
			want:         StatusActive,
			wantApproved: true,
		},
		{
			name: "anything else typed is the human still shaping the plan",
			ms:   planning(pending),
			payload: HookEvent{
				Event: EventUserPromptSubmit, SessionID: "sess-1", Prompt: "Also cover the migration.",
			},
			want: StatusActive,
		},
		{
			name: "the same words do not approve for an agent that asks permission",
			ms: planning(func(ms *Mission) {
				ms.Tool = ToolClaude
				pending(ms)
			}),
			payload: HookEvent{
				Event: EventUserPromptSubmit, SessionID: "sess-1", Prompt: "Implement the plan.",
			},
			want: StatusActive,
		},
		{
			name:        "a session that cleared the one before it is adopted",
			ms:          planning(pending),
			payload:     HookEvent{Event: EventSessionStart, SessionID: "sess-2", Source: SourceClear},
			want:        "",
			wantPending: true,
			wantSession: "sess-2",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Reduce(tc.ms, tc.payload, testNow)

			if res.ProposedStatus != tc.want {
				t.Errorf("ProposedStatus = %q, want %q", res.ProposedStatus, tc.want)
			}

			if res.Mission.PlanPending != tc.wantPending {
				t.Errorf("PlanPending = %v, want %v", res.Mission.PlanPending, tc.wantPending)
			}

			if res.Mission.PlanApproved != tc.wantApproved {
				t.Errorf("PlanApproved = %v, want %v", res.Mission.PlanApproved, tc.wantApproved)
			}

			if tc.wantSession != "" && res.Mission.AgentSessionID != tc.wantSession {
				t.Errorf("AgentSessionID = %q, want %q", res.Mission.AgentSessionID, tc.wantSession)
			}
		})
	}
}

// An event from a session q did not record is still discarded, unless it is that
// session announcing it replaced the recorded one.
func TestReduceSessionAdoption(t *testing.T) {
	cases := []struct {
		name        string
		payload     HookEvent
		wantSession string
		wantChanged bool
	}{
		{
			name:        "a stranger's stop is ignored",
			payload:     HookEvent{Event: EventStop, SessionID: "sess-2"},
			wantSession: "sess-1",
		},
		{
			name:        "a stranger's startup is ignored",
			payload:     HookEvent{Event: EventSessionStart, SessionID: "sess-2", Source: SourceStartup},
			wantSession: "sess-1",
		},
		{
			name:        "a clear is adopted",
			payload:     HookEvent{Event: EventSessionStart, SessionID: "sess-2", Source: SourceClear},
			wantSession: "sess-2",
			wantChanged: true,
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			res := Reduce(missionIn(StatusActive), tc.payload, testNow)

			if res.Mission.AgentSessionID != tc.wantSession {
				t.Errorf("AgentSessionID = %q, want %q", res.Mission.AgentSessionID, tc.wantSession)
			}

			if res.Changed != tc.wantChanged {
				t.Errorf("Changed = %v, want %v", res.Changed, tc.wantChanged)
			}
		})
	}
}

// Approval is recorded on the mission for an agent that asks permission too, so
// a session revived afterwards is not put back into plan mode.
func TestReducePlanApprovalIsRecorded(t *testing.T) {
	cases := []struct {
		name         string
		payload      HookEvent
		wantApproved bool
	}{
		{
			name:         "the plan-exit tool completing approves",
			payload:      HookEvent{Event: EventPostToolUse, SessionID: "sess-1", ToolName: ExitPlanModeTool},
			wantApproved: true,
		},
		{
			name:    "any other tool completing does not",
			payload: HookEvent{Event: EventPostToolUse, SessionID: "sess-1", ToolName: "Bash"},
		},
		{
			name:    "a rejected plan does not",
			payload: HookEvent{Event: EventPermissionDenied, SessionID: "sess-1", ToolName: ExitPlanModeTool},
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := missionIn(StatusDebrief)
			ms.PlanMode = true
			ms.PlanPending = true

			res := Reduce(ms, tc.payload, testNow)

			if res.Mission.PlanApproved != tc.wantApproved {
				t.Errorf("PlanApproved = %v, want %v", res.Mission.PlanApproved, tc.wantApproved)
			}
		})
	}
}
