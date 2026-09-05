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
