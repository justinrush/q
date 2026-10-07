package daemon

import (
	"context"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/git"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/runner"
)

// These tests pair two services over real git repositories. Each host has its
// own clone of one origin, as two machines would, and the only things faked
// are the agent and tmux: what is under test is whether the code on one host
// turns up on the other, which is a question for git and not for a stub.

const sharedOrigin = "example.com/acme/widget"

// host is one machine of a pair: a service, its clone, and where q keeps things.
type host struct {
	t     *testing.T
	svc   *Service
	clone string
	git   string
}

// noTmux stands in for the terminal a provisioner asks about sessions.
type noTmux struct{}

func (noTmux) HasSession(context.Context, string) bool   { return false }
func (noTmux) KillSession(context.Context, string) error { return nil }

// fixedLocator knows one repository and where this host keeps it.
type fixedLocator struct{ path string }

func (l fixedLocator) OriginURL(context.Context, string) (string, error) { return sharedOrigin, nil }

func (l fixedLocator) Locate(_ context.Context, url string) (string, bool) {
	return l.path, url == sharedOrigin
}

// provisioningLauncher launches a mission by really provisioning its worktrees
// and stopping short of starting an agent.
type provisioningLauncher struct{ workspace *git.Provisioner }

func (l provisioningLauncher) Launch(
	ctx context.Context,
	operation mission.Operation,
	ms mission.Mission,
) (mission.Mission, error) {
	repos, err := mission.MissionRepos(operation, ms)
	if err != nil {
		return ms, err
	}

	ms.LaunchRepos, ms.LaunchReposFrozen = repos, true
	operation.Repos = repos

	provisioned, err := l.workspace.Prepare(ctx, operation, &ms)
	if err != nil {
		return ms, err
	}

	started := time.Now()
	ms.Work = provisioned.Work
	ms.StartedAt = &started
	ms.Status = mission.StatusActive
	ms.AgentState = mission.AgentBusy

	return ms, nil
}

// gitIn runs a real git and returns its trimmed output.
func gitIn(t *testing.T, dir string, args ...string) string {
	t.Helper()

	out, err := exec.Command("git", append([]string{"-C", dir}, args...)...).CombinedOutput()
	if err != nil {
		t.Fatalf("git %v in %s: %v\n%s", args, dir, err, out)
	}

	return strings.TrimSpace(string(out))
}

// newHost builds one machine with a clone of origin.
func newHost(t *testing.T, root, name, origin string) *host {
	t.Helper()

	bin, err := exec.LookPath("git")
	if err != nil {
		t.Skip("git is not installed")
	}

	clone := filepath.Join(root, name, "dev", "widget")
	if err := os.MkdirAll(filepath.Dir(clone), 0o755); err != nil {
		t.Fatal(err)
	}

	gitIn(t, root, "clone", "-q", origin, clone)
	gitIn(t, clone, "config", "user.name", name)
	gitIn(t, clone, "config", "user.email", name+"@example.com")

	dirs := paths.Dirs{Data: filepath.Join(root, name, "data"), State: filepath.Join(root, name, "state")}

	store, err := mission.Open(dirs)
	if err != nil {
		t.Fatalf("mission.Open: %v", err)
	}

	hub := NewHub()
	t.Cleanup(hub.Close)

	client := git.New(bin, runner.OS{})
	workspace := git.NewProvisioner(dirs, client, noTmux{}, git.WithBranchPrefix(name))

	svc := NewService(store, hub, dirs,
		WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))),
		WithHostName(name),
		WithLocator(fixedLocator{path: clone}),
		WithWorktrees(git.NewMirrors(client, workspace)),
		WithLauncher(provisioningLauncher{workspace: workspace}),
		WithReclaimer(workspace),
	)

	return &host{t: t, svc: svc, clone: clone, git: bin}
}

// realPair returns a primary and a secondary, each with its own clone.
func realPair(t *testing.T) (laptop, mini *host) {
	t.Helper()

	// Isolated from the developer's git configuration, and resolved because the
	// macOS temp directory is a symlink that git reports the target of.
	t.Setenv("GIT_CONFIG_GLOBAL", os.DevNull)
	t.Setenv("GIT_CONFIG_SYSTEM", os.DevNull)

	root, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}

	seed := filepath.Join(root, "seed")
	origin := filepath.Join(root, "origin.git")

	if err := os.MkdirAll(seed, 0o755); err != nil {
		t.Fatal(err)
	}

	gitIn(t, seed, "init", "-q", "-b", "main")
	gitIn(t, seed, "config", "user.name", "seed")
	gitIn(t, seed, "config", "user.email", "seed@example.com")
	writeFile(t, seed, "README.md", "widget\n")
	writeFile(t, seed, ".gitignore", "build/\n")
	gitIn(t, seed, "add", "-A")
	gitIn(t, seed, "commit", "-q", "-m", "first")
	gitIn(t, root, "clone", "-q", "--bare", seed, origin)

	laptop, mini = newHost(t, root, "laptop", origin), newHost(t, root, "mini", origin)
	laptop.svc.apply(WithRemote(&wire{peer: mini.svc}))

	return laptop, mini
}

func writeFile(t *testing.T, dir, name, content string) {
	t.Helper()

	path := filepath.Join(dir, name)
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}

	if err := os.WriteFile(path, []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
}

func readFile(t *testing.T, dir, name string) string {
	t.Helper()

	data, err := os.ReadFile(filepath.Join(dir, name))
	if err != nil {
		return "<missing>"
	}

	return string(data)
}

// launchOn briefs a mission on h and launches it there.
func (h *host) launch(name string) mission.Mission {
	h.t.Helper()

	operation, err := h.svc.CreateOperation(api.CreateOperationRequest{
		Name:  "Widgets",
		Repos: []mission.Repo{{Name: "widget", Path: h.clone}},
	})
	if err != nil {
		h.t.Fatalf("CreateOperation: %v", err)
	}

	ms, err := h.svc.CreateMission(api.CreateMissionRequest{OperationID: operation.ID, Name: name, Prompt: "do it"})
	if err != nil {
		h.t.Fatalf("CreateMission: %v", err)
	}

	launched, err := h.svc.Start(h.t.Context(), ms.ID)
	if err != nil {
		h.t.Fatalf("Start: %v", err)
	}

	return launched
}

// worktree returns where h keeps a mission's widget worktree, or "" if it has
// none.
func (h *host) worktree(id mission.MissionID) string {
	h.t.Helper()

	ms, ok := h.svc.Snapshot().Mission(id)
	if !ok {
		return ""
	}

	work, ok := liveWork(ms, "widget")
	if !ok {
		return ""
	}

	return work.WorktreePath
}

func (h *host) mission(id mission.MissionID) mission.Mission {
	h.t.Helper()

	ms, _ := h.svc.Snapshot().Mission(id)

	return ms
}

// The whole point of pairing: an agent works on one machine, and its work —
// committed and not — is sitting in a real worktree on the other.
func TestAMissionsWorkIsMirroredOnThePeer(t *testing.T) {
	laptop, mini := realPair(t)
	ms := mini.launch("build the widget")

	source := mini.worktree(ms.ID)
	writeFile(t, source, "committed.go", "package widget\n")
	gitIn(t, source, "add", "-A")
	gitIn(t, source, "commit", "-q", "-m", "agent commit")
	writeFile(t, source, "README.md", "widget, improved\n")
	writeFile(t, source, "draft.go", "package widget // wip\n")
	writeFile(t, source, "build/out.bin", "ignored\n")

	exchange(t, laptop.svc)

	mirror := laptop.worktree(ms.ID)
	if mirror == "" {
		t.Fatalf("the laptop has no worktree: %+v", laptop.mission(ms.ID))
	}

	if got, want := gitIn(t, mirror, "rev-parse", "HEAD"), gitIn(t, source, "rev-parse", "HEAD"); got != want {
		t.Errorf("mirror tip = %s, want the agent's %s", got, want)
	}

	if got, want := gitIn(t, mirror, "status", "--porcelain=v1"), gitIn(t, source, "status", "--porcelain=v1"); got != want {
		t.Errorf("mirror status:\n%s\nwant the source's:\n%s", got, want)
	}

	if got := readFile(t, mirror, "draft.go"); got != "package widget // wip\n" {
		t.Errorf("uncommitted file in the mirror = %q", got)
	}

	if got := readFile(t, mirror, "build/out.bin"); got != "<missing>" {
		t.Errorf("an ignored file crossed over: %q", got)
	}

	// The mirror is a worktree of the laptop's own clone, on a branch of its own.
	if branch := gitIn(t, mirror, "rev-parse", "--abbrev-ref", "HEAD"); branch != "laptop/build-the-widget" {
		t.Errorf("mirror branch = %q", branch)
	}

	if laptop.mission(ms.ID).Lease.Holder != mini.svc.self {
		t.Error("mirroring a mission must not take it")
	}
}

func TestTheMirrorFollowsAsTheAgentKeepsWorking(t *testing.T) {
	laptop, mini := realPair(t)
	ms := mini.launch("iterate")

	source := mini.worktree(ms.ID)
	writeFile(t, source, "scratch.txt", "first attempt\n")
	exchange(t, laptop.svc)

	mirror := laptop.worktree(ms.ID)

	// The agent changes its mind: a new file gone, another edited, one commit.
	if err := os.Remove(filepath.Join(source, "scratch.txt")); err != nil {
		t.Fatal(err)
	}

	writeFile(t, source, "final.txt", "second attempt\n")
	gitIn(t, source, "add", "-A")
	gitIn(t, source, "commit", "-q", "-m", "settle on the second attempt")
	writeFile(t, source, "README.md", "widget, documented\n")

	exchange(t, laptop.svc)

	if got := readFile(t, mirror, "scratch.txt"); got != "<missing>" {
		t.Errorf("a file the agent deleted is still in the mirror: %q", got)
	}

	if got, want := gitIn(t, mirror, "status", "--porcelain=v1"), gitIn(t, source, "status", "--porcelain=v1"); got != want {
		t.Errorf("mirror status:\n%s\nwant:\n%s", got, want)
	}

	if got, want := gitIn(t, mirror, "log", "-1", "--format=%s"), "settle on the second attempt"; got != want {
		t.Errorf("mirror tip = %q, want %q", got, want)
	}
}

// Nothing changing must cost nothing: no git transfer, no state write.
func TestAnIdleMirrorSettlesAndStaysSettled(t *testing.T) {
	laptop, mini := realPair(t)
	ms := mini.launch("quiet")

	writeFile(t, mini.worktree(ms.ID), "note.txt", "one edit\n")

	exchange(t, laptop.svc)
	exchange(t, laptop.svc)
	exchange(t, laptop.svc)

	before := []time.Time{laptop.svc.Snapshot().UpdatedAt, mini.svc.Snapshot().UpdatedAt}

	exchange(t, laptop.svc)

	after := []time.Time{laptop.svc.Snapshot().UpdatedAt, mini.svc.Snapshot().UpdatedAt}

	if !before[0].Equal(after[0]) || !before[1].Equal(after[1]) {
		t.Errorf("an exchange with nothing to say rewrote state: %v -> %v", before, after)
	}

	base := laptop.mission(ms.ID).Work["widget"].SyncBase
	if base == "" || base != mini.mission(ms.ID).Work["widget"].SyncBase {
		t.Errorf("agreed snapshot: laptop %q mini %q, want the same one on both",
			base, mini.mission(ms.ID).Work["widget"].SyncBase)
	}
}

// Reviewing is editing. A fix made in the mirror while the agent is idle goes
// back to the machine the agent runs on.
func TestAnEditInTheMirrorReachesAnIdleAgent(t *testing.T) {
	laptop, mini := realPair(t)
	ms := mini.launch("review me")

	source := mini.worktree(ms.ID)
	writeFile(t, source, "widget.go", "package widget\n\nfunc New() {}\n")

	// The agent finishes its turn.
	if _, err := mini.svc.SetStatus(ms.ID, mission.StatusDebrief); err != nil {
		t.Fatal(err)
	}

	exchange(t, laptop.svc)
	exchange(t, laptop.svc)

	mirror := laptop.worktree(ms.ID)
	writeFile(t, mirror, "widget.go", "package widget\n\n// New makes a widget.\nfunc New() {}\n")

	exchange(t, laptop.svc)

	if got := readFile(t, source, "widget.go"); !strings.Contains(got, "// New makes a widget.") {
		t.Errorf("the agent's worktree did not receive the human's edit:\n%s", got)
	}

	if laptop.mission(ms.ID).HasLocalBadge(mission.BadgeLocalEdits) {
		t.Error("the mirror is still marked as edited after its edit was taken")
	}
}

// An agent mid-turn must not have files change under it. The edit is kept on
// the laptop and shown as pending rather than pushed into a running agent.
func TestAnEditInTheMirrorWaitsForABusyAgent(t *testing.T) {
	laptop, mini := realPair(t)
	ms := mini.launch("busy")

	source := mini.worktree(ms.ID)
	writeFile(t, source, "widget.go", "package widget\n")

	exchange(t, laptop.svc)
	exchange(t, laptop.svc)

	mirror := laptop.worktree(ms.ID)
	writeFile(t, mirror, "widget.go", "package widget // reviewed\n")

	exchange(t, laptop.svc)

	if got := readFile(t, source, "widget.go"); got != "package widget\n" {
		t.Errorf("a busy agent's file was overwritten: %q", got)
	}

	if got := readFile(t, mirror, "widget.go"); got != "package widget // reviewed\n" {
		t.Errorf("the human's edit was lost: %q", got)
	}

	if !laptop.mission(ms.ID).HasLocalBadge(mission.BadgeLocalEdits) {
		t.Error("the card does not say the mirror has edits waiting")
	}
}

// Neither side may be overwritten when both have changed.
func TestEditsOnBothSidesAreBothKept(t *testing.T) {
	laptop, mini := realPair(t)
	ms := mini.launch("contended")

	source := mini.worktree(ms.ID)
	writeFile(t, source, "a.txt", "from the agent\n")

	exchange(t, laptop.svc)
	exchange(t, laptop.svc)

	mirror := laptop.worktree(ms.ID)
	writeFile(t, mirror, "b.txt", "from the human\n")
	writeFile(t, source, "a.txt", "from the agent, again\n")

	exchange(t, laptop.svc)

	if got := readFile(t, mirror, "b.txt"); got != "from the human\n" {
		t.Errorf("the human's file = %q", got)
	}

	if got := readFile(t, source, "a.txt"); got != "from the agent, again\n" {
		t.Errorf("the agent's file = %q", got)
	}
}

// Closing a mission on the machine running it removes the other's mirror too,
// along with the hidden refs the two exchanged.
func TestClosingAMissionReclaimsItsMirror(t *testing.T) {
	laptop, mini := realPair(t)
	ms := mini.launch("finish me")

	writeFile(t, mini.worktree(ms.ID), "done.txt", "done\n")
	exchange(t, laptop.svc)

	mirror := laptop.worktree(ms.ID)
	if mirror == "" {
		t.Fatal("no mirror to reclaim")
	}

	if _, _, err := mini.svc.FinishMission(t.Context(), ms.ID, true); err != nil {
		t.Fatalf("FinishMission: %v", err)
	}

	exchange(t, laptop.svc)

	if _, err := os.Stat(mirror); !os.IsNotExist(err) {
		t.Errorf("the mirror worktree is still there: %v", err)
	}

	got := laptop.mission(ms.ID)
	if got.Status != mission.StatusClosed || got.MissionDir != "" || len(got.Work) != 0 {
		t.Errorf("laptop record = %s dir %q work %v, want it closed and cleared", got.Status, got.MissionDir, got.Work)
	}

	for _, h := range []*host{laptop, mini} {
		if refs := gitIn(t, h.clone, "for-each-ref", "refs/q"); refs != "" {
			t.Errorf("hidden refs left behind in %s:\n%s", h.clone, refs)
		}
	}
}

// Deleting from the machine that is not running the mission stops it on the
// one that is.
func TestDeletingAMissionReclaimsItOnBothHosts(t *testing.T) {
	laptop, mini := realPair(t)
	ms := mini.launch("delete me")

	source := mini.worktree(ms.ID)
	writeFile(t, source, "wip.txt", "wip\n")
	exchange(t, laptop.svc)

	mirror := laptop.worktree(ms.ID)

	if _, err := laptop.svc.DeleteMissionAndReclaim(t.Context(), ms.ID, true); err != nil {
		t.Fatalf("DeleteMissionAndReclaim: %v", err)
	}

	exchange(t, laptop.svc)

	for name, path := range map[string]string{"mirror": mirror, "source": source} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Errorf("the %s worktree is still there: %v", name, err)
		}
	}

	if _, ok := mini.svc.Snapshot().Mission(ms.ID); ok {
		t.Error("the mission is still on the machine that was running it")
	}
}
