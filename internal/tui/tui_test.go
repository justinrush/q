package tui

import (
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/charmbracelet/bubbles/key"
	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/git"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/tui/keys"
	"github.com/muesli/termenv"
)

// Render tests compare plain text, so color is switched off globally.
//
// lipgloss keeps its color profile in a package-level variable, which is why none of
// these tests may run in parallel.
func init() { lipgloss.SetColorProfile(termenv.Ascii) }

// testOperation returns an operation with a known palette slot.
func testOperation(id, name string, colorIdx int) mission.Operation {
	return mission.Operation{ID: mission.OperationID(id), Name: name, Slug: mission.Slug(name), ColorIdx: colorIdx}
}

// testMission returns a mission in a lane.
func testMission(id, name string, operationID mission.OperationID, status mission.Status) mission.Mission {
	return mission.Mission{
		ID:          mission.MissionID(id),
		OperationID: operationID,
		Name:        name,
		Slug:        mission.Slug(name),
		Tool:        mission.ToolClaude,
		Status:      status,
		AgentState:  mission.AgentUnknown,
		Prompt:      "do the thing",
	}
}

// The stripe is how a glance at the board tells you which investigation a card belongs
// to, so it has to carry the operation's name and span the card's full width.
func TestRenderCardShowsTheOperationStripe(t *testing.T) {
	ms := testMission("ms_1", "add endpoint", "op_1", mission.StatusDebrief)
	operation := testOperation("op_1", "Discussions API", 0)

	const width = 40

	out := renderCard(ms, operation, width, false)
	lines := strings.Split(out, "\n")

	stripe := lines[len(lines)-1]
	if !strings.Contains(stripe, "Discussions API") {
		t.Errorf("last line should be the operation stripe, got %q", stripe)
	}

	for i, line := range lines {
		if got := lipgloss.Width(line); got != width {
			t.Errorf("line %d width = %d, want %d: %q", i, got, width, line)
		}
	}
}

func TestRenderCardIncludesToolAndPlanMode(t *testing.T) {
	ms := testMission("ms_1", "add endpoint", "op_1", mission.StatusBriefing)
	ms.PlanMode = true

	out := renderCard(ms, testOperation("op_1", "T", 0), 44, false)

	for _, want := range []string{"add endpoint", "claude", "plan"} {
		if !strings.Contains(out, want) {
			t.Errorf("card should mention %q:\n%s", want, out)
		}
	}
}

// A blocked agent and a failed launch are the two things needing a human, so they take
// the detail line ahead of the agent's closing message.
func TestRenderCardPrioritisesWhatNeedsAttention(t *testing.T) {
	base := testMission("ms_1", "mission", "op_1", mission.StatusAwaiting)
	base.LastMessage = "finished the refactor"

	waiting := base
	waiting.WaitingFor = "Bash(rm -rf build)"

	out := renderCard(waiting, testOperation("op_1", "T", 0), 46, false)
	if !strings.Contains(out, "rm -rf build") {
		t.Errorf("a blocked agent should be shown:\n%s", out)
	}

	if strings.Contains(out, "finished the refactor") {
		t.Errorf("the closing message should yield to the block:\n%s", out)
	}

	failed := base
	failed.LaunchError = "fetching origin/main failed"

	out = renderCard(failed, testOperation("op_1", "T", 0), 46, false)
	if !strings.Contains(out, "launch failed") {
		t.Errorf("a failed launch should be shown:\n%s", out)
	}
}

func TestRenderCardShowsBadges(t *testing.T) {
	ms := testMission("ms_1", "mission", "op_1", mission.StatusActive)
	ms.Badges = []mission.Badge{{Kind: mission.BadgeAPIError, Detail: "rate_limit"}}

	out := renderCard(ms, testOperation("op_1", "T", 0), 50, false)
	if !strings.Contains(out, "api:rate_limit") {
		t.Errorf("badge missing:\n%s", out)
	}
}

// A card must stay inside its column however long the content is, or it would bleed
// into the neighbouring lane.
func TestRenderCardNeverExceedsItsWidth(t *testing.T) {
	ms := testMission("ms_1", strings.Repeat("very-long-name ", 12), "op_1", mission.StatusDebrief)
	ms.WaitingFor = strings.Repeat("blocked ", 20)

	for _, width := range []int{20, 24, 32, 48, 80} {
		out := renderCard(ms, testOperation("op_1", strings.Repeat("Operation ", 10), 3), width, false)

		for i, line := range strings.Split(out, "\n") {
			if got := lipgloss.Width(line); got > width {
				t.Errorf("width %d: line %d is %d wide: %q", width, i, got, line)
			}
		}
	}
}

func TestRenderCardHonoursMinimumWidth(t *testing.T) {
	out := renderCard(testMission("ms_1", "t", "op_1", mission.StatusBriefing), testOperation("op_1", "T", 0), 4, false)

	for _, line := range strings.Split(out, "\n") {
		if got := lipgloss.Width(line); got != MinCardWidth {
			t.Errorf("line width = %d, want the minimum %d: %q", got, MinCardWidth, line)
		}
	}
}

// Below the threshold, five columns would each be an unreadable sliver, so the board
// shows one lane properly instead.
func TestLayoutSwitchesToFocusModeWhenNarrow(t *testing.T) {
	narrow := computeLayout(focusModeBelow-1, 40, false, 2)
	if !narrow.Focus {
		t.Error("a narrow terminal should use focus mode")
	}

	if narrow.Widths[2] == 0 {
		t.Error("the focused lane should get the width")
	}

	wide := computeLayout(200, 40, false, 0)
	if wide.Focus {
		t.Error("a wide terminal should show all lanes")
	}
}

// Collapsing done frees its share for the lanes describing work still in flight.
func TestLayoutCollapsesDoneByDefault(t *testing.T) {
	collapsed := computeLayout(200, 40, false, 0)

	doneIdx := len(mission.Lanes) - 1
	if collapsed.Widths[doneIdx] != collapsedDoneWidth {
		t.Errorf("done width = %d, want %d", collapsed.Widths[doneIdx], collapsedDoneWidth)
	}

	expanded := computeLayout(200, 40, true, 0)
	if expanded.Widths[doneIdx] <= collapsedDoneWidth {
		t.Errorf("expanded done width = %d, want more than %d",
			expanded.Widths[doneIdx], collapsedDoneWidth)
	}

	// Expanding done must take room from the other lanes, not from nowhere.
	if expanded.Widths[0] >= collapsed.Widths[0] {
		t.Errorf("other lanes should shrink when done expands: %d then %d",
			collapsed.Widths[0], expanded.Widths[0])
	}
}

// The lanes must fill the terminal without overflowing it, or the rightmost column
// wraps and the grid breaks.
func TestLayoutFitsTheTerminal(t *testing.T) {
	for _, width := range []int{110, 137, 160, 200, 240} {
		for _, expanded := range []bool{false, true} {
			layout := computeLayout(width, 40, expanded, 0)

			total := (len(mission.Lanes) - 1) * laneGap
			for _, w := range layout.Widths {
				total += w
			}

			if total > width {
				t.Errorf("width %d expanded=%v: lanes total %d", width, expanded, total)
			}
		}
	}
}

func TestLayoutAlwaysShowsAtLeastOneCard(t *testing.T) {
	for _, height := range []int{1, 4, 6, 10, 40} {
		if got := computeLayout(200, height, false, 0).VisibleCards; got < 1 {
			t.Errorf("height %d: VisibleCards = %d, want at least 1", height, got)
		}
	}
}

// boardWith returns a board holding the given missions.
func boardWith(operations []mission.Operation, missions []mission.Mission) *Board {
	board := NewBoard()
	board.SetSize(200, 40)
	board.SetSnapshot(mission.Snapshot{Operations: operations, Missions: missions})

	return board
}

// keyMsg builds the key message bubbletea would deliver for a printable key.
func keyMsg(s string) tea.KeyMsg {
	return tea.KeyMsg{Type: tea.KeyRunes, Runes: []rune(s)}
}

// The board must not act on a card when its lane is empty.
func TestBoardActionsAreNoopsOnAnEmptyLane(t *testing.T) {
	board := boardWith(nil, nil)

	for _, action := range []func(*Board) tea.Cmd{
		(*Board).openDebrief, (*Board).editMission, (*Board).deleteMission,
		(*Board).togglePlan, (*Board).messageAgent, (*Board).moveCardRight,
	} {
		if cmd := action(board); cmd != nil {
			if msg := cmd(); msg != nil {
				t.Errorf("an empty lane produced %T", msg)
			}
		}
	}
}

// A mission in briefing has no session to debrief, so enter opens its editor instead of failing.
func TestOpenDebriefOnADraftOpensTheEditor(t *testing.T) {
	operations := []mission.Operation{testOperation("op_1", "T", 0)}
	board := boardWith(operations, []mission.Mission{testMission("ms_1", "draft mission", "op_1", mission.StatusBriefing)})

	cmd := board.openDebrief()
	if cmd == nil {
		t.Fatal("expected a command")
	}

	if _, ok := cmd().(editMissionMsg); !ok {
		t.Errorf("got %T, want editMissionMsg", cmd())
	}
}

func TestOpenDebriefOnALaunchedMissionOpensTheDebrief(t *testing.T) {
	started := time.Now()

	ms := testMission("ms_1", "running", "op_1", mission.StatusDebrief)
	ms.StartedAt = &started

	board := boardWith([]mission.Operation{testOperation("op_1", "T", 0)}, []mission.Mission{ms})
	board.lane = 3

	cmd := board.openDebrief()
	if cmd == nil {
		t.Fatal("expected a command")
	}

	if _, ok := cmd().(openDebriefMsg); !ok {
		t.Errorf("got %T, want openDebriefMsg", cmd())
	}
}

// Moving a launched card back into progress resumes an agent, which needs a message
// first, so it asks rather than acting.
func TestMovingALaunchedCardIntoProgressAsksFirst(t *testing.T) {
	started := time.Now()

	// Lanes run briefing, active, awaiting, debrief, closed, so the card has to start
	// in awaiting for one step left to land on active.
	ms := testMission("ms_1", "running", "op_1", mission.StatusAwaiting)
	ms.StartedAt = &started

	board := boardWith([]mission.Operation{testOperation("op_1", "T", 0)}, []mission.Mission{ms})
	board.lane = 2

	cmd := board.moveCardLeft()
	if cmd == nil {
		t.Fatal("expected a command")
	}

	if _, ok := cmd().(resumePromptMsg); !ok {
		t.Errorf("got %T, want resumePromptMsg", cmd())
	}
}

// A draft moving into progress launches, which needs no dialog.
func TestMovingADraftIntoProgressMovesDirectly(t *testing.T) {
	board := boardWith([]mission.Operation{testOperation("op_1", "T", 0)},
		[]mission.Mission{testMission("ms_1", "draft", "op_1", mission.StatusBriefing)})

	cmd := board.moveCardRight()
	if cmd == nil {
		t.Fatal("expected a command")
	}

	move, ok := cmd().(moveMissionMsg)
	if !ok {
		t.Fatalf("got %T, want moveMissionMsg", cmd())
	}

	if move.To != mission.StatusActive {
		t.Errorf("To = %q, want active", move.To)
	}
}

// Moving a card should not silently no-op at the ends of the board.
func TestMovingAtTheEdgeDoesNothing(t *testing.T) {
	board := boardWith([]mission.Operation{testOperation("op_1", "T", 0)},
		[]mission.Mission{testMission("ms_1", "draft", "op_1", mission.StatusBriefing)})

	if cmd := board.moveCardLeft(); cmd != nil {
		if msg := cmd(); msg != nil {
			t.Errorf("moving left from the first lane produced %T", msg)
		}
	}
}

func TestBoardFilterLimitsVisibleMissions(t *testing.T) {
	operations := []mission.Operation{testOperation("op_1", "One", 0), testOperation("op_2", "Two", 1)}
	missions := []mission.Mission{
		testMission("ms_1", "a", "op_1", mission.StatusBriefing),
		testMission("ms_2", "b", "op_2", mission.StatusBriefing),
	}

	board := boardWith(operations, missions)

	if got := len(board.missionsIn(0)); got != 2 {
		t.Fatalf("unfiltered lane has %d missions, want 2", got)
	}

	board.SetFilter("op_2")

	visible := board.missionsIn(0)
	if len(visible) != 1 || visible[0].ID != "ms_2" {
		t.Errorf("filtered lane = %+v", visible)
	}

	if label := board.FilterLabel(); !strings.Contains(label, "Two") {
		t.Errorf("FilterLabel = %q, want it to name the operation", label)
	}

	board.clearFilter()

	if got := len(board.missionsIn(0)); got != 2 {
		t.Errorf("clearing the filter left %d missions", got)
	}
}

// The selection must survive the data changing underneath it, which happens on every
// event from the daemon.
func TestSelectionSurvivesMissionsDisappearing(t *testing.T) {
	operations := []mission.Operation{testOperation("op_1", "T", 0)}
	missions := []mission.Mission{
		testMission("ms_1", "a", "op_1", mission.StatusBriefing),
		testMission("ms_2", "b", "op_1", mission.StatusBriefing),
		testMission("ms_3", "c", "op_1", mission.StatusBriefing),
	}

	board := boardWith(operations, missions)
	board.selectLast()

	if _, ok := board.Selected(); !ok {
		t.Fatal("expected a selection")
	}

	// The daemon reports that everything is gone.
	board.SetSnapshot(mission.Snapshot{Operations: operations})

	if _, ok := board.Selected(); ok {
		t.Error("nothing should be selected in an empty board")
	}

	board.SetSnapshot(mission.Snapshot{Operations: operations, Missions: missions[:1]})

	selected, ok := board.Selected()
	if !ok || selected.ID != "ms_1" {
		t.Errorf("selection did not recover: %+v %v", selected, ok)
	}
}

// Every key the board declares must reach a handler.
//
// This walks the keymap struct rather than the help groups. Checking only what appears in
// help is what previously let a declared binding ship with no implementation: it did
// nothing, and nothing failed.
func TestEveryBoardBindingHasAHandler(t *testing.T) {
	board := keys.NewBoard()

	declared := []struct {
		name    string
		binding key.Binding
	}{
		{"Left", board.Left},
		{"Right", board.Right},
		{"Up", board.Up},
		{"Down", board.Down},
		{"MoveLeft", board.MoveLeft},
		{"MoveRight", board.MoveRight},
		{"ReorderUp", board.ReorderUp},
		{"ReorderDn", board.ReorderDn},
		{"Lane", board.Lane},
		{"First", board.First},
		{"Last", board.Last},
		{"Open", board.Open},
		{"Message", board.Message},
		{"New", board.New},
		{"Edit", board.Edit},
		{"Delete", board.Delete},
		{"TogglePlan", board.TogglePlan},
		{"ToggleDone", board.ToggleDone},
		{"Status", board.Status},
		{"Filter", board.Filter},
		{"Clear", board.Clear},
	}

	for _, item := range declared {
		for _, k := range item.binding.Keys() {
			// The digit keys are handled by a range check rather than the table.
			if len(k) == 1 && k[0] >= '1' && k[0] <= '5' {
				continue
			}

			if _, ok := boardActions[k]; !ok {
				t.Errorf("Board.%s binds %q but no handler answers it", item.name, k)
			}
		}
	}
}

// Every handler must be reachable from a declared binding, or it is dead code that looks
// like a feature.
func TestEveryBoardHandlerIsBound(t *testing.T) {
	bound := map[string]bool{}

	for _, binding := range keys.All() {
		for _, k := range binding.Keys() {
			bound[k] = true
		}
	}

	for k := range boardActions {
		if !bound[k] {
			t.Errorf("handler for %q is not reachable from any declared binding", k)
		}
	}
}

// Moving a card two lanes has to move the same card twice. The daemon decides where a
// moved mission lands, so the cursor can only catch up on the next snapshot; without
// following it, a second press acts on whatever else is under the cursor.
func TestMovingACardTwiceFollowsTheSameCard(t *testing.T) {
	operations := []mission.Operation{testOperation("op_1", "T", 0)}

	// The target lane already holds cards, so an unfollowed cursor would land wrong.
	missions := []mission.Mission{
		testMission("ms_move", "moving", "op_1", mission.StatusAwaiting),
		testMission("ms_other1", "other one", "op_1", mission.StatusDebrief),
		testMission("ms_other2", "other two", "op_1", mission.StatusDebrief),
	}

	board := boardWith(operations, missions)
	board.lane = 2

	cmd := board.moveCardRight()
	if cmd == nil {
		t.Fatal("expected a command")
	}

	move, ok := cmd().(moveMissionMsg)
	if !ok || move.Mission.ID != "ms_move" {
		t.Fatalf("first move = %+v", cmd())
	}

	// The daemon appends the moved card to the end of its new lane.
	moved := missions[0]
	moved.Status = mission.StatusDebrief
	moved.Order = 99

	board.SetSnapshot(mission.Snapshot{
		Operations: operations,
		Missions:   []mission.Mission{moved, missions[1], missions[2]},
	})

	selected, ok := board.Selected()
	if !ok || selected.ID != "ms_move" {
		t.Fatalf("selection did not follow the card: %+v", selected)
	}

	cmd = board.moveCardRight()
	if cmd == nil {
		t.Fatal("expected a second command")
	}

	second, ok := cmd().(moveMissionMsg)
	if !ok {
		t.Fatalf("second move = %T", cmd())
	}

	if second.Mission.ID != "ms_move" {
		t.Errorf("second move acted on %q, want the same card", second.Mission.ID)
	}

	if second.To != mission.StatusClosed {
		t.Errorf("second move went to %q, want done", second.To)
	}
}

// A card that disappears must not leave the selection hunting for it forever.
func TestFollowGivesUpOnAMissingMission(t *testing.T) {
	operations := []mission.Operation{testOperation("op_1", "T", 0)}
	board := boardWith(operations, []mission.Mission{testMission("ms_1", "a", "op_1", mission.StatusBriefing)})

	board.Follow("ms_gone")
	board.SetSnapshot(mission.Snapshot{Operations: operations})

	if board.follow != "" {
		t.Errorf("follow = %q, want it cleared", board.follow)
	}
}

func TestEveryOperationBindingHasAHandler(t *testing.T) {
	view := NewOperations()

	for _, group := range view.keys.FullHelp() {
		for _, b := range group {
			for _, k := range b.Keys() {
				if _, ok := operationActions[k]; !ok {
					t.Errorf("key %q is bound to %q but has no handler", k, b.Help().Key)
				}
			}
		}
	}
}

// The board renders before any window-size message has arrived, so it must tolerate a
// zero size rather than panicking.
func TestBoardRendersAtZeroSize(t *testing.T) {
	board := NewBoard()
	board.SetSnapshot(mission.Snapshot{
		Operations: []mission.Operation{testOperation("op_1", "T", 0)},
		Missions:   []mission.Mission{testMission("ms_1", "a", "op_1", mission.StatusBriefing)},
	})

	if out := board.View(); out == "" {
		t.Error("expected some output")
	}
}

func TestOperationsViewRendersEmptyState(t *testing.T) {
	view := NewOperations()
	view.SetSize(120, 30)
	view.SetSnapshot(mission.Snapshot{})

	out := view.View()
	if !strings.Contains(out, "No operations yet") {
		t.Errorf("expected an empty-state hint:\n%s", out)
	}
}

func TestOperationsViewShowsRepoAndMissionCounts(t *testing.T) {
	operation := testOperation("op_1", "Discussions API", 0)
	operation.Summary = "wire discussions through"
	operation.Repos = []mission.Repo{{Name: "weave", Path: "/dev/weave", DefaultBranch: "main"}}

	view := NewOperations()
	view.SetSize(140, 30)
	view.SetSnapshot(mission.Snapshot{
		Operations: []mission.Operation{operation},
		Missions: []mission.Mission{
			testMission("ms_1", "a", "op_1", mission.StatusBriefing),
			testMission("ms_2", "b", "op_1", mission.StatusClosed),
		},
	})

	out := view.View()

	for _, want := range []string{"Discussions API", "wire discussions through", "weave", "main", "briefing", "closed"} {
		if !strings.Contains(out, want) {
			t.Errorf("operation detail missing %q:\n%s", want, out)
		}
	}
}

// parseRepoLines is how the operation form turns typed paths into repo records.
func TestParseRepoLines(t *testing.T) {
	repos := parseRepoLines("  /dev/weave  \n\n/dev/cloud-services/apps/core/service/azure-tf/\n")

	if len(repos) != 2 {
		t.Fatalf("len = %d, want 2: %+v", len(repos), repos)
	}

	if repos[0].Name != "weave" || repos[0].Path != "/dev/weave" {
		t.Errorf("first repo = %+v", repos[0])
	}

	// A trailing slash must not produce an empty name.
	if repos[1].Name != "azure-tf" {
		t.Errorf("second repo name = %q, want azure-tf", repos[1].Name)
	}
}

// enterKey is the key message bubbletea delivers for return.
func enterKey() tea.KeyMsg { return tea.KeyMsg{Type: tea.KeyEnter} }

// operationFormWith returns an operation form focused on its repo field, completing
// against a fixed set of checkouts so the test never walks a filesystem.
func operationFormWith(candidates []git.Candidate) *operationForm {
	form := newOperationForm(mission.Operation{}, Options{})
	form.repos.repoRoots = []string{"/dev"}
	form.repos.findRepos = func(fragment string) []git.Candidate {
		return git.Match(candidates, fragment)
	}

	// Two tabs is how the user reaches the repo field.
	form.moveField(1)
	form.moveField(1)

	return form
}

// Typing part of a repo's name is the whole point of the field: one match should
// become a full path with no further ceremony, and leave a line to type the next
// repo on.
func TestOperationFormCompletesAUniqueRepoFragment(t *testing.T) {
	form := operationFormWith([]git.Candidate{
		{Path: "/dev/mono/apps/azure-tf", Name: "azure-tf", Rel: "mono/apps/azure-tf"},
	})
	form.repos.SetValue("azure")

	next, cmd := form.Update(enterKey())

	if next != modal(form) || cmd != nil {
		t.Fatalf("completion should stay in the form, got %T and cmd %v", next, cmd != nil)
	}

	lines := form.repos.Lines()
	if len(lines) != 2 || lines[0] != "/dev/mono/apps/azure-tf" || lines[1] != "" {
		t.Fatalf("repos = %q, want the full path and an empty line after it", lines)
	}

	if form.repos.Line() != 1 {
		t.Errorf("cursor on line %d, want the new line 1", form.repos.Line())
	}

	if form.err != "" {
		t.Errorf("unexpected error: %q", form.err)
	}
}

// An exact name wins outright, or every "bob" would open a picker because
// "bob.next" also matches.
func TestOperationFormTakesAnExactNameOverALongerMatch(t *testing.T) {
	form := operationFormWith([]git.Candidate{
		{Path: "/dev/bob.next", Name: "bob.next", Rel: "bob.next"},
		{Path: "/dev/bob", Name: "bob", Rel: "bob"},
	})
	form.repos.SetValue("bob")

	next, _ := form.Update(enterKey())

	if next != modal(form) {
		t.Fatalf("got %T, want the form", next)
	}

	if got := form.repos.Lines()[0]; got != "/dev/bob" {
		t.Errorf("line = %q, want /dev/bob", got)
	}
}

func TestOperationFormPicksBetweenAmbiguousRepos(t *testing.T) {
	form := operationFormWith([]git.Candidate{
		{Path: "/dev/mono/labs/pipeline", Name: "pipeline", Rel: "mono/labs/pipeline"},
		{Path: "/dev/mono/apps/pipeline", Name: "pipeline", Rel: "mono/apps/pipeline"},
	})
	form.repos.SetValue("pipeline")

	next, _ := form.Update(enterKey())

	picker, ok := next.(*listModal)
	if !ok {
		t.Fatalf("got %T, want a picker", next)
	}

	if len(picker.items) != 2 {
		t.Fatalf("picker has %d items, want 2", len(picker.items))
	}

	// Canceling must land back in the form: the alternative is losing the name and
	// summary already typed.
	if back, _ := picker.Update(tea.KeyMsg{Type: tea.KeyEsc}); back != modal(form) {
		t.Fatalf("esc gave %T, want the form back", back)
	}

	picker.Update(keyMsg("j"))
	want := picker.items[1].Key

	after, cmd := picker.Update(enterKey())
	if cmd != nil {
		cmd()
	}

	if after != modal(form) {
		t.Fatalf("picking gave %T, want the form back", after)
	}

	if got := form.repos.Lines()[0]; got != want {
		t.Errorf("line = %q, want the picked %q", got, want)
	}
}

// A short fragment can match more checkouts than the picker can show, and a list cut
// off silently would read as the complete answer.
func TestRepoPickerReportsWhatItLeavesOut(t *testing.T) {
	candidates := make([]git.Candidate, 0, maxRepoChoices*2)
	for i := range maxRepoChoices * 2 {
		name := fmt.Sprintf("svc-%02d", i)
		candidates = append(candidates, git.Candidate{Path: "/dev/" + name, Name: name, Rel: name})
	}

	form := operationFormWith(candidates)
	form.repos.SetValue("svc")

	next, _ := form.Update(enterKey())

	picker, ok := next.(*listModal)
	if !ok {
		t.Fatalf("got %T, want a picker", next)
	}

	if len(picker.items) != maxRepoChoices {
		t.Errorf("picker shows %d items, want %d", len(picker.items), maxRepoChoices)
	}

	if !strings.Contains(picker.hint, fmt.Sprintf("%d matches", len(candidates))) {
		t.Errorf("hint = %q, want the total match count", picker.hint)
	}
}

func TestOperationFormReportsAFragmentThatMatchesNothing(t *testing.T) {
	form := operationFormWith([]git.Candidate{{Path: "/dev/weave", Name: "weave", Rel: "weave"}})
	form.repos.SetValue("nope")

	next, _ := form.Update(enterKey())

	if next != modal(form) {
		t.Fatalf("got %T, want the form", next)
	}

	if !strings.Contains(form.err, "nope") || !strings.Contains(form.err, "/dev") {
		t.Errorf("err = %q, want it to name the fragment and where it looked", form.err)
	}

	// The typed text is left alone, so it can be corrected rather than retyped.
	if got := form.repos.Lines()[0]; got != "nope" {
		t.Errorf("line = %q, want it untouched", got)
	}
}

// Pasting a full path has to keep working, without a search.
func TestOperationFormAcceptsAPathThatAlreadyExists(t *testing.T) {
	dir := t.TempDir()

	form := operationFormWith(nil)
	form.repos.findRepos = func(string) []git.Candidate {
		t.Error("an existing path should not need a search")

		return nil
	}
	form.repos.SetValue(dir)

	form.Update(enterKey())

	if got := form.repos.Lines()[0]; got != dir {
		t.Errorf("line = %q, want %q", got, dir)
	}

	if form.err != "" {
		t.Errorf("unexpected error: %q", form.err)
	}
}

// Enter on a blank line still means newline, so a stray press cannot raise an error.
func TestOperationFormEnterOnABlankRepoLineAddsALine(t *testing.T) {
	form := operationFormWith(nil)
	form.repos.SetValue("/dev/weave\n")

	form.Update(enterKey())

	if form.err != "" {
		t.Errorf("unexpected error: %q", form.err)
	}

	if got := len(form.repos.Lines()); got != 3 {
		t.Errorf("%d lines, want 3: %q", got, form.repos.Lines())
	}
}

// A fragment nobody completed is a typo. Saving it would store a path the daemon
// cannot resolve and put the failure off until the first launch.
func TestOperationFormRefusesAnUncompletedRepoLine(t *testing.T) {
	form := operationFormWith(nil)
	form.name.SetValue("Discussions API")
	form.repos.SetValue("/dev/weave\nazure")

	next, cmd := form.submit()

	if cmd != nil {
		t.Fatal("an operation with an uncompleted repo line should not submit")
	}

	if next != modal(form) {
		t.Fatalf("got %T, want the form", next)
	}

	if !strings.Contains(form.err, "azure") {
		t.Errorf("err = %q, want it to name the offending line", form.err)
	}

	if form.field != fieldOperationRepos {
		t.Errorf("focused field = %d, want the repo field %d", form.field, fieldOperationRepos)
	}

	form.repos.SetValue("/dev/weave")

	if _, cmd := form.submit(); cmd == nil {
		t.Error("an operation whose repos are all full paths should submit")
	}
}

// Completing the line the cursor is on, rather than the last one, is what makes the
// field editable after the fact.
func TestOperationFormCompletesTheLineUnderTheCursor(t *testing.T) {
	form := operationFormWith([]git.Candidate{{Path: "/dev/weave", Name: "weave", Rel: "weave"}})
	form.repos.SetLines([]string{"weave", "/dev/already-full"}, 0)

	form.Update(enterKey())

	lines := form.repos.Lines()
	if lines[0] != "/dev/weave" || lines[1] != "/dev/already-full" {
		t.Errorf("repos = %q, want only the first line completed", lines)
	}
}

// The form must not offer plan mode for an agent that has none, and must not carry a
// stale flag across a change of agent.
func TestMissionFormDropsPlanModeForCodex(t *testing.T) {
	operations := []mission.Operation{testOperation("op_1", "T", 0)}

	form := newMissionForm(mission.Mission{Tool: mission.ToolClaude, PlanMode: true}, operations, "op_1", Options{})
	form.name.SetValue("mission")
	form.prompt.SetValue("do it")

	form.tool = mission.ToolCodex

	_, cmd := form.submit(false)
	if cmd == nil {
		t.Fatal("expected the form to submit")
	}

	msg, ok := cmd().(submitMissionMsg)
	if !ok {
		t.Fatalf("got %T, want submitMissionMsg", cmd())
	}

	if msg.PlanMode {
		t.Error("codex missions must not be submitted with plan mode set")
	}
}

func TestMissionFormRequiresNameAndPrompt(t *testing.T) {
	operations := []mission.Operation{testOperation("op_1", "T", 0)}
	form := newMissionForm(mission.Mission{}, operations, "op_1", Options{})

	if _, cmd := form.submit(false); cmd != nil {
		t.Error("a nameless mission should not submit")
	}

	form.name.SetValue("mission")

	if _, cmd := form.submit(false); cmd != nil {
		t.Error("a mission with no prompt should not submit")
	}

	form.prompt.SetValue("do it")

	if _, cmd := form.submit(false); cmd == nil {
		t.Error("a complete mission should submit")
	}
}

func TestMissionFormCompletesAndSubmitsAdditionalRepos(t *testing.T) {
	operations := []mission.Operation{testOperation("op_1", "Misc", 0)}
	form := newMissionForm(mission.Mission{}, operations, "op_1", Options{})
	form.name.SetValue("small mission")
	form.prompt.SetValue("do it")
	form.repos.repoRoots = []string{"/dev"}
	form.repos.findRepos = func(fragment string) []git.Candidate {
		return git.Match([]git.Candidate{{Path: "/dev/mac", Name: "mac", Rel: "mac"}}, fragment)
	}
	form.repos.SetValue("mac")
	form.focusField(fieldMissionRepos)

	next, _ := form.Update(enterKey())
	if next != modal(form) {
		t.Fatalf("completion returned %T, want mission form", next)
	}

	_, cmd := form.submit(false)
	if cmd == nil {
		t.Fatal("expected the form to submit")
	}

	msg, ok := cmd().(submitMissionMsg)
	if !ok {
		t.Fatalf("got %T, want submitMissionMsg", cmd())
	}

	if len(msg.ExtraRepos) != 1 || msg.ExtraRepos[0].Path != "/dev/mac" {
		t.Fatalf("ExtraRepos = %+v, want mac", msg.ExtraRepos)
	}
}

func TestMissionFormLocksAdditionalReposWhileLaunching(t *testing.T) {
	ms := testMission("ms_1", "running", "op_1", mission.StatusActive)
	ms.ExtraRepos = []mission.Repo{{Name: "mac", Path: "/dev/mac"}}
	form := newMissionForm(ms, []mission.Operation{testOperation("op_1", "T", 0)}, "op_1", Options{})
	form.focusField(fieldMissionRepos)

	form.Update(keyMsg("x"))

	if form.repos.Value() != "/dev/mac" {
		t.Errorf("locked repos changed to %q", form.repos.Value())
	}

	if !strings.Contains(form.repoHelp(), "fixed once launched") {
		t.Errorf("repo help does not explain the lock: %q", form.repoHelp())
	}
}

// Tool and plan mode are baked into a running agent's arguments, so the form must not
// offer to change them after launch.
func TestMissionFormFixesToolAfterLaunch(t *testing.T) {
	started := time.Now()

	ms := testMission("ms_1", "running", "op_1", mission.StatusActive)
	ms.StartedAt = &started

	form := newMissionForm(ms, []mission.Operation{testOperation("op_1", "T", 0)}, "op_1", Options{})
	before := form.tool

	form.cycleTool(keySpace)

	if form.tool != before {
		t.Errorf("tool changed from %q to %q after launch", before, form.tool)
	}
}

func TestStatusMenuExplainsFinishingCleanup(t *testing.T) {
	started := time.Now()
	ms := testMission("ms_1", "running", "op_1", mission.StatusDebrief)
	ms.StartedAt = &started

	app := &App{board: NewBoard()}
	app.showStatusMenu(ms)

	menu, ok := app.modal.(*listModal)
	if !ok {
		t.Fatalf("modal = %T, want a lane picker", app.modal)
	}

	for _, item := range menu.items {
		if item.Key == string(mission.StatusClosed) {
			if !strings.Contains(item.Detail, "reclaims worktrees") {
				t.Errorf("done detail = %q", item.Detail)
			}

			return
		}
	}

	t.Error("done was not offered in the lane picker")
}

func TestDirtyFinishRequiresExplicitConfirmation(t *testing.T) {
	app := &App{board: NewBoard()}
	ms := testMission("ms_1", "running", "op_1", mission.StatusDebrief)

	cmd := app.handleFinishPlan(finishPlanMsg{
		Mission: ms,
		Plan: mission.Plan{
			NeedsForce:   true,
			KeptBranches: []string{"jarush/running"},
		},
	})
	if cmd != nil {
		t.Fatal("a dirty finish should wait for confirmation")
	}

	dialog, ok := app.modal.(*confirmModal)
	if !ok {
		t.Fatalf("modal = %T, want a confirmation", app.modal)
	}

	for _, want := range []string{"Uncommitted changes will be lost", "jarush/running"} {
		if !strings.Contains(dialog.body, want) {
			t.Errorf("confirmation does not mention %q: %s", want, dialog.body)
		}
	}

	if dialog.confirm != "discard and finish" || dialog.onConfirm == nil {
		t.Errorf("confirmation = %q, command nil = %v", dialog.confirm, dialog.onConfirm == nil)
	}
}

func TestCleanFinishNeedsNoConfirmation(t *testing.T) {
	app := &App{board: NewBoard()}
	ms := testMission("ms_1", "running", "op_1", mission.StatusDebrief)

	cmd := app.handleFinishPlan(finishPlanMsg{Mission: ms, Plan: mission.Plan{}})
	if cmd == nil {
		t.Fatal("a clean finish should proceed immediately")
	}

	if app.modal != nil {
		t.Errorf("clean finish opened %T", app.modal)
	}
}

func TestConfirmModalRequiresAnExplicitYes(t *testing.T) {
	var confirmed bool

	dialog := newConfirm("Delete", "sure?", "delete", true, func() tea.Msg {
		confirmed = true

		return nil
	})

	// Anything unrelated leaves it open.
	next, _ := dialog.Update(keyMsg("z"))
	if next == nil {
		t.Fatal("an unrelated key should not dismiss the dialog")
	}

	next, cmd := dialog.Update(keyMsg("n"))
	if next != nil {
		t.Error("n should dismiss the dialog")
	}

	if cmd != nil {
		cmd()
	}

	if confirmed {
		t.Error("n must not confirm")
	}

	dialog = newConfirm("Delete", "sure?", "delete", true, func() tea.Msg {
		confirmed = true

		return nil
	})

	_, cmd = dialog.Update(keyMsg("y"))
	if cmd == nil {
		t.Fatal("y should confirm")
	}

	cmd()

	if !confirmed {
		t.Error("y must confirm")
	}
}

// The resume dialog allows an empty answer, because sometimes only the lane is wrong.
func TestPromptModalEmptySubmission(t *testing.T) {
	var got string

	required := newPrompt("t", "h", "", false, false, func(text string) tea.Cmd {
		got = text

		return nil
	})

	if next, _ := required.submit(); next == nil {
		t.Error("a required prompt should not submit empty")
	}

	optional := newPrompt("t", "h", "", false, true, func(text string) tea.Cmd {
		got = text

		return nil
	})

	if next, _ := optional.submit(); next != nil {
		t.Error("an optional prompt should submit empty")
	}

	if got != "" {
		t.Errorf("got %q, want empty", got)
	}
}

func TestShortDuration(t *testing.T) {
	for _, tc := range []struct {
		in   time.Duration
		want string
	}{
		{30 * time.Second, "30s"},
		{5 * time.Minute, "5m"},
		{3 * time.Hour, "3h"},
		{50 * time.Hour, "2d"},
	} {
		if got := shortDuration(tc.in); got != tc.want {
			t.Errorf("shortDuration(%v) = %q, want %q", tc.in, got, tc.want)
		}
	}
}

// The keymap constants exist so a typo fails to compile rather than silently disabling
// a control, which only works if they match what the bindings declare.
func TestKeyNameConstantsMatchTheBindings(t *testing.T) {
	form := keys.NewForm()

	pairs := []struct {
		name    string
		binding []string
		want    string
	}{
		{"save", form.Submit.Keys(), keySave},
		{"cancel", form.Cancel.Keys(), keyEsc},
		{"next field", form.Next.Keys(), keyTab},
		{"toggle", form.Toggle.Keys(), keySpace},
	}

	for _, pair := range pairs {
		var found bool

		for _, k := range pair.binding {
			if k == pair.want {
				found = true
			}
		}

		if !found {
			t.Errorf("%s binding %q does not include the constant %q", pair.name, pair.binding, pair.want)
		}
	}
}
