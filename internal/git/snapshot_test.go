package git

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// These tests run a real git. What they check is git's behavior as much as
// q's — that a tree can be built without touching the index, that a reset
// sequence leaves uncommitted work uncommitted — and a fake that answered the
// way q hoped would prove nothing.

const (
	hostA mission.HostID    = "h_aaaaaaaaaaaa"
	hostB mission.HostID    = "h_bbbbbbbbbbbb"
	msID  mission.MissionID = "ms_aabbccddeeff"
)

// sandbox is a real repository with one commit, and a client to drive it.
type sandbox struct {
	t   *testing.T
	bin string
	dir string
	git *Client
}

// newSandbox creates a repository holding a.txt and b.txt.
func newSandbox(t *testing.T) *sandbox {
	t.Helper()

	bin := realGit(t)
	// The path is resolved because macOS's temp directory is a symlink, and git
	// reports the resolved form.
	dir, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	s := &sandbox{t: t, bin: bin, dir: dir, git: New(bin, runner.OS{})}

	// Isolated from the developer's own configuration, which may sign commits or
	// rewrite line endings.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	s.run(dir, "init", "-q", "-b", "main")
	s.run(dir, "config", "user.name", "tester")
	s.run(dir, "config", "user.email", "tester@example.com")
	s.write(dir, "a.txt", "alpha\n")
	s.write(dir, "b.txt", "bravo\n")
	s.write(dir, ".gitignore", "ignored/\n")
	s.run(dir, "add", "-A")
	s.run(dir, "commit", "-q", "-m", "first")

	return s
}

// run executes git in dir and returns its trimmed output.
func (s *sandbox) run(dir string, args ...string) string {
	s.t.Helper()

	out, err := exec.Command(s.bin, append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		s.t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}

	return strings.TrimSpace(string(out))
}

func (s *sandbox) write(dir, name, content string) {
	s.t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		s.t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		s.t.Fatal(err)
	}
}

func (s *sandbox) read(dir, name string) string {
	s.t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "<missing>"
	}

	return string(data)
}

// worktree adds a linked worktree on a new branch, as a mission's is.
func (s *sandbox) worktree(name string) string {
	s.t.Helper()

	path := filepath.Join(filepath.Dir(s.dir), filepath.Base(s.dir)+"-"+name)
	s.run(s.dir, "worktree", "add", "-q", "-b", "me/"+name, path, "HEAD")
	s.t.Cleanup(func() { _ = os.RemoveAll(path) })

	return path
}

// snap takes a snapshot under host's ref, failing the test on error.
func (s *sandbox) snap(worktree string, host mission.HostID) mission.Snap {
	s.t.Helper()

	snap, err := s.git.Snapshot(s.t.Context(), worktree, SnapshotRef(host, msID))
	if err != nil {
		s.t.Fatalf("Snapshot: %v", err)
	}

	return snap
}

func TestSnapshotOfACleanWorktreeIsItsHead(t *testing.T) {
	s := newSandbox(t)
	wt := s.worktree("clean")

	snap := s.snap(wt, hostA)

	if snap.Commit != snap.Head || snap.Head != s.run(wt, "rev-parse", "HEAD") {
		t.Errorf("snap = %+v, want the branch tip with no extra commit", snap)
	}

	if got := s.run(s.dir, "rev-parse", SnapshotRef(hostA, msID)); got != snap.Commit {
		t.Errorf("ref = %s, want it at the snapshot %s", got, snap.Commit)
	}
}

// The whole point: modified, new, and deleted files are all in the snapshot,
// ignored ones are not, and none of it disturbs what the agent is doing.
func TestSnapshotCapturesUncommittedWorkWithoutTouchingIt(t *testing.T) {
	s := newSandbox(t)
	wt := s.worktree("dirty")

	s.write(wt, "a.txt", "alpha, edited\n")
	s.write(wt, "new/c.txt", "charlie\n")
	s.write(wt, "ignored/secret.txt", "do not carry\n")
	s.write(wt, "staged.txt", "half-finished add\n")
	s.run(wt, "add", "staged.txt")

	if err := os.Remove(filepath.Join(wt, "b.txt")); err != nil {
		t.Fatal(err)
	}

	statusBefore := s.run(wt, "status", "--porcelain=v1")
	headBefore := s.run(wt, "rev-parse", "HEAD")

	snap := s.snap(wt, hostA)

	if snap.Commit == snap.Head {
		t.Fatal("a dirty worktree's snapshot must be its own commit")
	}

	if parent := s.run(wt, "rev-parse", snap.Commit+"^"); parent != headBefore {
		t.Errorf("snapshot parent = %s, want the branch tip %s", parent, headBefore)
	}

	files := s.run(wt, "ls-tree", "-r", "--name-only", snap.Commit)

	for _, want := range []string{"a.txt", "new/c.txt", "staged.txt", ".gitignore"} {
		if !strings.Contains(files, want) {
			t.Errorf("snapshot lacks %s:\n%s", want, files)
		}
	}

	for _, absent := range []string{"b.txt", "ignored/secret.txt"} {
		if strings.Contains(files, absent) {
			t.Errorf("snapshot contains %s:\n%s", absent, files)
		}
	}

	if got := s.run(wt, "show", snap.Commit+":a.txt"); got != "alpha, edited" {
		t.Errorf("a.txt in the snapshot = %q, want the edit", got)
	}

	// Nothing the agent can see has moved.
	if got := s.run(wt, "status", "--porcelain=v1"); got != statusBefore {
		t.Errorf("status changed:\nbefore:\n%s\nafter:\n%s", statusBefore, got)
	}

	if got := s.run(wt, "rev-parse", "HEAD"); got != headBefore {
		t.Errorf("HEAD moved from %s to %s", headBefore, got)
	}

	if author := s.run(wt, "log", "-1", "--format=%an", snap.Commit); author != "q" {
		t.Errorf("snapshot author = %q, want q rather than the user", author)
	}
}

// An idle worktree must not mint a commit on every exchange.
func TestSnapshotIsStableWhileNothingChanges(t *testing.T) {
	s := newSandbox(t)
	wt := s.worktree("idle")
	s.write(wt, "a.txt", "edited once\n")

	first := s.snap(wt, hostA)
	second := s.snap(wt, hostA)

	if second.Commit != first.Commit {
		t.Errorf("second snapshot = %s, want the first %s again", second.Commit, first.Commit)
	}

	// Not merely within the same second: the id must not depend on when it was
	// taken, or two hosts could never arrive at it independently.
	time.Sleep(1100 * time.Millisecond)

	if later := s.snap(wt, hostA); later.Commit != first.Commit {
		t.Errorf("a later snapshot of the same state = %s, want %s", later.Commit, first.Commit)
	}

	s.write(wt, "a.txt", "edited twice\n")

	if third := s.snap(wt, hostA); third.Commit == second.Commit {
		t.Error("a changed worktree reused the old snapshot")
	}
}

// A snapshot taken on the main worktree, whose index path git reports
// relatively, works the same.
func TestSnapshotWorksInTheMainWorktree(t *testing.T) {
	s := newSandbox(t)
	s.write(s.dir, "a.txt", "edited in place\n")

	if snap := s.snap(s.dir, hostA); snap.Commit == snap.Head {
		t.Error("the edit was not captured")
	}
}

// mirrorOf adds a second worktree and lays snap out in it, as the peer does.
func (s *sandbox) mirrorOf(name string, current, target mission.Snap) string {
	s.t.Helper()

	wt := s.worktree(name)

	if err := s.git.ApplySnapshot(s.t.Context(), wt, current, target); err != nil {
		s.t.Fatalf("ApplySnapshot: %v", err)
	}

	return wt
}

// Laid out on the other host, a snapshot is the same branch tip with the same
// work still uncommitted, so a human reviewing it sees what the agent sees.
func TestApplySnapshotReproducesTipAndUncommittedWork(t *testing.T) {
	s := newSandbox(t)
	source := s.worktree("source")

	s.write(source, "committed.txt", "in a commit\n")
	s.run(source, "add", "-A")
	s.run(source, "commit", "-q", "-m", "agent commit")
	s.write(source, "a.txt", "alpha, uncommitted\n")
	s.write(source, "new.txt", "untracked\n")

	if err := os.Remove(filepath.Join(source, "b.txt")); err != nil {
		t.Fatal(err)
	}

	snap := s.snap(source, hostA)
	mirror := s.mirrorOf("mirror", mission.Snap{}, snap)

	if got := s.run(mirror, "rev-parse", "HEAD"); got != snap.Head {
		t.Errorf("mirror HEAD = %s, want the real tip %s, not the snapshot commit", got, snap.Head)
	}

	if branch := s.run(mirror, "rev-parse", "--abbrev-ref", "HEAD"); branch != "me/mirror" {
		t.Errorf("mirror is on %q, want it still on its own branch", branch)
	}

	if got, want := s.run(mirror, "status", "--porcelain=v1"), s.run(source, "status", "--porcelain=v1"); got != want {
		t.Errorf("mirror status:\n%s\nwant the source's:\n%s", got, want)
	}

	if got := s.read(mirror, "a.txt"); got != "alpha, uncommitted\n" {
		t.Errorf("a.txt = %q", got)
	}

	// And the mirror's own snapshot is the very same commit, which is how the two
	// hosts know they agree.
	if again := s.snap(mirror, hostB); again.Commit != snap.Commit {
		t.Errorf("mirror snapshot = %s, want the source's %s", again.Commit, snap.Commit)
	}
}

// The case the index priming exists for. A file that was new and uncommitted
// in one snapshot and gone in the next must leave the mirror, or the mirror
// would look edited forever after.
func TestApplySnapshotRemovesFilesThatWereOnlyEverUncommitted(t *testing.T) {
	s := newSandbox(t)
	source := s.worktree("source")

	s.write(source, "scratch.txt", "temporary\n")
	first := s.snap(source, hostA)
	mirror := s.mirrorOf("mirror", mission.Snap{}, first)

	if s.read(mirror, "scratch.txt") != "temporary\n" {
		t.Fatal("the mirror never received the new file")
	}

	if err := os.Remove(filepath.Join(source, "scratch.txt")); err != nil {
		t.Fatal(err)
	}

	s.write(source, "a.txt", "alpha, moved on\n")

	second := s.snap(source, hostA)

	if err := s.git.ApplySnapshot(t.Context(), mirror, first, second); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	if got := s.read(mirror, "scratch.txt"); got != "<missing>" {
		t.Errorf("scratch.txt is still in the mirror: %q", got)
	}

	if again := s.snap(mirror, hostB); again.Commit != second.Commit {
		t.Errorf("mirror snapshot = %s, want %s: the mirror does not match what it was given", again.Commit, second.Commit)
	}
}

// Ignored files are this machine's own — build output, a local .env — and a
// snapshot from the other machine must not disturb them.
func TestApplySnapshotLeavesIgnoredFilesAlone(t *testing.T) {
	s := newSandbox(t)
	source := s.worktree("source")
	s.write(source, "a.txt", "changed\n")

	snap := s.snap(source, hostA)

	mirror := s.worktree("mirror")
	s.write(mirror, "ignored/local.env", "mine\n")

	if err := s.git.ApplySnapshot(t.Context(), mirror, mission.Snap{}, snap); err != nil {
		t.Fatalf("ApplySnapshot: %v", err)
	}

	if got := s.read(mirror, "ignored/local.env"); got != "mine\n" {
		t.Errorf("ignored file = %q, want it untouched", got)
	}
}

// Snapshots travel between two clones by git's own transport, under refs
// neither clone lists as a branch.
func TestHostRefsTravelBetweenClonesAndArePruned(t *testing.T) {
	s := newSandbox(t)

	clone := filepath.Join(filepath.Dir(s.dir), filepath.Base(s.dir)+"-clone")
	s.run(filepath.Dir(s.dir), "clone", "-q", s.dir, clone)
	t.Cleanup(func() { _ = os.RemoveAll(clone) })

	wt := s.worktree("work")
	s.write(wt, "a.txt", "work in progress\n")
	snap := s.snap(wt, hostA)

	if err := s.git.PushHostRefs(t.Context(), s.dir, clone, hostA); err != nil {
		t.Fatalf("PushHostRefs: %v", err)
	}

	if got := s.run(clone, "rev-parse", SnapshotRef(hostA, msID)); got != snap.Commit {
		t.Errorf("pushed ref = %s, want %s", got, snap.Commit)
	}

	if !s.git.HasCommit(t.Context(), clone, snap.Commit) {
		t.Error("the snapshot's objects did not arrive")
	}

	if branches := s.run(clone, "branch", "--all", "--list"); strings.Contains(branches, "refs/q") || strings.Contains(branches, "snap") {
		t.Errorf("a snapshot showed up as a branch:\n%s", branches)
	}

	// The other direction: something published in the clone is fetched back.
	s.run(clone, "update-ref", SnapshotRef(hostB, msID), snap.Commit)

	if err := s.git.FetchHostRefs(t.Context(), s.dir, clone, hostB); err != nil {
		t.Fatalf("FetchHostRefs: %v", err)
	}

	if got := s.run(s.dir, "rev-parse", SnapshotRef(hostB, msID)); got != snap.Commit {
		t.Errorf("fetched ref = %s, want %s", got, snap.Commit)
	}

	// A mission either side has finished with is forgotten on the next exchange.
	if err := s.git.ForgetMission(t.Context(), s.dir, msID); err != nil {
		t.Fatalf("ForgetMission: %v", err)
	}

	if err := s.git.PushHostRefs(t.Context(), s.dir, clone, hostA); err != nil {
		t.Fatalf("PushHostRefs after forgetting: %v", err)
	}

	if out, err := exec.Command(s.bin, "-C", clone, "rev-parse", "--verify", "--quiet", SnapshotRef(hostA, msID)).Output(); err == nil {
		t.Errorf("the forgotten ref is still in the clone: %s", out)
	}
}

// With nothing to publish, a push has to be a quiet no-op rather than an error:
// most repositories have no running mission most of the time.
func TestPushingWithNoRefsSucceeds(t *testing.T) {
	s := newSandbox(t)

	clone := filepath.Join(filepath.Dir(s.dir), filepath.Base(s.dir)+"-empty")
	s.run(filepath.Dir(s.dir), "clone", "-q", s.dir, clone)
	t.Cleanup(func() { _ = os.RemoveAll(clone) })

	if err := s.git.PushHostRefs(t.Context(), s.dir, clone, hostA); err != nil {
		t.Errorf("PushHostRefs with nothing to send: %v", err)
	}

	if err := s.git.FetchHostRefs(t.Context(), s.dir, clone, hostB); err != nil {
		t.Errorf("FetchHostRefs with nothing to fetch: %v", err)
	}
}
