package rosetta

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestEstimateTokens(t *testing.T) {
	tests := []struct {
		name string
		text string
		want int
	}{
		{"empty", "", 0},
		// Rounding must be conservative (ceil): 3 ASCII chars still cost a
		// token, underestimating defeats the context-window warning.
		{"short ascii", "abc", 1},
		{"four ascii", "abcd", 1},
		{"five ascii", "abcde", 2},
		{"cjk", "你好", 2},
		{"mixed", "ab你好cd", 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := EstimateTokens(tt.text); got != tt.want {
				t.Fatalf("EstimateTokens(%q) = %d, want %d", tt.text, got, tt.want)
			}
		})
	}
}

// Inline document payloads reach the wire as base64 and are priced by their
// length, not a fixed 3000-token floor; a URL reference still uses the
// default flat estimate.
func TestEstimateInputTokensInlineDocumentScalesWithPayload(t *testing.T) {
	base := &ChatRequest{Model: "m", Messages: []Message{User("hi")}}
	inline := &ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Blocks: []Block{
		{Type: BlockText, Text: "hi"},
		{Type: BlockFile, FileData: strings.Repeat("QUJD", 1000)},
	}}}}
	if got := inline.estimateInputTokens(MultimediaTokenEstimates{File: 1}); got <= base.estimateInputTokens(MultimediaTokenEstimates{}) {
		t.Fatalf("large inline document must scale with payload: %d vs %d", got, base.estimateInputTokens(MultimediaTokenEstimates{}))
	}
	// A URL reference has unknown size and must keep the coarse floor.
	withURL := &ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Blocks: []Block{
		{Type: BlockFile, FileData: "https://example.com/doc.pdf"},
	}}}}
	if got := withURL.estimateInputTokens(MultimediaTokenEstimates{}); got != 4+3000 {
		t.Fatalf("URL document estimate = %d, want 4+3000", got)
	}
}

// Sealed reasoning is replayed on later turns, so it must add to the estimate
// exactly like an Anthropic signature.
func TestEstimateInputTokensCountsSealedReasoning(t *testing.T) {
	base := &ChatRequest{Model: "m", Messages: []Message{{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockThinking, Thinking: "summary"},
	}}}}
	sealed := &ChatRequest{Model: "m", Messages: []Message{{Role: RoleAssistant, Blocks: []Block{
		{Type: BlockThinking, Thinking: "summary", Sealed: strings.Repeat("e", 4000)},
	}}}}
	if got := sealed.estimateInputTokens(MultimediaTokenEstimates{}); got <= base.estimateInputTokens(MultimediaTokenEstimates{}) {
		t.Fatalf("sealed reasoning must add to estimate: %d vs %d", got, base.estimateInputTokens(MultimediaTokenEstimates{}))
	}
}

func TestEstimateInputTokens(t *testing.T) {
	req := &ChatRequest{
		System:   "x",
		Messages: []Message{User("abcd")},
	}
	// system: 1 + 4 overhead; message: 1 + 4 overhead = 10
	if got := req.estimateInputTokens(MultimediaTokenEstimates{}); got != 10 {
		t.Fatalf("estimateInputTokens() = %d, want 10", got)
	}

	req = &ChatRequest{
		System: "abcd",
		Messages: []Message{
			User("你好"),
			{Role: RoleAssistant, Blocks: []Block{
				ToolCall("t1", "fn", `{"a":1}`),
			}},
			ToolResult("t1", "fn", "result"),
		},
	}
	// system 1+4; user 2+4; assistant tool call (7 ascii→2)+16+4; tool
	// result (6 ascii→2)+4 = 39
	if got := req.estimateInputTokens(MultimediaTokenEstimates{}); got != 39 {
		t.Fatalf("estimateInputTokens() = %d, want 39", got)
	}

	// Tool definitions add their overhead, name, description and schema.
	req = &ChatRequest{
		Messages: []Message{User("hi")},
		Tools: []ToolDefinition{
			{Name: "get_weather", Description: "current weather",
				Parameters: json.RawMessage(`{"type":"object","properties":{}}`)},
		},
	}
	// user 2+4; tool: 24 + name (11 ascii) + desc (15 ascii) + schema
	// (33 ascii) = 24 + 15 (rounded once over 61 ascii) = 39; total 44
	if got := req.estimateInputTokens(MultimediaTokenEstimates{}); got != 44 {
		t.Fatalf("estimateInputTokens() with tools = %d, want 44", got)
	}
}
