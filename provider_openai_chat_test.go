package rosetta

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"testing"
)

func newTestClient(t *testing.T, opts ...Option) *Client {
	t.Helper()
	all := append([]Option{WithEndpoint("http://api.test/v1"), WithAPIKey("k")}, opts...)
	c, err := NewClient(all...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestJoinEndpoint(t *testing.T) {
	tests := []struct{ base, path, want string }{
		{"https://api.openai.com/v1", "/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"https://api.openai.com/v1/", "/chat/completions", "https://api.openai.com/v1/chat/completions"},
		{"http://localhost:11434", "/chat/completions", "http://localhost:11434/v1/chat/completions"},
		{"http://localhost:11434/", "/chat/completions", "http://localhost:11434/v1/chat/completions"},
		{"https://gateway.example.com/api/paas/v4", "/chat/completions", "https://gateway.example.com/api/paas/v4/chat/completions"},
	}
	for _, tt := range tests {
		if got := joinEndpoint(tt.base, tt.path); got != tt.want {
			t.Errorf("joinEndpoint(%q) = %q, want %q", tt.base, got, tt.want)
		}
	}
}

func TestOpenAIChatBuildPayload(t *testing.T) {
	c := newTestClient(t)
	p := c.provider.(*openaiChatProvider)
	st := p.initialState()

	req := &ChatRequest{
		Model:           "gpt-4o",
		Messages:        []Message{User("hi")},
		MaxOutputTokens: 100,
		Temperature:     Float(0.7),
		TopP:            Float(0.9),
		StopSequences:   []string{"STOP"},
		Tools:           []ToolDefinition{{Name: "fn", Description: "d"}},
		Thinking:        &ThinkingConfig{Effort: EffortHigh},
		Extra:           map[string]any{"logit_bias": map[string]int{"x": 1}},
	}
	pl, err := p.buildPayload(req, false, st)
	if err != nil {
		t.Fatal(err)
	}
	if pl["model"] != "gpt-4o" || pl["max_completion_tokens"] != 100 {
		t.Fatalf("core fields wrong: %v %v", pl["model"], pl["max_completion_tokens"])
	}
	if pl["temperature"] != 0.7 || pl["top_p"] != 0.9 {
		t.Fatalf("sampling wrong: %v %v", pl["temperature"], pl["top_p"])
	}
	if pl["reasoning_effort"] != "high" {
		t.Fatalf("reasoning_effort = %v", pl["reasoning_effort"])
	}
	if _, has := pl["stream"]; has {
		t.Fatal("unary payload must not set stream")
	}
	if _, has := pl["stream_options"]; has {
		t.Fatal("unary payload must not set stream_options")
	}
	if _, has := pl["stop"]; !has {
		t.Fatal("stop sequences missing")
	}
	tools := pl["tools"].([]map[string]any)
	if tools[0]["type"] != "function" {
		t.Fatalf("tool wrapper wrong: %v", tools[0])
	}
	fn := tools[0]["function"].(map[string]any)
	if fn["name"] != "fn" {
		t.Fatalf("tool function wrong: %v", fn)
	}
	if len(fn["parameters"].(json.RawMessage)) == 0 {
		t.Fatal("empty parameters must get default schema")
	}
	if pl["logit_bias"] == nil {
		t.Fatal("Extra not merged")
	}

	// Stream mode adds stream_options by default.
	pl, err = p.buildPayload(req, true, st)
	if err != nil {
		t.Fatal(err)
	}
	so, ok := pl["stream_options"].(map[string]any)
	if !ok || so["include_usage"] != true {
		t.Fatalf("stream_options = %v", pl["stream_options"])
	}

	// BudgetTokens maps to the nearest effort.
	req.Thinking = &ThinkingConfig{BudgetTokens: 3000}
	pl, _ = p.buildPayload(req, false, st)
	if pl["reasoning_effort"] != "low" {
		t.Fatalf("budget->effort = %v, want low", pl["reasoning_effort"])
	}
}

func TestOpenAIChatBuildPayloadQuirksAndOptions(t *testing.T) {
	tests := []struct {
		name  string
		opts  []Option
		key   string
		want  bool // field present
		value any  // expected value when present
	}{
		{"default", nil, "max_completion_tokens", true, nil},
		{"legacy quirk", []Option{WithQuirks(Quirks{LegacyMaxTokens: true})}, "max_tokens", true, nil},
		{"no stream usage", []Option{WithQuirks(Quirks{NoStreamUsage: true})}, "max_completion_tokens", true, nil},
		{"pinned field", []Option{WithMaxTokensField("max_tokens")}, "max_tokens", true, nil},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			c := newTestClient(t, tt.opts...)
			p := c.provider.(*openaiChatProvider)
			req := &ChatRequest{Model: "m", Messages: []Message{User("hi")}, MaxOutputTokens: 5}
			pl, err := p.buildPayload(req, true, p.initialState())
			if err != nil {
				t.Fatal(err)
			}
			v, has := pl[tt.key]
			if tt.want && !has {
				t.Fatalf("missing field %q in %v", tt.key, pl)
			}
			if tt.value != nil && v != tt.value {
				t.Fatalf("%q = %v", tt.key, v)
			}
			if tt.name == "no stream usage" {
				if _, has := pl["stream_options"]; has {
					t.Fatal("NoStreamUsage quirk must drop stream_options")
				}
			}
		})
	}
}

// A field pinned via WithMaxTokensField must win over a probed sticky
// downgrade.
func TestOpenAIChatPinBeatsSticky(t *testing.T) {
	c := newTestClient(t, WithMaxTokensField("max_tokens"))
	p := c.provider.(*openaiChatProvider)
	yes := true
	p.stickyLegacy = &yes // pretend a probe learned the opposite
	st := p.initialState()
	req := &ChatRequest{Model: "m", Messages: []Message{User("hi")}, MaxOutputTokens: 5}
	pl, err := p.buildPayload(req, false, st)
	if err != nil {
		t.Fatal(err)
	}
	if _, has := pl["max_tokens"]; !has {
		t.Fatalf("pinned max_tokens was overridden by sticky state: %v", pl)
	}
}

func TestOpenAIChatEncodeMessages(t *testing.T) {
	c := newTestClient(t)
	p := c.provider.(*openaiChatProvider)

	req := &ChatRequest{
		System: "be nice",
		Messages: []Message{
			System("extra sys"),
			User("hello"),
			{Role: RoleUser, Blocks: []Block{
				{Type: BlockText, Text: "look "},
				{Type: BlockImage, ImageURL: "data:image/png;base64,AAA"},
			}},
			AssistantBlocks(
				Block{Type: BlockText, Text: "calling"},
				ToolCall("t1", "fn", `{"x":1}`),
			),
			ToolResult("t1", "fn", "result"),
		},
	}
	msgs, err := p.encodeMessages(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 6 {
		t.Fatalf("got %d messages: %+v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "system" || msgs[0]["content"] != "be nice" {
		t.Fatalf("system prompt wrong: %v", msgs[0])
	}
	if msgs[1]["content"] != "extra sys" {
		t.Fatalf("system message wrong: %v", msgs[1])
	}
	if msgs[2]["content"] != "hello" {
		t.Fatalf("plain user wrong: %v", msgs[2])
	}
	parts := msgs[3]["content"].([]map[string]any)
	if parts[0]["type"] != "text" || parts[1]["type"] != "image_url" {
		t.Fatalf("multimodal user wrong: %v", parts)
	}
	mm := msgs[4]
	if mm["content"] != "calling" {
		t.Fatalf("assistant text wrong: %v", mm)
	}
	tcs := mm["tool_calls"].([]map[string]any)
	if tcs[0]["id"] != "t1" || tcs[0]["type"] != "function" {
		t.Fatalf("tool call wrong: %v", tcs[0])
	}
	if msgs[5]["role"] != "tool" || msgs[5]["tool_call_id"] != "t1" {
		t.Fatalf("tool result wrong: %v", msgs[5])
	}

	// Assistant with neither text nor tool calls gets empty content.
	msgs, err = p.encodeMessages(&ChatRequest{Messages: []Message{User("hi"), {Role: RoleAssistant}}})
	if err != nil {
		t.Fatal(err)
	}
	if msgs[1]["content"] != "" {
		t.Fatalf("empty assistant content = %v", msgs[1]["content"])
	}

	// Unsupported roles are rejected.
	if _, err := p.encodeMessages(&ChatRequest{Messages: []Message{{Role: Role("bogus")}}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestOpenAIChatSanitize(t *testing.T) {
	c := newTestClient(t)
	p := c.provider.(*openaiChatProvider)

	// reasoning_effort rejected → dropped and sticky.
	st := p.initialState()
	if !p.sanitize(st, "reasoning_effort is not supported", "invalid_request_error", false) {
		t.Fatal("reasoning_effort rejection must trigger a retry")
	}
	if st.reasoning {
		t.Fatal("reasoning must be dropped")
	}

	// stream_options rejected → dropped and sticky.
	st = p.initialState()
	if !p.sanitize(st, "unknown field stream_options", "invalid_request_error", true) {
		t.Fatal("stream_options rejection must trigger a retry")
	}
	if st.streamOptions {
		t.Fatal("stream_options must be dropped")
	}

	// max_completion_tokens rejected → legacy fallback, sticky.
	st = p.initialState()
	if !p.sanitize(st, "max_completion_tokens unrecognized", "invalid_request_error", false) {
		t.Fatal("max_completion_tokens rejection must trigger a retry")
	}
	if st.tokensField != "max_tokens" {
		t.Fatalf("tokensField = %s", st.tokensField)
	}
	if p.stickyLegacy == nil || !*p.stickyLegacy {
		t.Fatal("sticky legacy flag not set")
	}

	// The reverse flip: a service requiring the modern field.
	c2 := newTestClient(t, WithQuirks(Quirks{LegacyMaxTokens: true}))
	p2 := c2.provider.(*openaiChatProvider)
	st = p2.initialState()
	if !p2.sanitize(st, "max_tokens is not supported; use max_completion_tokens", "invalid_request_error", false) {
		t.Fatal("max_tokens rejection must trigger a retry")
	}
	if st.tokensField != "max_completion_tokens" {
		t.Fatalf("tokensField = %s", st.tokensField)
	}

	// Sticky state persists into the next request.
	st = p.initialState()
	if st.tokensField != "max_tokens" {
		t.Fatalf("sticky state lost: %s", st.tokensField)
	}

	// Unrelated errors must not downgrade anything.
	st = p.initialState()
	if p.sanitize(st, "the server had an unknown internal failure", "api_error", false) {
		t.Fatal("non-parameter errors must not trigger a downgrade")
	}
}

func TestOpenAIChatDecodeResponse(t *testing.T) {
	body := []byte(`{
		"id": "cmpl-1", "model": "gpt-4o",
		"choices": [{
			"message": {
				"content": "hi",
				"reasoning_content": "thinking...",
				"tool_calls": [{"id": "t1", "type": "function", "function": {"name": "fn", "arguments": "{\"x\":1}"}}]
			},
			"finish_reason": "tool_calls"
		}],
		"usage": {"prompt_tokens": 10, "completion_tokens": 5, "total_tokens": 15,
			"prompt_tokens_details": {"cached_tokens": 4},
			"completion_tokens_details": {"reasoning_tokens": 3}}
	}`)
	resp, err := decodeOpenAIChatResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "cmpl-1" || resp.Model != "gpt-4o" {
		t.Fatalf("header wrong: %+v", resp)
	}
	if len(resp.Content) != 3 {
		t.Fatalf("content blocks = %d", len(resp.Content))
	}
	if resp.Content[0].Type != BlockThinking || resp.Content[0].Thinking != "thinking..." {
		t.Fatalf("thinking block wrong: %+v", resp.Content[0])
	}
	if resp.Content[1].Text != "hi" {
		t.Fatalf("text block wrong: %+v", resp.Content[1])
	}
	if resp.Content[2].ToolCallID != "t1" || resp.Content[2].Arguments != `{"x":1}` {
		t.Fatalf("tool block wrong: %+v", resp.Content[2])
	}
	if resp.StopReason != StopToolUse {
		t.Fatalf("stop = %s", resp.StopReason)
	}
	if resp.Usage.InputTokens != 10 || resp.Usage.CachedInputTokens != 4 || resp.Usage.ReasoningTokens != 3 {
		t.Fatalf("usage wrong: %+v", resp.Usage)
	}
	if len(resp.Raw) == 0 {
		t.Fatal("Raw body missing")
	}
}

func TestOpenAIChatDecodeResponseEdgeCases(t *testing.T) {
	// Content as an array of parts (third-party quirk).
	resp, err := decodeOpenAIChatResponse([]byte(`{"choices":[{"message":{"content":[{"type":"text","text":"a"},{"type":"text","text":"b"}]}}],"usage":null}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.Text() != "a\nb" {
		t.Fatalf("array content = %q", resp.Text())
	}

	// finish_reason as a bare number (third-party quirk).
	resp, err = decodeOpenAIChatResponse([]byte(`{"choices":[{"message":{"content":"x"},"finish_reason":1}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopOther {
		t.Fatalf("numeric finish_reason = %s", resp.StopReason)
	}

	// Inline error object with a 200 status.
	_, err = decodeOpenAIChatResponse([]byte(`{"error":{"message":"boom","type":"server_error","code":42}}`))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "boom" || apiErr.Code != "42" {
		t.Fatalf("err = %v", err)
	}

	// No choices at all.
	if _, err := decodeOpenAIChatResponse([]byte(`{"choices":[]}`)); err == nil {
		t.Fatal("empty choices must error")
	}
}

func TestParseOpenAIError(t *testing.T) {
	// Canonical error object.
	apiErr := parseOpenAIError(429, []byte(`{"error":{"message":"slow down","type":"rate_limit","code":"r1"}}`), "POST", "http://u", "req-1")
	if apiErr.Message != "slow down" || apiErr.Type != "rate_limit" || apiErr.Code != "r1" {
		t.Fatalf("apiErr = %+v", apiErr)
	}
	if !apiErr.Retryable || apiErr.RequestID != "req-1" {
		t.Fatalf("retryable/requestid wrong: %+v", apiErr)
	}

	// "error" as a bare string (third-party quirk).
	apiErr = parseOpenAIError(400, []byte(`{"error":"bad key"}`), "POST", "http://u", "")
	if apiErr.Message != "bad key" {
		t.Fatalf("bare string error = %+v", apiErr)
	}

	// Non-JSON body.
	apiErr = parseOpenAIError(502, []byte("Bad Gateway"), "GET", "http://u", "")
	if apiErr.Message != "Bad Gateway" || !apiErr.Retryable {
		t.Fatalf("plain body error = %+v", apiErr)
	}
}

func TestMapOpenAIStop(t *testing.T) {
	tests := map[string]StopReason{
		"stop": StopEnd, "length": StopLength, "tool_calls": StopToolUse,
		"function_call": StopToolUse, "content_filter": StopContentFilter, "whatever": StopOther,
	}
	for in, want := range tests {
		if got := mapOpenAIStop(in); got != want {
			t.Errorf("mapOpenAIStop(%q) = %s, want %s", in, got, want)
		}
	}
}

// Events after the [DONE] sentinel must not leak past MessageEnd.
func TestOpenAIChatStreamEventsEndDiscipline(t *testing.T) {
	c := newTestClient(t)
	p := c.provider.(*openaiChatProvider)
	body := "data: {\"id\":\"c1\",\"model\":\"m\",\"choices\":[{\"delta\":{\"role\":\"assistant\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\n" +
		"data: {\"choices\":[{\"delta\":{},\"finish_reason\":\"stop\"}],\"usage\":{\"prompt_tokens\":1,\"completion_tokens\":1,\"total_tokens\":2}}\n\n" +
		"data: [DONE]\n\n" +
		"data: {\"choices\":[{\"delta\":{\"content\":\"late\"}}]}\n\n"
	next := p.streamEvents(strings.NewReader(body))

	if ev, err := next(); err != nil || ev.Type != EventMessageStart {
		t.Fatalf("first = %v %v", ev, err)
	}
	if ev, err := next(); err != nil || ev.Type != EventTextDelta || ev.Text != "a" {
		t.Fatalf("second = %v %v", ev, err)
	}
	if ev, err := next(); err != nil || ev.Type != EventMessageEnd {
		t.Fatalf("third = %v %v", ev, err)
	}
	// Red until fixed: a chunk after [DONE] leaks a TextDelta.
	if ev, err := next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after [DONE] got ev=%v err=%v, want io.EOF", ev, err)
	}
}

func TestOpenAIChatStreamEventsErrors(t *testing.T) {
	c := newTestClient(t)
	p := c.provider.(*openaiChatProvider)

	// Mid-stream error object.
	next := p.streamEvents(strings.NewReader("data: {\"error\":{\"message\":\"boom\",\"type\":\"server_error\"}}\n\n"))
	if _, err := next(); err == nil {
		t.Fatal("error chunk must fail the stream")
	}

	// Malformed chunks are skipped, not fatal.
	next = p.streamEvents(strings.NewReader("data: not-json\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"ok\"}}]}\n\n"))
	if ev, err := next(); err != nil || ev.Type != EventMessageStart {
		t.Fatalf("start ev=%v err=%v", ev, err)
	}
	if ev, err := next(); err != nil || ev.Type != EventTextDelta || ev.Text != "ok" {
		t.Fatalf("ev=%v err=%v", ev, err)
	}

	// Truncated stream without [DONE] → StopOther, no phantom StopEnd.
	next = p.streamEvents(strings.NewReader("data: {\"choices\":[{\"delta\":{\"content\":\"partial\"}}]}\n\n"))
	var last *Event
	for {
		ev, err := next()
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		last = ev
		if ev.Type == EventMessageEnd {
			break
		}
	}
	if last.StopReason != StopOther {
		t.Fatalf("truncated stream end = %+v, want StopOther", last)
	}
}
