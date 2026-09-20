package rosetta

// Anthropic error-path tests. The Anthropic adapter's non-2xx handling used
// to be the one protocol branch with no test at all: parseAnthropicError sat
// at 0% coverage while its OpenAI sibling had a full status matrix. That left
// two things unverified — the request-id recovery Anthropic gateways depend
// on, and the thinking-budget rectifier's retry loop, where a wrong decision
// means either a dead request or a retry storm.

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
)

// The error envelope is Anthropic-shaped ({"type":"error","error":{...}}),
// and some gateways omit the request-id header but echo the id in the body.
func TestParseAnthropicError(t *testing.T) {
	const url = "http://api.test/v1/messages"
	body := []byte(`{"type":"error","error":{"type":"invalid_request_error",` +
		`"message":"max_tokens: must be greater than 0"},"request_id":"req_from_body"}`)

	got := parseAnthropicError(http.StatusBadRequest, body, http.MethodPost, url, "req_from_header")
	if got.StatusCode != http.StatusBadRequest {
		t.Errorf("StatusCode = %d, want 400", got.StatusCode)
	}
	if got.Message != "max_tokens: must be greater than 0" {
		t.Errorf("Message = %q, want the nested error message", got.Message)
	}
	if got.Type != "invalid_request_error" {
		t.Errorf("Type = %q, want invalid_request_error", got.Type)
	}
	if got.Method != http.MethodPost || got.URL != url {
		t.Errorf("Method/URL = %s %s, want POST %s", got.Method, got.URL, url)
	}
	// The header is authoritative when it is present.
	if got.RequestID != "req_from_header" {
		t.Errorf("RequestID = %q, want the header value to win", got.RequestID)
	}
	// 400 is a caller mistake, not a transient condition.
	if got.Retryable {
		t.Error("400 must not be marked retryable")
	}

	// Without a header the body's top-level request_id is recovered.
	got = parseAnthropicError(http.StatusBadRequest, body, http.MethodPost, url, "")
	if got.RequestID != "req_from_body" {
		t.Errorf("RequestID = %q, want the body's request_id", got.RequestID)
	}

	// Neither source present: the field stays empty instead of inventing one.
	got = parseAnthropicError(http.StatusServiceUnavailable,
		[]byte(`{"type":"error","error":{"type":"overloaded_error","message":"overloaded"}}`),
		http.MethodPost, url, "")
	if got.RequestID != "" {
		t.Errorf("RequestID = %q, want empty", got.RequestID)
	}
	if !got.Retryable {
		t.Error("503 must be marked retryable")
	}

	// A body that is not JSON at all must still produce a usable error.
	got = parseAnthropicError(http.StatusBadGateway, []byte("<html>bad gateway</html>"),
		http.MethodPost, url, "")
	if got.Message == "" {
		t.Error("unparseable body must fall back to a message")
	}
}

// A non-2xx Chat surfaces as *APIError carrying the provider's own fields.
func TestAnthropicChatErrorSurfacesAsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("request-id", "req_live_1")
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"authentication_error",`+
			`"message":"invalid x-api-key"}}`)
	}))
	defer srv.Close()

	c := newCacheTestClient(t, srv, WithMaxRetries(0))
	_, err := c.Chat(context.Background(), &ChatRequest{Model: "claude-x", Messages: []Message{User("hi")}})

	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusUnauthorized {
		t.Errorf("StatusCode = %d, want 401", apiErr.StatusCode)
	}
	if apiErr.Type != "authentication_error" || apiErr.Message != "invalid x-api-key" {
		t.Errorf("Type/Message = %q/%q, want the provider's envelope", apiErr.Type, apiErr.Message)
	}
	if apiErr.RequestID != "req_live_1" {
		t.Errorf("RequestID = %q, want req_live_1", apiErr.RequestID)
	}
	if apiErr.Retryable {
		t.Error("401 must not be marked retryable")
	}
}

// The rectifier rewrites the budget once on a 400 that cites the thinking
// budget, then retries. Two requests must go out, and the second must carry
// the rewritten numbers — before this test the loop had no coverage at all,
// so a regression would have surfaced as a dead request or a retry storm.
func TestAnthropicThinkingRectifyRetriesOnce(t *testing.T) {
	var mu sync.Mutex
	var payloads []map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		raw, _ := io.ReadAll(r.Body)
		var doc map[string]any
		_ = json.Unmarshal(raw, &doc)
		mu.Lock()
		payloads = append(payloads, doc)
		n := len(payloads)
		mu.Unlock()

		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error",`+
				`"message":"thinking.budget_tokens: must be less than max_tokens"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer srv.Close()

	c := newCacheTestClient(t, srv, WithMaxRetries(0))
	resp, err := c.Chat(context.Background(), &ChatRequest{
		Model:    "claude-x",
		Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{BudgetTokens: 2048},
	})
	if err != nil {
		t.Fatalf("rectified retry must succeed, got %v", err)
	}
	if resp.Text() != "ok" {
		t.Fatalf("resp.Text() = %q, want ok", resp.Text())
	}

	mu.Lock()
	defer mu.Unlock()
	if len(payloads) != 2 {
		t.Fatalf("requests = %d, want exactly 2 (one rejected, one rectified)", len(payloads))
	}
	budgetOf := func(i int) float64 {
		t.Helper()
		th, ok := payloads[i]["thinking"].(map[string]any)
		if !ok {
			t.Fatalf("request %d has no thinking config: %#v", i, payloads[i])
		}
		v, _ := th["budget_tokens"].(float64)
		return v
	}
	maxOf := func(i int) float64 {
		t.Helper()
		v, _ := payloads[i]["max_tokens"].(float64)
		return v
	}

	if budgetOf(0) != 2048 {
		t.Errorf("first budget = %v, want the caller's 2048", budgetOf(0))
	}
	if budgetOf(1) != 32000 {
		t.Errorf("retried budget = %v, want the rectified 32000", budgetOf(1))
	}
	if maxOf(1) != 64000 {
		t.Errorf("retried max_tokens = %v, want 64000 so the budget fits", maxOf(1))
	}
	if maxOf(1) <= budgetOf(1) {
		t.Error("max_tokens must stay strictly above the thinking budget")
	}
}

// An unrelated 400 must reach the caller untouched — retrying it would burn
// a request and fail identically.
func TestAnthropicUnrelatedErrorDoesNotRectify(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		mu.Unlock()
		w.WriteHeader(http.StatusBadRequest)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error",`+
			`"message":"messages: at least one message is required"}}`)
	}))
	defer srv.Close()

	c := newCacheTestClient(t, srv, WithMaxRetries(0))
	_, err := c.Chat(context.Background(), &ChatRequest{
		Model:    "claude-x",
		Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{BudgetTokens: 2048},
	})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 1 {
		t.Fatalf("attempts = %d, want 1 — an unrelated 400 must not be retried", attempts)
	}
	// The rectifier must not have touched the plan either.
	if apiErr.Message == "" {
		t.Error("the provider's message must survive to the caller")
	}
}

// StreamChat reports a non-2xx before any stream exists.
func TestAnthropicStreamChatErrorSurfacesAsAPIError(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("request-id", "req_stream_1")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = io.WriteString(w, `{"type":"error","error":{"type":"rate_limit_error",`+
			`"message":"rate limit exceeded"}}`)
	}))
	defer srv.Close()

	c := newCacheTestClient(t, srv, WithMaxRetries(0))
	s, err := c.ChatStream(context.Background(), &ChatRequest{Model: "claude-x", Messages: []Message{User("hi")}})
	if s != nil {
		t.Error("no stream must be returned alongside the error")
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v (%T), want *APIError", err, err)
	}
	if apiErr.StatusCode != http.StatusTooManyRequests {
		t.Errorf("StatusCode = %d, want 429", apiErr.StatusCode)
	}
	if !apiErr.Retryable {
		t.Error("429 must be marked retryable for callers that retry themselves")
	}
	if apiErr.RequestID != "req_stream_1" {
		t.Errorf("RequestID = %q, want req_stream_1", apiErr.RequestID)
	}
}

// The streaming path runs the same rectifier, so a 400 citing the budget must
// be rewritten and retried there too, then hand back a usable stream.
func TestAnthropicStreamChatRectifyRetriesOnce(t *testing.T) {
	var mu sync.Mutex
	attempts := 0
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		attempts++
		n := attempts
		mu.Unlock()
		if n == 1 {
			w.WriteHeader(http.StatusBadRequest)
			_, _ = io.WriteString(w, `{"type":"error","error":{"type":"invalid_request_error",`+
				`"message":"thinking.budget_tokens must be less than max_tokens"}}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer srv.Close()

	c := newCacheTestClient(t, srv, WithMaxRetries(0))
	s, err := c.ChatStream(context.Background(), &ChatRequest{
		Model:    "claude-x",
		Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{BudgetTokens: 2048},
	})
	if err != nil {
		t.Fatalf("rectified stream must start, got %v", err)
	}
	var text string
	for s.Next() {
		if ev := s.Event(); ev != nil && ev.Type == EventTextDelta {
			text += ev.Text
		}
	}
	if err := s.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	if text != "ok" {
		t.Errorf("stream text = %q, want ok", text)
	}
	mu.Lock()
	defer mu.Unlock()
	if attempts != 2 {
		t.Fatalf("attempts = %d, want 2", attempts)
	}
}
