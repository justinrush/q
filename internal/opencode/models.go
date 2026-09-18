package opencode

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/justinrush/q/internal/mission"
	"github.com/justinrush/q/internal/runner"
)

// Prober reads the catalog emitted by opencode models --verbose (or plain opencode models).
type Prober struct {
	bin    string
	run    runner.Runner
	models []string
}

// NewProber returns a prober querying opencode for available models.
func NewProber(bin string, run runner.Runner, models []string) *Prober {
	return &Prober{
		bin:    bin,
		run:    run,
		models: append([]string(nil), models...),
	}
}

// Tool reports that this prober queries opencode.
func (*Prober) Tool() mission.Tool { return mission.ToolOpencode }

// Probe asks opencode which models and variants it offers.
func (p *Prober) Probe(ctx context.Context) (mission.ModelSet, error) {
	ctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	var (
		options []mission.ModelOption
		probeErr error
	)

	// First try verbose mode for display names and effort variants.
	result, err := p.run.Run(ctx, runner.Spec{Name: p.bin, Args: []string{"models", "--verbose"}})
	if err == nil {
		options, err = parseModelsVerbose(result.Out())
	}

	// Fall back to plain models list if verbose failed.
	if err != nil {
		probeErr = err
		plainResult, plainErr := p.run.Run(ctx, runner.Spec{Name: p.bin, Args: []string{"models"}})
		if plainErr == nil {
			options, err = parseModelsPlain(plainResult.Out())
		}
	}

	// If both probing strategies failed, fall back to configured models.
	if err != nil {
		if len(p.models) == 0 {
			return mission.ModelSet{}, fmt.Errorf("asking opencode for models: %w", probeErr)
		}

		seen := map[string]bool{}
		for _, model := range p.models {
			if !seen[model] && model != "" && mission.ValidateModelFlag("model", model) == nil {
				options = append(options, mission.ModelOption{Value: model, Label: model})
				seen[model] = true
			}
		}

		if len(options) == 0 {
			return mission.ModelSet{}, fmt.Errorf("asking opencode for models: %w", probeErr)
		}

		def, defEffort := pickDefaults(options)
		return mission.ModelSet{
			Options:       options,
			Default:       def,
			DefaultEffort: defEffort,
			ProbedAt:      time.Now(),
			Err:           "using configured models: " + probeErr.Error(),
		}, nil
	}

	def, defEffort := pickDefaults(options)
	return mission.ModelSet{
		Options:       options,
		Default:       def,
		DefaultEffort: defEffort,
		ProbedAt:      time.Now(),
	}, nil
}

func pickDefaults(options []mission.ModelOption) (string, string) {
	if len(options) == 0 {
		return "", ""
	}

	// Prefer Claude Sonnet models if available in OpenCode Zen catalog.
	preferred := []string{
		"opencode/claude-sonnet-4-5",
		"opencode/claude-sonnet-4",
		"opencode/gpt-5",
	}

	chosen := options[0]
	for _, pref := range preferred {
		for _, opt := range options {
			if opt.Value == pref {
				chosen = opt
				goto found
			}
		}
	}

found:
	return chosen.Value, defaultEffort(chosen)
}

func defaultEffort(opt mission.ModelOption) string {
	if slices.Contains(opt.Efforts, "high") {
		return "high"
	}
	if len(opt.Efforts) > 0 {
		return opt.Efforts[0]
	}
	return ""
}

type verboseDoc struct {
	ID         string         `json:"id"`
	ProviderID string         `json:"providerID"`
	Name       string         `json:"name"`
	Variants   map[string]any `json:"variants"`
}

var standardEffortOrder = []string{"low", "medium", "high", "xhigh", "max"}

func parseModelsVerbose(output string) ([]mission.ModelOption, error) {
	var options []mission.ModelOption
	seen := map[string]bool{}

	scanner := bufio.NewScanner(strings.NewReader(output))
	var currentHeader string
	var jsonBuf strings.Builder
	depth := 0

	processDoc := func(raw string, header string) {
		var doc verboseDoc
		if err := json.Unmarshal([]byte(raw), &doc); err != nil {
			return
		}

		fullID := doc.ID
		if doc.ProviderID != "" && !strings.Contains(fullID, "/") {
			fullID = doc.ProviderID + "/" + doc.ID
		}
		if fullID == "" {
			fullID = header
		}
		if fullID == "" || seen[fullID] || mission.ValidateModelFlag("model", fullID) != nil {
			return
		}

		label := doc.Name
		if label == "" {
			label = fullID
		}

		var efforts []string
		if len(doc.Variants) > 0 {
			for _, std := range standardEffortOrder {
				if _, ok := doc.Variants[std]; ok {
					efforts = append(efforts, std)
				}
			}
			var others []string
			for k := range doc.Variants {
				if !slices.Contains(standardEffortOrder, k) {
					others = append(others, k)
				}
			}
			sort.Strings(others)
			efforts = append(efforts, others...)
		}

		options = append(options, mission.ModelOption{
			Value:   fullID,
			Label:   label,
			Efforts: efforts,
		})
		seen[fullID] = true
	}

	for scanner.Scan() {
		line := scanner.Text()
		trimmed := strings.TrimSpace(line)

		if depth == 0 {
			if strings.HasPrefix(trimmed, "{") {
				depth += strings.Count(line, "{") - strings.Count(line, "}")
				jsonBuf.Reset()
				jsonBuf.WriteString(line)
				jsonBuf.WriteByte('\n')
				if depth == 0 {
					processDoc(jsonBuf.String(), currentHeader)
				}
			} else if trimmed != "" && trimmed != "Fetching available models..." {
				currentHeader = trimmed
			}
		} else {
			depth += strings.Count(line, "{") - strings.Count(line, "}")
			jsonBuf.WriteString(line)
			jsonBuf.WriteByte('\n')
			if depth == 0 {
				processDoc(jsonBuf.String(), currentHeader)
			}
		}
	}

	if len(options) == 0 {
		return nil, fmt.Errorf("opencode returned no models")
	}

	return options, nil
}

func parseModelsPlain(output string) ([]mission.ModelOption, error) {
	var options []mission.ModelOption
	seen := map[string]bool{}

	for _, line := range strings.Split(output, "\n") {
		line = strings.TrimSpace(line)
		if line == "" || line == "Fetching available models..." {
			continue
		}
		if mission.ValidateModelFlag("model", line) != nil {
			continue
		}
		if !seen[line] {
			options = append(options, mission.ModelOption{
				Value: line,
				Label: line,
			})
			seen[line] = true
		}
	}

	if len(options) == 0 {
		return nil, fmt.Errorf("opencode returned no models")
	}

	return options, nil
}
