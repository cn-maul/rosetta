package rosetta

import "testing"

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

func TestEstimateInputTokens(t *testing.T) {
	req := &ChatRequest{
		System:   "x",
		Messages: []Message{User("abcd")},
	}
	// system: 1 + 4 overhead; message: 1 + 4 overhead = 10
	if got := req.estimateInputTokens(); got != 10 {
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
	if got := req.estimateInputTokens(); got != 39 {
		t.Fatalf("estimateInputTokens() = %d, want 39", got)
	}
}
