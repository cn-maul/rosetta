package rosetta

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// RG-16: an injected tokenizer replaces the heuristic in both the local
// estimate and the context check.
func TestTokenCounterInjection(t *testing.T) {
	counter := func(s string) int { return len(strings.Fields(s)) * 10 }
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat), WithTokenCounter(counter))

	if got := c.EstimateTokens("a b c"); got != 30 {
		t.Fatalf("EstimateTokens = %d, want 30", got)
	}
	req := &ChatRequest{Model: "m", Messages: []Message{User("a b c")}}
	// 4 per message + 30 for the text.
	if got := c.estimateInputTokens(req); got != 34 {
		t.Fatalf("estimateInputTokens = %d, want 34", got)
	}

	// Without a counter the heuristic is used.
	plain := newTestClient(t, WithProtocol(ProtoOpenAIChat))
	if got := plain.EstimateTokens("a b c"); got != EstimateTokens("a b c") {
		t.Fatalf("fallback = %d", got)
	}
}

// Anthropic count_tokens returns the provider's exact count and must not
// send max_tokens/stream, which some gateways reject on that endpoint.
func TestAnthropicCountTokens(t *testing.T) {
	var gotPath string
	var gotBody map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPath = r.URL.Path
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &gotBody)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"input_tokens":42}`)
	}))
	defer srv.Close()

	c, err := NewClient(
		WithEndpoint(srv.URL),
		WithAPIKey("k"),
		WithProtocol(ProtoAnthropic),
	)
	if err != nil {
		t.Fatal(err)
	}
	n, err := c.CountTokens(context.Background(), &ChatRequest{
		Model:    "claude-sonnet-4-5",
		Messages: []Message{User("hello")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if n != 42 {
		t.Fatalf("count = %d, want 42", n)
	}
	if !strings.HasSuffix(gotPath, "/messages/count_tokens") {
		t.Fatalf("path = %q", gotPath)
	}
	if _, has := gotBody["max_tokens"]; has {
		t.Fatal("max_tokens must not be sent to count_tokens")
	}
	if _, has := gotBody["stream"]; has {
		t.Fatal("stream must not be sent to count_tokens")
	}
}

// Protocols without a count endpoint say so instead of guessing.
func TestCountTokensUnsupportedProtocol(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat))
	_, err := c.CountTokens(context.Background(), &ChatRequest{
		Model: "m", Messages: []Message{User("hi")},
	})
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("err = %v, want ErrNotSupported", err)
	}
}
