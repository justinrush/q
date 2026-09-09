package tui

import (
	"fmt"
	"slices"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/charmbracelet/lipgloss"
	"github.com/justinrush/q/internal/git"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/tui/styles"
)

// wantBranchesMsg asks the app to look up one repo's branches.
//
// The modal emits an intent rather than holding the daemon client, which keeps
// every network call in App where the context and error handling already live.
type wantBranchesMsg struct {
	Repo mission.Repo
	// Refresh asks origin as well as reading local refs.
	Refresh bool
}

// branchesMsg carries the answer back.
type branchesMsg struct {
	Repo     string
	Branches []string
}

// maxBranchChoices is how many branches the picker shows.
//
// Like the repo picker it does not scroll, so a repo with hundreds of branches
// has to be cut off somewhere; the dialog says how many were left out so a
// truncated list cannot be mistaken for the whole answer.
const maxBranchChoices = 12

// branchModal chooses the branch each of a mission's repos is based on.
//
// It edits the form's map in place rather than accumulating its own copy and
// handing it back, because the picker it opens is a third modal on the stack and
// threading a result back through two layers of "return to parent" would be a lot
// of machinery for a map with one key per repo.
type branchModal struct {
	repos []mission.Repo
	// chosen is the form's map. Absent means the repo's default branch.
	chosen map[string]string
	// branches is what each repo's origin offers, keyed by repo name, filled in
	// as the daemon answers.
	branches map[string][]string
	cursor   int
	parent   modal
}

// newBranchModal builds the per-repo list. chosen is retained, not copied.
func newBranchModal(repos []mission.Repo, chosen map[string]string, parent modal) *branchModal {
	return &branchModal{
		repos:    repos,
		chosen:   chosen,
		branches: map[string][]string{},
		parent:   parent,
	}
}

// Update implements modal.
func (m *branchModal) Update(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch msg.String() {
	case keyEsc, keyQuit:
		return m.parent, nil
	case keyUp, keyVimUp:
		m.cursor = clamp(m.cursor-1, max(0, len(m.repos)-1))
	case keyDown, keyVimDown:
		m.cursor = clamp(m.cursor+1, max(0, len(m.repos)-1))
	case keyToggleX:
		// Clearing an override is how a repo goes back to its default branch,
		// which cannot be expressed by picking, since the default is not
		// necessarily in the list.
		if len(m.repos) > 0 {
			delete(m.chosen, m.repos[m.cursor].Name)
		}
	case keyEnter:
		if len(m.repos) == 0 {
			return m.parent, nil
		}

		repo := m.repos[m.cursor]

		return newBranchPicker(repo, m.chosen[repo.Name], m.branches[repo.Name], m),
			emit(wantBranchesMsg{Repo: repo, Refresh: true})
	}

	return m, nil
}

// setBranches records what a repo's origin offers, for a picker opened later.
func (m *branchModal) setBranches(repo string, branches []string) {
	m.branches[repo] = branches
}

// View implements modal.
func (m *branchModal) View(width, height int) string {
	rows := []string{
		styles.ModalTitle.Render("Base branches"),
		styles.FieldLabel.Render("Each worktree is cut from this branch. The mission's own branch is unchanged."),
		"",
	}

	if len(m.repos) == 0 {
		rows = append(rows, styles.CardDetail.Render("this mission has no repositories"))
	}

	nameWidth := 0
	for _, repo := range m.repos {
		nameWidth = max(nameWidth, lipgloss.Width(repo.Name))
	}

	for i, repo := range m.repos {
		marker := "  "
		if i == m.cursor {
			marker = "▸ "
		}

		name := repo.Name + strings.Repeat(" ", nameWidth-lipgloss.Width(repo.Name))

		value := styles.CardDetail.Render("default branch")
		if branch := m.chosen[repo.Name]; branch != "" {
			value = branch
		}

		rows = append(rows, marker+name+"   "+value)
	}

	rows = append(rows, "", styles.Footer.Render("enter choose   x clear   esc back"))

	return center(styles.Modal.Render(lipgloss.JoinVertical(lipgloss.Left, rows...)), width, height)
}

// branchPicker chooses one branch by typing.
//
// It is not a listModal because that one spends j and k on navigation, which
// would make half the alphabet unreachable in a box whose whole purpose is
// typing. Arrows move the cursor here; every printable key filters.
type branchPicker struct {
	repo mission.Repo
	// query is the typed fragment.
	query *textArea
	// branches is everything origin offers, unfiltered.
	branches []string
	// current is the repo's existing choice, highlighted when the box is opened.
	current string
	cursor  int
	parent  *branchModal
}

// newBranchPicker builds the picker for one repo.
//
// The box opens empty with the repo's existing choice merely highlighted, rather
// than opening with that choice as its contents: the query filters, so seeding it
// would hide every other branch behind a fragment the user then has to delete.
func newBranchPicker(repo mission.Repo, current string, branches []string, parent *branchModal) *branchPicker {
	picker := &branchPicker{
		repo:     repo,
		query:    newTextArea("", false),
		branches: branches,
		current:  current,
		parent:   parent,
	}

	picker.homeCursor()

	return picker
}

// homeCursor highlights the repo's existing choice, while the query is untouched.
func (p *branchPicker) homeCursor() {
	if p.current == "" || p.query.Value() != "" {
		return
	}

	if i := slices.Index(p.matches(), p.current); i >= 0 {
		p.cursor = i
	}
}

// Update implements modal.
func (p *branchPicker) Update(msg tea.KeyMsg) (modal, tea.Cmd) {
	switch msg.String() {
	case keyEsc:
		return p.parent, nil
	case keyUp:
		p.cursor = clamp(p.cursor-1, max(0, len(p.matches())-1))

		return p, nil
	case keyDown:
		p.cursor = clamp(p.cursor+1, max(0, len(p.matches())-1))

		return p, nil
	case keyEnter:
		p.accept()

		return p.parent, nil
	}

	cmd := p.query.Update(msg)
	// A filtered list is shorter than the one the cursor was placed in, so the
	// selection has to come back into range or enter would pick nothing.
	p.cursor = clamp(p.cursor, max(0, len(p.matches())-1))

	return p, cmd
}

// accept records the highlighted branch, or the raw query when nothing matches.
//
// Taking the typed text verbatim is deliberate: the list is only as complete as
// what origin has been asked for, and a branch pushed a moment ago should not be
// unusable because the picker has not heard of it yet. Provisioning names the
// branch if it turns out not to exist.
func (p *branchPicker) accept() {
	matches := p.matches()

	switch {
	case len(matches) > 0:
		p.parent.chosen[p.repo.Name] = matches[p.cursor]
	case strings.TrimSpace(p.query.Value()) != "":
		p.parent.chosen[p.repo.Name] = strings.TrimSpace(p.query.Value())
	default:
		delete(p.parent.chosen, p.repo.Name)
	}
}

// setBranches replaces the list, keeping the highlighted row in range.
func (p *branchPicker) setBranches(branches []string) {
	p.branches = branches
	p.cursor = clamp(p.cursor, max(0, len(p.matches())-1))
	p.homeCursor()
}

// matches ranks the branches the query could mean.
func (p *branchPicker) matches() []string {
	return git.MatchBranches(p.branches, p.query.Value())
}

// View implements modal.
func (p *branchPicker) View(width, height int) string {
	inner := min(max(width-20, 30), 90)

	rows := []string{
		styles.ModalTitle.Render("Base branch for " + p.repo.Name),
		styles.FieldLabel.Render("type to filter"),
		"",
		p.query.View(inner),
		"",
	}

	matches := p.matches()

	shown := matches
	if len(shown) > maxBranchChoices {
		shown = shown[:maxBranchChoices]
	}

	if len(matches) == 0 {
		rows = append(rows, styles.CardDetail.Render("no branch matches; enter uses what you typed"))
	}

	for i, branch := range shown {
		marker := "  "
		if i == p.cursor {
			marker = "▸ "
		}

		rows = append(rows, marker+branch)
	}

	if len(matches) > len(shown) {
		rows = append(rows,
			styles.CardDetail.Render(fmt.Sprintf("… %d more, keep typing", len(matches)-len(shown))))
	}

	rows = append(rows, "", styles.Footer.Render("enter select   ↑↓ move   esc back"))

	return center(styles.Modal.Render(lipgloss.JoinVertical(lipgloss.Left, rows...)), width, height)
}

// branchSummary renders the overrides for the mission form, e.g. "q=feat/x".
func branchSummary(repos []mission.Repo, chosen map[string]string) string {
	var parts []string

	for _, repo := range repos {
		if branch := chosen[repo.Name]; branch != "" {
			parts = append(parts, repo.Name+"="+branch)
		}
	}

	return strings.Join(parts, "  ")
}
