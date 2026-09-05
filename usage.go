package rosetta

import (
	"context"
	"sync"
	"time"
)

// Usage is token accounting for one request, unified across protocols.
type Usage struct {
	InputTokens  int64
	OutputTokens int64
	TotalTokens  int64
	// CachedInputTokens counts input tokens served from cache (OpenAI
	// prompt_tokens_details.cached_tokens; Anthropic cache_read_input_tokens).
	CachedInputTokens int64
	// ReasoningTokens counts tokens spent on thinking (where reported).
	ReasoningTokens int64
}

// IsZero reports whether no token accounting was reported at all.
func (u Usage) IsZero() bool {
	return u.InputTokens == 0 && u.OutputTokens == 0 && u.TotalTokens == 0
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
}

// ModelUsage aggregates usage for one key (model or protocol).
type ModelUsage struct {
	Requests          int64 `json:"requests"`
	InputTokens       int64 `json:"input_tokens"`
	OutputTokens      int64 `json:"output_tokens"`
	TotalTokens       int64 `json:"total_tokens"`
	CachedInputTokens int64 `json:"cached_input_tokens"`
	ReasoningTokens   int64 `json:"reasoning_tokens"`
	UsageMissing      int64 `json:"usage_missing"`
}

// UsageSnapshot is a point-in-time view of accumulated usage.
type UsageSnapshot struct {
	TotalRequests     int64                   `json:"total_requests"`
	InputTokens       int64                   `json:"input_tokens"`
	OutputTokens      int64                   `json:"output_tokens"`
	TotalTokens       int64                   `json:"total_tokens"`
	CachedInputTokens int64                   `json:"cached_input_tokens"`
	ReasoningTokens   int64                   `json:"reasoning_tokens"`
	UsageMissing      int64                   `json:"usage_missing"`
	ByModel           map[string]ModelUsage   `json:"by_model"`
	ByProtocol        map[Protocol]ModelUsage `json:"by_protocol"`
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

// Record accumulates one observation.
func (t *MemoryUsageTracker) Record(_ context.Context, r UsageRecord) {
	t.mu.Lock()
	defer t.mu.Unlock()
	add := func(m *ModelUsage) {
		m.Requests++
		m.InputTokens += r.Usage.InputTokens
		m.OutputTokens += r.Usage.OutputTokens
		m.TotalTokens += r.Usage.TotalTokens
		m.CachedInputTokens += r.Usage.CachedInputTokens
		m.ReasoningTokens += r.Usage.ReasoningTokens
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
		TotalRequests:     t.total.Requests,
		InputTokens:       t.total.InputTokens,
		OutputTokens:      t.total.OutputTokens,
		TotalTokens:       t.total.TotalTokens,
		CachedInputTokens: t.total.CachedInputTokens,
		ReasoningTokens:   t.total.ReasoningTokens,
		UsageMissing:      t.total.UsageMissing,
		ByModel:           make(map[string]ModelUsage, len(t.byModel)),
		ByProtocol:        make(map[Protocol]ModelUsage, len(t.byProto)),
	}
	for k, v := range t.byModel {
		snap.ByModel[k] = v
	}
	for k, v := range t.byProto {
		snap.ByProtocol[k] = v
	}
	return snap
}
