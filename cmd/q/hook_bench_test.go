package main

import (
	"context"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/paths"
)

// BenchmarkHookDeliver is a hook's trip to the daemon, from reading the
// handle to the daemon's answer, against a daemon that answers at once. It
// leaves out process start, which scripts/bench-exec.sh measures.
func BenchmarkHookDeliver(b *testing.B) {
	var requests int

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++

		if r.URL.Path == "/v1/health" {
			_, _ = w.Write([]byte(`{"version":"bench"}`))

			return
		}

		w.WriteHeader(http.StatusNoContent)
	}))
	b.Cleanup(srv.Close)

	root := b.TempDir()
	dirs := paths.Dirs{Data: filepath.Join(root, "data"), State: filepath.Join(root, "state")}

	if err := os.MkdirAll(dirs.State, 0o700); err != nil {
		b.Fatal(err)
	}

	if err := api.WriteHandle(dirs.DaemonFile(), api.Handle{
		PID: os.Getpid(), Addr: strings.TrimPrefix(srv.URL, "http://"), Token: "bench",
	}); err != nil {
		b.Fatal(err)
	}

	req := api.HookRequest{
		Tool: mission.ToolClaude, Event: "PreToolUse", MissionID: "ms_000000000000",
		Payload: []byte(`{"session_id":"s","tool_name":"Bash","tool_input":{"command":"ls"}}`),
	}

	b.ReportAllocs()
	b.ResetTimer()

	for range b.N {
		deliver(context.Background(), dirs, req)
	}

	b.StopTimer()

	if spooled, _ := os.ReadDir(dirs.SpoolDir()); len(spooled) > 0 {
		b.Fatalf("%d hooks were spooled instead of delivered", len(spooled))
	}

	b.ReportMetric(float64(requests)/float64(b.N), "requests/hook")
}
