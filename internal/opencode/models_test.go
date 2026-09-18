package opencode

import (
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/justinrush/q/internal/runner"
)

const sampleVerboseOutput = `opencode/big-pickle
{
  "id": "big-pickle",
  "providerID": "opencode",
  "name": "Big Pickle",
  "capabilities": {
    "reasoning": false
  }
}
opencode/claude-sonnet-4-5
{
  "id": "claude-sonnet-4-5",
  "providerID": "opencode",
  "name": "Claude Sonnet 4.5",
  "variants": {
    "max": {
      "thinking": { "budgetTokens": 32000 }
    },
    "high": {
      "thinking": { "budgetTokens": 16000 }
    }
  }
}
opencode/claude-fable-5
{
  "id": "claude-fable-5",
  "providerID": "opencode",
  "name": "Claude Fable 5",
  "variants": {
    "low": {},
    "medium": {},
    "high": {},
    "xhigh": {},
    "max": {}
  }
}
`

const samplePlainOutput = `Fetching available models...
opencode/big-pickle
opencode/claude-sonnet-4-5
opencode/claude-fable-5
`

func TestParseModelsVerbose(t *testing.T) {
	options, err := parseModelsVerbose(sampleVerboseOutput)
	if err != nil {
		t.Fatalf("parseModelsVerbose failed: %v", err)
	}

	if len(options) != 3 {
		t.Fatalf("len(options) = %d, want 3", len(options))
	}

	opt0 := options[0]
	if opt0.Value != "opencode/big-pickle" || opt0.Label != "Big Pickle" || len(opt0.Efforts) != 0 {
		t.Errorf("opt0 = %+v", opt0)
	}

	opt1 := options[1]
	if opt1.Value != "opencode/claude-sonnet-4-5" || opt1.Label != "Claude Sonnet 4.5" {
		t.Errorf("opt1 = %+v", opt1)
	}
	wantEfforts1 := []string{"high", "max"}
	if !reflect.DeepEqual(opt1.Efforts, wantEfforts1) {
		t.Errorf("opt1.Efforts = %v, want %v", opt1.Efforts, wantEfforts1)
	}

	opt2 := options[2]
	wantEfforts2 := []string{"low", "medium", "high", "xhigh", "max"}
	if !reflect.DeepEqual(opt2.Efforts, wantEfforts2) {
		t.Errorf("opt2.Efforts = %v, want %v", opt2.Efforts, wantEfforts2)
	}
}

func TestParseModelsPlain(t *testing.T) {
	options, err := parseModelsPlain(samplePlainOutput)
	if err != nil {
		t.Fatalf("parseModelsPlain failed: %v", err)
	}

	if len(options) != 3 {
		t.Fatalf("len(options) = %d, want 3", len(options))
	}

	if options[1].Value != "opencode/claude-sonnet-4-5" || options[1].Label != "opencode/claude-sonnet-4-5" {
		t.Errorf("options[1] = %+v", options[1])
	}
}

func TestProberProbe(t *testing.T) {
	ctx := context.Background()

	// 1. Success on verbose
	fakeRun := runner.NewFake()
	fakeRun.Expect("/bin/opencode models --verbose", sampleVerboseOutput)

	prober := NewProber("/bin/opencode", fakeRun, nil)
	set, err := prober.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe() failed: %v", err)
	}
	if len(set.Options) != 3 {
		t.Fatalf("len(set.Options) = %d, want 3", len(set.Options))
	}
	if set.Default != "opencode/claude-sonnet-4-5" {
		t.Errorf("Default = %q, want opencode/claude-sonnet-4-5", set.Default)
	}
	if set.DefaultEffort != "high" {
		t.Errorf("DefaultEffort = %q, want high", set.DefaultEffort)
	}

	// 2. Fall back to plain when verbose errors
	fakeRun2 := runner.NewFake()
	fakeRun2.ExpectError("/bin/opencode models --verbose", errors.New("unknown flag --verbose"))
	fakeRun2.Expect("/bin/opencode models", samplePlainOutput)

	prober2 := NewProber("/bin/opencode", fakeRun2, nil)
	set2, err := prober2.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe() plain fallback failed: %v", err)
	}
	if len(set2.Options) != 3 {
		t.Fatalf("len(set2.Options) = %d, want 3", len(set2.Options))
	}
	if set2.Default != "opencode/claude-sonnet-4-5" {
		t.Errorf("Default = %q, want opencode/claude-sonnet-4-5", set2.Default)
	}

	// 3. Fall back to configured models when both commands error
	fakeRun3 := runner.NewFake()
	fakeRun3.ExpectError("/bin/opencode models --verbose", errors.New("command failed"))
	fakeRun3.ExpectError("/bin/opencode models", errors.New("command failed"))

	prober3 := NewProber("/bin/opencode", fakeRun3, []string{"opencode/claude-sonnet-4-5", "opencode/gpt-5"})
	set3, err := prober3.Probe(ctx)
	if err != nil {
		t.Fatalf("Probe() configured fallback failed: %v", err)
	}
	if len(set3.Options) != 2 {
		t.Fatalf("len(set3.Options) = %d, want 2", len(set3.Options))
	}
	if set3.Err == "" {
		t.Errorf("expected set.Err to report configured models fallback")
	}

	// 4. Failure when no configured fallback models exist
	fakeRun4 := runner.NewFake()
	fakeRun4.ExpectError("/bin/opencode models --verbose", errors.New("command failed"))
	fakeRun4.ExpectError("/bin/opencode models", errors.New("command failed"))
	prober4 := NewProber("/bin/opencode", fakeRun4, nil)
	_, err = prober4.Probe(ctx)
	if err == nil {
		t.Fatal("expected error when no models available")
	}
}
