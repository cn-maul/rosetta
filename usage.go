package rosetta

import (
	"context"
	"maps"
	"sync"
	"time"
)

// Usage is token accounting for one request, unified across protocols.
type Usage struct {
	InputTokens  int64 `json:"input_tokens"`
	OutputTokens int64 `json:"output_tokens"`
	TotalTokens  int64 `json:"total_tokens"`
	// CachedInputTokens counts input tokens served from cache (OpenAI
	// prompt_tokens_details.cached_tokens; Anthropic cache_read_input_tokens).
	CachedInputTokens int64 `json:"cached_input_tokens"`
	// CachedCreationTokens counts input tokens written into the cache by
	// this request (Anthropic cache_creation_input_tokens). The OpenAI
	// protocols cache prefixes automatically and report no separate write
	// figure, so it stays zero there.
	CachedCreationTokens int64 `json:"cached_creation_tokens"`
	// ReasoningTokens counts tokens spent on thinking (where reported).
	ReasoningTokens int64 `json:"reasoning_tokens"`
	// AudioInputTokens / AudioOutputTokens count audio tokens billed in the
	// prompt / completion (OpenAI audio models).
	AudioInputTokens  int64 `json:"audio_input_tokens,omitempty"`
	AudioOutputTokens int64 `json:"audio_output_tokens,omitempty"`
	// AcceptedPredictionTokens / RejectedPredictionTokens are speculative
	// decoding accounting from OpenAI predicted outputs.
	AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens int64 `json:"rejected_prediction_tokens,omitempty"`
	// CacheCreation5mTokens / CacheCreation1hTokens split Anthropic's
	// cache_creation_input_tokens by the TTL tier that produced them.
	CacheCreation5mTokens int64 `json:"cache_creation_5m_tokens,omitempty"`
	CacheCreation1hTokens int64 `json:"cache_creation_1h_tokens,omitempty"`
	// CacheCreation30mTokens / CacheCreation12hTokens split OpenAI's
	// cache_write_tokens_details by TTL (Responses only).
	CacheCreation30mTokens int64 `json:"cache_creation_30m_tokens,omitempty"`
	CacheCreation12hTokens int64 `json:"cache_creation_12h_tokens,omitempty"`
	// CacheHitTokens / CacheMissTokens are DeepSeek's split of prompt
	// tokens; CachedInputTokens mirrors CacheHitTokens there.
	CacheHitTokens  int64 `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens int64 `json:"cache_miss_tokens,omitempty"`
}

// IsZero reports whether no token accounting was reported at all.
//
// The cache and reasoning fields count: a response that reports only cache
// activity (or only thinking tokens) did report usage, and classifying it as
// "no usage" would inflate UsageMissing and drop the record's real numbers.
func (u Usage) IsZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0 &&
		u.CachedInputTokens == 0 && u.CachedCreationTokens == 0 && u.ReasoningTokens == 0 &&
		u.AudioInputTokens == 0 && u.AudioOutputTokens == 0 &&
		u.AcceptedPredictionTokens == 0 && u.RejectedPredictionTokens == 0 &&
		u.CacheCreation5mTokens == 0 && u.CacheCreation1hTokens == 0 &&
		u.CacheCreation30mTokens == 0 && u.CacheCreation12hTokens == 0 &&
		u.CacheHitTokens == 0 && u.CacheMissTokens == 0
}

// UsageRecord is one observation fed to a UsageTracker.
type UsageRecord struct {
	Time     time.Time
	Protocol Protocol
	Model    string
	Usage    Usage
	// UsageMissing marks responses where the provider returned no usage
	// data at all (common among third-party compatible services).
	UsageMissing bool
	// Latency is the wall-clock duration of the call. For streams it is
	// the time until the stream ended (or was closed).
	Latency time.Duration
	// Attempts is the number of HTTP attempts the transport made. 0 means
	// the caller did not supply it.
	Attempts int
	// StatusCode is the final HTTP status (200 on success).
	StatusCode int
	// RequestID is the provider's request id, useful for support tickets.
	RequestID string
	// Err is the terminal error, if the call failed.
	Err error
}

// ModelUsage aggregates usage for one key (model or protocol).
type ModelUsage struct {
	Requests                 int64 `json:"requests"`
	InputTokens              int64 `json:"input_tokens"`
	OutputTokens             int64 `json:"output_tokens"`
	TotalTokens              int64 `json:"total_tokens"`
	CachedInputTokens        int64 `json:"cached_input_tokens"`
	CachedCreationTokens     int64 `json:"cached_creation_tokens"`
	ReasoningTokens          int64 `json:"reasoning_tokens"`
	AudioInputTokens         int64 `json:"audio_input_tokens,omitempty"`
	AudioOutputTokens        int64 `json:"audio_output_tokens,omitempty"`
	AcceptedPredictionTokens int64 `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens int64 `json:"rejected_prediction_tokens,omitempty"`
	CacheCreation5mTokens    int64 `json:"cache_creation_5m_tokens,omitempty"`
	CacheCreation1hTokens    int64 `json:"cache_creation_1h_tokens,omitempty"`
	CacheCreation30mTokens   int64 `json:"cache_creation_30m_tokens,omitempty"`
	CacheCreation12hTokens   int64 `json:"cache_creation_12h_tokens,omitempty"`
	CacheHitTokens           int64 `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens          int64 `json:"cache_miss_tokens,omitempty"`
	UsageMissing             int64 `json:"usage_missing"`
}

// UsageSnapshot is a point-in-time view of accumulated usage.
type UsageSnapshot struct {
	TotalRequests            int64                   `json:"total_requests"`
	InputTokens              int64                   `json:"input_tokens"`
	OutputTokens             int64                   `json:"output_tokens"`
	TotalTokens              int64                   `json:"total_tokens"`
	CachedInputTokens        int64                   `json:"cached_input_tokens"`
	CachedCreationTokens     int64                   `json:"cached_creation_tokens"`
	ReasoningTokens          int64                   `json:"reasoning_tokens"`
	AudioInputTokens         int64                   `json:"audio_input_tokens,omitempty"`
	AudioOutputTokens        int64                   `json:"audio_output_tokens,omitempty"`
	AcceptedPredictionTokens int64                   `json:"accepted_prediction_tokens,omitempty"`
	RejectedPredictionTokens int64                   `json:"rejected_prediction_tokens,omitempty"`
	CacheCreation5mTokens    int64                   `json:"cache_creation_5m_tokens,omitempty"`
	CacheCreation1hTokens    int64                   `json:"cache_creation_1h_tokens,omitempty"`
	CacheCreation30mTokens   int64                   `json:"cache_creation_30m_tokens,omitempty"`
	CacheCreation12hTokens   int64                   `json:"cache_creation_12h_tokens,omitempty"`
	CacheHitTokens           int64                   `json:"cache_hit_tokens,omitempty"`
	CacheMissTokens          int64                   `json:"cache_miss_tokens,omitempty"`
	UsageMissing             int64                   `json:"usage_missing"`
	ByModel                  map[string]ModelUsage   `json:"by_model"`
	ByProtocol               map[Protocol]ModelUsage `json:"by_protocol"`
}

// UsageTracker receives usage observations. Implementations must be safe
// for concurrent use. Record must not block the request path for long.
type UsageTracker interface {
	Record(ctx context.Context, r UsageRecord)
	Snapshot() UsageSnapshot
}

// MemoryUsageTracker is an in-memory UsageTracker, safe for concurrent use.
// It is the default tracker wired by WithUsageTracker; persistence is the
// caller's concern (implement the interface against a store of choice).
type MemoryUsageTracker struct {
	mu      sync.Mutex
	total   ModelUsage
	byModel map[string]ModelUsage
	byProto map[Protocol]ModelUsage
}

// NewMemoryUsageTracker returns an empty in-memory tracker.
func NewMemoryUsageTracker() *MemoryUsageTracker {
	return &MemoryUsageTracker{
		byModel: make(map[string]ModelUsage),
		byProto: make(map[Protocol]ModelUsage),
	}
}

// maxAggregateTokens is the per-field ceiling a single observation may
// contribute before saturation. A provider reporting a total near MaxInt64
// would otherwise make `+=` in Record wrap the running aggregate negative
// (audit C21); clamping the input bounds keeps every sum monotonic.
const maxAggregateTokens = 1 << 40

// clampTokens bounds one token count to [0, maxAggregateTokens]: negatives
// (which no provider should send, but a hostile or buggy one can) and
// absurd magnitudes are both folded back into range.
func clampTokens(v int64) int64 {
	if v < 0 {
		return 0
	}
	if v > maxAggregateTokens {
		return maxAggregateTokens
	}
	return v
}

// Record accumulates one observation. A zero-value MemoryUsageTracker is
// usable directly: the maps are lazily created here so the exported type
// does not require NewMemoryUsageTracker (audit B10).
func (t *MemoryUsageTracker) Record(_ context.Context, r UsageRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if t.byModel == nil {
		t.byModel = make(map[string]ModelUsage)
	}
	if t.byProto == nil {
		t.byProto = make(map[Protocol]ModelUsage)
	}
	r.Usage.InputTokens = clampTokens(r.Usage.InputTokens)
	r.Usage.OutputTokens = clampTokens(r.Usage.OutputTokens)
	r.Usage.TotalTokens = clampTokens(r.Usage.TotalTokens)
	r.Usage.CachedInputTokens = clampTokens(r.Usage.CachedInputTokens)
	r.Usage.CachedCreationTokens = clampTokens(r.Usage.CachedCreationTokens)
	r.Usage.ReasoningTokens = clampTokens(r.Usage.ReasoningTokens)
	r.Usage.AudioInputTokens = clampTokens(r.Usage.AudioInputTokens)
	r.Usage.AudioOutputTokens = clampTokens(r.Usage.AudioOutputTokens)
	r.Usage.AcceptedPredictionTokens = clampTokens(r.Usage.AcceptedPredictionTokens)
	r.Usage.RejectedPredictionTokens = clampTokens(r.Usage.RejectedPredictionTokens)
	r.Usage.CacheCreation5mTokens = clampTokens(r.Usage.CacheCreation5mTokens)
	r.Usage.CacheCreation1hTokens = clampTokens(r.Usage.CacheCreation1hTokens)
	r.Usage.CacheCreation30mTokens = clampTokens(r.Usage.CacheCreation30mTokens)
	r.Usage.CacheCreation12hTokens = clampTokens(r.Usage.CacheCreation12hTokens)
	r.Usage.CacheHitTokens = clampTokens(r.Usage.CacheHitTokens)
	r.Usage.CacheMissTokens = clampTokens(r.Usage.CacheMissTokens)
	add := func(m *ModelUsage) {
		m.Requests++
		m.InputTokens += r.Usage.InputTokens
		m.OutputTokens += r.Usage.OutputTokens
		m.TotalTokens += r.Usage.TotalTokens
		m.CachedInputTokens += r.Usage.CachedInputTokens
		m.CachedCreationTokens += r.Usage.CachedCreationTokens
		m.ReasoningTokens += r.Usage.ReasoningTokens
		m.AudioInputTokens += r.Usage.AudioInputTokens
		m.AudioOutputTokens += r.Usage.AudioOutputTokens
		m.AcceptedPredictionTokens += r.Usage.AcceptedPredictionTokens
		m.RejectedPredictionTokens += r.Usage.RejectedPredictionTokens
		m.CacheCreation5mTokens += r.Usage.CacheCreation5mTokens
		m.CacheCreation1hTokens += r.Usage.CacheCreation1hTokens
		m.CacheCreation30mTokens += r.Usage.CacheCreation30mTokens
		m.CacheCreation12hTokens += r.Usage.CacheCreation12hTokens
		m.CacheHitTokens += r.Usage.CacheHitTokens
		m.CacheMissTokens += r.Usage.CacheMissTokens
		if r.UsageMissing {
			m.UsageMissing++
		}
	}
	add(&t.total)
	m := t.byModel[r.Model]
	add(&m)
	t.byModel[r.Model] = m
	p := t.byProto[r.Protocol]
	add(&p)
	t.byProto[r.Protocol] = p
}

// Snapshot returns a deep copy of the accumulated statistics.
func (t *MemoryUsageTracker) Snapshot() UsageSnapshot {
	t.mu.Lock()
	defer t.mu.Unlock()
	snap := UsageSnapshot{
		TotalRequests:            t.total.Requests,
		InputTokens:              t.total.InputTokens,
		OutputTokens:             t.total.OutputTokens,
		TotalTokens:              t.total.TotalTokens,
		CachedInputTokens:        t.total.CachedInputTokens,
		CachedCreationTokens:     t.total.CachedCreationTokens,
		ReasoningTokens:          t.total.ReasoningTokens,
		AudioInputTokens:         t.total.AudioInputTokens,
		AudioOutputTokens:        t.total.AudioOutputTokens,
		AcceptedPredictionTokens: t.total.AcceptedPredictionTokens,
		RejectedPredictionTokens: t.total.RejectedPredictionTokens,
		CacheCreation5mTokens:    t.total.CacheCreation5mTokens,
		CacheCreation1hTokens:    t.total.CacheCreation1hTokens,
		CacheCreation30mTokens:   t.total.CacheCreation30mTokens,
		CacheCreation12hTokens:   t.total.CacheCreation12hTokens,
		CacheHitTokens:           t.total.CacheHitTokens,
		CacheMissTokens:          t.total.CacheMissTokens,
		UsageMissing:             t.total.UsageMissing,
		ByModel:                  make(map[string]ModelUsage, len(t.byModel)),
		ByProtocol:               make(map[Protocol]ModelUsage, len(t.byProto)),
	}
	maps.Copy(snap.ByModel, t.byModel)
	maps.Copy(snap.ByProtocol, t.byProto)
	return snap
}
