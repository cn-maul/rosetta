package rosetta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// newTestClient starts an httptest server backed by h and wires a Client
// to it. Extra options are applied after endpoint/key.
func newTestClient(t *testing.T, h http.Handler, opts ...Option) *Client {
	t.Helper()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	all := append([]Option{
		WithEndpoint(srv.URL),
		WithAPIKey("test-key"),
		WithRetryBase(time.Millisecond),
	}, opts...)
	c, err := NewClient(all...)
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	return c
}

func readPayload(t *testing.T, r *http.Request) map[string]any {
	t.Helper()
	body, err := io.ReadAll(r.Body)
	if err != nil {
		t.Fatalf("reading request body: %v", err)
	}
	var m map[string]any
	if err := json.Unmarshal(body, &m); err != nil {
		t.Fatalf("request body not JSON: %v (%s)", err, body)
	}
	return m
}

func TestChatMapsRequestAndResponse(t *testing.T) {
	var gotAuth, gotPath string
	var gotPayload map[string]any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAuth = r.Header.Get("Authorization")
		gotPath = r.URL.Path
		gotPayload = readPayload(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id": "chatcmpl-1",
			"model": "gpt-4o",
			"choices": [{
				"index": 0,
				"message": {
					"role": "assistant",
					"reasoning_content": "pondering",
					"content": "hello world",
					"tool_calls": [{"id": "call_1", "type": "function",
						"function": {"name": "get_weather", "arguments": "{\"city\":\"SF\"}"}}]
				},
				"finish_reason": "tool_calls"
			}],
			"usage": {
				"prompt_tokens": 120, "completion_tokens": 30, "total_tokens": 150,
				"prompt_tokens_details": {"cached_tokens": 64},
				"completion_tokens_details": {"reasoning_tokens": 12}
			}
		}`)
	}))

	resp, err := c.Chat(context.Background(), &ChatRequest{
		Model:           "gpt-5",
		System:          "be brief",
		Messages:        []Message{User("hi")},
		MaxOutputTokens: 512,
		Temperature:     Float(0.7),
		Tools:           []ToolDefinition{{Name: "get_weather", Description: "weather", Parameters: json.RawMessage(`{"type":"object"}`)}},
		Thinking:        &ThinkingConfig{Effort: EffortHigh},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if gotAuth != "Bearer test-key" {
		t.Errorf("Authorization = %q", gotAuth)
	}
	if !strings.HasSuffix(gotPath, "/chat/completions") {
		t.Errorf("path = %q", gotPath)
	}
	for _, check := range []struct {
		key  string
		want any
	}{
		{"model", "gpt-5"},
		{"max_completion_tokens", float64(512)},
		{"temperature", 0.7},
		{"reasoning_effort", "high"},
	} {
		if v, ok := gotPayload[check.key]; !ok || v != check.want {
			t.Errorf("payload[%q] = %v (want %v)", check.key, v, check.want)
		}
	}
	if msgs, ok := gotPayload["messages"].([]any); !ok || len(msgs) != 2 {
		t.Fatalf("messages = %v", gotPayload["messages"])
	} else if m0 := msgs[0].(map[string]any); m0["role"] != "system" || m0["content"] != "be brief" {
		t.Errorf("system message = %v", m0)
	}

	if resp.ID != "chatcmpl-1" || resp.Model != "gpt-4o" {
		t.Errorf("id/model = %q/%q", resp.ID, resp.Model)
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("stop reason = %q", resp.StopReason)
	}
	var types []BlockType
	for _, b := range resp.Content {
		types = append(types, b.Type)
	}
	if fmt.Sprint(types) != "[thinking text tool_call]" {
		t.Errorf("content block types = %v", types)
	}
	if resp.Text() != "hello world" || resp.ThinkingText() != "pondering" {
		t.Errorf("text=%q thinking=%q", resp.Text(), resp.ThinkingText())
	}
	if tc := resp.ToolCalls(); len(tc) != 1 || tc[0].ToolCallID != "call_1" ||
		tc[0].ToolName != "get_weather" || tc[0].Arguments != `{"city":"SF"}` {
		t.Errorf("tool calls = %+v", tc)
	}
	want := Usage{InputTokens: 120, OutputTokens: 30, TotalTokens: 150, CachedInputTokens: 64, ReasoningTokens: 12}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestChatStreamEvents(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := readPayload(t, r)
		if payload["stream"] != true {
			t.Errorf("stream flag missing")
		}
		if _, ok := payload["stream_options"]; !ok {
			t.Errorf("stream_options missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		chunks := []string{
			`{"id":"c1","model":"deepseek-chat","choices":[{"index":0,"delta":{"role":"assistant","content":"你"}}]}`,
			`{"choices":[{"index":0,"delta":{"content":"好"}}]}`,
			`{"choices":[{"index":0,"delta":{"reasoning_content":"deep thought"}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call_9","function":{"name":"f","arguments":"{\"x\":"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"arguments":"1}"}}]}}]}`,
			`{"choices":[{"index":0,"delta":{},"finish_reason":"tool_calls"}]}`,
			`{"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`,
		}
		for _, ch := range chunks {
			fmt.Fprintf(w, "data: %s\n\n", ch)
			fl.Flush()
		}
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))

	stream, err := c.ChatStream(context.Background(), &ChatRequest{
		Model: "deepseek-chat", Messages: []Message{User("hi")},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer stream.Close()

	var types []EventType
	var texts []string
	for stream.Next() {
		ev := stream.Event()
		types = append(types, ev.Type)
		switch ev.Type {
		case EventTextDelta, EventThinkingDelta:
			texts = append(texts, ev.Text)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	wantTypes := fmt.Sprint([]EventType{
		EventMessageStart, EventTextDelta, EventTextDelta, EventThinkingDelta,
		EventToolCall, EventToolCall, EventMessageEnd,
	})
	if got := fmt.Sprint(types); got != wantTypes {
		t.Errorf("event types = %s, want %s", got, wantTypes)
	}
	if got := fmt.Sprint(texts); got != "[你 好 deep thought]" {
		t.Errorf("delta texts = %s", got)
	}

	resp, err := stream.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if resp.Text() != "你好" || resp.ThinkingText() != "deep thought" {
		t.Errorf("collected text=%q thinking=%q", resp.Text(), resp.ThinkingText())
	}
	tcs := resp.ToolCalls()
	if len(tcs) != 1 || tcs[0].ToolCallID != "call_9" || tcs[0].ToolName != "f" || tcs[0].Arguments != `{"x":1}` {
		t.Errorf("tool calls = %+v", tcs)
	}
	if resp.StopReason != StopToolUse || resp.Usage.OutputTokens != 5 || resp.Usage.InputTokens != 10 {
		t.Errorf("stop=%q usage=%+v", resp.StopReason, resp.Usage)
	}
	if u := stream.Usage(); u.TotalTokens != 15 {
		t.Errorf("stream.Usage() = %+v", u)
	}
}

// TestChatStreamSkipsKeepAlive ensures comment/keep-alive lines and empty
// chunks never produce events.
func TestChatStreamSkipsKeepAlive(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, ": keep-alive\n\n")
		fmt.Fprint(w, "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"a\"},\"finish_reason\":\"stop\"}]}\n\n")
		fl.Flush()
		fmt.Fprint(w, ": ping\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
		fl.Flush()
	}))
	stream, err := c.ChatStream(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("x")}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer stream.Close()
	resp, err := stream.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if resp.Text() != "a" || resp.StopReason != StopEnd {
		t.Errorf("resp = %+v", resp)
	}
}

// TestLegacyMaxTokensFallback probes the sticky max_tokens downgrade.
func TestLegacyMaxTokensFallback(t *testing.T) {
	var requests int
	var sawFirstTokenField string
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		payload := readPayload(t, r)
		if requests == 1 {
			if _, ok := payload["max_completion_tokens"]; ok {
				sawFirstTokenField = "max_completion_tokens"
				w.WriteHeader(http.StatusBadRequest)
				fmt.Fprint(w, `{"error":{"message":"Unrecognized request argument supplied: max_completion_tokens","type":"invalid_request_error"}}`)
				return
			}
		}
		if _, ok := payload["max_tokens"]; !ok {
			t.Errorf("attempt %d: max_tokens missing", requests)
		}
		if _, ok := payload["max_completion_tokens"]; ok {
			t.Errorf("attempt %d: still sending max_completion_tokens", requests)
		}
		fmt.Fprint(w, `{"id":"1","model":"m","choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))

	req := &ChatRequest{Model: "m", Messages: []Message{User("hi")}, MaxOutputTokens: 100}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat 1: %v", err)
	}
	if sawFirstTokenField != "max_completion_tokens" {
		t.Errorf("first attempt did not probe max_completion_tokens")
	}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat 2: %v", err)
	}
	if requests != 3 { // probe+fallback on first chat, direct hit on second
		t.Errorf("requests = %d, want 3 (sticky downgrade)", requests)
	}
}

// TestStreamOptionsFallback drops stream_options on a 400 and remembers it.
func TestStreamOptionsFallback(t *testing.T) {
	var requests int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		payload := readPayload(t, r)
		if _, bad := payload["stream_options"]; bad && requests <= 2 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"error":{"message":"Unrecognized request arguments supplied: stream_options"}}`)
			return
		}
		if _, bad := payload["stream_options"]; bad {
			t.Errorf("stream_options still present after fallback")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"x\"},\"finish_reason\":\"stop\"}]}\n\n")
		fmt.Fprint(w, "data: [DONE]\n\n")
	}))
	stream, err := c.ChatStream(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	if _, err := stream.Collect(); err != nil {
		t.Fatalf("Collect: %v", err)
	}
	stream.Close()
	stream2, err := c.ChatStream(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("ChatStream 2: %v", err)
	}
	if _, err := stream2.Collect(); err != nil {
		t.Fatalf("Collect 2: %v", err)
	}
	stream2.Close()
	if requests != 3 { // 2 for first stream (probe+retry), 1 for second
		t.Errorf("requests = %d, want 3", requests)
	}
}

func TestQuirksLegacyMaxTokens(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := readPayload(t, r)
		if _, ok := payload["max_tokens"]; !ok {
			t.Errorf("quirk not applied: max_tokens missing")
		}
		fmt.Fprint(w, `{"id":"1","model":"m","choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}), WithQuirks(Quirks{LegacyMaxTokens: true}))
	if _, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}, MaxOutputTokens: 64}); err != nil {
		t.Fatalf("Chat: %v", err)
	}
}

func TestRetryOn429(t *testing.T) {
	var requests int
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.Header().Set("Retry-After", "0")
			w.WriteHeader(http.StatusTooManyRequests)
			fmt.Fprint(w, `{"error":{"message":"rate limited","type":"rate_limit_error"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"1","model":"m","choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}), WithMaxRetries(1))
	resp, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text() != "ok" || requests != 2 {
		t.Errorf("resp=%v requests=%d", resp.Text(), requests)
	}
}

func TestAPIErrorParsing(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusUnauthorized)
		fmt.Fprint(w, `{"error":{"message":"Incorrect API key","type":"invalid_request_error","code":"invalid_api_key"}}`)
	}))
	_, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("want *APIError, got %T: %v", err, err)
	}
	if apiErr.StatusCode != 401 || apiErr.Message != "Incorrect API key" ||
		apiErr.Type != "invalid_request_error" || apiErr.Code != "invalid_api_key" {
		t.Errorf("apiErr = %+v", apiErr)
	}
	if apiErr.Retryable {
		t.Errorf("401 must not be retryable")
	}
	if !strings.Contains(apiErr.Error(), "401") {
		t.Errorf("Error() = %q", apiErr.Error())
	}
}

func TestUsageStatsAndListModels(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case strings.HasSuffix(r.URL.Path, "/chat/completions"):
			fmt.Fprint(w, `{"id":"1","model":"m","choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":3,"completion_tokens":7,"total_tokens":10}}`)
		case strings.HasSuffix(r.URL.Path, "/models"):
			fmt.Fprint(w, `{"object":"list","data":[{"id":"model-a","object":"model"},{"id":"model-b"}]}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}), WithUsageTracker(NewMemoryUsageTracker()))

	ctx := context.Background()
	for i := 0; i < 2; i++ {
		if _, err := c.Chat(ctx, &ChatRequest{Model: "m", Messages: []Message{User("hi")}}); err != nil {
			t.Fatalf("Chat: %v", err)
		}
	}
	stats := c.Stats()
	if stats.TotalRequests != 2 || stats.InputTokens != 6 || stats.OutputTokens != 14 || stats.TotalTokens != 20 {
		t.Errorf("stats = %+v", stats)
	}
	if mu := stats.ByModel["m"]; mu.Requests != 2 {
		t.Errorf("ByModel = %+v", stats.ByModel)
	}
	if pu := stats.ByProtocol[ProtoOpenAIChat]; pu.Requests != 2 {
		t.Errorf("ByProtocol = %+v", stats.ByProtocol)
	}

	models, err := c.ListModels(ctx)
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	// The merged catalog contains both discovered and built-in models.
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	ra, okA := byID["model-a"]
	if !okA || ra.Known {
		t.Errorf("model-a missing or wrongly Known: %+v", ra)
	}
	gpt4o, okB := byID["gpt-4o"]
	if !okB || !gpt4o.Known || gpt4o.ContextWindow != 128000 {
		t.Errorf("builtin gpt-4o missing or wrong: %+v", gpt4o)
	}
}

func TestStreamPartialOnError(t *testing.T) {
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		fmt.Fprint(w, "data: {\"id\":\"c\",\"model\":\"m\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"partial\"}}]}\n\n")
		fl.Flush()
		// Abrupt mid-stream failure: hijack and close without [DONE].
		conn, _, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		conn.Close()
	}))
	stream, err := c.ChatStream(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer stream.Close()
	for stream.Next() {
	}
	if stream.Err() == nil {
		t.Errorf("expected stream error after abrupt close")
	}
	if got := stream.Partial().Text(); got != "partial" {
		t.Errorf("partial text = %q", got)
	}
}

func TestValidationAndConstruction(t *testing.T) {
	if _, err := NewClient(WithEndpoint("http://x")); !errors.Is(err, ErrNoAPIKey) {
		t.Errorf("want ErrNoAPIKey, got %v", err)
	}
	if _, err := NewClient(WithAPIKey("k"), WithProtocol(Protocol("bogus"))); err == nil {
		t.Errorf("unknown protocol must fail")
	}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	if _, err := c.Chat(context.Background(), &ChatRequest{Messages: []Message{User("hi")}}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("want ErrInvalidRequest, got %v", err)
	}
	if _, err := c.ChatStream(context.Background(), &ChatRequest{Model: "m"}); !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("want ErrInvalidRequest for empty messages, got %v", err)
	}
	if c.Protocol() != ProtoOpenAIChat {
		t.Errorf("default protocol = %q", c.Protocol())
	}
}

func TestJoinEndpoint(t *testing.T) {
	cases := []struct{ base, path, want string }{
		{"https://api.openai.com", "/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/", "/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://open.bigmodel.cn/api/paas/v4", "/chat/completions", "https://open.bigmodel.cn/api/paas/v4/chat/completions"},
		{"http://localhost:11434", "/chat/completions", "http://localhost:11434/v1/chat/completions"},
	}
	for _, tc := range cases {
		if got := joinEndpoint(tc.base, tc.path); got != tc.want {
			t.Errorf("joinEndpoint(%q,%q) = %q, want %q", tc.base, tc.path, got, tc.want)
		}
	}
}

func TestRequestMappingToolRoundtrip(t *testing.T) {
	// Assistant tool call + tool result must encode to role:"tool".
	var msgs []any
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := readPayload(t, r)
		msgs = payload["messages"].([]any)
		fmt.Fprint(w, `{"id":"1","model":"m","choices":[{"index":0,"message":{"content":"done"},"finish_reason":"stop"}]}`)
	}))
	req := &ChatRequest{
		Model: "m",
		Messages: []Message{
			User("weather?"),
			AssistantBlocks(ToolCall("call_1", "get_weather", `{"city":"SF"}`)),
			ToolResult("call_1", "get_weather", "sunny 22C"),
		},
	}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(msgs) != 3 {
		t.Fatalf("messages = %d", len(msgs))
	}
	toolMsg := msgs[2].(map[string]any)
	if toolMsg["role"] != "tool" || toolMsg["tool_call_id"] != "call_1" || toolMsg["content"] != "sunny 22C" {
		t.Errorf("tool message = %v", toolMsg)
	}
	asst := msgs[1].(map[string]any)
	if tcs, ok := asst["tool_calls"].([]any); !ok || len(tcs) != 1 {
		t.Errorf("assistant tool_calls = %v", asst["tool_calls"])
	}
}
