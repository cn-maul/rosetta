package rosetta

import (
	"encoding/json"
	"strings"
)

// StopReason unifies why generation ended.
type StopReason string

const (
	StopEnd           StopReason = "end"
	StopLength        StopReason = "length"
	StopToolUse       StopReason = "tool_use"
	StopContentFilter StopReason = "content_filter"
	StopRefusal       StopReason = "refusal"
	StopOther         StopReason = "other"
)

// ChatResponse is a unified non-streaming completion result. Content holds
// the generated blocks in provider order: typically text, possibly
// interleaved with thinking and tool_call blocks.
type ChatResponse struct {
	ID         string
	Model      string
	Content    []Block
	StopReason StopReason
	Usage      Usage
	Raw        json.RawMessage
	// SystemFingerprint identifies the backend configuration that served
	// the request (OpenAI protocols; empty elsewhere).
	SystemFingerprint string
	// ServiceTier echoes the service tier the provider actually used.
	ServiceTier string
	// Logprobs is the provider's raw log-probability payload when
	// requested. It is deliberately not modelled: the shape differs per
	// protocol and per provider, and losing it entirely is worse.
	Logprobs json.RawMessage
	// Unknown holds provider output items the SDK does not model — server
	// tool calls and results (web_search_call, code_interpreter_call,
	// server_tool_use, *_tool_result), image generation results, and any
	// future item type. Each entry keeps the provider's raw JSON in Raw,
	// so a gateway can forward it rather than silently dropping it.
	Unknown []Block
}

// Text concatenates all text blocks of the response.
func (r *ChatResponse) Text() string {
	var b strings.Builder
	for _, blk := range r.Content {
		if blk.Type == BlockText {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(blk.Text)
		}
	}
	return b.String()
}

// ThinkingText concatenates all thinking blocks.
func (r *ChatResponse) ThinkingText() string {
	var b strings.Builder
	for _, blk := range r.Content {
		if blk.Type == BlockThinking {
			if b.Len() > 0 {
				b.WriteString("\n")
			}
			b.WriteString(blk.Thinking)
		}
	}
	return b.String()
}

// ToolCalls returns all tool_call blocks in the response.
func (r *ChatResponse) ToolCalls() []Block {
	var out []Block
	for _, blk := range r.Content {
		if blk.Type == BlockToolCall {
			out = append(out, blk)
		}
	}
	return out
}
