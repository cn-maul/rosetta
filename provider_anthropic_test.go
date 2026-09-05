package rosetta

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"testing"
)

func newAnthroClient(t *testing.T, h http.Handler, opts ...Option) *Client {
	t.Helper()
	return newTestClient(t, h, append([]Option{WithProtocol(ProtoAnthropic)}, opts...)...)
}

func TestAnthropicChatMapping(t *testing.T) {
	var gotAPIKey, gotVersion string
	var gotPayload map[string]any
	c := newAnthroClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotAPIKey = r.Header.Get("x-api-key")
		gotVersion = r.Header.Get("anthropic-version")
		gotPayload = readPayload(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id":"msg_01","model":"claude-sonnet-4-5",
			"content":[
				{"type":"thinking","thinking":"hmm","signature":"sigABC"},
				{"type":"text","text":"hello"},
				{"type":"tool_use","id":"toolu_1","name":"get_weather","input":{"city":"SF"}}
			],
			"stop_reason":"tool_use",
			"usage":{"input_tokens":10,"output_tokens":20,"cache_read_input_tokens":5}
		}`)
	}))

	resp, err := c.Chat(context.Background(), &ChatRequest{
		Model:    "claude-sonnet-4-5",
		System:   "sys1",
		Messages: []Message{System("sys2"), User("a"), User("b")},
		Tools:    []ToolDefinition{{Name: "get_weather", Description: "w", Parameters: []byte(`{"type":"object"}`)}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	if gotAPIKey != "test-key" || gotVersion != "2023-06-01" {
		t.Errorf("headers: x-api-key=%q anthropic-version=%q", gotAPIKey, gotVersion)
	}
	if gotPayload["system"] != "sys1\n\nsys2" {
		t.Errorf("system = %v", gotPayload["system"])
	}
	if gotPayload["max_tokens"] != float64(4096) {
		t.Errorf("default max_tokens = %v", gotPayload["max_tokens"])
	}
	msgs := gotPayload["messages"].([]any)
	if len(msgs) != 1 {
		t.Fatalf("consecutive user messages must merge into one, got %d", len(msgs))
	}
	m0 := msgs[0].(map[string]any)
	if m0["role"] != "user" {
		t.Errorf("role = %v", m0["role"])
	}
	if contents := m0["content"].([]any); len(contents) != 2 {
		t.Errorf("content blocks = %v", contents)
	}

	// Tool roundtrip: assistant tool_use + tool_result message.
	if _, err := c.Chat(context.Background(), &ChatRequest{
		Model: "claude-sonnet-4-5",
		Messages: []Message{
			User("weather?"),
			AssistantBlocks(ToolCall("toolu_1", "get_weather", `{"city":"SF"}`)),
			ToolResult("toolu_1", "get_weather", "sunny"),
		},
	}); err != nil {
		t.Fatalf("Chat 2: %v", err)
	}
	msgs2 := gotPayload["messages"].([]any)
	if len(msgs2) != 3 {
		t.Fatalf("messages = %d, want 3", len(msgs2))
	}
	asst := msgs2[1].(map[string]any)
	asstBlocks := asst["content"].([]any)
	if tu := asstBlocks[0].(map[string]any); tu["type"] != "tool_use" || tu["id"] != "toolu_1" {
		t.Errorf("assistant block = %v", tu)
	} else if tu["input"].(map[string]any)["city"] != "SF" {
		t.Errorf("tool input not decoded: %v", tu["input"])
	}
	toolTurn := msgs2[2].(map[string]any)
	if toolTurn["role"] != "user" {
		t.Errorf("tool result must ride in a user turn, got %v", toolTurn["role"])
	}
	if tr := toolTurn["content"].([]any)[0].(map[string]any); tr["type"] != "tool_result" || tr["tool_use_id"] != "toolu_1" || tr["content"] != "sunny" {
		t.Errorf("tool_result block = %v", tr)
	}

	// Response decoding.
	if resp.StopReason != StopToolUse {
		t.Errorf("stop reason = %q", resp.StopReason)
	}
	if len(resp.Content) != 3 || resp.Content[0].Type != BlockThinking ||
		resp.Content[0].Signature != "sigABC" || resp.Text() != "hello" {
		t.Errorf("content = %+v", resp.Content)
	}
	if tc := resp.ToolCalls(); len(tc) != 1 || tc[0].ToolCallID != "toolu_1" || tc[0].Arguments != `{"city":"SF"}` {
		t.Errorf("tool calls = %+v", tc)
	}
	want := Usage{InputTokens: 10, OutputTokens: 20, TotalTokens: 30, CachedInputTokens: 5}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestAnthropicThinkingClamp(t *testing.T) {
	var gotPayload map[string]any
	c := newAnthroClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPayload = readPayload(t, r)
		fmt.Fprint(w, `{"id":"m","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	// Budget far above max_tokens: SDK must grow max_tokens, and drop
	// the incompatible sampling params instead of letting the API 400.
	_, err := c.Chat(context.Background(), &ChatRequest{
		Model:           "claude-sonnet-4-5",
		Messages:        []Message{User("hi")},
		MaxOutputTokens: 2048,
		Temperature:     Float(0.5),
		TopP:            Float(0.9),
		Thinking:        &ThinkingConfig{BudgetTokens: 50000},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if gotPayload["max_tokens"] != float64(54096) {
		t.Errorf("max_tokens = %v, want 54096 (budget 50000 + 4096)", gotPayload["max_tokens"])
	}
	th := gotPayload["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] != float64(50000) {
		t.Errorf("thinking = %v", th)
	}
	if _, has := gotPayload["temperature"]; has {
		t.Errorf("temperature must be dropped in thinking mode")
	}
	if _, has := gotPayload["top_p"]; has {
		t.Errorf("top_p must be dropped in thinking mode")
	}
}

func TestAnthropicRectifier(t *testing.T) {
	var requests int
	var lastPayload map[string]any
	c := newAnthroClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		lastPayload = readPayload(t, r)
		if requests == 1 {
			w.WriteHeader(http.StatusBadRequest)
			fmt.Fprint(w, `{"type":"error","error":{"type":"invalid_request_error","message":"messages.1.content.0.thinking.budget_tokens: values must be less than max_tokens"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"m","model":"claude-sonnet-4-5","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	_, err := c.Chat(context.Background(), &ChatRequest{
		Model:           "claude-sonnet-4-5",
		Messages:        []Message{User("hi")},
		MaxOutputTokens: 4096,
		Thinking:        &ThinkingConfig{Effort: EffortLow},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if requests != 2 {
		t.Fatalf("requests = %d, want 2", requests)
	}
	th := lastPayload["thinking"].(map[string]any)
	if th["budget_tokens"] != float64(32000) {
		t.Errorf("rectified budget = %v, want 32000", th["budget_tokens"])
	}
	if lastPayload["max_tokens"] != float64(64000) {
		t.Errorf("rectified max_tokens = %v, want 64000", lastPayload["max_tokens"])
	}
}

func TestAnthropicStream(t *testing.T) {
	c := newAnthroClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		events := []string{
			`{"type":"message_start","message":{"id":"msg_1","model":"claude-sonnet-4-5","usage":{"input_tokens":100,"output_tokens":1,"cache_read_input_tokens":50}}}`,
			`{"type":"ping"}`,
			`{"type":"content_block_start","index":0,"content_block":{"type":"thinking"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"thinking_delta","thinking":"let me think"}}`,
			`{"type":"content_block_delta","index":0,"delta":{"type":"signature_delta","signature":"sigX"}}`,
			`{"type":"content_block_stop","index":0}`,
			`{"type":"content_block_start","index":1,"content_block":{"type":"text"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"你好"}}`,
			`{"type":"content_block_delta","index":1,"delta":{"type":"text_delta","text":"世界"}}`,
			`{"type":"content_block_stop","index":1}`,
			`{"type":"content_block_start","index":2,"content_block":{"type":"tool_use","id":"toolu_9","name":"calc"}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"{\"x\":"}}`,
			`{"type":"content_block_delta","index":2,"delta":{"type":"input_json_delta","partial_json":"1}"}}`,
			`{"type":"content_block_stop","index":2}`,
			`{"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":77}}`,
			`{"type":"message_stop"}`,
		}
		for _, ev := range events {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", eventName(ev), ev)
			fl.Flush()
		}
	}))

	stream, err := c.ChatStream(context.Background(), &ChatRequest{
		Model: "claude-sonnet-4-5", Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{Effort: EffortMedium},
	})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer stream.Close()

	var types []EventType
	for stream.Next() {
		types = append(types, stream.Event().Type)
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	want := fmt.Sprint([]EventType{
		EventMessageStart,
		EventThinkingDelta, EventThinkingDelta, // thinking + signature
		EventTextDelta, EventTextDelta,
		EventToolCall, EventToolCall, EventToolCall, // start + 2 arg fragments
		EventMessageEnd,
	})
	if got := fmt.Sprint(types); got != want {
		t.Errorf("events = %s, want %s", got, want)
	}

	resp, err := stream.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if resp.Text() != "你好世界" {
		t.Errorf("text = %q", resp.Text())
	}
	if len(resp.Content) != 3 || resp.Content[0].Signature != "sigX" {
		t.Errorf("content = %+v", resp.Content)
	}
	if tc := resp.ToolCalls(); len(tc) != 1 || tc[0].ToolCallID != "toolu_9" || tc[0].ToolName != "calc" || tc[0].Arguments != `{"x":1}` {
		t.Errorf("tool calls = %+v", tc)
	}
	if resp.StopReason != StopToolUse {
		t.Errorf("stop = %q", resp.StopReason)
	}
	wantUsage := Usage{InputTokens: 100, OutputTokens: 77, TotalTokens: 177, CachedInputTokens: 50}
	if resp.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", resp.Usage, wantUsage)
	}
}

func TestAnthropicStreamEOFTolerance(t *testing.T) {
	// Server that never sends message_stop (or message_delta): the SDK
	// synthesizes an end event at EOF and reports StopOther so callers
	// can tell the stream did not terminate normally.
	c := newAnthroClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, `event: message_start
data: {"type":"message_start","message":{"id":"m","model":"claude-sonnet-4-5","usage":{"input_tokens":9,"output_tokens":0}}}

`)
		fmt.Fprint(w, `event: content_block_delta
data: {"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"cut"}}

`)
	}))
	stream, err := c.ChatStream(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer stream.Close()
	resp, err := stream.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if got := resp.Text(); got != "cut" {
		t.Errorf("text = %q", got)
	}
	if resp.StopReason != StopOther {
		t.Errorf("stop = %q, want StopOther for unterminated stream", resp.StopReason)
	}
	if resp.Usage.InputTokens != 9 {
		t.Errorf("usage = %+v", resp.Usage)
	}
}

func TestAnthropicListModels(t *testing.T) {
	c := newAnthroClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("x-api-key") != "test-key" {
			t.Errorf("missing x-api-key")
		}
		fmt.Fprint(w, `{"data":[{"id":"claude-sonnet-4-5","display_name":"Claude Sonnet 4.5","type":"model"}]}`)
	}))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	remote, ok := byID["claude-sonnet-4-5"]
	if !ok || !remote.Known || remote.DisplayName != "Claude Sonnet 4.5" || remote.Protocol != ProtoAnthropic {
		t.Errorf("merged entry wrong: %+v (ok=%v)", remote, ok)
	}
	// Manual/builtin layers merged in with Known=true; the remote entry
	// had no capability data but the builtin layer supplies it.
	if remote.SupportsThinking != true || remote.ContextWindow != 200000 {
		t.Errorf("remote entry did not inherit builtin capability data: %+v", remote)
	}
}

func TestAnthropicValidationAndImages(t *testing.T) {
	// Assistant-first conversation is rejected by Anthropic.
	c := newAnthroClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}))
	_, err := c.Chat(context.Background(), &ChatRequest{
		Model: "m", Messages: []Message{Assistant("hi")},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("want ErrInvalidRequest for assistant-first, got %v", err)
	}
	// Only-system conversations are rejected too.
	_, err = c.Chat(context.Background(), &ChatRequest{
		Model: "m", Messages: []Message{System("sys")},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Errorf("want ErrInvalidRequest for system-only, got %v", err)
	}
	// Image encoding rules.
	if src, err := encodeAnthropicImage("data:image/png;base64,AAAA"); err != nil ||
		src["type"] != "base64" || src["media_type"] != "image/png" || src["data"] != "AAAA" {
		t.Errorf("data URL source = %v, err %v", src, err)
	}
	if src, err := encodeAnthropicImage("https://x.test/i.jpg"); err != nil || src["type"] != "url" || src["url"] != "https://x.test/i.jpg" {
		t.Errorf("https source = %v, err %v", src, err)
	}
	if _, err := encodeAnthropicImage("ftp://bad"); err == nil {
		t.Errorf("unsupported scheme must error")
	}
}

// eventName extracts the "type" field for SSE event-name decoration in
// tests (the SDK ignores the event: line, but real servers send one).
func eventName(chunk string) string {
	var s struct {
		Type string `json:"type"`
	}
	_ = json.Unmarshal([]byte(chunk), &s)
	return s.Type
}

func TestAnthropicOverloadedRetry(t *testing.T) {
	var requests int
	c := newAnthroClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		requests++
		if requests == 1 {
			w.WriteHeader(529) // Anthropic overloaded
			fmt.Fprint(w, `{"type":"error","error":{"type":"overloaded_error","message":"Overloaded"}}`)
			return
		}
		fmt.Fprint(w, `{"id":"m","model":"m","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}), WithMaxRetries(1))
	resp, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.Text() != "ok" || requests != 2 {
		t.Errorf("resp=%q requests=%d", resp.Text(), requests)
	}
}
