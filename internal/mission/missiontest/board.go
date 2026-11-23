// Package missiontest builds boards for benchmarks across packages.
//
// The default shape is taken from a real, heavily used work machine
// (2026-11-23): a small live board whose state files were nonetheless megabytes,
// because a few Codex missions carried thousands of per-response usage records
// and cross-tool handoffs carried whole conversations.
package missiontest

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
)

// Shape sizes a board.
type Shape struct {
	// Hot is the number of missions not yet closed.
	Hot int
	// Closed is the number of closed missions.
	Closed int
	// Live is how many of the hot missions are running (active).
	Live int
	// CodexEvery makes every Nth hot mission a Codex mission with
	// CodexResponses usage records; ClosedCodexEvery and ClosedCodexResponses
	// do the same for closed ones.
	CodexEvery, CodexResponses             int
	ClosedCodexEvery, ClosedCodexResponses int
	// HandoffEvery gives every Nth mission a cross-tool handoff conversation.
	HandoffEvery int
	// HandoffBytes is the size of each handoff conversation.
	HandoffBytes int
	// PromptBytes is the size of each brief.
	PromptBytes int
}

// Work is the shape of the work machine's board: 22 hot missions and 224
// closed ones, about 2.5 MB and 3.5 MB on disk.
var Work = Shape{
	Hot: 22, Closed: 224, Live: 3,
	CodexEvery: 3, CodexResponses: 900,
	ClosedCodexEvery: 12, ClosedCodexResponses: 500,
	HandoffEvery: 20, HandoffBytes: 100_000,
	PromptBytes: 800,
}

// Small is a board with no history to speak of.
var Small = Shape{Hot: 8, Closed: 10, Live: 2, CodexEvery: 4, CodexResponses: 50, ClosedCodexEvery: 4, ClosedCodexResponses: 50, PromptBytes: 400}

// OperationID is the operation every mission in a built board belongs to.
const OperationID mission.OperationID = "op_aabbccddeeff"

const claudeModel = "claude-opus-5-5"

var repo = mission.Repo{Name: "weave", Path: "/dev/weave", CommonDir: "/dev/weave/.git", DefaultBranch: "main"}

// LiveID returns the ID of the i'th live mission in a built board.
func LiveID(i int) mission.MissionID { return mission.MissionID(fmt.Sprintf("ms_live%08d", i)) }

// Board builds a snapshot of the given shape. It is deterministic.
func Board(shape Shape) mission.Snapshot {
	base := time.Date(2026, 11, 1, 9, 0, 0, 0, time.UTC)
	snap := mission.Snapshot{
		Operations: []mission.Operation{{
			ID: OperationID, Name: "Operation", Slug: "operation", Summary: "the work",
			Repos: []mission.Repo{repo},
		}},
	}

	n := 0
	add := func(id mission.MissionID, status mission.Status, codex int) {
		m := Mission(shape, id, status, n, base)
		if codex > 0 {
			m.Tool, m.Model = mission.ToolCodex, "gpt-6.2"
			m.CodexUsage = codexUsage(m.TranscriptPath, codex, m.CreatedAt)
		}

		snap.Missions = append(snap.Missions, m)
		n++
	}

	every := func(i, n, responses int) int {
		if n > 0 && i%n == 0 {
			return responses
		}

		return 0
	}

	for i := range shape.Live {
		add(LiveID(i), mission.StatusActive, every(i, shape.CodexEvery, shape.CodexResponses))
	}

	for i := range max(shape.Hot-shape.Live, 0) {
		status := mission.StatusDebrief
		if i%7 == 6 {
			status = mission.StatusBriefing
		}

		add(mission.MissionID(fmt.Sprintf("ms_hot%09d", i)), status, every(i+shape.Live, shape.CodexEvery, shape.CodexResponses))
	}

	for i := range shape.Closed {
		add(mission.MissionID(fmt.Sprintf("ms_cls%09d", i)), mission.StatusClosed, every(i, shape.ClosedCodexEvery, shape.ClosedCodexResponses))
	}

	return snap
}

// Mission builds the n'th mission of a board.
func Mission(shape Shape, id mission.MissionID, status mission.Status, n int, base time.Time) mission.Mission {
	created := base.Add(time.Duration(n) * time.Minute)
	m := mission.Mission{
		ID: id, OperationID: OperationID, Name: fmt.Sprintf("Mission %d", n), Slug: fmt.Sprintf("mission-%d", n),
		Tool: mission.ToolClaude, Model: claudeModel, Status: status, Order: n,
		Prompt:      strings.Repeat("Make the thing better. ", max(shape.PromptBytes/23, 1)),
		LastMessage: "I changed the store so it no longer rewrites the closed missions on every mutation, and the tests pass.",
		MissionDir:  "/missions/" + string(id), TmuxSession: "q-" + string(id), AgentPaneID: "%" + fmt.Sprint(n),
		AgentSessionID: fmt.Sprintf("sess-%d", n), TranscriptPath: fmt.Sprintf("/transcripts/%s.jsonl", id),
		LaunchRepos: []mission.Repo{repo},
		Work: map[string]mission.RepoWork{repo.Name: {
			RepoName: repo.Name, WorktreePath: "/missions/" + string(id) + "/weave", Branch: "q/" + string(id),
			BaseRef: "refs/remotes/origin/main", BaseSHA: "0123456789abcdef0123456789abcdef01234567",
		}},
		CreatedAt: created, UpdatedAt: created.Add(time.Hour), LastEventAt: created.Add(time.Hour),
		Usage: usage(n),
	}

	if status == mission.StatusClosed {
		finished := created.Add(2 * time.Hour)
		m.FinishedAt = &finished
	}

	if shape.HandoffEvery > 0 && n%shape.HandoffEvery == 1 && shape.HandoffBytes > 0 {
		m.ToolHandoff = mission.ToolHandoff{
			State: "complete", ResumeState: "resumed",
			Plan:         strings.Repeat("Step: do the next part. ", 400),
			Conversation: strings.Repeat("user: please continue\nassistant: continuing with the work\n", shape.HandoffBytes/57),
		}
	}

	return m
}

func usage(n int) mission.Usage {
	tokens := mission.ModelTokens{Input: int64(1000 * n), Output: int64(300 * n), CacheRead: int64(9000 * n)}

	return mission.Usage{
		PerModel: map[string]mission.ModelTokens{claudeModel: tokens},
		USD:      float64(n) / 10,
		Breakdown: []mission.UsageSlice{
			{Tool: mission.ToolClaude, Model: claudeModel, Effort: "high", Phase: mission.PhaseImplement, Tokens: tokens, USD: float64(n) / 10},
		},
	}
}

func codexUsage(path string, responses int, at time.Time) mission.CodexUsage {
	out := make(map[string]mission.CodexResponse, responses)
	for i := range responses {
		out[fmt.Sprintf("resp_0147bf2bb5455be4016af7%026d", i)] = mission.CodexResponse{
			Effort: "high", Model: "gpt-6.2",
			Tokens: mission.ModelTokens{Input: int64(20000 + i), Output: int64(300 + i%50), CacheRead: int64(18000 + i)},
			Phase:  mission.PhaseImplement, At: at.Add(time.Duration(i) * time.Second),
			Turn: fmt.Sprintf("turn-%032d", i/40), TurnAt: at.Add(time.Duration(i/40) * time.Minute),
		}
	}

	return mission.CodexUsage{path: out}
}

// Store opens a store in a temporary directory holding the given board.
func Store(tb testing.TB, snap mission.Snapshot, opts ...mission.StoreOption) (*mission.Store, paths.Dirs) {
	tb.Helper()

	root := tb.TempDir()
	dirs := paths.Dirs{Data: filepath.Join(root, "data"), State: filepath.Join(root, "state"), Config: filepath.Join(root, "config")}

	store, err := mission.Open(dirs, opts...)
	if err != nil {
		tb.Fatal(err)
	}

	if err := store.Mutate("seed", func(s *mission.Snapshot) error {
		for _, op := range snap.Operations {
			s.PutOperation(op)
		}

		for _, m := range snap.Missions {
			s.PutMission(m)
		}

		return nil
	}); err != nil {
		tb.Fatal(err)
	}

	tb.Cleanup(func() { _ = store.Close() })

	return store, dirs
}

// StateEnv names a directory holding a real state.json (and state-closed.json)
// to benchmark against instead of a built board. Copy it from a machine whose
// performance you want to reproduce; it is copied again before each use, so
// the original is never written.
const StateEnv = "Q_BENCH_STATE"

// RealStore opens a copy of the state named by [StateEnv], skipping the
// benchmark when it is unset.
func RealStore(tb testing.TB, opts ...mission.StoreOption) (*mission.Store, paths.Dirs) {
	tb.Helper()

	dirs := RealDirs(tb)

	store, err := mission.Open(dirs, opts...)
	if err != nil {
		tb.Fatal(err)
	}

	tb.Cleanup(func() { _ = store.Close() })

	return store, dirs
}

// RealDirs copies the state named by [StateEnv] into temporary directories,
// skipping the benchmark when it is unset.
func RealDirs(tb testing.TB) paths.Dirs {
	tb.Helper()

	src := os.Getenv(StateEnv)
	if src == "" {
		tb.Skipf("%s is not set", StateEnv)
	}

	root := tb.TempDir()
	dirs := paths.Dirs{Data: filepath.Join(root, "data"), State: filepath.Join(root, "state"), Config: filepath.Join(root, "config")}

	if err := os.MkdirAll(dirs.Data, 0o700); err != nil {
		tb.Fatal(err)
	}

	for _, name := range []string{"state.json", "state-closed.json"} {
		data, err := os.ReadFile(filepath.Join(src, name))
		if os.IsNotExist(err) {
			continue
		}

		if err != nil {
			tb.Fatal(err)
		}

		if err := os.WriteFile(filepath.Join(dirs.Data, name), data, 0o600); err != nil {
			tb.Fatal(err)
		}
	}

	return dirs
}
