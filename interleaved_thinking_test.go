package rosetta

// Interleaved-thinking beta header (audit C22, resolved against Anthropic's
// extended-thinking docs on 2026-09-20).
//
// "interleaved-thinking-2025-05-14" is what lets a model reason between tool
// calls inside one assistant turn. Anthropic requires it on Claude Opus 4.5,
// Sonnet 4.5 and earlier Claude 4 models; the adaptive-thinking generation
// (Opus 4.6+, Sonnet 5) ignores it as deprecated, Haiku 4.5 ignores it too,
// and the Claude API accepts it for any model without erroring. Bedrock and
// Vertex AI are the exception — they reject the header on models outside
// Anthropic's whitelist, which is why WithInterleavedThinking(false) exists.
//
// These tests pin the three things that matter: the payload probe, the
// tri-state switch, and the fact that two betas join into one header instead
// of overwriting each other.

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

func TestPayloadNeedsInterleavedThinking(t *testing.T) {
	tools := []map[string]any{{"name": "t", "input_schema": map[string]any{"type": "object"}}}
	tests := []struct {
		name string
		v    any
		want bool
	}{
		{"nil", nil, false},
		{"not a map", "thinking", false},
		{"tools without thinking", map[string]any{"tools": tools}, false},
		{"thinking without tools", map[string]any{"thinking": map[string]any{"type": "enabled"}}, false},
		{"thinking with empty tools", map[string]any{
			"thinking": map[string]any{"type": "enabled"},
			"tools":    []map[string]any{},
		}, false},
		{"thinking with tools", map[string]any{
			"thinking": map[string]any{"type": "enabled"},
			"tools":    tools,
		}, true},
		{"Extra-shaped tools []any", map[string]any{
			"thinking": map[string]any{"type": "enabled"},
			"tools":    []any{map[string]any{"name": "t"}},
		}, true},
		{"empty Extra-shaped tools", map[string]any{
			"thinking": map[string]any{"type": "enabled"},
			"tools":    []any{},
		}, false},
		{"tools of an unexpected type", map[string]any{
			"thinking": map[string]any{"type": "enabled"},
			"tools":    "not-a-list",
		}, false},
	}
	for _, tc := range tests {
		if got := payloadNeedsInterleavedThinking(tc.v); got != tc.want {
			t.Errorf("%s: payloadNeedsInterleavedThinking = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// The header follows what actually reaches the wire: thinking plus tools
// request it on their own, and the option pins the answer either way.
func TestInterleavedThinkingBetaHeader(t *testing.T) {
	tool := ToolDefinition{Name: "get_weather"}

	tests := []struct {
		name string
		req  *ChatRequest
		opt  *bool
		want string
	}{
		{
			"thinking plus tools requests it",
			&ChatRequest{Model: "claude-x", Messages: []Message{User("hi")},
				Thinking: &ThinkingConfig{BudgetTokens: 2048}, Tools: []ToolDefinition{tool}},
			nil, interleavedThinkingBeta,
		},
		{
			"thinking alone does not",
			&ChatRequest{Model: "claude-x", Messages: []Message{User("hi")},
				Thinking: &ThinkingConfig{BudgetTokens: 2048}},
			nil, "",
		},
		{
			"tools alone do not",
			&ChatRequest{Model: "claude-x", Messages: []Message{User("hi")},
				Tools: []ToolDefinition{tool}},
			nil, "",
		},
		{
			"forced on without thinking",
			&ChatRequest{Model: "claude-x", Messages: []Message{User("hi")}},
			boolPtr(true), interleavedThinkingBeta,
		},
		{
			"forced off in spite of the payload",
			&ChatRequest{Model: "claude-x", Messages: []Message{User("hi")},
				Thinking: &ThinkingConfig{BudgetTokens: 2048}, Tools: []ToolDefinition{tool}},
			boolPtr(false), "",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var betas []string
			var mu sync.Mutex
			srv := recordingAnthropicServer(t, &betas, &mu)
			var opts []Option
			if tc.opt != nil {
				opts = append(opts, WithInterleavedThinking(*tc.opt))
			}
			c := newCacheTestClient(t, srv, opts...)
			if _, err := c.Chat(context.Background(), tc.req); err != nil {
				t.Fatalf("chat: %v", err)
			}
			mu.Lock()
			got := betas[len(betas)-1]
			mu.Unlock()
			if got != tc.want {
				t.Fatalf("anthropic-beta = %q, want %q", got, tc.want)
			}
		})
	}
}

// A request that needs both betas advertises both. Setting them one at a time
// would leave whichever came first silently missing, and Anthropic rejects
// the request for the beta that vanished.
func TestBothBetasJoinIntoOneHeader(t *testing.T) {
	marker := func() *CacheControl { return ExtendedCache() }
	tool := ToolDefinition{Name: "t", CacheControl: marker()}

	tests := []struct {
		name string
		req  *ChatRequest
		opt  *bool
		want string
	}{
		{
			"cache ttl and interleaved together",
			&ChatRequest{Model: "claude-x", Messages: []Message{{Role: RoleUser, Blocks: []Block{
				{Type: BlockText, Text: "seg", CacheControl: marker()},
			}}}, Thinking: &ThinkingConfig{BudgetTokens: 2048}, Tools: []ToolDefinition{tool}},
			nil,
			extendedCacheTTLBeta + "," + interleavedThinkingBeta,
		},
		{
			"cache ttl without tools keeps only its own beta",
			&ChatRequest{Model: "claude-x", Messages: []Message{{Role: RoleUser, Blocks: []Block{
				{Type: BlockText, Text: "seg", CacheControl: marker()},
			}}}, Thinking: &ThinkingConfig{BudgetTokens: 2048}},
			nil,
			extendedCacheTTLBeta,
		},
		{
			"pinning interleaved off leaves the cache beta intact",
			&ChatRequest{Model: "claude-x", Messages: []Message{{Role: RoleUser, Blocks: []Block{
				{Type: BlockText, Text: "seg", CacheControl: marker()},
			}}}, Thinking: &ThinkingConfig{BudgetTokens: 2048}, Tools: []ToolDefinition{tool}},
			boolPtr(false),
			extendedCacheTTLBeta,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var betas []string
			var mu sync.Mutex
			srv := recordingAnthropicServer(t, &betas, &mu)
			var opts []Option
			if tc.opt != nil {
				opts = append(opts, WithInterleavedThinking(*tc.opt))
			}
			c := newCacheTestClient(t, srv, opts...)
			if _, err := c.Chat(context.Background(), tc.req); err != nil {
				t.Fatalf("chat: %v", err)
			}
			mu.Lock()
			got := betas[len(betas)-1]
			mu.Unlock()
			if got != tc.want {
				t.Fatalf("anthropic-beta = %q, want %q", got, tc.want)
			}
		})
	}
}

// The streaming path derives headers from the same payload, so it must send
// the beta too — a streaming agent loop is exactly the case the beta covers.
func TestInterleavedThinkingBetaOnStream(t *testing.T) {
	var betas []string
	var mu sync.Mutex
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		betas = append(betas, r.Header.Get("anthropic-beta"))
		mu.Unlock()
		// A 200 that is not event-stream is replayed as a buffered stream;
		// the header is what this test is about.
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(anthropicOKBody))
	}))
	defer srv.Close()

	c := newCacheTestClient(t, srv)
	s, err := c.ChatStream(context.Background(), &ChatRequest{
		Model: "claude-x", Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{BudgetTokens: 2048},
		Tools:    []ToolDefinition{{Name: "t"}},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	for s.Next() {
	}
	if err := s.Err(); err != nil {
		t.Fatalf("stream: %v", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if len(betas) == 0 || betas[0] != interleavedThinkingBeta {
		t.Fatalf("anthropic-beta on stream = %q, want %q", betas, interleavedThinkingBeta)
	}
}

func boolPtr(v bool) *bool { return &v }
