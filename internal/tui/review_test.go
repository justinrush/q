package tui

import (
	"strings"
	"testing"
	"time"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/justinrush/q/internal/mission"
)

func launchedMission(tool mission.Tool, status mission.Status) mission.Mission {
	ms := testMission("ms_1", "Fix login", "op_1", status)
	ms.Tool = tool

	started := time.Date(2026, 10, 12, 9, 0, 0, 0, time.UTC)
	ms.StartedAt = &started

	return ms
}

func TestReviewFormDefaults(t *testing.T) {
	models := map[mission.Tool]mission.ModelSet{
		mission.ToolCodex: {
			Default: "gpt-5.4", DefaultEffort: "medium",
			Options: []mission.ModelOption{{Value: "gpt-5.4", Efforts: []string{"medium", "high"}}},
		},
		mission.ToolClaude: {Default: "opus"},
	}

	cases := []struct {
		name       string
		source     mission.Tool
		review     ReviewDefaults
		wantTool   mission.Tool
		wantModel  string
		wantEffort string
	}{
		{
			name:   "a different agent from the one reviewed, on its own default model",
			source: mission.ToolClaude, wantTool: mission.ToolCodex, wantModel: "gpt-5.4", wantEffort: "medium",
		},
		{
			name:   "the configured reviewer, on its own default model",
			source: mission.ToolCodex, review: ReviewDefaults{Tool: mission.ToolCodex},
			wantTool: mission.ToolCodex, wantModel: "gpt-5.4", wantEffort: "medium",
		},
		{
			name:   "the configured reviewer, model, and effort",
			source: mission.ToolCodex, review: ReviewDefaults{Tool: mission.ToolClaude, Model: "fable", Effort: "max"},
			wantTool: mission.ToolClaude, wantModel: "fable", wantEffort: "max",
		},
		{
			name:   "the configured reviewer and effort, on its own default model",
			source: mission.ToolClaude, review: ReviewDefaults{Tool: mission.ToolCodex, Effort: "high"},
			wantTool: mission.ToolCodex, wantModel: "gpt-5.4", wantEffort: "high",
		},
		{
			name:   "a configured model is ignored without the agent it belongs to",
			source: mission.ToolClaude, review: ReviewDefaults{Model: "fable", Effort: "max"},
			wantTool: mission.ToolCodex, wantModel: "gpt-5.4", wantEffort: "medium",
		},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := newReviewForm(
				launchedMission(tc.source, mission.StatusDebrief),
				Options{Models: models, Review: tc.review}, nil, time.Time{},
			)

			if form.tool != tc.wantTool || form.model != tc.wantModel || form.effort != tc.wantEffort {
				t.Errorf("agent = %s/%s/%s, want %s/%s/%s",
					form.tool, form.model, form.effort, tc.wantTool, tc.wantModel, tc.wantEffort)
			}

			if form.field != fieldMissionTool {
				t.Errorf("field = %d, want the agent focused", form.field)
			}

			if got := form.name.Value(); got != "Review Fix login" {
				t.Errorf("name = %q", got)
			}
		})
	}
}

func TestReviewFormOffersOnlyReviewFields(t *testing.T) {
	form := newReviewForm(launchedMission(mission.ToolClaude, mission.StatusDebrief), Options{}, nil, time.Time{})

	var visited []int

	for range len(reviewFields) {
		visited = append(visited, form.field)
		form.Update(tea.KeyMsg{Type: tea.KeyTab})
	}

	want := []int{fieldMissionTool, fieldMissionModel, fieldMissionEffort, fieldMissionPrompt, fieldMissionName}
	for i := range want {
		if visited[i] != want[i] {
			t.Fatalf("tab order = %v, want %v", visited, want)
		}
	}

	if form.field != fieldMissionTool {
		t.Errorf("tabbing past the last field landed on %d, want it to wrap", form.field)
	}

	view := form.View(120, 40)
	for _, absent := range []string{"Operation", "Plan mode", "Additional repos", "ctrl+g"} {
		if strings.Contains(view, absent) {
			t.Errorf("review form shows %q", absent)
		}
	}

	if next, _ := form.Update(ctrlG()); next != modal(form) {
		t.Error("ctrl+g opened the branch picker on a review form")
	}
}

func TestReviewFormSubmit(t *testing.T) {
	cases := []struct {
		name      string
		key       tea.KeyMsg
		focus     string
		rename    string
		wantName  string
		wantFocus string
		wantSent  bool
	}{
		{name: "with no focus", key: tea.KeyMsg{Type: tea.KeyCtrlS}, wantName: "Review Fix login", wantSent: true},
		{
			name: "with a focus and a new name", key: tea.KeyMsg{Type: tea.KeyCtrlR},
			focus: "Check the migrations.", rename: "Second opinion",
			wantName: "Second opinion", wantFocus: "Check the migrations.", wantSent: true,
		},
		{name: "refuses an empty name", key: tea.KeyMsg{Type: tea.KeyCtrlS}, rename: " "},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			form := newReviewForm(launchedMission(mission.ToolClaude, mission.StatusDebrief), Options{}, nil, time.Time{})
			form.prompt.SetValue(tc.focus)

			if tc.rename != "" {
				form.name.SetValue(tc.rename)
			}

			_, cmd := form.Update(tc.key)
			if !tc.wantSent {
				if cmd != nil {
					t.Fatal("the form submitted")
				}

				return
			}

			if cmd == nil {
				t.Fatal("the form did not submit")
			}

			msg, ok := cmd().(submitMissionMsg)
			if !ok {
				t.Fatalf("got %T, want submitMissionMsg", cmd())
			}

			if msg.ReviewOf != "ms_1" || msg.Name != tc.wantName || msg.Prompt != tc.wantFocus || msg.Tool != mission.ToolCodex {
				t.Errorf("submitted %+v", msg)
			}
		})
	}
}

func TestBoardReviewKey(t *testing.T) {
	reviewOfAnother := launchedMission(mission.ToolCodex, mission.StatusDebrief)
	reviewOfAnother.ReviewOf = "ms_9"

	cases := []struct {
		name      string
		ms        mission.Mission
		wantForm  bool
		wantToast string
	}{
		{name: "a finished mission opens the form", ms: launchedMission(mission.ToolClaude, mission.StatusDebrief), wantForm: true},
		{name: "a running mission opens the form", ms: launchedMission(mission.ToolClaude, mission.StatusActive), wantForm: true},
		{
			name: "a brief has nothing to review", ms: testMission("ms_1", "Fix login", "op_1", mission.StatusBriefing),
			wantToast: "has not been launched",
		},
		{name: "a closed mission has no worktrees", ms: launchedMission(mission.ToolClaude, mission.StatusClosed), wantToast: "is closed"},
		{name: "a review is not reviewed", ms: reviewOfAnother, wantToast: "is itself a review"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			app := testApp(t)

			cmd := app.showReviewForm(tc.ms)
			form, isForm := app.modal.(*missionForm)

			if tc.wantForm {
				if !isForm || form.reviewOf != tc.ms.ID {
					t.Fatalf("modal = %T, want a review form for %s", app.modal, tc.ms.ID)
				}

				return
			}

			if isForm {
				t.Fatal("a review form was opened")
			}

			toast, ok := cmd().(toastMsg)
			if !ok || !toast.err || !strings.Contains(toast.text, tc.wantToast) {
				t.Errorf("got %+v, want an error mentioning %q", cmd(), tc.wantToast)
			}
		})
	}
}

func TestBoardReviewKeyEmitsIntent(t *testing.T) {
	action, ok := boardActions["r"]
	if !ok {
		t.Fatal("r is not bound on the board")
	}

	ms := launchedMission(mission.ToolClaude, mission.StatusDebrief)

	board := NewBoard()
	board.SetSnapshot(mission.Snapshot{
		Operations: []mission.Operation{testOperation("op_1", "T", 0)},
		Missions:   []mission.Mission{ms},
	})
	board.lane = 3

	cmd := action(board)
	if cmd == nil {
		t.Fatal("r did nothing with a mission selected")
	}

	if msg, ok := cmd().(reviewMissionMsg); !ok || msg.Mission.ID != ms.ID {
		t.Errorf("got %#v, want a review intent for %s", cmd(), ms.ID)
	}
}

func TestCardMarksAReview(t *testing.T) {
	cases := []struct {
		name     string
		reviewOf mission.MissionID
		want     bool
	}{
		{name: "review", reviewOf: "ms_9", want: true},
		{name: "ordinary mission"},
	}

	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			ms := launchedMission(mission.ToolCodex, mission.StatusActive)
			ms.ReviewOf = tc.reviewOf

			if got := strings.Contains(metaLine(ms, "", 80), "review"); got != tc.want {
				t.Errorf("meta line marks a review = %v, want %v", got, tc.want)
			}
		})
	}
}
