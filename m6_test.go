package rosetta

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

// TestDirtyVendorPayload exercises jsonx lenient decoding against the
// shapes broken third-party services actually emit: array content,
// string-encoded usage numbers, null finish_reason, float token counts.
func TestDirtyVendorPayload(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "dirty-1",
			"model": "weird-model",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"content": [{"type": "text", "text": "part1"}, {"type": "text", "text": "part2"}]
				},
				"finish_reason": null
			}],
			"usage": {
				"prompt_tokens": "12",
				"completion_tokens": "34.0",
				"total_tokens": null,
				"prompt_tokens_details": {"cached_tokens": "8"}
			}
		}`)
	}))
	resp, err := c.Chat(context.Background(), &ChatRequest{Model: "weird-model", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text() != "part1\npart2" {
		t.Errorf("array content not joined: %q", resp.Text())
	}
	if resp.StopReason != StopEnd {
		t.Errorf("null finish_reason must default to StopEnd, got %q", resp.StopReason)
	}
	want := Usage{InputTokens: 12, OutputTokens: 34, TotalTokens: 46, CachedInputTokens: 8}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
}

// TestUsageMissingCounting: a 200 response with no usage data must be
// recorded as a usage-missing request.
func TestUsageMissingCounting(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"1","model":"m","choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}), WithUsageTracker(NewMemoryUsageTracker()))
	if _, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	stats := c.Stats()
	if stats.TotalRequests != 1 || stats.UsageMissing != 1 {
		t.Errorf("stats = %+v, want 1 request with 1 usage_missing", stats)
	}
}

// TestStreamErrorEvent: an in-stream error chunk surfaces as an error
// while preserving partial content.
func TestStreamErrorEvent(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, "data: {\"error\":{\"message\":\"model exploded\",\"type\":\"server_error\"}}\n\n")
		fl.Flush()
	}))
	stream, err := c.ChatStream(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer stream.Close()
	for stream.Next() {
	}
	var apiErr *APIError
	if !errors.As(stream.Err(), &apiErr) || apiErr.Message != "model exploded" {
		t.Fatalf("want in-stream APIError, got %v", stream.Err())
	}
	if got := stream.Partial().Text(); got != "partial" {
		t.Errorf("partial text = %q", got)
	}
}
