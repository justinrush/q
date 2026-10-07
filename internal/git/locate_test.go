package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"github.com/justinrush/q/internal/runner"
)

func TestNormalizeURLAgreesAcrossTransports(t *testing.T) {
	const want = "gitlab.com/justinarush/mac"

	for _, remote := range []string{
		"git@gitlab.com:justinarush/mac.git",
		"https://gitlab.com/justinarush/mac.git",
		"https://gitlab.com/justinarush/mac",
		"https://oauth2:token@GitLab.com/justinarush/mac.git",
		"ssh://git@gitlab.com:22/justinarush/mac.git",
		"ssh://git@gitlab.com/justinarush/mac/",
		"  git@gitlab.com:justinarush/mac.git\n",
	} {
		if got := NormalizeURL(remote); got != want {
			t.Errorf("NormalizeURL(%q) = %q, want %q", remote, got, want)
		}
	}
}

// A path is case sensitive on some forges, so only the host is folded.
func TestNormalizeURLKeepsPathCase(t *testing.T) {
	if got := NormalizeURL("git@GitHub.com:Org/Repo.git"); got != "github.com/Org/Repo" {
		t.Errorf("got %q, want the host lowered and the path as written", got)
	}
}

func TestNormalizeURLLeavesLocalPathsAlone(t *testing.T) {
	for _, remote := range []string{"/srv/git/mac.git", "../mac", ""} {
		if got := NormalizeURL(remote); got != remote {
			t.Errorf("NormalizeURL(%q) = %q, want it unchanged", remote, got)
		}
	}
}

// realGit returns a real git, skipping the test when there is none.
func realGit(t *testing.T) string {
	t.Helper()

	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	return bin
}

// initRepo creates a repository at dir with the given origin.
func initRepo(t *testing.T, bin, dir, origin string) {
	t.Helper()

	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"init", "-q"},
		{"remote", "add", "origin", origin},
	} {
		if origin == "" && args[0] == "remote" {
			continue
		}

		cmd := exec.Command(bin, append([]string{"-C", dir}, args...)...)
		if out, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("git %v: %v\n%s", args, err, out)
		}
	}
}

// Verified against a real git: the locator has to find a checkout by what git
// itself reports as its origin, whichever way that origin was written.
func TestLocatorFindsACheckoutByItsOrigin(t *testing.T) {
	bin := realGit(t)
	root := t.TempDir()

	initRepo(t, bin, filepath.Join(root, "mac"), "https://gitlab.com/justinarush/mac.git")
	initRepo(t, bin, filepath.Join(root, "nested", "deeper", "mac-copy"), "git@gitlab.com:justinarush/mac.git")
	initRepo(t, bin, filepath.Join(root, "q"), "git@github.com:justinrush/q.git")
	initRepo(t, bin, filepath.Join(root, "scratch"), "")

	locator := NewLocator(New(bin, runner.OS{}), ScanOptions{Roots: []string{root}})

	got, ok := locator.Locate(t.Context(), "gitlab.com/justinarush/mac")
	if !ok || got != filepath.Join(root, "mac") {
		t.Errorf("Locate = %q %v, want the shallowest checkout of that origin", got, ok)
	}

	if got, ok := locator.Locate(t.Context(), "github.com/justinrush/q"); !ok || got != filepath.Join(root, "q") {
		t.Errorf("Locate = %q %v, want the q checkout", got, ok)
	}

	if _, ok := locator.Locate(t.Context(), "example.com/not/cloned"); ok {
		t.Error("a repository that is not checked out here was located")
	}

	origin, err := locator.OriginURL(t.Context(), filepath.Join(root, "scratch"))
	if err != nil || origin != "" {
		t.Errorf("OriginURL of a repo with no origin = %q, %v; want empty and no error", origin, err)
	}
}
