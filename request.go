package rosetta

import (
	"encoding/json"
	"fmt"
	"maps"
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
// or BudgetTokens is typically set; if both are set BudgetTokens wins on
// Anthropic (native budget) and maps to the nearest Effort elsewhere.
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

// ToolDefinition describes a callable tool (function) offered to the model.
// The SDK passes tools through verbatim; executing them is the caller's job.
type ToolDefinition struct {
	Name        string
	Description string
	// Parameters is a JSON Schema object describing arguments.
	Parameters json.RawMessage
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

// Reserved-key sets are per API family, not a global union: blocking the
// union would reject legitimate keys on requests that have no SDK-managed
// counterpart (e.g. ChatRequest.Extra["user"] — chat payloads have no user
// field, but embeddings do). Each caller passes the set for its own API.
// WithExtraOverrides(true) lifts the check for callers who really mean it.
var (
	// chatReservedPayloadKeys covers the three chat protocols' spellings
	// (OpenAI Chat, Responses, Anthropic).
	chatReservedPayloadKeys = map[string]bool{
		// request identity and transport
		"model": true, "stream": true,
		// conversation content
		"messages": true, "input": true, "system": true,
		// output caps (all three protocols' spellings)
		"max_tokens": true, "max_completion_tokens": true, "max_output_tokens": true,
		// sampling
		"temperature": true, "top_p": true, "stop": true, "stop_sequences": true,
		// tools and thinking
		"tools": true, "tool_choice": true, "stream_options": true,
		"thinking": true, "reasoning": true, "reasoning_effort": true,
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

// Float returns a pointer to v (for Temperature/TopP fields).
//
//go:fix inline
func Float(v float64) *float64 { return new(v) }

// Bool returns a pointer to v.
//
//go:fix inline
func Bool(v bool) *bool { return new(v) }

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
	if r.Temperature != nil && (*r.Temperature < 0 || *r.Temperature > 2) {
		return fmt.Errorf("%w: Temperature %v outside [0, 2]", ErrInvalidRequest, *r.Temperature)
	}
	if r.TopP != nil && (*r.TopP < 0 || *r.TopP > 1) {
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
	}
	for i, m := range r.Messages {
		if err := m.validate(); err != nil {
			return fmt.Errorf("%w: Messages[%d]: %w", ErrInvalidRequest, i, err)
		}
	}
	return nil
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
