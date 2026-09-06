// Reading codex's rollout JSONL.
//
// codex records usage twice over, and the two records are not equally safe to
// read. Every response appends a `token_usage_record` carrying that response's
// own usage plus a `response_id` and the `thread_id` it belongs to; separately,
// a `token_count` event carries running totals and the account's rate-limit
// windows. This reads the per-response records for tokens and the events only
// for limits, which sidesteps both of the counting traps codex is known for:
//
//   - a `token_count` emitted because only the rate limits moved repeats the
//     previous non-zero `last_token_usage`, so summing deltas from events
//     double-counts whatever response came before the refresh;
//   - a subagent's rollout opens with a replay of its parent's whole usage
//     history, re-timestamped, so a reader that trusts every record it sees in
//     a subagent file can inflate a session's cost by a factor of dozens.
//
// Deduplicating by `response_id` defeats the first and keeping only records
// whose `thread_id` is the rollout's own defeats the second.
//
// Unlike the claude reader, this one has been written against codex's protocol
// definitions rather than against an observed file: the schema below is taken
// from RolloutItemWire, TokenUsageRecord, TokenUsage and RateLimitSnapshot in
// openai/codex. It has not been run against a rollout codex actually wrote.

package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"os"
	"sync"
	"time"

	"github.com/justinrush/q/internal/mission"
)

// Rollout record types, as RolloutItemWire tags them.
const (
	rolloutTokenUsage  = "token_usage_record"
	rolloutEventMsg    = "event_msg"
	rolloutTurnContext = "turn_context"
)

// eventTokenCount is the event_msg payload carrying rate-limit windows.
const eventTokenCount = "token_count"

// unknownCodexModel labels usage from a rollout that never recorded which model
// served it. It is deliberately not a real model id, so it cannot collide with
// one in the price table and quietly acquire a rate.
const unknownCodexModel = "codex (model not recorded)"

// exhaustedPercent is the point at which a rate-limit window counts as used up.
const exhaustedPercent = 100.0

// Window lengths codex reports, in minutes, mapped to the names q uses.
var codexWindowKinds = map[int64]string{
	300:   "five_hour",
	10080: "weekly",
}

// Codex meters a codex session from its rollout file.
type Codex struct {
	pricing Pricing

	mu     sync.Mutex
	cached map[string]codexEntry
}

// codexEntry remembers one rollout's last scan.
type codexEntry struct {
	size    int64
	modTime time.Time
	counts  codexCounts
}

// codexCounts is one rollout's contribution.
type codexCounts struct {
	// byResponse is keyed by response id, so a record replayed into a subagent
	// rollout or repeated by a rate-limit refresh is counted once.
	byResponse map[string]mission.ModelTokens
	model      string
	limit      mission.Limit
}

// NewCodex returns a meter that prices with the given table.
func NewCodex(pricing Pricing) *Codex {
	if pricing.Models == nil {
		pricing = DefaultPricing()
	}

	return &Codex{pricing: pricing, cached: map[string]codexEntry{}}
}

// Tool implements [mission.Meter].
func (c *Codex) Tool() mission.Tool { return mission.ToolCodex }

// Meter implements [mission.Meter].
//
// codex reports a rollout path on its hooks only sometimes — it runs ephemeral
// sessions that keep none — and q deliberately does not go looking for one by
// guessing at the layout of codex's session directory. A mission q was never
// told the path for simply carries no cost.
func (c *Codex) Meter(ms mission.Mission) (mission.Metering, bool, error) {
	if ms.TranscriptPath == "" {
		return mission.Metering{}, false, nil
	}

	counts, err := c.scan(ms.TranscriptPath)
	if err != nil {
		return mission.Metering{}, false, err
	}

	if len(counts.byResponse) == 0 {
		return mission.Metering{Limit: counts.limit}, false, nil
	}

	// The rollout names the model per turn, but an ephemeral or truncated one
	// may never have. The mission's own choice is the next best answer — it is
	// what q put on the command line — and only when there is neither does the
	// usage get a name no price can match.
	model := counts.model
	if model == "" {
		model = ms.Model
	}

	if model == "" {
		model = unknownCodexModel
	}

	var total mission.ModelTokens
	for _, tokens := range counts.byResponse {
		total = total.Add(tokens)
	}

	perModel := map[string]mission.ModelTokens{model: total}

	return mission.Metering{
		Usage: c.pricing.Value(perModel),
		Limit: counts.limit,
	}, true, nil
}

// scan reads a rollout, reusing the previous result when it has not changed.
func (c *Codex) scan(path string) (codexCounts, error) {
	info, err := os.Stat(path)
	if errors.Is(err, fs.ErrNotExist) {
		return codexCounts{}, nil
	}

	if err != nil {
		return codexCounts{}, err
	}

	c.mu.Lock()
	entry, ok := c.cached[path]
	c.mu.Unlock()

	if ok && entry.size == info.Size() && entry.modTime.Equal(info.ModTime()) {
		return entry.counts, nil
	}

	counts, err := parseRollout(path)
	if err != nil {
		return codexCounts{}, err
	}

	c.mu.Lock()
	c.cached[path] = codexEntry{size: info.Size(), modTime: info.ModTime(), counts: counts}
	c.mu.Unlock()

	return counts, nil
}

// parseRollout reads one rollout file.
func parseRollout(path string) (codexCounts, error) {
	f, err := os.Open(path)
	if errors.Is(err, fs.ErrNotExist) {
		return codexCounts{}, nil
	}

	if err != nil {
		return codexCounts{}, err
	}
	defer func() { _ = f.Close() }()

	counts := codexCounts{byResponse: map[string]mission.ModelTokens{}}

	// A subagent rollout replays its parent's records before recording any of
	// its own, and the replay is indistinguishable from real work except by the
	// thread it names. The first thread this file records for itself is the one
	// it belongs to; anything attributed elsewhere is somebody else's history.
	var thread string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)

	for scanner.Scan() {
		var line rolloutLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}

		switch line.Type {
		case rolloutTurnContext:
			if model := line.Payload.model(); model != "" {
				counts.model = model
			}
		case rolloutEventMsg:
			if limit, ok := line.Payload.limit(line.observedAt()); ok {
				counts.limit = limit
			}
		case rolloutTokenUsage:
			record := line.Payload

			if record.ResponseID == "" {
				continue
			}

			if thread == "" {
				thread = record.ThreadID
			}

			if record.ThreadID != thread {
				continue
			}

			if tokens := record.Usage.tokens(); !tokens.Zero() {
				counts.byResponse[record.ResponseID] = tokens
			}
		}
	}

	// A read error mid-file leaves what was counted, the same as for a
	// transcript the agent is still writing to.
	_ = scanner.Err()

	return counts, nil
}

// rolloutLine is one record of a rollout file. Every record is a timestamp, a
// type tag, and a payload whose shape the tag names.
type rolloutLine struct {
	Timestamp string         `json:"timestamp"`
	Type      string         `json:"type"`
	Payload   rolloutPayload `json:"payload"`
}

// observedAt is when codex wrote the record, falling back to now for a rollout
// whose timestamps this version of q cannot read.
//
// It matters that this is the record's own time rather than the time of the
// scan: a scan's result is cached, so stamping it with the clock would make the
// same record look newer every time the file around it changed.
func (l rolloutLine) observedAt() time.Time {
	at, err := time.Parse(time.RFC3339, l.Timestamp)
	if err != nil {
		return time.Now()
	}

	return at
}

// rolloutPayload is the union of the payload fields this package reads. The
// records are disjoint by tag, so one struct serves them all rather than
// decoding each line twice to find out which shape it is.
type rolloutPayload struct {
	// token_usage_record.
	ThreadID   string          `json:"thread_id"`
	ResponseID string          `json:"response_id"`
	Usage      codexTokenUsage `json:"usage"`

	// event_msg.
	Type       string           `json:"type"`
	RateLimits *codexRateLimits `json:"rate_limits"`

	// turn_context.
	CollaborationMode *struct {
		Model    string `json:"model"`
		Settings *struct {
			Model string `json:"model"`
		} `json:"settings"`
	} `json:"collaboration_mode"`
}

// model returns the model a turn ran under, preferring the current nesting over
// the flatter one older rollouts used.
func (p rolloutPayload) model() string {
	if p.CollaborationMode == nil {
		return ""
	}

	if p.CollaborationMode.Settings != nil && p.CollaborationMode.Settings.Model != "" {
		return p.CollaborationMode.Settings.Model
	}

	return p.CollaborationMode.Model
}

// limit converts a token_count event's rate-limit windows into a limit,
// reporting false when no window is used up.
//
// codex reports how much of each window is gone rather than only refusing when
// one is, so unlike claude it can say a window is exhausted the moment it is,
// without waiting for a request to be turned away.
func (p rolloutPayload) limit(observedAt time.Time) (mission.Limit, bool) {
	if p.Type != eventTokenCount || p.RateLimits == nil {
		return mission.Limit{}, false
	}

	var out mission.Limit

	for _, window := range []*codexRateLimitWindow{p.RateLimits.Primary, p.RateLimits.Secondary} {
		if window == nil || window.UsedPercent < exhaustedPercent || window.ResetsAt == nil {
			continue
		}

		resets := time.Unix(*window.ResetsAt, 0)
		if !out.ResetsAt.IsZero() && !resets.After(out.ResetsAt) {
			continue
		}

		// The window that reopens last is the one that actually gates the next
		// mission, so a weekly cap outranks a five-hour one that has already
		// run down inside it.
		out = mission.Limit{
			Tool:       mission.ToolCodex,
			Kind:       window.kind(),
			ResetsAt:   resets,
			ObservedAt: observedAt,
		}
	}

	return out, !out.ResetsAt.IsZero()
}

// codexRateLimits mirrors codex's RateLimitSnapshot.
type codexRateLimits struct {
	Primary   *codexRateLimitWindow `json:"primary"`
	Secondary *codexRateLimitWindow `json:"secondary"`
}

// codexRateLimitWindow mirrors codex's RateLimitWindow.
type codexRateLimitWindow struct {
	UsedPercent float64 `json:"used_percent"`
	// WindowMinutes names the window, e.g. 300 for a five-hour cap.
	WindowMinutes *int64 `json:"window_minutes"`
	// ResetsAt is a unix timestamp.
	ResetsAt *int64 `json:"resets_at"`
}

// kind names the window in the terms q reports limits in.
func (w codexRateLimitWindow) kind() string {
	if w.WindowMinutes == nil {
		return ""
	}

	return codexWindowKinds[*w.WindowMinutes]
}

// codexTokenUsage mirrors codex's TokenUsage.
type codexTokenUsage struct {
	InputTokens           int64 `json:"input_tokens"`
	CachedInputTokens     int64 `json:"cached_input_tokens"`
	CacheWriteInputTokens int64 `json:"cache_write_input_tokens"`
	OutputTokens          int64 `json:"output_tokens"`
	TotalTokens           int64 `json:"total_tokens"`
}

// tokens converts codex's counts to q's.
//
// Two conventions differ from claude's and are handled here rather than at the
// call site. codex's input_tokens is inclusive of the cached part — its own
// non_cached_input() subtracts one from the other — so charging both would bill
// every cached token twice. And reasoning_output_tokens is a share of
// output_tokens rather than an addition to it, which is why it is not read at
// all.
func (u codexTokenUsage) tokens() mission.ModelTokens {
	input := u.InputTokens - u.CachedInputTokens
	if input < 0 {
		input = 0
	}

	return mission.ModelTokens{
		Input:        input,
		Output:       u.OutputTokens,
		CacheRead:    u.CachedInputTokens,
		CacheWrite5m: u.CacheWriteInputTokens,
	}
}
