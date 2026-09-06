package rosetta

import (
	"errors"
	"strings"
	"testing"
)

func TestResponsesBuildPayload(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)

	req := &ChatRequest{
		Model:           "gpt-5",
		Messages:        []Message{User("hi")},
		MaxOutputTokens: 200,
		Temperature:     Float(0.7),
		Tools:           []ToolDefinition{{Name: "fn", Description: "d"}},
		Extra:           map[string]any{"previous_response_id": "resp_0"},
	}
	pl, err := p.buildPayload(req, false)
	if err != nil {
		t.Fatal(err)
	}
	if pl["model"] != "gpt-5" || pl["max_output_tokens"] != 200 {
		t.Fatalf("core fields wrong: %v %v", pl["model"], pl["max_output_tokens"])
	}
	if pl["temperature"] != 0.7 {
		t.Fatalf("temperature = %v", pl["temperature"])
	}
	if _, has := pl["stop"]; has {
		t.Fatal("stop must not be sent (unsupported by protocol)")
	}
	tools := pl["tools"].([]map[string]any)
	if tools[0]["type"] != "function" || tools[0]["name"] != "fn" {
		t.Fatalf("flat tool shape wrong: %v", tools[0])
	}
	if pl["previous_response_id"] != "resp_0" {
		t.Fatal("Extra not merged")
	}

	// Thinking: reasoning.effort, sampling dropped.
	req.Thinking = &ThinkingConfig{Effort: EffortLow}
	pl, err = p.buildPayload(req, true)
	if err != nil {
		t.Fatal(err)
	}
	rs := pl["reasoning"].(map[string]any)
	if rs["effort"] != "low" {
		t.Fatalf("reasoning = %v", pl["reasoning"])
	}
	if _, has := pl["temperature"]; has {
		t.Fatal("temperature must be dropped for reasoning requests")
	}
	if pl["stream"] != true {
		t.Fatal("stream flag missing")
	}
}

func TestResponsesEncodeInput(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)

	req := &ChatRequest{
		System: "sys",
		Messages: []Message{
			System("more sys"),
			User("hello"),
			{Role: RoleUser, Blocks: []Block{
				{Type: BlockText, Text: "look"},
				{Type: BlockImage, ImageURL: "https://example.com/cat.png"},
			}},
			AssistantBlocks(
				Block{Type: BlockText, Text: "calling"},
				ToolCall("c1", "fn", `{"x":1}`),
			),
			ToolResult("c1", "fn", "out"),
		},
	}
	items, err := p.encodeInput(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 7 {
		t.Fatalf("got %d items: %+v", len(items), items)
	}
	if items[0]["role"] != "system" || items[0]["content"] != "sys" {
		t.Fatalf("system item wrong: %v", items[0])
	}
	if items[1]["role"] != "system" {
		t.Fatalf("system message wrong: %v", items[1])
	}
	if items[2]["content"] != "hello" {
		t.Fatalf("plain user wrong: %v", items[2])
	}
	parts := items[3]["content"].([]map[string]any)
	if parts[0]["type"] != "input_text" || parts[1]["type"] != "input_image" {
		t.Fatalf("multimodal user wrong: %v", parts)
	}
	if items[4]["role"] != "assistant" {
		t.Fatalf("assistant item wrong: %v", items[4])
	}
	fc := items[5]
	if fc["type"] != "function_call" || fc["call_id"] != "c1" || fc["name"] != "fn" {
		t.Fatalf("function_call wrong: %v", fc)
	}
	out := items[6]
	if out["type"] != "function_call_output" || out["call_id"] != "c1" || out["output"] != "out" {
		t.Fatalf("function_call_output wrong: %v", out)
	}

	if _, err := p.encodeInput(&ChatRequest{Messages: []Message{{Role: Role("bogus")}}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v", err)
	}
}

func TestResponsesDecode(t *testing.T) {
	body := []byte(`{
		"id": "resp_1", "model": "gpt-5", "status": "completed",
		"output": [
			{"type": "reasoning", "summary": [{"type": "summary_text", "text": "s1"}], "content": [{"type": "reasoning_text", "text": "s2"}]},
			{"type": "message", "content": [{"type": "output_text", "text": "hi"}]},
			{"type": "function_call", "call_id": "c1", "name": "fn", "arguments": "{\"x\":1}"}
		],
		"usage": {"input_tokens": 4, "output_tokens": 3, "total_tokens": 7,
			"input_tokens_details": {"cached_tokens": 2},
			"output_tokens_details": {"reasoning_tokens": 1}}
	}`)
	resp, err := decodeResponsesResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 3 {
		t.Fatalf("blocks = %d", len(resp.Content))
	}
	if resp.Content[0].Type != BlockThinking || resp.Content[0].Thinking != "s1\ns2" {
		t.Fatalf("thinking block = %+v", resp.Content[0])
	}
	if resp.Content[1].Text != "hi" {
		t.Fatalf("text block = %+v", resp.Content[1])
	}
	if resp.Content[2].ToolCallID != "c1" {
		t.Fatalf("tool block = %+v", resp.Content[2])
	}
	if resp.StopReason != StopEnd {
		t.Fatalf("stop = %s", resp.StopReason)
	}
	if resp.Usage.InputTokens != 4 || resp.Usage.CachedInputTokens != 2 || resp.Usage.ReasoningTokens != 1 {
		t.Fatalf("usage = %+v", resp.Usage)
	}

	// Incomplete with a reason maps to a unified stop reason.
	resp, err = decodeResponsesResponse([]byte(`{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopLength {
		t.Fatalf("incomplete stop = %s", resp.StopReason)
	}

	// Missing status on a unary 200 is a clean end.
	resp, err = decodeResponsesResponse([]byte(`{"output":[]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopEnd {
		t.Fatalf("stop = %s, want StopEnd", resp.StopReason)
	}

	// Inline error with 200.
	_, err = decodeResponsesResponse([]byte(`{"error":{"message":"boom","type":"api_error"}}`))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "boom" {
		t.Fatalf("err = %v", err)
	}
}

func TestResponsesStreamEvents(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hi\"}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"delta\":\"th\"}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":2,\"item\":{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"fn\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":2,\"delta\":\"[1,2\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"
	next := p.streamEvents(strings.NewReader(body))

	want := []EventType{EventMessageStart, EventTextDelta, EventThinkingDelta, EventToolCall, EventToolCall, EventMessageEnd}
	var got []EventType
	var endEv *Event
	for {
		ev, err := next()
		if err != nil {
			t.Fatalf("unexpected error at %v: %v", got, err)
		}
		got = append(got, ev.Type)
		if ev.Type == EventToolCall && ev.ToolID == "c1" {
			if ev.ToolName != "fn" {
				t.Fatalf("tool call start = %+v", ev)
			}
		}
		if ev.Type == EventMessageEnd {
			endEv = ev
			break
		}
	}
	if len(got) != len(want) {
		t.Fatalf("events = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("events = %v, want %v", got, want)
		}
	}
	if endEv.StopReason != StopEnd || endEv.Usage == nil || endEv.Usage.TotalTokens != 3 {
		t.Fatalf("end event = %+v", endEv)
	}

	// response.incomplete ends the stream with StopLength.
	next = p.streamEvents(strings.NewReader(
		"data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	if ev, err := next(); err != nil || ev.Type != EventMessageEnd || ev.StopReason != StopLength {
		t.Fatalf("incomplete end = %+v err=%v", ev, err)
	}
}

func TestResponsesStreamEventsErrors(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)

	// response.failed surfaces the embedded error.
	next := p.streamEvents(strings.NewReader(
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"bad\",\"type\":\"api_error\"}}}\n\n"))
	if _, err := next(); err == nil {
		t.Fatal("response.failed must fail the stream")
	}

	// Bare error event.
	next = p.streamEvents(strings.NewReader(
		"data: {\"type\":\"error\",\"code\":\"srv_err\",\"message\":\"oops\"}\n\n"))
	_, err := next()
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "oops" || apiErr.Code != "srv_err" {
		t.Fatalf("err = %v", err)
	}

	// Malformed events are skipped.
	next = p.streamEvents(strings.NewReader(
		"data: not-json\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
	if ev, err := next(); err != nil || ev.Type != EventTextDelta || ev.Text != "ok" {
		t.Fatalf("ev=%v err=%v", ev, err)
	}
}

func TestMapResponsesStop(t *testing.T) {
	tests := []struct {
		status, reason string
		want           StopReason
	}{
		{"completed", "", StopEnd},
		{"incomplete", "max_output_tokens", StopLength},
		{"incomplete", "content_filter", StopContentFilter},
		{"incomplete", "weird", StopOther},
		{"in_progress", "", StopOther},
	}
	for _, tt := range tests {
		if got := mapResponsesStop(tt.status, tt.reason); got != tt.want {
			t.Errorf("mapResponsesStop(%q,%q) = %s, want %s", tt.status, tt.reason, got, tt.want)
		}
	}
}
