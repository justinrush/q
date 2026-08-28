// Package repofind turns part of a directory name into the git checkouts it
// could mean.
//
// It exists for one bit of friction: an operation spans several repos, and typing
// "/Users/me/dev/cloud-services/apps/core/service/azure-tf" for each of them is
// the slowest part of setting an operation up. The names are short and memorable;
// the paths are neither. So q searches a small set of roots, defaulting to
// ~/dev, and lets "azure-tf" stand for the checkout.
//
// Only git checkouts are offered. An operation's repos are worktree sources, so a
// directory that is not a repo could never be used as one, and offering it would
// only push the failure later.
package repofind

import (
	"io/fs"
	"os"
	"path/filepath"
	"slices"
	"strings"
)

// DefaultRoots are searched when the user has configured nothing. Roots that do
// not exist cost nothing, so offering the three common layouts is better than
// making every user configure one.
var DefaultRoots = []string{"~/dev", "~/src", "~/code"}

// DefaultMaxDepth is how many directory levels below a root are searched.
//
// Five is enough for the nesting checkouts actually have — a monorepo-style
// container of repos, e.g. ~/dev/cloud-services/apps/core/service/example — while
// keeping the walk to a few tens of milliseconds.
const DefaultMaxDepth = 5

// DefaultSkip are directories never descended into. Hidden directories are
// skipped too, which is what keeps the walk out of .git itself.
var DefaultSkip = []string{"node_modules", "vendor", "target", "Library"}

// Options is where and how far to search. The zero value is usable and means the
// defaults above.
type Options struct {
	// Roots are the directories to search, each of which may start with ~.
	Roots []string
	// MaxDepth bounds how far below a root the walk descends.
	MaxDepth int
	// Skip are directory names never descended into.
	Skip []string
}

// withDefaults fills in anything the caller left unset.
func (o Options) withDefaults() Options {
	if len(o.Roots) == 0 {
		o.Roots = DefaultRoots
	}

	if o.MaxDepth <= 0 {
		o.MaxDepth = DefaultMaxDepth
	}

	if len(o.Skip) == 0 {
		o.Skip = DefaultSkip
	}

	return o
}

// skips reports whether a directory name is one never descended into.
func (o Options) skips(name string) bool { return slices.Contains(o.Skip, name) }

// Candidate is one git checkout found under a root.
type Candidate struct {
	// Path is the absolute path to the checkout.
	Path string
	// Name is the leaf directory name, which is the part the user types.
	Name string
	// Rel is Path relative to the root it was found under. It is what a picker
	// shows, because it is short but still says which container a checkout is in.
	Rel string
	// Exact is true when the typed fragment was the whole leaf name. It matters
	// because "bob" should mean ~/dev/bob even when ~/dev/bob.next also matches.
	Exact bool
}

// Match ranks, best first, the checkouts a fragment could mean.
//
// The leaf name is tried first, since that is how a repo is referred to out loud.
// A fragment is also matched against the root-relative path, so "labs/access"
// narrows to a nested checkout that "access" alone would leave ambiguous.
func Match(candidates []Candidate, fragment string) []Candidate {
	fragment = strings.ToLower(strings.Trim(strings.TrimSpace(fragment), "/"))
	if fragment == "" {
		return nil
	}

	type hit struct {
		candidate Candidate
		rank      int
	}

	hits := make([]hit, 0, len(candidates))

	for _, candidate := range candidates {
		rank := rankOf(candidate, fragment)
		if rank < 0 {
			continue
		}

		candidate.Exact = rank == rankExact
		hits = append(hits, hit{candidate: candidate, rank: rank})
	}

	slices.SortStableFunc(hits, func(a, b hit) int {
		if a.rank != b.rank {
			return a.rank - b.rank
		}

		// A shallower checkout first: ~/dev/weave is a likelier "weave" than
		// something buried four levels down.
		if depthA, depthB := strings.Count(a.candidate.Rel, "/"), strings.Count(b.candidate.Rel, "/"); depthA != depthB {
			return depthA - depthB
		}

		return strings.Compare(a.candidate.Rel, b.candidate.Rel)
	})

	out := make([]Candidate, 0, len(hits))
	for _, scored := range hits {
		out = append(out, scored.candidate)
	}

	return out
}

// The ranks Match sorts by, best first.
const (
	rankExact = iota
	rankNamePrefix
	rankNameSubstring
	rankPathSubstring
)

// rankOf scores a candidate against a lowercased fragment, returning -1 for no match.
func rankOf(candidate Candidate, fragment string) int {
	name := strings.ToLower(candidate.Name)

	switch {
	case name == fragment:
		return rankExact
	case strings.HasPrefix(name, fragment):
		return rankNamePrefix
	case strings.Contains(name, fragment):
		return rankNameSubstring
	case strings.Contains(strings.ToLower(candidate.Rel), fragment):
		return rankPathSubstring
	}

	return -1
}

// Roots returns the configured directories as absolute, deduplicated paths,
// falling back to DefaultRoots when nothing is configured.
func Roots(configured []string) []string {
	if len(configured) == 0 {
		configured = DefaultRoots
	}

	var out []string

	for _, entry := range configured {
		if entry = strings.TrimSpace(entry); entry == "" {
			continue
		}

		abs := Expand(entry)
		if abs == "" {
			var err error
			if abs, err = filepath.Abs(entry); err != nil {
				continue
			}
		}

		if !slices.Contains(out, abs) {
			out = append(out, abs)
		}
	}

	return out
}

// Expand resolves a path the user typed, returning "" when the text is not a path.
//
// A bare name is not treated as a relative path: the TUI's working directory is
// whatever shell started it, which the user cannot see, so resolving "weave"
// against it would silently mean something different from one session to the next.
func Expand(p string) string {
	p = strings.TrimSpace(p)

	if after, ok := strings.CutPrefix(p, "~"); ok && (after == "" || strings.HasPrefix(after, "/")) {
		home, err := os.UserHomeDir()
		if err != nil {
			return ""
		}

		return filepath.Join(home, after)
	}

	if filepath.IsAbs(p) || strings.HasPrefix(p, "./") || strings.HasPrefix(p, "../") || p == "." {
		abs, err := filepath.Abs(p)
		if err != nil {
			return ""
		}

		return abs
	}

	return ""
}

// Dir reports whether typed text is already a directory, returning it absolute.
// Pasting a full path has to keep working.
func Dir(p string) (string, bool) {
	abs := Expand(p)
	if abs == "" {
		return "", false
	}

	info, err := os.Stat(abs)
	if err != nil || !info.IsDir() {
		return "", false
	}

	return abs, true
}

// Scan returns every git checkout under roots, in walk order.
//
// Unreadable directories are skipped rather than reported: a completion that
// offers what it could find is more useful than one that refuses to offer
// anything because a stray directory denied a read.
func Scan(opts Options) []Candidate {
	opts = opts.withDefaults()

	var out []Candidate

	seen := make(map[string]bool)

	for _, root := range Roots(opts.Roots) {
		for _, candidate := range scanRoot(filepath.Clean(root), opts) {
			if seen[candidate.Path] {
				continue
			}

			seen[candidate.Path] = true
			out = append(out, candidate)
		}
	}

	return out
}

// scanRoot walks one root.
//
// The walk stops descending at a checkout. A repo inside a repo is a vendored
// copy or a worktree far more often than it is something to start an agent in,
// and pruning there is also what keeps the walk cheap.
func scanRoot(root string, opts Options) []Candidate {
	var out []Candidate

	_ = filepath.WalkDir(root, func(path string, entry fs.DirEntry, err error) error {
		if err != nil {
			return nil
		}

		if path != root {
			name := entry.Name()
			if strings.HasPrefix(name, ".") || opts.skips(name) {
				return skipIfDir(entry)
			}
		}

		// A symlinked checkout is offered but not descended into, which keeps a
		// link pointing back up its own tree from looping the walk.
		if !entry.IsDir() {
			if entry.Type()&fs.ModeSymlink != 0 && isRepo(path) {
				out = append(out, newCandidate(root, path))
			}

			return nil
		}

		if isRepo(path) {
			out = append(out, newCandidate(root, path))

			return fs.SkipDir
		}

		if depth(root, path) >= opts.MaxDepth {
			return fs.SkipDir
		}

		return nil
	})

	return out
}

// skipIfDir prunes a directory and ignores anything else.
func skipIfDir(entry fs.DirEntry) error {
	if entry.IsDir() {
		return fs.SkipDir
	}

	return nil
}

// newCandidate describes a checkout found under root.
func newCandidate(root, path string) Candidate {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		rel = path
	}

	return Candidate{Path: path, Name: filepath.Base(path), Rel: rel}
}

// isRepo reports whether a directory holds a .git entry. It may be a file rather
// than a directory, which is how git records a worktree.
func isRepo(dir string) bool {
	_, err := os.Lstat(filepath.Join(dir, ".git"))

	return err == nil
}

// depth is how many levels below root a path sits.
func depth(root, path string) int {
	rel, err := filepath.Rel(root, path)
	if err != nil || rel == "." {
		return 0
	}

	return strings.Count(rel, string(filepath.Separator)) + 1
}

// Display collapses the home directory to ~, so a picker shows the short form of
// a path the user recognizes.
func Display(path string) string {
	home, err := os.UserHomeDir()
	if err != nil || home == "" {
		return path
	}

	if rel, ok := strings.CutPrefix(path, home); ok && (rel == "" || strings.HasPrefix(rel, "/")) {
		return "~" + rel
	}

	return path
}
