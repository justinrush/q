package mission

import (
	"strings"
	"testing"
)

// codexQuestion is a real closing message from a codex mission that sat in debrief while
// its agent was parked at the prompt waiting for an answer. Note that it ends on a
// numbered list rather than on a question mark, which is why the scan has to step
// back over list items.
const codexQuestion = `The documented Ruff check found another pre-existing issue: generate_pss_patches.py:1
has a shebang but lacks its executable bit (EXE001).

The generated YAML passes formatting checks; Yamllint reports only the same inherited
long command-line warnings found in the existing Deployment.

Should I:

1. Recommended: fix the generator's executable bit in a separate commit, then finish
   validating and commit the generated PSS updates.
2. Ignore EXE001 as unrelated and finish using a targeted Ruff exclusion.`

func TestClosingQuestionRecognisesAnAsk(t *testing.T) {
	for _, tc := range []struct {
		name    string
		message string
		want    string
	}{
		{
			name:    "an ask above numbered options",
			message: codexQuestion,
			want:    "Should I:",
		},
		{
			name:    "a plain trailing question",
			message: "Tests pass on both repos.\n\nWant me to push the branch?",
			want:    "Want me to push the branch?",
		},
		{
			name:    "a bolded ask",
			message: "I can go either way here.\n\n**Which would you prefer:**\n- keep the shim\n- delete it",
			want:    "Which would you prefer:",
		},
		{
			name:    "an ask handing the decision back without a question mark",
			message: "Both options are viable.\n\nLet me know how you want to proceed.",
			want:    "Let me know how you want to proceed.",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := closingQuestion(tc.message)

			if !strings.HasPrefix(got, tc.want) {
				t.Errorf("closingQuestion = %q, want it to start with %q", got, tc.want)
			}
		})
	}
}

// The cost of over-eager detection is a finished mission parked in "awaiting orders"
// forever, so a turn that merely mentions its own reasoning or signs off politely has
// to stay in debrief.
func TestClosingQuestionIgnoresAStatement(t *testing.T) {
	for _, tc := range []struct{ name, message string }{
		{"plain completion", "Added the endpoint and its tests. All green."},
		{"a sign-off", "Done. Let me know if you want anything else."},
		{"a rhetorical question mid-message", "Why did it fail? The mock was stale. Fixed and re-run; both suites pass."},
		{"a summary list", "Changes:\n\n- added the endpoint\n- added tests"},
		{"no message at all", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := closingQuestion(tc.message); got != "" {
				t.Errorf("closingQuestion = %q, want no question", got)
			}
		})
	}
}

// The whole point of the detector: this exact message used to leave a card reading
// "ready for debrief" while codex sat waiting for an answer.
func TestStopEndingInAQuestionWaitsOnTheHuman(t *testing.T) {
	res := Reduce(missionIn(StatusActive), HookEvent{
		Event: EventStop, SessionID: "sess-1", LastAssistantMessage: codexQuestion,
	}, testNow)

	if res.ProposedStatus != StatusAwaiting {
		t.Errorf("ProposedStatus = %q, want %q", res.ProposedStatus, StatusAwaiting)
	}

	if res.Mission.AgentState != AgentWaiting {
		t.Errorf("AgentState = %q, want waiting", res.Mission.AgentState)
	}

	if !strings.Contains(res.Mission.WaitingFor, "Should I") {
		t.Errorf("WaitingFor = %q, want it to carry the ask", res.Mission.WaitingFor)
	}

	if len(res.Mission.WaitingFor) > maxWaitingFor+len("…") {
		t.Errorf("WaitingFor length = %d, want it bounded", len(res.Mission.WaitingFor))
	}

	// The card's subtitle has to be the ask, not a sentence from the middle of the
	// agent's reasoning.
	if !strings.Contains(res.Mission.LastMessage, "Should I") {
		t.Errorf("LastMessage = %q, want the ask rather than the first line", res.Mission.LastMessage)
	}

	if res.Mission.FinishedAt != nil {
		t.Error("FinishedAt should stay unset while the agent is still blocked")
	}
}

// Background work and a pending plan both outrank the question, because both describe
// the session more precisely than its closing prose does.
func TestQuestionYieldsToBackgroundWorkAndPendingPlans(t *testing.T) {
	res := Reduce(missionIn(StatusActive), HookEvent{
		Event: EventStop, SessionID: "sess-1",
		LastAssistantMessage: codexQuestion, BackgroundTasks: 1,
	}, testNow)

	if res.ProposedStatus != StatusActive {
		t.Errorf("ProposedStatus = %q, want active while background work runs", res.ProposedStatus)
	}

	planned := missionIn(StatusDebrief)
	planned.PlanPending = true

	res = Reduce(planned, HookEvent{
		Event: EventStop, SessionID: "sess-1", LastAssistantMessage: codexQuestion,
	}, testNow)

	if res.ProposedStatus != "" {
		t.Errorf("ProposedStatus = %q, want a pending plan debrief left alone", res.ProposedStatus)
	}
}

// A Stop can land microseconds after the PermissionRequest for the prompt the agent is
// actually sitting on, and prose from the turn that led up to it must not replace the
// live prompt's description.
func TestQuestionDoesNotOverwriteALivePermissionPrompt(t *testing.T) {
	blocked := missionIn(StatusAwaiting)
	blocked.WaitingFor = "Bash(rm -rf build)"

	res := Reduce(blocked, HookEvent{
		Event: EventStop, SessionID: "sess-1", LastAssistantMessage: codexQuestion,
	}, testNow)

	if res.Mission.WaitingFor != "Bash(rm -rf build)" {
		t.Errorf("WaitingFor = %q, want the live prompt kept", res.Mission.WaitingFor)
	}
}

// Answering in the pane is what clears the block, and it must work the same way for a
// question as for a permission prompt.
func TestAnsweringAQuestionInThePaneReturnsToInProgress(t *testing.T) {
	waiting := missionIn(StatusAwaiting)
	waiting.WaitingFor = "Should I: 1. fix it separately 2. ignore it"

	res := Reduce(waiting, ev(EventUserPromptSubmit), testNow)

	if res.ProposedStatus != StatusActive {
		t.Errorf("ProposedStatus = %q, want active", res.ProposedStatus)
	}

	if res.Mission.WaitingFor != "" {
		t.Errorf("WaitingFor = %q, want cleared", res.Mission.WaitingFor)
	}
}
