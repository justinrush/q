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
// The schema has also been checked against local Codex 0.163.0 rollouts.
// Completed response measurements are persisted so thread replacement and
// removal of an older rollout do not erase the planning phase's cost.

package usage

import (
	"bufio"
	"encoding/json"
	"errors"
	"io/fs"
	"maps"
	"os"
	"slices"
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
	byResponse map[string]requestUsage
	effort     string
	model      string
	phase      mission.Phase
	limit      mission.Limit
	// turn is the session turn under way and turnAt when it began. Both stay
	// empty in a subagent's rollout, whose turns are its own and not prompts.
	turn     string
	turnAt   time.Time
	subagent bool
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
// Rollout paths come from hooks or runtime thread metadata. Checkpoints keep
// completed responses even after an older file is removed.
func (c *Codex) Meter(ms mission.Mission) (mission.Metering, bool, error) {
	checkpoint := ms.CodexUsage.Clone()
	if ms.TranscriptPath != "" {
		if checkpoint == nil {
			checkpoint = mission.CodexUsage{}
		}
		if _, ok := checkpoint[ms.TranscriptPath]; !ok {
			checkpoint[ms.TranscriptPath] = nil
		}
	}
	if len(checkpoint) == 0 {
		return mission.Metering{}, false, nil
	}
	var limit mission.Limit
	for _, path := range slices.Sorted(maps.Keys(checkpoint)) {
		counts, err := c.scan(path)
		if err != nil {
			// A saved contribution remains valid when an old rollout is unreadable.
			if len(checkpoint[path]) != 0 && path != ms.TranscriptPath {
				continue
			}
			return mission.Metering{}, false, err
		}
		if path == ms.TranscriptPath {
			limit = counts.limit
		}
		fallback := ""
		if !ms.SplitsPlan() && len(checkpoint) == 1 {
			fallback = ms.Model
		}
		checkpoint[path] = checkpointResponses(checkpoint[path], counts, fallback)
	}
	iterating := iterationStart(checkpoint)
	perModel, perPhase := codexModels(checkpoint, iterating)
	requests := map[string]requestUsage{}
	for _, path := range slices.Sorted(maps.Keys(checkpoint)) {
		for id, r := range checkpoint[path] {
			if _, ok := requests[id]; !ok {
				requests[id] = requestUsage{model: r.Model, effort: r.Effort, phase: responsePhase(r, iterating), tokens: r.Tokens}
			}
		}
	}
	measuredUsage := c.pricing.ValuePhases(perModel, perPhase)
	measuredUsage.Breakdown = c.pricing.breakdown(requests)
	return mission.Metering{
		Usage: measuredUsage, Limit: limit, CodexUsage: checkpoint,
	}, len(perModel) != 0, nil
}

// checkpointResponses merges completed measurements without erasing responses
// after a partial read, and freezes fallback attribution when first measured.
func checkpointResponses(saved map[string]mission.CodexResponse, counts codexCounts, fallback string) map[string]mission.CodexResponse {
	if len(counts.byResponse) == 0 {
		return saved
	}
	if saved == nil {
		saved = map[string]mission.CodexResponse{}
	}
	for id, response := range counts.byResponse {
		model := response.model
		if model == "" {
			model = saved[id].Model
		}
		if model == "" {
			model = fallback
		}
		if model == "" {
			model = unknownCodexModel
		}
		phase := response.phase
		if phase == "" {
			phase = saved[id].Phase
		}
		effort := response.effort
		if effort == "" {
			effort = saved[id].Effort
		}
		next := mission.CodexResponse{Model: model, Tokens: response.tokens, Phase: phase, Effort: effort, At: response.at, Turn: response.turn, TurnAt: response.turnAt}
		if next.At.IsZero() {
			next.At, next.Turn, next.TurnAt = saved[id].At, saved[id].Turn, saved[id].TurnAt
		}
		saved[id] = next
	}
	return saved
}

// codexModels deduplicates responses across paths (including renamed rollouts).
// Stable path order also keeps ambiguous duplicate attribution deterministic.
// The phase split is nil when any response has no recorded phase.
func codexModels(checkpoint mission.CodexUsage, iterating time.Time) (map[string]mission.ModelTokens, map[mission.Phase]map[string]mission.ModelTokens) {
	perModel := map[string]mission.ModelTokens{}
	perPhase := map[mission.Phase]map[string]mission.ModelTokens{}
	seen := map[string]bool{}
	for _, path := range slices.Sorted(maps.Keys(checkpoint)) {
		for id, response := range checkpoint[path] {
			if seen[id] {
				continue
			}
			seen[id] = true
			perModel[response.Model] = perModel[response.Model].Add(response.Tokens)
			if perPhase == nil {
				continue
			}
			phase := responsePhase(response, iterating)
			if phase == "" {
				perPhase = nil
				continue
			}
			if perPhase[phase] == nil {
				perPhase[phase] = map[string]mission.ModelTokens{}
			}
			perPhase[phase][response.Model] = perPhase[phase][response.Model].Add(response.Tokens)
		}
	}
	return perModel, perPhase
}

// iterationStart is when the first session turn after the initial
// implementation began, or zero when there has been none yet.
//
// Turns are compared across every thread in the checkpoint, so a model handoff
// that moves the implementation to a replacement thread does not make its
// first turn look like a follow-up, nor reset the count for later ones.
func iterationStart(checkpoint mission.CodexUsage) time.Time {
	type turn struct {
		at    time.Time
		phase mission.Phase
	}

	turns := map[string]turn{}
	for _, responses := range checkpoint {
		for _, r := range responses {
			if r.Turn == "" || r.TurnAt.IsZero() {
				continue
			}
			// A turn that leaves plan mode records its later responses as
			// implementing, so any one of them makes it the implementation.
			t, ok := turns[r.Turn]
			if !ok {
				t = turn{at: r.TurnAt, phase: r.Phase}
			} else if r.Phase == mission.PhaseImplement {
				t.phase = r.Phase
			}
			turns[r.Turn] = t
		}
	}

	ordered := slices.SortedFunc(maps.Values(turns), func(a, b turn) int { return a.at.Compare(b.at) })
	for i, t := range ordered {
		if t.phase != mission.PhaseImplement {
			continue
		}
		if i+1 < len(ordered) {
			return ordered[i+1].at
		}
		break
	}

	return time.Time{}
}

// responsePhase places a response, moving anything recorded once iteration
// began out of the phase its mode alone would give it.
func responsePhase(r mission.CodexResponse, iterating time.Time) mission.Phase {
	if !iterating.IsZero() && !r.At.IsZero() && !r.At.Before(iterating) {
		return mission.PhaseIterate
	}

	return r.Phase
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

	counts := codexCounts{byResponse: map[string]requestUsage{}}

	// A subagent rollout replays its parent's records before recording any of
	// its own, and the replay is indistinguishable from real work except by the
	// thread it names. Session metadata identifies the owner; legacy files without
	// metadata fall back to the first usage record's thread.
	var thread string

	scanner := bufio.NewScanner(f)
	scanner.Buffer(make([]byte, 0, 64<<10), maxLineBytes)

	for scanner.Scan() {
		var line rolloutLine
		if err := json.Unmarshal(scanner.Bytes(), &line); err != nil {
			continue
		}

		switch line.Type {
		case "session_meta":
			// Forked files may begin with their parent's usage. The file's own
			// session metadata, not the first usage record, identifies its owner.
			if line.Payload.ID != "" {
				thread = line.Payload.ID
			}
			counts.subagent = line.Payload.subagent()
		case rolloutTurnContext:
			if model := line.Payload.model(); model != "" {
				counts.model = model
			}
			counts.phase = line.Payload.phase()
			counts.effort = line.Payload.effort()
			// Codex repeats a turn's context within it, so only a new id
			// starts a turn.
			if id := line.Payload.TurnID; !counts.subagent && id != "" && id != counts.turn {
				counts.turn, counts.turnAt = id, line.observedAt()
			}
		case rolloutEventMsg:
			if limit, ok := line.Payload.limit(line.observedAt()); ok {
				counts.limit = limit
			}
		case rolloutTokenUsage:
			record := line.Payload

			if record.ResponseID == "" || record.ThreadID == "" {
				continue
			}

			if thread == "" {
				thread = record.ThreadID
			}

			if record.ThreadID != thread {
				continue
			}

			counts.recordResponse(record, line.observedAt())
		}
	}

	// A read error mid-file leaves what was counted, the same as for a
	// transcript the agent is still writing to.
	_ = scanner.Err()

	return counts, nil
}

// recordResponse keeps the original model when a response is replayed after a
// model switch, while accepting the response's latest reported token counts.
func (c *codexCounts) recordResponse(record rolloutPayload, at time.Time) {
	tokens := record.Usage.tokens()
	if tokens.Zero() {
		return
	}
	current := requestUsage{model: c.model, phase: c.phase, effort: c.effort, at: at, turn: c.turn, turnAt: c.turnAt}
	if previous, ok := c.byResponse[record.ResponseID]; ok {
		current = previous
	}
	current.tokens = tokens
	c.byResponse[record.ResponseID] = current
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
	// session_meta. Source is a client name for a session and an object
	// naming the kind for a subagent.
	ID     string          `json:"id"`
	Source json.RawMessage `json:"source"`
	// token_usage_record.
	ThreadID   string          `json:"thread_id"`
	ResponseID string          `json:"response_id"`
	Usage      codexTokenUsage `json:"usage"`

	// event_msg.
	Type       string           `json:"type"`
	RateLimits *codexRateLimits `json:"rate_limits"`

	// turn_context.
	TurnID            string `json:"turn_id"`
	Effort            string `json:"effort"`
	Model             string `json:"model"`
	CollaborationMode *struct {
		Mode     string `json:"mode"`
		Model    string `json:"model"`
		Settings *struct {
			Model           string `json:"model"`
			ReasoningEffort string `json:"reasoning_effort"`
		} `json:"settings"`
	} `json:"collaboration_mode"`
}

// subagent reports whether a session_meta record is a subagent's.
func (p rolloutPayload) subagent() bool {
	var source struct {
		Subagent json.RawMessage `json:"subagent"`
	}

	return json.Unmarshal(p.Source, &source) == nil && len(source.Subagent) != 0
}

// model returns the model a turn ran under, preferring the current nesting over
// the flatter one older rollouts used.
func (p rolloutPayload) model() string {
	if p.Model != "" {
		return p.Model
	}
	if p.CollaborationMode == nil {
		return ""
	}

	if p.CollaborationMode.Settings != nil && p.CollaborationMode.Settings.Model != "" {
		return p.CollaborationMode.Settings.Model
	}

	return p.CollaborationMode.Model
}

// phase places a turn in a phase from its collaboration mode, or "" for a turn
// that does not record one. Unlike the model, a missing mode is not carried
// over from the previous turn, because a mode change is what it would hide.
func (p rolloutPayload) phase() mission.Phase {
	if p.CollaborationMode == nil {
		return ""
	}

	switch p.CollaborationMode.Mode {
	case "":
		return ""
	case "plan":
		return mission.PhasePlan
	default:
		return mission.PhaseImplement
	}
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

func (p rolloutPayload) effort() string {
	if p.Effort != "" {
		return p.Effort
	}
	if p.CollaborationMode != nil && p.CollaborationMode.Settings != nil {
		return p.CollaborationMode.Settings.ReasoningEffort
	}
	return ""
}
