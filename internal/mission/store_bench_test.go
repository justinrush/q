package mission_test

import (
	"encoding/json"
	"fmt"
	"testing"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/mission/missiontest"
)

// boards are the shapes every store benchmark runs against. "real" uses the
// state named by Q_BENCH_STATE and is skipped without it.
var boards = []struct {
	name  string
	shape missiontest.Shape
}{
	{"small", missiontest.Small},
	{"work", missiontest.Work},
}

func eachStore(b *testing.B, run func(b *testing.B, store *mission.Store, live mission.MissionID)) {
	for _, board := range boards {
		b.Run(board.name, func(b *testing.B) {
			store, _ := missiontest.Store(b, missiontest.Board(board.shape))
			b.ResetTimer()
			run(b, store, missiontest.LiveID(0))
		})
	}

	b.Run("real", func(b *testing.B) {
		store, _ := missiontest.RealStore(b)
		live := firstLive(b, store.Snapshot())
		b.ResetTimer()
		run(b, store, live)
	})
}

func firstLive(b *testing.B, snap mission.Snapshot) mission.MissionID {
	for _, m := range snap.Missions {
		if !m.Status.Terminal() {
			return m.ID
		}
	}

	b.Skip("no live mission in the real state")

	return ""
}

// BenchmarkStoreMutate is one store write: what a hook, a runtime reading or
// a usage reading costs.
func BenchmarkStoreMutate(b *testing.B) {
	b.Run("sync", func(b *testing.B) {
		eachStore(b, func(b *testing.B, store *mission.Store, live mission.MissionID) {
			b.ReportAllocs()

			for i := range b.N {
				if err := store.Mutate("bench", func(s *mission.Snapshot) error {
					m, _ := s.Mission(live)
					m.WaitingFor = fmt.Sprint(i)
					s.PutMission(m)

					return nil
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	})

	b.Run("deferred", func(b *testing.B) {
		eachStore(b, func(b *testing.B, store *mission.Store, live mission.MissionID) {
			b.ReportAllocs()

			for i := range b.N {
				if err := store.MutateDeferred("bench", func(s *mission.Snapshot) (bool, error) {
					m, _ := s.Mission(live)
					m.WaitingFor = fmt.Sprint(i)
					s.PutMission(m)

					return false, nil
				}); err != nil {
					b.Fatal(err)
				}
			}
		})
	})

	b.Run("noop", func(b *testing.B) {
		eachStore(b, func(b *testing.B, store *mission.Store, _ mission.MissionID) {
			b.ReportAllocs()

			for range b.N {
				if err := store.Mutate("bench", func(*mission.Snapshot) error { return nil }); err != nil {
					b.Fatal(err)
				}
			}
		})
	})
}

// BenchmarkStoreSnapshot is the copy every reader takes: the runtime poll, the
// reconciler, the scheduler and every API read.
func BenchmarkStoreSnapshot(b *testing.B) {
	eachStore(b, func(b *testing.B, store *mission.Store, _ mission.MissionID) {
		b.ReportAllocs()

		for range b.N {
			_ = store.Snapshot()
		}
	})
}

// BenchmarkStoreOpen is loading the state, which a daemon does before it
// answers anything.
func BenchmarkStoreOpen(b *testing.B) {
	for _, board := range boards {
		b.Run(board.name, func(b *testing.B) {
			store, dirs := missiontest.Store(b, missiontest.Board(board.shape))
			if err := store.Close(); err != nil {
				b.Fatal(err)
			}

			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				s, err := mission.Open(dirs)
				if err != nil {
					b.Fatal(err)
				}

				_ = s.Close()
			}
		})
	}

	b.Run("real", func(b *testing.B) {
		dirs := missiontest.RealDirs(b)

		b.ReportAllocs()
		b.ResetTimer()

		for range b.N {
			s, err := mission.Open(dirs)
			if err != nil {
				b.Fatal(err)
			}

			_ = s.Close()
		}
	})
}

// BenchmarkSnapshotEncode is what any whole-board read costs on the wire:
// GET /v1/state, the first SSE frame and every q mission list.
func BenchmarkSnapshotEncode(b *testing.B) {
	eachStore(b, func(b *testing.B, store *mission.Store, _ mission.MissionID) {
		snap := store.Snapshot()

		b.ReportAllocs()
		b.ResetTimer()

		var n int

		for range b.N {
			data, err := json.Marshal(snap)
			if err != nil {
				b.Fatal(err)
			}

			n = len(data)
		}

		b.ReportMetric(float64(n), "bytes/snapshot")
	})
}

// BenchmarkSnapshotPayload is the board a sync round sends to the peer.
func BenchmarkSnapshotPayload(b *testing.B) {
	eachStore(b, func(b *testing.B, store *mission.Store, _ mission.MissionID) {
		snap := store.Snapshot()

		b.ReportAllocs()
		b.ResetTimer()

		for range b.N {
			if _, err := json.Marshal(snap.Payload()); err != nil {
				b.Fatal(err)
			}
		}
	})
}
