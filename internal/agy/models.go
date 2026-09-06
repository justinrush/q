package agy

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// Prober reads the tab-separated catalog emitted by agy models (verified with
// 1.1.27). That version rejects --json. No prompt or model inference is needed.
type Prober struct {
	bin    string
	run    runner.Runner
	models []string
}

func NewProber(bin string, run runner.Runner, models []string) *Prober {
	return &Prober{bin: bin, run: run, models: append([]string(nil), models...)}
}

func (*Prober) Tool() mission.Tool { return mission.ToolAgy }

func (p *Prober) Probe(ctx context.Context) (mission.ModelSet, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	result, err := p.run.Run(ctx, runner.Spec{Name: p.bin, Args: []string{"models"}})
	var options []mission.ModelOption
	if err == nil {
		options, err = parseModels(result.Out())
	}
	if err != nil {
		if len(p.models) == 0 {
			return mission.ModelSet{}, fmt.Errorf("asking agy for models: %w", err)
		}
		seen := map[string]bool{}
		for _, model := range p.models {
			if !seen[model] && model != "" && mission.ValidateModelFlag("model", model) == nil {
				options = append(options, modelOption(model, model))
				seen[model] = true
			}
		}
		if len(options) == 0 {
			return mission.ModelSet{}, fmt.Errorf("asking agy for models: %w", err)
		}
		return mission.ModelSet{Options: options, ProbedAt: time.Now(), Err: "using configured models: " + err.Error()}, nil
	}
	// The catalog does not mark a default. Leaving it empty preserves agy's own
	// choice; withOverrides applies any explicit preference in q's config.
	return mission.ModelSet{Options: options, ProbedAt: time.Now()}, nil
}

func parseModels(output string) ([]mission.ModelOption, error) {
	var options []mission.ModelOption
	seen := map[string]bool{}
	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "Fetching available models..." {
			continue
		}
		id, label, ok := strings.Cut(line, "\t")
		id, label = strings.TrimSpace(id), strings.TrimSpace(label)
		if !ok || id == "" || label == "" || mission.ValidateModelFlag("model", id) != nil {
			return nil, fmt.Errorf("unexpected agy model catalog format")
		}
		if !seen[id] {
			options = append(options, modelOption(id, label))
			seen[id] = true
		}
	}
	if len(options) == 0 {
		return nil, fmt.Errorf("agy returned no models")
	}
	return options, nil
}

func modelOption(id, label string) mission.ModelOption {
	// These are CLI-wide --effort values reported by agy --help, not guesses
	// based on a model's name or a hardcoded catalog of available models.
	return mission.ModelOption{Value: id, Label: label, Efforts: []string{"low", "medium", "high"}}
}
