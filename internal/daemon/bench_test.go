package daemon

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/justinrush/q/internal/api"
	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/mission/missiontest"
	"github.com/justinrush/q/internal/paths"
	"github.com/justinrush/q/internal/terminal"
)

// benchBoards are the shapes the daemon benchmarks run against. "real" loads
// the state named by Q_BENCH_STATE and is skipped without it.
var benchBoards = []struct {
	name  string
	shape missiontest.Shape
}{
	{"small", missiontest.Small},
	{"work", missiontest.Work},
}

func eachService(b *testing.B, run func(b *testing.B, svc *Service), opts ...Option) {
	for _, board := range benchBoards {
		b.Run(board.name, func(b *testing.B) {
			store, dirs := missiontest.Store(b, missiontest.Board(board.shape))
			svc := benchService(b, store, dirs, opts...)
			b.ResetTimer()
			run(b, svc)
		})
	}

	b.Run("real", func(b *testing.B) {
		store, dirs := missiontest.RealStore(b)
		svc := benchService(b, store, dirs, opts...)
		b.ResetTimer()
		run(b, svc)
	})
}

func benchService(b *testing.B, store *mission.Store, dirs paths.Dirs, opts ...Option) *Service {
	b.Helper()

	hub := NewHub()
	b.Cleanup(hub.Close)

	opts = append([]Option{WithLogger(slog.New(slog.NewTextHandler(io.Discard, nil))), WithClock(time.Now)}, opts...)

	return NewService(store, hub, dirs, opts...)
}

// subscribe holds a board open for the benchmark, so broadcasts are encoded
// as they are when someone is watching, and drains it so it is never dropped.
func subscribe(b *testing.B, svc *Service) {
	b.Helper()

	id, frames := svc.Hub().Subscribe()
	done := make(chan struct{})

	go func() {
		defer close(done)

		for range frames {
		}
	}()

	b.Cleanup(func() {
		svc.Hub().Unsubscribe(id)
		<-done
	})
}

// firstHot is a mission the runtime poll and the reconciler both read.
func firstHot(b *testing.B, svc *Service) mission.Mission {
	b.Helper()

	for _, m := range svc.store.Snapshot().Missions {
		if !m.Status.Terminal() && m.Status != mission.StatusBriefing {
			return m
		}
	}

	b.Skip("no running mission on the board")

	return mission.Mission{}
}

// idleRuntime answers every read with a thread that is not doing anything,
// which is what codex reports for a finished mission sitting in debrief: the
// poll's commonest case by far.
type idleRuntime struct{}

func (idleRuntime) Read(_ context.Context, ms mission.Mission) (mission.Reading, bool, error) {
	return mission.Reading{SessionID: ms.AgentSessionID, Activity: mission.ActivityIdle, HookEpoch: ms.HookEpoch}, true, nil
}

func (idleRuntime) Close() error { return nil }

// BenchmarkPollRuntimes is one tick of the loop that runs every second for as
// long as the daemon is up.
func BenchmarkPollRuntimes(b *testing.B) {
	for _, watched := range []string{"unwatched", "watched"} {
		b.Run(watched, func(b *testing.B) {
			eachService(b, func(b *testing.B, svc *Service) {
				if watched == "watched" {
					subscribe(b, svc)
				}

				ctx := context.Background()
				svc.pollRuntimes(ctx) // settle the first readings

				b.ReportAllocs()
				b.ResetTimer()

				for range b.N {
					svc.pollRuntimes(ctx)
				}
			}, WithRuntime(mission.ToolCodex, idleRuntime{}), WithRuntime(mission.ToolClaude, idleRuntime{}))
		})
	}
}

// BenchmarkPollRuntimesChanging is a tick in which one mission's reading
// changes, so the store writes and the board is told.
func BenchmarkPollRuntimesChanging(b *testing.B) {
	eachService(b, func(b *testing.B, svc *Service) {
		subscribe(b, svc)

		ms := firstHot(b, svc)
		activities := []mission.Activity{mission.ActivityBusy, mission.ActivityIdle}

		b.ReportAllocs()
		b.ResetTimer()

		for i := range b.N {
			svc.applyRuntimeReading(ms.ID, mission.Reading{
				SessionID: ms.AgentSessionID, HookEpoch: ms.HookEpoch, Activity: activities[i%2],
			})
		}
	})
}

// BenchmarkReconcile is one pass of the reconciler, which runs every 15s, with
// every session alive and nothing to correct.
func BenchmarkReconcile(b *testing.B) {
	eachService(b, func(b *testing.B, svc *Service) {
		ctx := context.Background()
		svc.Reconcile(ctx)

		b.ReportAllocs()
		b.ResetTimer()

		for range b.N {
			svc.Reconcile(ctx)
		}
	}, WithProbe(stubProbe{pane: terminal.PaneInfo{ID: "%1", Command: "codex"}}), WithMeter(&fakeMeter{}))
}

// BenchmarkApplyHook is the daemon's side of an agent hook. PreToolUse is
// written behind; Stop is written at once.
func BenchmarkApplyHook(b *testing.B) {
	for _, event := range []string{"PreToolUse", "Stop"} {
		b.Run(event, func(b *testing.B) {
			eachService(b, func(b *testing.B, svc *Service) {
				subscribe(b, svc)

				ms := firstHot(b, svc)
				payload, _ := json.Marshal(map[string]any{
					"session_id": ms.AgentSessionID, "transcript_path": ms.TranscriptPath, "tool_name": "Bash",
				})

				b.ReportAllocs()
				b.ResetTimer()

				for range b.N {
					svc.ApplyHook(api.HookRequest{
						Tool: ms.Tool, Event: event, MissionID: ms.ID, HookEpoch: ms.HookEpoch, Payload: payload,
					})
				}
			}, WithMeter(&fakeMeter{}))
		})
	}
}

// BenchmarkMissionFrame is what the board is sent each time one mission
// changes.
func BenchmarkMissionFrame(b *testing.B) {
	eachService(b, func(b *testing.B, svc *Service) {
		var largest mission.Mission

		for _, m := range svc.store.Snapshot().Missions {
			if !m.Status.Terminal() && len(m.CodexUsage) > 0 {
				largest = m
			}
		}

		if largest.ID == "" {
			largest = firstHot(b, svc)
		}

		b.ReportAllocs()
		b.ResetTimer()

		var n int

		for range b.N {
			frame, err := encodeFrame(api.EventMission, largest)
			if err != nil {
				b.Fatal(err)
			}

			n = len(frame)
		}

		b.ReportMetric(float64(n), "bytes/frame")
	})
}

// BenchmarkAPIRead is the board's reads: the first frame every board
// receives, a full refresh, and the health check every q command makes.
func BenchmarkAPIRead(b *testing.B) {
	for _, path := range []string{"/v1/state", "/v1/health"} {
		b.Run(path[4:], func(b *testing.B) {
			eachService(b, func(b *testing.B, svc *Service) {
				srv := NewServer(Config{
					Service: svc, Hub: svc.Hub(), Queue: newHookQueue(), Dirs: svc.dirs,
					Token: testToken, Version: "test", Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				})
				// The handlers are called past the guard, whose cost is fixed.
				handler := map[string]http.HandlerFunc{"/v1/state": srv.handleState, "/v1/health": srv.handleHealth}[path]

				b.ReportAllocs()
				b.ResetTimer()

				var n int

				for range b.N {
					req := httptest.NewRequest(http.MethodGet, path, nil)
					rec := httptest.NewRecorder()
					handler.ServeHTTP(rec, req)

					if rec.Code != http.StatusOK {
						b.Fatalf("GET %s: %d %s", path, rec.Code, rec.Body)
					}

					n = rec.Body.Len()
				}

				b.ReportMetric(float64(n), "bytes/response")
			})
		})
	}

	b.Run("first-frame", func(b *testing.B) {
		eachService(b, func(b *testing.B, svc *Service) {
			b.ReportAllocs()

			for range b.N {
				if _, err := encodeFrame(api.EventSnapshot, svc.Snapshot()); err != nil {
					b.Fatal(err)
				}
			}
		})
	})
}
