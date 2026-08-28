package tui

import (
	"fmt"
	"path/filepath"
	"strconv"
	"strings"

	tea "github.com/charmbracelet/bubbletea"
	"github.com/justinrush/q/internal/git"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
)

// repoField is a one-checkout-per-line input with repository discovery.
type repoField struct {
	*textArea
	findRepos func(fragment string) []git.Candidate
	repoRoots []string
}

// maxRepoChoices is how many matches the repo picker shows.
//
// The picker does not scroll, so a short fragment matching a hundred checkouts has
// to be cut off somewhere; the dialog says how many were left out so a truncated
// list cannot be mistaken for the whole answer.
const maxRepoChoices = 12

func newRepoField(repos []mission.Repo, opts git.ScanOptions) *repoField {
	var paths []string
	for _, repo := range repos {
		paths = append(paths, repo.Path)
	}

	return &repoField{
		textArea:  newTextArea(strings.Join(paths, "\n"), true),
		findRepos: newRepoFinder(opts),
		repoRoots: git.ScanRoots(opts.Roots),
	}
}

// newRepoFinder returns a fragment resolver that scans the roots once, on first use.
//
// The scan runs inside the keypress that needs it rather than as a background
// command: it takes a few tens of milliseconds over a few hundred checkouts, and a
// completion that arrives after the next keystroke would be worse than one that
// makes the terminal pause imperceptibly. Caching keeps every later completion free.
func newRepoFinder(opts git.ScanOptions) func(string) []git.Candidate {
	var (
		candidates []git.Candidate
		scanned    bool
	)

	return func(fragment string) []git.Candidate {
		if !scanned {
			candidates, scanned = git.Scan(opts), true
		}

		return git.Match(candidates, fragment)
	}
}

// complete turns the repo line under the cursor into a full checkout path.
//
// Enter is spent on completion rather than on a newline because finishing a line is
// what pressing it means here: the completed path takes the line, and the cursor
// lands on the next one ready for the next repo. An empty line still takes the
// newline, so a stray press cannot produce an error message.
func (f *repoField) complete(owner modal, msg tea.KeyMsg) (modal, string) {
	row, lines := f.Line(), f.Lines()

	fragment := ""
	if row < len(lines) {
		fragment = strings.TrimSpace(lines[row])
	}

	if fragment == "" {
		f.Update(msg)

		return owner, ""
	}

	// A path that already names a directory is taken as typed, so pasting a full
	// path keeps working.
	if dir, ok := paths.Dir(fragment); ok {
		f.accept(row, dir)

		return owner, ""
	}

	matches := f.findRepos(fragment)

	switch {
	case len(matches) == 0:
		err := "no git checkout matching " + strconv.Quote(fragment) + " under " +
			strings.Join(f.displayRoots(), ", ")

		return owner, err
	// One match, or one whose name is exactly what was typed, needs no picker: "bob"
	// means ~/dev/bob even when ~/dev/bob.next matches too.
	case len(matches) == 1, matches[0].Exact && !matches[1].Exact:
		f.accept(row, matches[0].Path)

		return owner, ""
	}

	return f.picker(owner, row, fragment, matches), ""
}

// accept writes a resolved path over the line at row and moves to the next line.
func (f *repoField) accept(row int, path string) {
	lines := f.Lines()
	for len(lines) <= row {
		lines = append(lines, "")
	}

	lines[row] = path

	if row+1 >= len(lines) {
		lines = append(lines, "")
	}

	f.SetLines(lines, row+1)
}

// picker offers the matching checkouts, returning to the owning form either way.
func (f *repoField) picker(owner modal, row int, fragment string, matches []git.Candidate) modal {
	hint := "one of these"

	shown := matches
	if len(shown) > maxRepoChoices {
		shown = shown[:maxRepoChoices]
		hint = fmt.Sprintf("%d matches; showing %d, type more to narrow", len(matches), maxRepoChoices)
	}

	// The rows are whole paths rather than names plus a location, because the paths
	// are what differs between two checkouts called the same thing.
	var items []listItem
	for _, candidate := range shown {
		items = append(items, listItem{Key: candidate.Path, Label: paths.Display(candidate.Path)})
	}

	return newList("Repos matching "+strconv.Quote(fragment), hint, items, func(picked string) tea.Cmd {
		f.accept(row, picked)

		return nil
	}).under(owner)
}

// displayRoots is the searched roots, shortened for a message.
func (f *repoField) displayRoots() []string {
	var out []string
	for _, root := range f.repoRoots {
		out = append(out, paths.Display(root))
	}

	return out
}

// firstUncompletedRepo returns the first repo line that is not an absolute path,
// which is what a fragment the user never completed looks like.
func firstUncompletedRepo(text string) string {
	for line := range strings.SplitSeq(text, "\n") {
		if line = strings.TrimSpace(line); line != "" && !filepath.IsAbs(line) {
			return line
		}
	}

	return ""
}

// parseRepoLines turns one-path-per-line text into repo records.
//
// Only the name and path are set. The default branch and git directory need a
// subprocess, which the daemon runs when it provisions worktrees.
func parseRepoLines(text string) []mission.Repo {
	var repos []mission.Repo

	for line := range strings.SplitSeq(text, "\n") {
		path := strings.TrimSpace(line)
		if path == "" {
			continue
		}

		repos = append(repos, mission.Repo{Name: repoLeaf(path), Path: path})
	}

	return repos
}

// repoLeaf is the final path element, which is how q names a repo.
func repoLeaf(path string) string {
	trimmed := strings.TrimRight(path, "/")
	if idx := strings.LastIndex(trimmed, "/"); idx >= 0 {
		return trimmed[idx+1:]
	}

	return trimmed
}
