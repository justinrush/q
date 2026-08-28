package statem

import (
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/domain"
	"github.com/justinrush/q/internal/hookspec"
)

var testNow = time.Date(2026, 8, 17, 12, 0, 0, 0, time.UTC)

// missionIn returns a launched mission sitting in the given lane.
func missionIn(status domain.Status) domain.Mission {
	return domain.Mission{
		ID:             "ms_aabbccddeeff",
		Tool:           domain.ToolClaude,
		Status:         status,
		AgentState:     domain.AgentBusy,
		AgentSessionID: "sess-1",
		HookEpoch:      1,
	}
}

// ev builds a payload for an event.
func ev(event string) hookspec.Payload {
	return hookspec.Payload{Event: event, SessionID: "sess-1"}
}

// The full event-to-lane table. Each row states the lane an event argues for from a
// given starting lane, with the empty status meaning "argues for none".
func TestReduceTransitionTable(t *testing.T) {
	for _, tc := range []struct {
		name     string
		from     domain.Status
		payload  hookspec.Payload
		want     domain.Status
		wantMore func(*testing.T, domain.Mission)
	}{
		{
			name:    "session start confirms the agent came up",
			from:    domain.StatusBriefing,
			payload: hookspec.Payload{Event: hookspec.EventSessionStart, SessionID: "sess-1", Source: hookspec.SourceStartup},
			want:    domain.StatusActive,
		},
		{
			name:    "session start on resume also reports active",
			from:    domain.StatusDebrief,
			payload: hookspec.Payload{Event: hookspec.EventSessionStart, SessionID: "sess-1", Source: hookspec.SourceResume},
			want:    domain.StatusActive,
		},
		{
			name:    "user prompt clears a block",
			from:    domain.StatusAwaiting,
			payload: ev(hookspec.EventUserPromptSubmit),
			want:    domain.StatusActive,
			wantMore: func(t *testing.T, mission domain.Mission) {
				if mission.WaitingFor != "" {
					t.Errorf("WaitingFor = %q, want cleared", mission.WaitingFor)
				}
			},
		},
		{
			name:    "pre tool use argues for no lane",
			from:    domain.StatusActive,
			payload: ev(hookspec.EventPreToolUse),
			want:    "",
		},
		{
			name:    "permission request blocks on the human",
			from:    domain.StatusActive,
			payload: hookspec.Payload{Event: hookspec.EventPermissionRequest, SessionID: "sess-1", ToolName: "Bash"},
			want:    domain.StatusAwaiting,
			wantMore: func(t *testing.T, mission domain.Mission) {
				if mission.WaitingFor != "Bash" {
					t.Errorf("WaitingFor = %q, want %q", mission.WaitingFor, "Bash")
				}

				if mission.AgentState != domain.AgentWaiting {
					t.Errorf("AgentState = %q", mission.AgentState)
				}
			},
		},
		{
			name: "exit plan mode request goes to debrief, not waiting",
			from: domain.StatusActive,
			payload: hookspec.Payload{
				Event: hookspec.EventPermissionRequest, SessionID: "sess-1",
				ToolName: hookspec.ExitPlanModeTool,
			},
			want: domain.StatusDebrief,
			wantMore: func(t *testing.T, mission domain.Mission) {
				if !mission.PlanPending {
					t.Error("PlanPending should be set")
				}
			},
		},
		{
			name: "exit plan mode completing returns to active",
			from: domain.StatusDebrief,
			payload: hookspec.Payload{
				Event: hookspec.EventPostToolUse, SessionID: "sess-1",
				ToolName: hookspec.ExitPlanModeTool,
			},
			want: domain.StatusActive,
			wantMore: func(t *testing.T, mission domain.Mission) {
				if mission.PlanPending {
					t.Error("PlanPending should be cleared")
				}
			},
		},
		{
			name:    "stop ends the turn",
			from:    domain.StatusActive,
			payload: hookspec.Payload{Event: hookspec.EventStop, SessionID: "sess-1", LastAssistantMessage: "all done"},
			want:    domain.StatusDebrief,
			wantMore: func(t *testing.T, mission domain.Mission) {
				if mission.LastMessage != "all done" {
					t.Errorf("LastMessage = %q", mission.LastMessage)
				}

				if mission.AgentState != domain.AgentIdle {
					t.Errorf("AgentState = %q, want idle", mission.AgentState)
				}

				if mission.FinishedAt == nil {
					t.Error("FinishedAt should be set")
				}
			},
		},
		{
			name:    "api failure needs attention",
			from:    domain.StatusActive,
			payload: hookspec.Payload{Event: hookspec.EventStopFailure, SessionID: "sess-1", Reason: "rate_limit"},
			want:    domain.StatusAwaiting,
			wantMore: func(t *testing.T, mission domain.Mission) {
				if !mission.HasBadge(domain.BadgeAPIError) {
					t.Error("expected an api badge")
				}

				if !strings.Contains(mission.WaitingFor, "rate_limit") {
					t.Errorf("WaitingFor = %q, want it to name the reason", mission.WaitingFor)
				}
			},
		},
		{
			name:    "session end reports debrief and a dead agent",
			from:    domain.StatusActive,
			payload: hookspec.Payload{Event: hookspec.EventSessionEnd, SessionID: "sess-1", Reason: "other"},
			want:    domain.StatusDebrief,
			wantMore: func(t *testing.T, mission domain.Mission) {
				if mission.AgentState != domain.AgentDead {
					t.Errorf("AgentState = %q, want dead", mission.AgentState)
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
	mission := missionIn(domain.StatusAwaiting)
	mission.WaitingFor = "Bash"

	res := Reduce(mission, hookspec.Payload{
		Event: hookspec.EventPostToolUse, SessionID: "sess-1", ToolName: "Bash",
	}, testNow)

	if res.ProposedStatus != domain.StatusActive {
		t.Errorf("ProposedStatus = %q, want active", res.ProposedStatus)
	}

	if res.Mission.WaitingFor != "" {
		t.Errorf("WaitingFor = %q, want cleared", res.Mission.WaitingFor)
	}
}

// A completed tool call on a card that was never blocked must not reshuffle it.
func TestPostToolUseDoesNotDisturbAnInProgressCard(t *testing.T) {
	res := Reduce(missionIn(domain.StatusActive), hookspec.Payload{
		Event: hookspec.EventPostToolUse, SessionID: "sess-1", ToolName: "Read",
	}, testNow)

	if res.ProposedStatus != "" {
		t.Errorf("ProposedStatus = %q, want no proposal", res.ProposedStatus)
	}
}

// Ordinary denials come from the user's own guard hooks, which the agent handles
// itself. Treating them as blocked would park cards in waiting for no reason.
func TestOrdinaryPermissionDenialDoesNotBlock(t *testing.T) {
	res := Reduce(missionIn(domain.StatusActive), hookspec.Payload{
		Event: hookspec.EventPermissionDenied, SessionID: "sess-1", ToolName: "Bash",
	}, testNow)

	if res.ProposedStatus != "" {
		t.Errorf("ProposedStatus = %q, want no proposal", res.ProposedStatus)
	}
}

// A rejected plan does need a human.
func TestRejectedPlanNeedsDirection(t *testing.T) {
	mission := missionIn(domain.StatusDebrief)
	mission.PlanPending = true

	res := Reduce(mission, hookspec.Payload{
		Event: hookspec.EventPermissionDenied, SessionID: "sess-1", ToolName: hookspec.ExitPlanModeTool,
	}, testNow)

	if res.ProposedStatus != domain.StatusAwaiting {
		t.Errorf("ProposedStatus = %q, want waiting", res.ProposedStatus)
	}

	if res.Mission.PlanPending {
		t.Error("PlanPending should be cleared")
	}
}

// A pending plan debrief outranks a Stop; downgrading it would strip the card of its
// "needs your approval" meaning.
func TestStopDoesNotDowngradeAPendingPlan(t *testing.T) {
	mission := missionIn(domain.StatusDebrief)
	mission.PlanPending = true

	res := Reduce(mission, ev(hookspec.EventStop), testNow)

	if res.ProposedStatus != "" {
		t.Errorf("ProposedStatus = %q, want no proposal", res.ProposedStatus)
	}

	if res.Mission.FinishedAt != nil {
		t.Error("a pending plan is not finished")
	}
}

// In-flight background work means paused, not finished.
func TestStopWithBackgroundWorkStaysInProgress(t *testing.T) {
	res := Reduce(missionIn(domain.StatusActive), hookspec.Payload{
		Event: hookspec.EventStop, SessionID: "sess-1", BackgroundTasks: 2,
	}, testNow)

	if res.ProposedStatus != domain.StatusActive {
		t.Errorf("ProposedStatus = %q, want active", res.ProposedStatus)
	}

	if !res.Mission.HasBadge(domain.BadgeBackground) {
		t.Error("expected a background badge")
	}

	if res.Mission.FinishedAt != nil {
		t.Error("a paused mission is not finished")
	}
}

// Sixty seconds of quiet is neither blocked nor finished.
func TestIdleNotificationOnlyMarksStale(t *testing.T) {
	res := Reduce(missionIn(domain.StatusActive), hookspec.Payload{
		Event: hookspec.EventNotification, SessionID: "sess-1",
		NotificationType: hookspec.NotificationIdlePrompt,
	}, testNow)

	if res.ProposedStatus != "" {
		t.Errorf("ProposedStatus = %q, want no lane change for idle", res.ProposedStatus)
	}

	if !res.Mission.HasBadge(domain.BadgeStale) {
		t.Error("expected a stale badge")
	}
}

// The permission notification arrives six seconds after PermissionRequest already
// reported the same thing, so it may only promote a card that still looks busy.
func TestPermissionNotificationOnlyPromotesABusyCard(t *testing.T) {
	busy := Reduce(missionIn(domain.StatusActive), hookspec.Payload{
		Event: hookspec.EventNotification, SessionID: "sess-1",
		NotificationType: hookspec.NotificationPermissionPrompt,
		Message:          "Claude needs your permission to use Bash",
	}, testNow)

	if busy.ProposedStatus != domain.StatusAwaiting {
		t.Errorf("ProposedStatus = %q, want waiting", busy.ProposedStatus)
	}

	debriefed := Reduce(missionIn(domain.StatusDebrief), hookspec.Payload{
		Event: hookspec.EventNotification, SessionID: "sess-1",
		NotificationType: hookspec.NotificationPermissionPrompt,
	}, testNow)

	if debriefed.ProposedStatus != "" {
		t.Errorf("ProposedStatus = %q, want no proposal from a debriefed card", debriefed.ProposedStatus)
	}
}

// A clear, compact, or fork continues the same work and must not drag a card out of
// debrief.
func TestSessionRestartSourcesDoNotReopenADebriefedCard(t *testing.T) {
	for _, source := range []string{hookspec.SourceClear, hookspec.SourceCompact, hookspec.SourceFork} {
		res := Reduce(missionIn(domain.StatusDebrief), hookspec.Payload{
			Event: hookspec.EventSessionStart, SessionID: "sess-1", Source: source,
		}, testNow)

		if res.ProposedStatus != "" {
			t.Errorf("source %q proposed %q, want no lane change", source, res.ProposedStatus)
		}
	}
}

// Only a human files a card, and only a human takes it back.
func TestDoneIsTerminal(t *testing.T) {
	for _, event := range hookspec.AllEvents {
		res := Reduce(missionIn(domain.StatusClosed), ev(event), testNow)

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
	for _, event := range hookspec.AllEvents {
		res := Reduce(missionIn(domain.StatusBriefing), ev(event), testNow)

		if event == hookspec.EventSessionStart {
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
	res := Reduce(missionIn(domain.StatusActive), hookspec.Payload{
		Event: hookspec.EventStop, SessionID: "stale-session",
	}, testNow)

	if res.Changed || res.ProposedStatus != "" {
		t.Errorf("a stale session moved the card to %q", res.ProposedStatus)
	}
}

// codex has no --session-id, so its mission has no recorded id until SessionStart
// arrives; events must be accepted in that window.
func TestEventsAreAcceptedBeforeTheSessionIDIsKnown(t *testing.T) {
	mission := missionIn(domain.StatusActive)
	mission.AgentSessionID = ""
	mission.Tool = domain.ToolCodex

	res := Reduce(mission, hookspec.Payload{
		Event: hookspec.EventSessionStart, SessionID: "codex-uuid-v7", Source: hookspec.SourceStartup,
	}, testNow)

	if res.Mission.AgentSessionID != "codex-uuid-v7" {
		t.Errorf("AgentSessionID = %q, want it learned from the hook", res.Mission.AgentSessionID)
	}
}

// Whichever of the two arrives first, precedence resolves the window to waiting,
// because a card that wrongly reads as finished is worse than one that wrongly asks
// for attention.
func TestPermissionRequestOutranksStopInEitherOrder(t *testing.T) {
	permission := hookspec.Payload{
		Event: hookspec.EventPermissionRequest, SessionID: "sess-1", ToolName: "Bash",
	}
	stop := hookspec.Payload{Event: hookspec.EventStop, SessionID: "sess-1"}

	for _, order := range [][]hookspec.Payload{
		{permission, stop},
		{stop, permission},
	} {
		mission := missionIn(domain.StatusActive)

		var winner domain.Status

		for _, payload := range order {
			res := Reduce(mission, payload, testNow)
			mission = res.Mission

			if res.ProposedStatus.Precedence() > winner.Precedence() {
				winner = res.ProposedStatus
			}
		}

		if winner != domain.StatusAwaiting {
			t.Errorf("window resolved to %q, want waiting", winner)
		}
	}
}

func TestSessionStartClearsHooksSilentBadge(t *testing.T) {
	mission := missionIn(domain.StatusActive)
	mission.Badges = mission.WithBadge(domain.BadgeHooksSilent, "")

	res := Reduce(mission, hookspec.Payload{
		Event: hookspec.EventSessionStart, SessionID: "sess-1", Source: hookspec.SourceStartup,
	}, testNow)

	if res.Mission.HasBadge(domain.BadgeHooksSilent) {
		t.Error("SessionStart proves hooks are working, so the badge must clear")
	}
}

func TestCompactionBadgeIsPairedOnAndOff(t *testing.T) {
	mission := missionIn(domain.StatusActive)

	res := Reduce(mission, ev(hookspec.EventPreCompact), testNow)
	if !res.Mission.HasBadge(domain.BadgeCompacting) {
		t.Error("expected a compacting badge")
	}

	res = Reduce(res.Mission, ev(hookspec.EventPostCompact), testNow)
	if res.Mission.HasBadge(domain.BadgeCompacting) {
		t.Error("compacting badge should clear")
	}
}

func TestLastMessageIsTruncatedToOneLine(t *testing.T) {
	long := strings.Repeat("x", maxLastMessage+50)

	res := Reduce(missionIn(domain.StatusActive), hookspec.Payload{
		Event: hookspec.EventStop, SessionID: "sess-1",
		LastAssistantMessage: "first line\nsecond line",
	}, testNow)

	if res.Mission.LastMessage != "first line" {
		t.Errorf("LastMessage = %q, want just the first line", res.Mission.LastMessage)
	}

	res = Reduce(missionIn(domain.StatusActive), hookspec.Payload{
		Event: hookspec.EventStop, SessionID: "sess-1", LastAssistantMessage: long,
	}, testNow)

	if len(res.Mission.LastMessage) > maxLastMessage+len("…") {
		t.Errorf("LastMessage length = %d, want it bounded", len(res.Mission.LastMessage))
	}
}

func TestReduceStampsEventTime(t *testing.T) {
	res := Reduce(missionIn(domain.StatusActive), ev(hookspec.EventPreToolUse), testNow)

	if !res.Mission.LastEventAt.Equal(testNow) {
		t.Errorf("LastEventAt = %v, want %v", res.Mission.LastEventAt, testNow)
	}
}

func TestUnknownEventIsIgnored(t *testing.T) {
	res := Reduce(missionIn(domain.StatusActive), ev("SomethingNew"), testNow)

	if res.ProposedStatus != "" {
		t.Errorf("ProposedStatus = %q, want none", res.ProposedStatus)
	}
}
