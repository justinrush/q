package usage

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/justinrush/q/internal/mission"
)

// filler is a tool result, which is most of what a real transcript holds.
var filler = `{"type":"user","message":{"content":[{"type":"tool_result","content":"` + strings.Repeat("output ", 300) + `"}]}}`

// growing writes a transcript of about size bytes with records every few
// lines, and returns a function that appends one more record.
func growing(b *testing.B, size int, record func(i int) string) (string, func()) {
	b.Helper()

	path := filepath.Join(b.TempDir(), "session.jsonl")

	var body strings.Builder

	i := 0
	for body.Len() < size {
		body.WriteString(filler)
		body.WriteString("\n")
		body.WriteString(record(i))
		body.WriteString("\n")
		i++
	}

	if err := os.WriteFile(path, []byte(body.String()), 0o600); err != nil {
		b.Fatal(err)
	}

	f, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		b.Fatal(err)
	}

	b.Cleanup(func() { _ = f.Close() })

	return path, func() {
		if _, err := f.WriteString(record(i) + "\n"); err != nil {
			b.Fatal(err)
		}
		i++
	}
}

var transcriptSizes = []int{1 << 20, 10 << 20}

// BenchmarkClaudeMeter is metering a running claude session whose transcript
// grew since the last reading, as the reconciler does every 15s and every
// turn end does.
func BenchmarkClaudeMeter(b *testing.B) {
	for _, size := range transcriptSizes {
		b.Run(fmt.Sprintf("%dMB", size>>20), func(b *testing.B) {
			path, grow := growing(b, size, func(i int) string {
				return assistant(fmt.Sprintf("req_%d", i), "model-a", 10, 20, 3000, 0, 0)
			})
			meter := NewClaude(testPricing)
			ms := mission.Mission{Tool: mission.ToolClaude, TranscriptPath: path}

			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				grow()

				if _, _, err := meter.Meter(ms); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkCodexMeter is the same for a codex rollout.
func BenchmarkCodexMeter(b *testing.B) {
	for _, size := range transcriptSizes {
		b.Run(fmt.Sprintf("%dMB", size>>20), func(b *testing.B) {
			path, grow := growing(b, size, func(i int) string {
				return tokenRecord("thr-1", fmt.Sprintf("resp_%d", i), 30000, 28000, 0, 300)
			})
			meter := NewCodex(codexPricing)
			ms := mission.Mission{Tool: mission.ToolCodex, Model: "gpt-test", TranscriptPath: path}

			b.ReportAllocs()
			b.ResetTimer()

			for range b.N {
				grow()

				metering, _, err := meter.Meter(ms)
				if err != nil {
					b.Fatal(err)
				}

				ms.CodexUsage = metering.CodexUsage
			}
		})
	}
}
