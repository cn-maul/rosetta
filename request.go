package rosetta

import (
	"encoding/json"
	"fmt"
	"maps"
	"math"
)

// Effort is a protocol-independent thinking dial.
type Effort string

const (
	EffortUnset  Effort = ""
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
)

// ThinkingConfig requests reasoning from the model. Exactly one of Effort
// or BudgetTokens is typically set. When both are set the precedence is
// protocol-specific and fixed: Anthropic takes BudgetTokens as the native
// budget, while the OpenAI protocols — which accept only a coarse level —
// take the explicit Effort (see effort()).
type ThinkingConfig struct {
	// Effort is a coarse dial: low/medium/high. OpenAI Chat sends it as
	// reasoning_effort; Responses as reasoning.effort; Anthropic maps it
	// to budget_tokens (≈2048/8192/32768, clamped to max_tokens-1024).
	Effort Effort
	// BudgetTokens is an explicit thinking budget. Anthropic sends the
	// value as-is (minimum 1024); OpenAI protocols map to the nearest
	// Effort level.
	BudgetTokens int
}

// effortFromBudget maps an explicit token budget to the nearest Effort.
func effortFromBudget(n int) Effort {
	switch {
	case n <= 4096:
		return EffortLow
	case n <= 16384:
		return EffortMedium
	default:
		return EffortHigh
	}
}

// anthropicBudget maps an Effort to an Anthropic budget_tokens value.
func anthropicBudget(e Effort) int {
	switch e {
	case EffortLow:
		return 2048
	case EffortMedium:
		return 8192
	default:
		return 32768
	}
}

// CacheControl marks a prompt-cache breakpoint on a content block or tool.
// Anthropic stores everything from the start of the prompt up to and
// including the marked item, then reuses it on later requests that share
// the same prefix; it is how you build a cache hit on that protocol. The
// OpenAI protocols cache prefixes automatically and ignore this, so it has
// no effect there. Anthropic accepts up to four breakpoints per request and
// requires each cached region to be at least 1024 (Sonnet) / 2048 (Haiku
// class) tokens — shorter prefixes are simply not cached upstream.
type CacheControl struct {
	// Type is the cache strategy; empty defaults to Anthropic's only
	// supported value, "ephemeral".
	Type string
	// TTL is the cache lifetime: "5m" (default, refreshed on each hit) or
	// "1h" (extended cache, billed at a higher write rate). Empty uses the
	// provider default.
	TTL string
}

// EphemeralCache returns a default (5-minute) Anthropic cache breakpoint.
func EphemeralCache() *CacheControl { return &CacheControl{} }

// ExtendedCache returns a 1-hour Anthropic cache breakpoint.
func ExtendedCache() *CacheControl { return &CacheControl{TTL: "1h"} }

// validate checks the breakpoint's Type and TTL, which Anthropic constrains
// to "ephemeral" and "5m"/"1h" respectively.
func (c *CacheControl) validate() error {
	switch c.Type {
	case "", "ephemeral":
	default:
		return fmt.Errorf("cache type %q unsupported (Anthropic only accepts \"ephemeral\")", c.Type)
	}
	switch c.TTL {
	case "", "5m", "1h":
		return nil
	default:
		return fmt.Errorf("cache TTL %q unsupported (use \"5m\" or \"1h\")", c.TTL)
	}
}

// toWire renders Anthropic's cache_control object, or nil when the receiver
// is nil (callers use the nil result to mean "no breakpoint").
func (c *CacheControl) toWire() map[string]any {
	if c == nil {
		return nil
	}
	typ := c.Type
	if typ == "" {
		typ = "ephemeral"
	}
	out := map[string]any{"type": typ}
	if c.TTL != "" {
		out["ttl"] = c.TTL
	}
	return out
}

// ToolDefinition describes a callable tool (function) offered to the model.
// The SDK passes tools through verbatim; executing them is the caller's job.
type ToolDefinition struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object describing arguments.
	Parameters json.RawMessage
	// CacheControl marks an Anthropic cache breakpoint after this tool,
	// caching the whole tool set that precedes it (usually set on the last
	// tool). Ignored by the OpenAI protocols.
	CacheControl *CacheControl
}

// ChatRequest is a protocol-independent chat completion request.
type ChatRequest struct {
	// Model is the provider model id, e.g. "gpt-4o", "deepseek-chat",
	// "claude-sonnet-4-5".
	Model string
	// Messages is the conversation so far, in order.
	Messages []Message
	// System is a convenience top-level system prompt. It is merged ahead
	// of any RoleSystem messages (OpenAI: system message; Anthropic: the
	// top-level system field).
	System string

	// MaxOutputTokens caps generated tokens. When zero, the client's
	// WithDefaultMaxOutputTokens applies; Anthropic (which requires the
	// field) falls back to 4096.
	MaxOutputTokens int
	Temperature     *float64
	TopP            *float64
	StopSequences   []string

	// Tools offered to the model; results come back as BlockToolCall.
	Tools []ToolDefinition

	// Thinking requests reasoning; nil disables it (protocol default).
	Thinking *ThinkingConfig

	// Extra is merged into the protocol payload last, letting callers
	// reach provider-specific fields the SDK does not model. Values must
	// be JSON-marshalable. Keys that collide with SDK-managed payload
	// fields are rejected with ErrInvalidRequest unless
	// WithExtraOverrides(true) is set.
	Extra map[string]any
}

// Reserved-key sets are per protocol, matching exactly the top-level fields
// that each adapter's payload builder writes: blocking a key the adapter
// never sets (e.g. "tool_choice" on Anthropic, "input" on OpenAI Chat) would
// reject a legitimate Extra passthrough. Each caller passes the set for its
// own protocol. WithExtraOverrides(true) lifts the check entirely.
var (
	// openaiChatReservedPayloadKeys covers POST /chat/completions. Both
	// output-cap spellings are reserved because the probe picks one at
	// runtime (max_completion_tokens, or max_tokens on legacy services).
	openaiChatReservedPayloadKeys = map[string]bool{
		"model": true, "messages": true, "stream": true,
		"max_tokens": true, "max_completion_tokens": true,
		"temperature": true, "top_p": true, "stop": true,
		"tools": true, "reasoning_effort": true, "stream_options": true,
	}
	// openaiResponsesReservedPayloadKeys covers POST /responses.
	openaiResponsesReservedPayloadKeys = map[string]bool{
		"model": true, "input": true, "stream": true,
		"max_output_tokens": true,
		"temperature":       true, "top_p": true,
		"tools": true, "reasoning": true,
	}
	// anthropicReservedPayloadKeys covers POST /v1/messages.
	anthropicReservedPayloadKeys = map[string]bool{
		"model": true, "messages": true, "system": true, "stream": true,
		"max_tokens":  true,
		"temperature": true, "top_p": true, "stop_sequences": true,
		"tools": true, "thinking": true,
	}
	// embeddingsReservedPayloadKeys covers POST /embeddings payloads.
	embeddingsReservedPayloadKeys = map[string]bool{
		"model": true, "input": true, "dimensions": true, "user": true,
		"encoding_format": true,
	}
	// rerankReservedPayloadKeys covers POST /rerank (Cohere format).
	rerankReservedPayloadKeys = map[string]bool{
		"model": true, "query": true, "documents": true, "top_n": true,
		"return_documents": true,
	}
)

// mergeExtra copies Extra into the payload, enforcing the reserved-key
// rule for the given API family unless overrides are explicitly enabled.
func mergeExtra(dst map[string]any, extra map[string]any, allowOverride bool, reserved map[string]bool) error {
	if len(extra) == 0 {
		return nil
	}
	if !allowOverride {
		for k := range extra {
			if reserved[k] {
				return fmt.Errorf("%w: Extra key %q collides with an SDK-managed field (pass WithExtraOverrides(true) to override anyway)", ErrInvalidRequest, k)
			}
		}
	}
	maps.Copy(dst, extra)
	return nil
}

// wireSliceLen reports the element count of a payload value that is (or
// JSON-normalizes to) an array; ok is false when the value is absent or not
// an array. After a WithExtraOverrides replacement the value can be any JSON
// shape — a []any parsed from Extra, not just the SDK's []string — so a plain
// type switch would silently ignore the override and validate against a stale
// count (audit C20).
func wireSliceLen(v any) (int, bool) {
	if v == nil {
		return 0, false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, false
	}
	var arr []json.RawMessage
	if err := json.Unmarshal(raw, &arr); err != nil {
		return 0, false
	}
	return len(arr), true
}

// wireInt coerces a payload value to an int through JSON, so an override of a
// numeric field arriving as a float64 (from json) is still read.
func wireInt(v any) (int, bool) {
	if v == nil {
		return 0, false
	}
	raw, err := json.Marshal(v)
	if err != nil {
		return 0, false
	}
	var n int64
	if err := json.Unmarshal(raw, &n); err != nil {
		return 0, false
	}
	return int(n), true
}

// Float returns a pointer to v (for Temperature/TopP fields).
//
//go:fix inline
func Float(v float64) *float64 { return new(v) }

// Bool returns a pointer to v.
//
//go:fix inline
func Bool(v bool) *bool { return new(v) }

// maxWireInt bounds token-count fields that reach a provider's JSON body.
// Any real context window is orders of magnitude smaller; the cap exists to
// stop absurd values from wrapping int arithmetic (audit B14). 2^30 is chosen
// so the Anthropic plan's `budget + 4096` growth stays positive even on a
// 32-bit int.
const maxWireInt = 1 << 30

// validate checks structural requirements shared by all protocols, so
// obviously broken requests fail locally with ErrInvalidRequest instead of
// surfacing as provider-specific 400s (or worse, silently degraded
// payloads) that differ across protocols.
func (r *ChatRequest) validate() error {
	if r == nil {
		return fmt.Errorf("%w: nil request", ErrInvalidRequest)
	}
	if r.Model == "" {
		return fmt.Errorf("%w: Model is required", ErrInvalidRequest)
	}
	if len(r.Messages) == 0 && r.System == "" {
		return fmt.Errorf("%w: Messages must not be empty", ErrInvalidRequest)
	}
	if r.MaxOutputTokens < 0 {
		return fmt.Errorf("%w: MaxOutputTokens must not be negative", ErrInvalidRequest)
	}
	if r.MaxOutputTokens > maxWireInt {
		return fmt.Errorf("%w: MaxOutputTokens %d exceeds the supported maximum %d", ErrInvalidRequest, r.MaxOutputTokens, maxWireInt)
	}
	if r.Temperature != nil && !inRange(*r.Temperature, 0, 2) {
		return fmt.Errorf("%w: Temperature %v outside [0, 2]", ErrInvalidRequest, *r.Temperature)
	}
	if r.TopP != nil && !inRange(*r.TopP, 0, 1) {
		return fmt.Errorf("%w: TopP %v outside [0, 1]", ErrInvalidRequest, *r.TopP)
	}
	if r.Thinking != nil {
		switch r.Thinking.Effort {
		case EffortUnset, EffortLow, EffortMedium, EffortHigh:
		default:
			return fmt.Errorf("%w: unknown Thinking.Effort %q", ErrInvalidRequest, r.Thinking.Effort)
		}
		if r.Thinking.BudgetTokens < 0 {
			return fmt.Errorf("%w: Thinking.BudgetTokens must not be negative", ErrInvalidRequest)
		}
		if r.Thinking.BudgetTokens > maxWireInt {
			return fmt.Errorf("%w: Thinking.BudgetTokens %d exceeds the supported maximum %d", ErrInvalidRequest, r.Thinking.BudgetTokens, maxWireInt)
		}
	}
	for i, s := range r.StopSequences {
		if s == "" {
			return fmt.Errorf("%w: StopSequences[%d] is empty", ErrInvalidRequest, i)
		}
	}
	for i, m := range r.Messages {
		if err := m.validate(); err != nil {
			return fmt.Errorf("%w: Messages[%d]: %w", ErrInvalidRequest, i, err)
		}
	}
	seenTool := make(map[string]bool, len(r.Tools))
	for i, t := range r.Tools {
		if t.Name == "" {
			return fmt.Errorf("%w: Tools[%d].Name must not be empty", ErrInvalidRequest, i)
		}
		if seenTool[t.Name] {
			return fmt.Errorf("%w: duplicate tool name %q", ErrInvalidRequest, t.Name)
		}
		seenTool[t.Name] = true
		if len(t.Parameters) > 0 && !json.Valid(t.Parameters) {
			return fmt.Errorf("%w: Tools[%d].Parameters is not valid JSON", ErrInvalidRequest, i)
		}
		if t.CacheControl != nil {
			if err := t.CacheControl.validate(); err != nil {
				return fmt.Errorf("%w: Tools[%d]: %w", ErrInvalidRequest, i, err)
			}
		}
	}
	if len(r.Extra) > 0 {
		// A non-serializable Extra (NaN, a channel, a self-referencing map)
		// fails later inside json.Marshal and surfaces as a TransportError
		// naming a full URL, so probe it here where it is an ErrInvalidRequest.
		if _, err := json.Marshal(r.Extra); err != nil {
			return fmt.Errorf("%w: Extra is not JSON-serializable: %v", ErrInvalidRequest, err)
		}
	}
	return nil
}

// inRange reports whether v is finite and within [lo, hi]. NaN and the
// infinities compare false against every bound, so they must be rejected
// explicitly — otherwise they slip past validation and json.Marshal later
// rejects them, misclassifying a local programming error as a transport
// failure (audit B13).
func inRange(v, lo, hi float64) bool {
	return !math.IsNaN(v) && !math.IsInf(v, 0) && v >= lo && v <= hi
}

// effort resolves the effective effort dial for the request.
func (r *ChatRequest) effort() Effort {
	if r.Thinking == nil {
		return EffortUnset
	}
	if r.Thinking.Effort != EffortUnset {
		return r.Thinking.Effort
	}
	if r.Thinking.BudgetTokens > 0 {
		return effortFromBudget(r.Thinking.BudgetTokens)
	}
	// Thinking requested without a level: default to medium.
	return EffortMedium
}
