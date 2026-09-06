package tui

import (
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/tui/styles"
)

// submitMissionMsg carries a completed mission form.
type submitMissionMsg struct {
	// ID is empty when creating.
	ID          mission.MissionID
	Name        string
	Prompt      string
	Tool        mission.Tool
	Model       string
	Effort      string
	PlanMode    bool
	OperationID mission.OperationID
	ExtraRepos  []mission.Repo
	// Launch requests that the mission be started immediately after saving.
	Launch bool
}

// submitOperationMsg carries a completed operation form.
type submitOperationMsg struct {
	// ID is empty when creating.
	ID      mission.OperationID
	Name    string
	Summary string
	Repos   []mission.Repo
}

// missionForm creates or edits a mission.
type missionForm struct {
	id         mission.MissionID
	operations []mission.Operation

	name   *textArea
	prompt *textArea
	repos  *repoField

	tool         mission.Tool
	model        string
	effort       string
	planMode     bool
	operationIdx int
	field        int
	err          string
	// models is what each agent offers, as the daemon last learned it. It can be
	// empty — no daemon has reached the agent yet — in which case the form offers
	// the agent's own default and says so.
	models map[mission.Tool]mission.ModelSet
	// launched is true when editing an already-started mission, which fixes the tool
	// and plan mode because both are baked into the running agent's arguments.
	launched bool
	// reposLocked covers the launch-in-progress interval before StartedAt is set.
	reposLocked bool
}

// missionFormFields are the form's focusable fields, in tab order.
const (
	fieldMissionName = iota
	fieldMissionOperation
	fieldMissionTool
	fieldMissionModel
	fieldMissionEffort
	fieldMissionPlan
	fieldMissionRepos
	fieldMissionPrompt
	fieldMissionCount
)

// newMissionForm builds a mission form. Pass the zero mission to create.
func newMissionForm(ms mission.Mission, operations []mission.Operation, defaultOperation mission.OperationID, opts Options) *missionForm {
	form := &missionForm{
		id:          ms.ID,
		operations:  operations,
		name:        newTextArea(ms.Name, false),
		prompt:      newTextArea(ms.Prompt, true),
		repos:       newRepoField(ms.ExtraRepos, opts.Repos),
		tool:        ms.Tool,
		model:       ms.Model,
		effort:      ms.Effort,
		planMode:    ms.PlanMode,
		launched:    ms.Launched(),
		reposLocked: ms.Status != "" && ms.Status != mission.StatusBriefing,
		models:      opts.Models,
	}

	if form.tool == "" {
		form.tool = opts.DefaultTool
	}

	// A mission being created has no model yet, so it starts on whatever its
	// agent reports as its own. An existing one keeps what it was given, even a
	// model the current catalog no longer lists.
	if ms.ID == "" {
		form.resetModel()
	}

	wanted := ms.OperationID
	if wanted == "" {
		wanted = defaultOperation
	}

	for i, operation := range operations {
		if operation.ID == wanted {
			form.operationIdx = i
		}
	}

	form.prompt.Blur()
	form.repos.Blur()

	return form
}

// Update implements modal.
func (f *missionForm) Update(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch msg.String() {
	case keyEsc:
		return nil, emit(modalDismissed{})
	case keyTab:
		f.moveField(1)

		return f, nil
	case keyShiftTab:
		f.moveField(-1)

		return f, nil
	case keySave:
		return f.submit(false)
	case keyLaunch:
		// Saving and launching in one step, since creating a mission in order to leave
		// it in briefing is the less common intent.
		return f.submit(true)
	case keyEnter:
		if f.field == fieldMissionRepos && !f.reposLocked {
			next, err := f.repos.complete(f, msg)
			f.err = err

			return next, nil
		}
	}

	return f.updateField(msg)
}

// updateField routes a keypress to the focused field.
func (f *missionForm) updateField(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch f.field {
	case fieldMissionName:
		f.name.Update(msg)
	case fieldMissionPrompt:
		f.prompt.Update(msg)
	case fieldMissionOperation:
		f.cycleOperation(msg.String())
	case fieldMissionTool:
		f.cycleTool(msg.String())
	case fieldMissionModel:
		f.cycleModel(msg.String())
	case fieldMissionEffort:
		f.cycleEffort(msg.String())
	case fieldMissionPlan:
		f.togglePlan(msg.String())
	case fieldMissionRepos:
		if !f.reposLocked {
			f.repos.Update(msg)
		}
	}

	return f, nil
}

// moveField changes focus, keeping the text fields' cursors in step.
func (f *missionForm) moveField(delta int) {
	f.focusField(((f.field+delta)%fieldMissionCount + fieldMissionCount) % fieldMissionCount)
}

func (f *missionForm) focusField(field int) {
	f.field = field

	f.name.Blur()
	f.prompt.Blur()
	f.repos.Blur()

	switch f.field {
	case fieldMissionName:
		_ = f.name.Focus()
	case fieldMissionPrompt:
		_ = f.prompt.Focus()
	case fieldMissionRepos:
		_ = f.repos.Focus()
	}
}

// cycleOperation changes the selected operation.
func (f *missionForm) cycleOperation(keyName string) {
	if len(f.operations) == 0 {
		return
	}

	switch keyName {
	case keyLeft, keyVimLeft, keyUp, keyVimUp:
		f.operationIdx = (f.operationIdx - 1 + len(f.operations)) % len(f.operations)
	case keyRight, keyVimRight, keyDown, keyVimDown, keySpace:
		f.operationIdx = (f.operationIdx + 1) % len(f.operations)
	}
}

// cycleTool changes the agent.
//
// Plan mode is dropped when switching to codex, because codex has no equivalent flag
// and leaving it set would promise a behavior that cannot happen.
func (f *missionForm) cycleTool(keyName string) {
	if f.launched {
		return
	}

	// The direction is discarded: with two agents, forward and backward are the
	// same move, and Tool.Next is the rotation the board already uses.
	if _, ok := cycleDelta(keyName); ok {
		f.tool = f.tool.Next()

		// The models one agent offers mean nothing to another, so the choice is
		// re-made from the new agent's own list rather than carried across.
		f.resetModel()
	}
}

// cycleModel changes the model.
func (f *missionForm) cycleModel(keyName string) {
	if f.launched {
		return
	}

	delta, ok := cycleDelta(keyName)
	if !ok {
		return
	}

	f.model = f.modelSet().NextModel(f.model, delta)
	f.clampEffort()
}

// cycleEffort changes the reasoning effort.
func (f *missionForm) cycleEffort(keyName string) {
	if f.launched || !f.modelSet().SupportsEffort(f.model) {
		return
	}

	if delta, ok := cycleDelta(keyName); ok {
		f.effort = f.modelSet().NextEffort(f.model, f.effort, delta)
	}
}

// cycleDelta reads a keypress as a step through a list, or reports that it is
// not one.
func cycleDelta(keyName string) (int, bool) {
	switch keyName {
	case keyLeft, keyVimLeft, keyUp, keyVimUp:
		return -1, true
	case keyRight, keyVimRight, keyDown, keyVimDown, keySpace:
		return 1, true
	}

	return 0, false
}

// modelSet is what the selected agent offers.
func (f *missionForm) modelSet() mission.ModelSet { return f.models[f.tool] }

// setModels replaces the catalog behind an open form, filling in a choice the
// form could not make when it opened because nothing had been fetched yet.
func (f *missionForm) setModels(models map[mission.Tool]mission.ModelSet) {
	f.models = models

	if f.model == "" && !f.launched {
		f.resetModel()
	}

	f.clampEffort()
}

// resetModel takes the selected agent's own default.
func (f *missionForm) resetModel() {
	set := f.modelSet()
	f.model = set.Default
	f.effort = set.DefaultEffort
	f.clampEffort()
}

// clampEffort drops an effort the selected model does not accept, so switching
// to a model with no effort levels cannot leave a stale one attached to it.
func (f *missionForm) clampEffort() {
	if !f.modelSet().ValidEffort(f.model, f.effort) {
		f.effort = ""
	}
}

// togglePlan flips plan mode when the chosen agent supports it.
func (f *missionForm) togglePlan(keyName string) {
	if f.launched || !f.tool.SupportsPlanMode() {
		return
	}

	if keyName == keySpace || keyName == keyEnter || keyName == keyToggleX {
		f.planMode = !f.planMode
	}
}

// submit validates and emits the form.
func (f *missionForm) submit(launch bool) (modal, tea.Cmd) {
	name := strings.TrimSpace(f.name.Value())
	if name == "" {
		f.err = "a mission needs a name"
		f.focusField(fieldMissionName)

		return f, nil
	}

	if strings.TrimSpace(f.prompt.Value()) == "" {
		f.err = "a mission needs a prompt: it is what the agent is told to do"
		f.focusField(fieldMissionPrompt)

		return f, nil
	}

	if len(f.operations) == 0 {
		f.err = "create an operation first; a mission belongs to one"

		return f, nil
	}

	if !f.reposLocked {
		line := firstUncompletedRepo(f.repos.Value())
		if line != "" {
			f.err = strconv.Quote(line) + " is not a full path: press enter on that line to complete it"
			f.focusField(fieldMissionRepos)

			return f, nil
		}
	}

	return nil, emit(submitMissionMsg{
		ID:          f.id,
		Name:        name,
		Prompt:      f.prompt.Value(),
		Tool:        f.tool,
		Model:       f.model,
		Effort:      f.effort,
		PlanMode:    f.planMode && f.tool.SupportsPlanMode(),
		OperationID: f.operations[f.operationIdx].ID,
		ExtraRepos:  parseRepoLines(f.repos.Value()),
		Launch:      launch,
	})
}

// View implements modal.
func (f *missionForm) View(width, height int) string {
	inner := min(max(width-20, 40), 90)

	title := "New mission"
	if f.id != "" {
		title = "Edit mission"
	}

	rows := []string{
		styles.ModalTitle.Render(title),
		"",
		f.label("Name", fieldMissionName),
		f.name.View(inner),
		"",
		f.label("Operation", fieldMissionOperation) + "  " + f.operationValue(),
		f.label("Agent", fieldMissionTool) + "   " + f.toolValue(),
		f.label("Model", fieldMissionModel) + "   " + f.modelValue(),
		f.label("Effort", fieldMissionEffort) + "  " + f.effortValue(),
		f.label("Plan mode", fieldMissionPlan) + " " + f.planValue(),
		"",
		f.label("Additional repos", fieldMissionRepos),
		f.repoHelp(),
		f.repos.View(inner),
		"",
		f.label("Prompt", fieldMissionPrompt),
		f.prompt.View(inner),
	}

	if f.err != "" {
		rows = append(rows, "", styles.CardError.Render(f.err))
	}

	footer := "tab field   space change   ctrl+s save   ctrl+r save and launch   esc cancel"
	if f.field == fieldMissionRepos && !f.reposLocked {
		footer = "enter complete path   " + footer
	}

	rows = append(rows, "", styles.Footer.Render(footer))

	return center(styles.Modal.Render(lipgloss.JoinVertical(lipgloss.Left, rows...)), width, height)
}

func (f *missionForm) repoHelp() string {
	if f.reposLocked {
		return styles.CardDetail.Render("  fixed once launched")
	}

	return styles.CardDetail.Render("  added to the operation repos; type a name and press enter")
}

// label renders a field label, highlighted when focused.
func (f *missionForm) label(text string, field int) string {
	if f.field == field {
		return styles.FieldLabelFocused.Render("▸ " + text)
	}

	return styles.FieldLabel.Render("  " + text)
}

// operationValue renders the selected operation with its stripe color.
func (f *missionForm) operationValue() string {
	if len(f.operations) == 0 {
		return styles.CardError.Render("no operations yet")
	}

	operation := f.operations[f.operationIdx]

	return lipgloss.NewStyle().Foreground(styles.OperationColor(operation.ColorIdx)).Render("█ ") + operation.Name
}

// toolValue renders the agent choice.
func (f *missionForm) toolValue() string {
	value := f.tool.Glyph() + " " + f.tool.String()
	if f.launched {
		return value + styles.CardDetail.Render("  (fixed once launched)")
	}

	return value
}

// modelValue renders the model choice, with the agent's own description beside it.
func (f *missionForm) modelValue() string {
	set := f.modelSet()

	if f.model == "" {
		if len(set.Options) == 0 {
			return styles.Disabled.Render("the agent's own default") +
				styles.CardDetail.Render("  "+f.catalogHint())
		}

		return styles.Disabled.Render("the agent's own default")
	}

	value := f.model

	if f.launched {
		return value + styles.CardDetail.Render("  (fixed once launched)")
	}

	var notes []string

	if f.model == set.Default {
		notes = append(notes, "default")
	}

	if opt, ok := set.Option(f.model); ok && opt.Detail != "" {
		notes = append(notes, opt.Detail)
	} else if !ok {
		// A model the catalog does not list is still launched as written, so the
		// form says it is unrecognized rather than quietly correcting it.
		notes = append(notes, "not in "+string(f.tool)+"'s list")
	}

	if len(notes) == 0 {
		return value
	}

	return value + styles.CardDetail.Render("  "+strings.Join(notes, " · "))
}

// catalogHint explains an empty model list, which is a reachability problem
// rather than an agent that offers nothing.
func (f *missionForm) catalogHint() string {
	if f.modelSet().Err != "" {
		return string(f.tool) + " could not be asked; run q doctor"
	}

	return string(f.tool) + " has not been asked yet"
}

// effortValue renders the effort choice, explaining when it is unavailable.
func (f *missionForm) effortValue() string {
	set := f.modelSet()

	if !set.SupportsEffort(f.model) {
		return styles.Disabled.Render("unavailable") +
			styles.CardDetail.Render("  "+f.effortHint(set))
	}

	if f.effort == "" {
		return styles.Disabled.Render("the model's own default")
	}

	value := f.effort
	if f.launched {
		return value + styles.CardDetail.Render("  (fixed once launched)")
	}

	return value
}

// effortHint explains why no effort can be chosen.
//
// "this model takes none" and "q could not ask" are different facts and the
// difference matters: the first is settled, the second is a board that will
// answer differently once the agent is reachable.
func (f *missionForm) effortHint(set mission.ModelSet) string {
	switch {
	case f.model == "":
		return "pick a model first"
	case set.Err != "":
		return string(f.tool) + " could not be asked which efforts it takes"
	default:
		return "this model takes no effort setting"
	}
}

// planValue renders the plan-mode choice, explaining when it is unavailable.
func (f *missionForm) planValue() string {
	if !f.tool.SupportsPlanMode() {
		return styles.Disabled.Render("unavailable") +
			styles.CardDetail.Render("  codex has no plan mode")
	}

	if f.planMode {
		return "[x] plan first, then stop for approval"
	}

	return "[ ] off"
}

// operationForm creates or edits an operation.
type operationForm struct {
	id mission.OperationID

	name    *textArea
	summary *textArea
	repos   *repoField
	field   int
	err     string
}

// operationForm fields, in tab order.
const (
	fieldOperationName = iota
	fieldOperationSummary
	fieldOperationRepos
	fieldOperationCount
)

// newOperationForm builds an operation form. Pass the zero operation to create.
func newOperationForm(operation mission.Operation, opts Options) *operationForm {
	form := &operationForm{
		id:      operation.ID,
		name:    newTextArea(operation.Name, false),
		summary: newTextArea(operation.Summary, true),
		repos:   newRepoField(operation.Repos, opts.Repos),
	}

	form.summary.Blur()
	form.repos.Blur()

	return form
}

// Update implements modal.
func (f *operationForm) Update(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch msg.String() {
	case keyEsc:
		return nil, emit(modalDismissed{})
	case keyTab:
		f.moveField(1)

		return f, nil
	case keyShiftTab:
		f.moveField(-1)

		return f, nil
	case keySave:
		return f.submit()
	case keyEnter:
		if f.field == fieldOperationRepos {
			next, err := f.repos.complete(f, msg)
			f.err = err

			return next, nil
		}
	}

	switch f.field {
	case fieldOperationName:
		f.name.Update(msg)
	case fieldOperationSummary:
		f.summary.Update(msg)
	case fieldOperationRepos:
		f.repos.Update(msg)
	}

	return f, nil
}

// moveField changes focus.
func (f *operationForm) moveField(delta int) {
	f.focusField(((f.field+delta)%fieldOperationCount + fieldOperationCount) % fieldOperationCount)
}

// focusField gives one field the cursor.
func (f *operationForm) focusField(field int) {
	f.field = field

	f.name.Blur()
	f.summary.Blur()
	f.repos.Blur()

	switch f.field {
	case fieldOperationName:
		_ = f.name.Focus()
	case fieldOperationSummary:
		_ = f.summary.Focus()
	case fieldOperationRepos:
		_ = f.repos.Focus()
	}
}

// submit validates and emits the form.
func (f *operationForm) submit() (modal, tea.Cmd) {
	name := strings.TrimSpace(f.name.Value())
	if name == "" {
		f.err = "an operation needs a name"
		f.focusField(fieldOperationName)

		return f, nil
	}

	// A line that was never completed is a typo, not a repo. Saving it would store a
	// path the daemon cannot resolve and defer the failure to the first launch.
	if line := firstUncompletedRepo(f.repos.Value()); line != "" {
		f.err = strconv.Quote(line) + " is not a full path: press enter on that line to complete it"
		f.focusField(fieldOperationRepos)

		return f, nil
	}

	return nil, emit(submitOperationMsg{
		ID:      f.id,
		Name:    name,
		Summary: f.summary.Value(),
		Repos:   parseRepoLines(f.repos.Value()),
	})
}

// View implements modal.
func (f *operationForm) View(width, height int) string {
	inner := min(max(width-20, 40), 90)

	title := "New operation"
	if f.id != "" {
		title = "Edit operation"
	}

	rows := []string{
		styles.ModalTitle.Render(title),
		"",
		f.label("Name", fieldOperationName),
		f.name.View(inner),
		"",
		f.label("Summary", fieldOperationSummary),
		styles.CardDetail.Render("  handed to every agent on this operation"),
		f.summary.View(inner),
		"",
		f.label("Repos", fieldOperationRepos),
		styles.CardDetail.Render("  one per line: type part of a name and press enter to complete it"),
		styles.CardDetail.Render("  searched: " + strings.Join(f.repos.displayRoots(), ", ")),
		f.repos.View(inner),
	}

	if f.err != "" {
		rows = append(rows, "", styles.CardError.Render(f.err))
	}

	footer := "tab field   ctrl+s save   esc cancel"
	if f.field == fieldOperationRepos {
		footer = "enter complete path   " + footer
	}

	rows = append(rows, "", styles.Footer.Render(footer))

	return center(styles.Modal.Render(lipgloss.JoinVertical(lipgloss.Left, rows...)), width, height)
}

// label renders a field label, highlighted when focused.
func (f *operationForm) label(text string, field int) string {
	if f.field == field {
		return styles.FieldLabelFocused.Render("▸ " + text)
	}

	return styles.FieldLabel.Render("  " + text)
}

// toCreateOperationRequest converts the form message into an API request.
func (m submitOperationMsg) toCreateOperationRequest() api.CreateOperationRequest {
	return api.CreateOperationRequest{Name: m.Name, Summary: m.Summary, Repos: m.Repos}
}

// toUpdateOperationRequest converts the form message into an API patch.
func (m submitOperationMsg) toUpdateOperationRequest() api.UpdateOperationRequest {
	return api.UpdateOperationRequest{Name: &m.Name, Summary: &m.Summary, Repos: &m.Repos}
}
