// Reading claude's JSONL transcript.
//
// The transcript is append-only and claude is usually still writing to it, so
// every read here is defensive: an unparseable line is skipped rather than
// failing the scan, because the last line of a live transcript is routinely a
// half-written one and refusing to report a cost until the agent pauses would
// defeat the point of a running total.

package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/justinrush/q/internal/mission"
)

// maxLineBytes bounds one transcript line. Individual messages carry whole file
// contents and tool output, so this is generous.
const maxLineBytes = 32 << 20

// Claude meters a claude session from its transcript.
//
// A zero Claude is usable and prices with the built-in table.
type Claude struct {
	pricing Pricing

	mu     sync.Mutex
	cached map[string]cacheEntry
}

// cacheEntry remembers a file's last scan, so an unchanged transcript costs a
// stat rather than a parse.
//
// Re-reading whole transcripts on the reconciler's tick would otherwise mean
// parsing a couple of megabytes per active mission every fifteen seconds
// forever, almost always to arrive at the number already on the card.
type cacheEntry struct {
	size    int64
	modTime time.Time
	counts  fileCounts
}

// fileCounts is one transcript file's contribution.
type fileCounts struct {
	// byRequest is keyed by request id, because claude writes a record per
	// streaming update and every one of them repeats the same cumulative usage
	// for that request. Summing the records rather than the requests inflates
	// every total by however many updates each response happened to take.
	byRequest map[string]requestUsage
	quota     mission.Limit
}

// requestUsage is one API request's usage, and which model served it.
type requestUsage struct {
	model  string
	tokens mission.ModelTokens
}

// NewClaude returns a meter that prices with the given table.
func NewClaude(pricing Pricing) *Claude {
	if pricing.Models == nil {
		pricing = DefaultPricing()
	}

	return &Claude{pricing: pricing, cached: map[string]cacheEntry{}}
}

// Tool implements [mission.Meter].
func (c *Claude) Tool() mission.Tool { return mission.ToolClaude }

// Meter implements [mission.Meter].
//
// It reports false when the mission has no transcript yet, which is the normal
// state of a mission that has been created but not launched.
func (c *Claude) Meter(ms mission.Mission) (mission.Metering, bool, error) {
	if ms.TranscriptPath == "" {
		return mission.Metering{}, false, nil
	}

	return c.read(ms.TranscriptPath)
}

// read scans a transcript and every subagent transcript beneath it.
func (c *Claude) read(transcript string) (mission.Metering, bool, error) {
	paths, err := transcriptSet(transcript)
	if err != nil {
		return mission.Metering{}, false, err
	}

	byRequest := map[string]requestUsage{}

	var quota mission.Limit

	for _, path := range paths {
		counts, err := c.scan(path)
		if err != nil {
			return mission.Metering{}, false, err
		}

		for id, usage := range counts.byRequest {
			byRequest[id] = usage
		}

		if counts.quota.ObservedAt.After(quota.ObservedAt) {
			quota = counts.quota
		}
	}

	if len(byRequest) == 0 {
		return mission.Metering{Limit: quota}, false, nil
	}

	perModel := map[string]mission.ModelTokens{}
	for _, usage := range byRequest {
		perModel[usage.model] = perModel[usage.model].Add(usage.tokens)
	}

	return mission.Metering{Usage: c.pricing.Value(perModel), Limit: quota}, true, nil
}

// transcriptSet returns the session transcript plus each of its subagents'.
//
// A subagent writes to its own file and its request ids never appear in the
// parent's, so a session whose work was mostly delegated would otherwise report
// a small fraction of what it actually spent.
func transcriptSet(transcript string) ([]string, error) {
	out := []string{transcript}

	dir := strings.TrimSuffix(transcript, filepath.Ext(transcript))

	subagents, err := filepath.Glob(filepath.Join(dir, "subagents", "*.jsonl"))
	if err != nil {
		return nil, fmt.Errorf("listing subagent transcripts of %s: %w", transcript, err)
	}

	return append(out, subagents...), nil
}

// scan reads one transcript, reusing the previous result when the file has not
// changed since.
func (c *Claude) scan(path string) (fileCounts, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		// A transcript q has been told about but that does not exist yet is the
		// normal state between a session starting and its first response.
		return fileCounts{}, nil
	}

	if err != nil {
		return fileCounts{}, fmt.Errorf("reading %s: %w", path, err)
	}

	c.mu.Lock()
	entry, ok := c.cached[path]
	c.mu.Unlock()

	if ok && entry.size == info.Size() && entry.modTime.Equal(info.ModTime()) {
		return entry.counts, nil
	}

	counts, err := parseTranscript(path)
	if err != nil {
		return fileCounts{}, err
	}

	c.mu.Lock()
	c.cached[path] = cacheEntry{size: info.Size(), modTime: info.ModTime(), counts: counts}
	c.mu.Unlock()

	return counts, nil
}

// parseTranscript reads every assistant record out of one transcript.
func parseTranscript(path string) (fileCounts, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return fileCounts{}, nil
	}

	if err != nil {
		return fileCounts{}, fmt.Errorf("opening %s: %w", path, err)
	}
	defer func() { _ = f.Close() }()

	counts := fileCounts{byRequest: map[string]requestUsage{}}

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)

	for scanner.Scan() {
		var rec assistantRecord
		if err := json.Unmarshal(scanner.Bytes(), &rec); err != nil {
			// A half-written trailing line while the agent is mid-turn, or a
			// record shape this version of q does not know. Neither is worth
			// abandoning the rest of the file over.
			continue
		}

		if rec.Type != "assistant" || rec.RequestID == "" {
			continue
		}

		if quota, ok := rec.limit(); ok && quota.ObservedAt.After(counts.quota.ObservedAt) {
			counts.quota = quota
		}

		tokens := rec.Message.Usage.tokens()
		if tokens.Zero() {
			// Claude records a message it composed itself with a "<synthetic>"
			// model and no tokens. Counting it would put a model with no
			// published price into the breakdown and mark a perfectly ordinary
			// session's cost as incomplete.
			continue
		}

		counts.byRequest[rec.RequestID] = requestUsage{model: rec.Message.Model, tokens: tokens}
	}

	if err := scanner.Err(); err != nil {
		// A line past the buffer bound, or a read error on a file being
		// rewritten. Report what was counted rather than nothing.
		return counts, nil
	}

	return counts, nil
}

// assistantRecord is the part of a transcript record this package reads.
type assistantRecord struct {
	Type      string    `json:"type"`
	RequestID string    `json:"requestId"`
	Timestamp time.Time `json:"timestamp"`
	Message   struct {
		Model string   `json:"model"`
		Usage rawUsage `json:"usage"`
	} `json:"message"`
	// QuotaLimits is present only on a record whose request was refused, which
	// is the only moment claude writes down anything about a usage window.
	QuotaLimits *struct {
		Status        string `json:"status"`
		RateLimitType string `json:"rateLimitType"`
		ResetsAt      int64  `json:"resetsAt"`
	} `json:"quotaLimits"`
}

// limit converts a refusal record into a limit, reporting false for a record
// that carries none or that reports the request was allowed.
func (r assistantRecord) limit() (mission.Limit, bool) {
	if r.QuotaLimits == nil || r.QuotaLimits.Status == "allowed" || r.QuotaLimits.ResetsAt == 0 {
		return mission.Limit{}, false
	}

	observed := r.Timestamp
	if observed.IsZero() {
		observed = time.Unix(r.QuotaLimits.ResetsAt, 0)
	}

	return mission.Limit{
		Tool:       mission.ToolClaude,
		Kind:       r.QuotaLimits.RateLimitType,
		ResetsAt:   time.Unix(r.QuotaLimits.ResetsAt, 0),
		ObservedAt: observed,
	}, true
}

// rawUsage mirrors the usage block claude records per response.
type rawUsage struct {
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	CacheReadInputTokens     int64 `json:"cache_read_input_tokens"`
	CacheCreationInputTokens int64 `json:"cache_creation_input_tokens"`
	CacheCreation            *struct {
		Ephemeral5m int64 `json:"ephemeral_5m_input_tokens"`
		Ephemeral1h int64 `json:"ephemeral_1h_input_tokens"`
	} `json:"cache_creation"`
}

// tokens converts a usage block to the counts q stores.
//
// The two cache-write TTLs are priced differently, so the split is kept when
// claude reports one. When it does not, the undifferentiated total is charged
// at the cheaper five-minute rate: under-reporting a cost is the safer error
// than inventing an hour-long write that may not have happened.
func (u rawUsage) tokens() mission.ModelTokens {
	out := mission.ModelTokens{
		Input:     u.InputTokens,
		Output:    u.OutputTokens,
		CacheRead: u.CacheReadInputTokens,
	}

	if u.CacheCreation != nil && u.CacheCreation.Ephemeral5m+u.CacheCreation.Ephemeral1h > 0 {
		out.CacheWrite5m = u.CacheCreation.Ephemeral5m
		out.CacheWrite1h = u.CacheCreation.Ephemeral1h

		return out
	}

	out.CacheWrite5m = u.CacheCreationInputTokens

	return out
}
