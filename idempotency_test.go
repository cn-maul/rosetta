package rosetta

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
)

// The default chat path sends an Idempotency-Key so a transport-level retry
// of a 429/503 can be deduplicated by the provider.
func TestChatIdempotencyKeyDefault(t *testing.T) {
	var sawKey atomic.Bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Idempotency-Key") != "" {
			sawKey.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"1","model":"gpt-4o","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`))
	}))
	defer srv.Close()

	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), &ChatRequest{Model: "gpt-4o", Messages: []Message{User("hi")}}); err != nil {
		t.Fatal(err)
	}
	if !sawKey.Load() {
		t.Fatal("default chat must send an Idempotency-Key header")
	}
}

// NoIdempotencyKey reverts to the pre-v0.6.0 behavior: no Idempotency-Key
// header and no transport-level retry of a chat POST — a 429 surfaces to the
// caller immediately after a single attempt.
func TestChatNoIdempotencyKeyQuirk(t *testing.T) {
	var sawKey atomic.Bool
	var calls atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		if r.Header.Get("Idempotency-Key") != "" {
			sawKey.Store(true)
		}
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusTooManyRequests)
		_, _ = w.Write([]byte(`{"error":{"message":"rate limited","type":"rate_limit_error"}}`))
	}))
	defer srv.Close()

	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithQuirks(Quirks{NoIdempotencyKey: true}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Chat(context.Background(), &ChatRequest{Model: "gpt-4o", Messages: []Message{User("hi")}})
	if err == nil {
		t.Fatal("a 429 must surface as an error")
	}
	if sawKey.Load() {
		t.Fatal("NoIdempotencyKey must suppress the Idempotency-Key header")
	}
	if calls.Load() != 1 {
		t.Fatalf("NoIdempotencyKey must not retry the 429: calls=%d, want 1", calls.Load())
	}
}
