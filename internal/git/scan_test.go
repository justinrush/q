package git

import (
	"os"
	"path/filepath"
	"slices"
	"testing"

	"github.com/justinrush/q/internal/paths"
)

// mkRepo creates a directory under root and marks it as a git checkout.
func mkRepo(t *testing.T, root, rel string) string {
	t.Helper()

	path := filepath.Join(root, rel)
	if err := os.MkdirAll(filepath.Join(path, ".git"), 0o700); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}

	return path
}

// mkDir creates a plain directory: a container of checkouts rather than one itself.
func mkDir(t *testing.T, root, rel string) string {
	t.Helper()

	path := filepath.Join(root, rel)
	if err := os.MkdirAll(path, 0o700); err != nil {
		t.Fatalf("MkdirAll %s: %v", path, err)
	}

	return path
}

// rels returns the found checkouts' root-relative paths, sorted for comparison.
func rels(candidates []Candidate) []string {
	out := make([]string, 0, len(candidates))
	for _, candidate := range candidates {
		out = append(out, candidate.Rel)
	}

	slices.Sort(out)

	return out
}

// A checkout may sit directly under a root or several containers down, and the walk
// must not go looking inside one it has already found: a vendored copy or a worktree
// is not something to start an agent in, and descending would make the walk crawl.
func TestScanFindsNestedCheckoutsAndPrunesInsideThem(t *testing.T) {
	root := t.TempDir()

	mkRepo(t, root, "weave")
	mkRepo(t, root, "monorepo/apps/azure-tf")
	mkRepo(t, root, "weave/vendored/dependency")
	mkDir(t, root, "monorepo/apps/not-a-repo")

	got := rels(Scan(ScanOptions{Roots: []string{root}}))
	want := []string{"monorepo/apps/azure-tf", "weave"}

	if !slices.Equal(got, want) {
		t.Errorf("Scan = %v, want %v", got, want)
	}
}

func TestScanStopsAtMaxDepth(t *testing.T) {
	root := t.TempDir()

	mkRepo(t, root, "cloud-services/apps/core/service/batch-management-service")
	mkRepo(t, root, "a/b/c/d/e/too-deep")

	got := rels(Scan(ScanOptions{Roots: []string{root}}))
	want := []string{filepath.Join("cloud-services", "apps", "core", "service", "batch-management-service")}

	if !slices.Equal(got, want) {
		t.Errorf("Scan = %v, want %v (a checkout at maxDepth should be offered and one past it should not)", got, want)
	}
}

// Hidden and dependency directories are skipped, which is also what keeps the walk
// from treating the contents of a .git directory as candidates.
func TestScanSkipsHiddenAndDependencyDirs(t *testing.T) {
	root := t.TempDir()

	mkRepo(t, root, ".cache/hidden")
	mkRepo(t, root, "node_modules/some-package")
	mkRepo(t, root, "kept")

	if got := rels(Scan(ScanOptions{Roots: []string{root}})); !slices.Equal(got, []string{"kept"}) {
		t.Errorf("Scan = %v, want [kept]", got)
	}
}

func TestScanDeduplicatesOverlappingRoots(t *testing.T) {
	root := t.TempDir()
	mkRepo(t, root, "weave")

	if got := Scan(ScanOptions{Roots: []string{root, root + string(filepath.Separator)}}); len(got) != 1 {
		t.Errorf("Scan over the same root twice = %v, want one candidate", rels(got))
	}
}

// "bob" must mean bob, not bob.next, or the completion would open a picker for a
// name the user already typed in full.
func TestMatchPrefersAnExactName(t *testing.T) {
	candidates := []Candidate{
		{Path: "/dev/bob.next", Name: "bob.next", Rel: "bob.next"},
		{Path: "/dev/bob", Name: "bob", Rel: "bob"},
	}

	got := Match(candidates, "bob")

	if len(got) != 2 {
		t.Fatalf("Match = %v, want both candidates", rels(got))
	}

	if got[0].Name != "bob" || !got[0].Exact {
		t.Errorf("first match = %+v, want bob marked exact", got[0])
	}

	if got[1].Exact {
		t.Errorf("second match = %+v, must not be marked exact", got[1])
	}
}

func TestMatchRanksPrefixOverSubstringAndShallowFirst(t *testing.T) {
	candidates := []Candidate{
		{Path: "/dev/mono/apps/deep-weave", Name: "deep-weave", Rel: "mono/apps/deep-weave"},
		{Path: "/dev/weave-ui", Name: "weave-ui", Rel: "weave-ui"},
		{Path: "/dev/mono/weave-api", Name: "weave-api", Rel: "mono/weave-api"},
	}

	got := Match(candidates, "WEAVE")

	want := []string{"weave-ui", "weave-api", "deep-weave"}
	for i, name := range want {
		if got[i].Name != name {
			t.Errorf("match %d = %q, want %q (order: %v)", i, got[i].Name, name, rels(got))
		}
	}
}

// A fragment with a slash is how an otherwise ambiguous nested checkout is narrowed.
func TestMatchAcceptsAPathFragment(t *testing.T) {
	candidates := []Candidate{
		{Path: "/dev/mono/labs/pipeline", Name: "pipeline", Rel: "mono/labs/pipeline"},
		{Path: "/dev/mono/apps/pipeline", Name: "pipeline", Rel: "mono/apps/pipeline"},
	}

	got := Match(candidates, "labs/pipe")

	if len(got) != 1 || got[0].Rel != "mono/labs/pipeline" {
		t.Errorf("Match = %v, want only mono/labs/pipeline", rels(got))
	}
}

func TestMatchIgnoresEmptyFragment(t *testing.T) {
	if got := Match([]Candidate{{Name: "weave"}}, "   "); got != nil {
		t.Errorf("Match with no fragment = %v, want nothing", rels(got))
	}
}

func TestRootsDefaultsToTheCommonLayoutsAndHonorsConfiguredOnes(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	want := []string{
		filepath.Join(home, "dev"),
		filepath.Join(home, "src"),
		filepath.Join(home, "code"),
	}
	if got := ScanRoots(nil); !slices.Equal(got, want) {
		t.Errorf("Roots = %v, want the defaults under %s", got, home)
	}

	want = []string{filepath.Join(home, "work"), "/srv/src"}
	if got := ScanRoots([]string{"~/work", "/srv/src"}); !slices.Equal(got, want) {
		t.Errorf("Roots = %v, want %v", got, want)
	}
}

// A configured skip list replaces the default one, so a user who vendors into a
// directory q would otherwise prune can still reach it.
func TestScanHonorsAConfiguredSkipList(t *testing.T) {
	root := t.TempDir()

	mkRepo(t, root, "node_modules/some-package")
	mkRepo(t, root, "generated/thing")

	got := rels(Scan(ScanOptions{Roots: []string{root}, Skip: []string{"generated"}}))
	if !slices.Equal(got, []string{filepath.Join("node_modules", "some-package")}) {
		t.Errorf("Scan = %v, want the node_modules checkout and not the generated one", got)
	}
}

// A shallower depth limit is honoured, which is the knob for a machine where the
// default walk is too slow.
func TestScanHonorsAConfiguredMaxDepth(t *testing.T) {
	root := t.TempDir()

	mkRepo(t, root, "shallow")
	mkRepo(t, root, "a/b/deep")

	got := rels(Scan(ScanOptions{Roots: []string{root}, MaxDepth: 1}))
	if !slices.Equal(got, []string{"shallow"}) {
		t.Errorf("Scan = %v, want only the shallow checkout", got)
	}
}

// A bare name must not resolve against the working directory: the TUI's cwd is
// whatever shell started it, which the user cannot see.
func TestExpandOnlyResolvesThingsThatLookLikePaths(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got := paths.Expand("~/dev/weave"); got != filepath.Join(home, "dev", "weave") {
		t.Errorf("Expand(~/dev/weave) = %q", got)
	}

	if got := paths.Expand("weave"); got != "" {
		t.Errorf("Expand(weave) = %q, want it left alone", got)
	}

	if got := paths.Expand("/dev/weave"); got != "/dev/weave" {
		t.Errorf("Expand(/dev/weave) = %q", got)
	}
}

func TestDirAcceptsAnExistingDirectoryOnly(t *testing.T) {
	root := t.TempDir()
	repo := mkRepo(t, root, "weave")

	if got, ok := paths.Dir(repo); !ok || got != repo {
		t.Errorf("Dir(%s) = %q, %v", repo, got, ok)
	}

	if _, ok := paths.Dir(filepath.Join(root, "missing")); ok {
		t.Error("Dir accepted a path that does not exist")
	}

	file := filepath.Join(root, "weave", ".git", "HEAD")
	if err := os.WriteFile(file, []byte("ref: refs/heads/main\n"), 0o600); err != nil {
		t.Fatalf("WriteFile: %v", err)
	}

	if _, ok := paths.Dir(file); ok {
		t.Error("Dir accepted a file")
	}
}

func TestDisplayCollapsesHome(t *testing.T) {
	home := t.TempDir()
	t.Setenv("HOME", home)

	if got := paths.Display(filepath.Join(home, "dev", "weave")); got != "~/dev/weave" {
		t.Errorf("Display = %q, want ~/dev/weave", got)
	}

	if got := paths.Display("/srv/src/weave"); got != "/srv/src/weave" {
		t.Errorf("Display = %q, want the path unchanged", got)
	}
}
