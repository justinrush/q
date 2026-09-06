package agy

import (
	"context"
	"errors"
	"testing"

	"github.com/justinrush/q/internal/runner"
)

func TestModelProbe(t *testing.T) {
	run := runner.NewFake()
	run.StrictMode = true
	run.Expect("/bin/agy models", "Fetching available models...\ngemini-3.8-flash-high\tGemini 3.8 Flash (High)\nclaude-sonnet-4-6\tClaude Sonnet 4.6 (Thinking)\n")
	set, err := NewProber("/bin/agy", run, []string{"fallback"}).Probe(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(set.Options) != 2 || set.Default != "" || set.ProbedAt.IsZero() || !set.ValidEffort("gemini-3.8-flash-high", "high") {
		t.Fatalf("%+v", set)
	}
	if set.Options[1].Label != "Claude Sonnet 4.6 (Thinking)" {
		t.Fatal(set.Options)
	}
}

func TestProbeFailuresAndFallback(t *testing.T) {
	for _, output := range []string{"", "Fetching available models...", "login required", "--bad\tBad", "model only\tLabel"} {
		run := runner.NewFake().Expect("/bin/agy models", output)
		if _, err := NewProber("/bin/agy", run, nil).Probe(context.Background()); err == nil {
			t.Fatalf("accepted %q", output)
		}
	}
	run := runner.NewFake().ExpectError("/bin/agy models", errors.New("offline"))
	set, err := NewProber("/bin/agy", run, []string{"custom-model", "custom-model"}).Probe(context.Background())
	if err != nil || set.Err == "" || len(set.Options) != 1 || set.Options[0].Value != "custom-model" {
		t.Fatalf("%+v, %v", set, err)
	}
	if _, err := NewProber("/bin/agy", run, nil).Probe(context.Background()); err == nil {
		t.Fatal("failure hidden")
	}
}
