package rosetta

import (
	"context"
	"fmt"
	"net/http"
	"testing"
)

func newResponsesClient(t *testing.T, h http.Handler, opts ...Option) *Client {
	t.Helper()
	return newTestClient(t, h, append([]Option{WithProtocol(ProtoOpenAIResponses)}, opts...)...)
}

func TestResponsesChatMapping(t *testing.T) {
	var gotPayload map[string]any
	c := newResponsesClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/responses" {
			t.Errorf("path = %q", r.URL.Path)
		}
		gotPayload = readPayload(t, r)
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, `{
			"id":"resp_1","model":"gpt-5","status":"completed",
			"output":[
				{"type":"reasoning","summary":[{"type":"summary_text","text":"pondering"}]},
				{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}]},
				{"type":"function_call","call_id":"call_1","name":"get_weather","arguments":"{\"city\":\"SF\"}"}
			],
			"usage":{
				"input_tokens":11,"output_tokens":22,"total_tokens":33,
				"input_tokens_details":{"cached_tokens":6},
				"output_tokens_details":{"reasoning_tokens":9}
			}
		}`)
	}))

	resp, err := c.Chat(context.Background(), &ChatRequest{
		Model:           "gpt-5",
		System:          "be brief",
		Messages:        []Message{User("hi")},
		MaxOutputTokens: 900,
		Thinking:        &ThinkingConfig{Effort: EffortLow},
		Tools:           []ToolDefinition{{Name: "get_weather", Description: "w"}},
	})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}

	// Request shape.
	if gotPayload["max_output_tokens"] != float64(900) {
		t.Errorf("max_output_tokens = %v", gotPayload["max_output_tokens"])
	}
	if r := gotPayload["reasoning"].(map[string]any); r["effort"] != "low" {
		t.Errorf("reasoning = %v", r)
	}
	tools := gotPayload["tools"].([]any)
	t0 := tools[0].(map[string]any)
	if t0["type"] != "function" || t0["name"] != "get_weather" {
		t.Errorf("responses tools must be flat objects, got %v", t0)
	}
	if _, nested := t0["function"]; nested {
		t.Errorf("chat-style nested tool shape leaked: %v", t0)
	}
	input := gotPayload["input"].([]any)
	if len(input) != 2 {
		t.Fatalf("input items = %d, want 2 (system + user)", len(input))
	}
	if i0 := input[0].(map[string]any); i0["role"] != "system" || i0["content"] != "be brief" {
		t.Errorf("system item = %v", i0)
	}
	if i1 := input[1].(map[string]any); i1["content"] != "hi" {
		t.Errorf("user item = %v", i1)
	}

	// Response decoding.
	if resp.ID != "resp_1" || resp.StopReason != StopEnd {
		t.Errorf("id=%q stop=%q", resp.ID, resp.StopReason)
	}
	if resp.Text() != "hello" || resp.ThinkingText() != "pondering" {
		t.Errorf("text=%q thinking=%q", resp.Text(), resp.ThinkingText())
	}
	if tc := resp.ToolCalls(); len(tc) != 1 || tc[0].ToolCallID != "call_1" || tc[0].Arguments != `{"city":"SF"}` {
		t.Errorf("tool calls = %+v", tc)
	}
	want := Usage{InputTokens: 11, OutputTokens: 22, TotalTokens: 33, CachedInputTokens: 6, ReasoningTokens: 9}
	if resp.Usage != want {
		t.Errorf("usage = %+v, want %+v", resp.Usage, want)
	}
}

func TestResponsesIncompleteMapsToStopLength(t *testing.T) {
	c := newResponsesClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"r","model":"m","status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"trunc"}]}],"usage":{"input_tokens":5,"output_tokens":900,"total_tokens":905}}`)
	}))
	resp, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if resp.StopReason != StopLength || resp.Text() != "trunc" {
		t.Errorf("stop=%q text=%q", resp.StopReason, resp.Text())
	}
}

func TestResponsesStreamEvents(t *testing.T) {
	c := newResponsesClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := readPayload(t, r)
		if payload["stream"] != true {
			t.Errorf("stream flag missing")
		}
		w.Header().Set("Content-Type", "text/event-stream")
		fl := w.(http.Flusher)
		events := []struct{ name, data string }{
			{"response.created", `{"type":"response.created","response":{"id":"resp_9","model":"gpt-5"}}`},
			{"response.output_item.added", `{"type":"response.output_item.added","output_index":0,"item":{"type":"reasoning","id":"rs_1"}}`},
			{"response.reasoning_summary_text.delta", `{"type":"response.reasoning_summary_text.delta","delta":"step one"}`},
			{"response.output_item.added", `{"type":"response.output_item.added","output_index":1,"item":{"type":"message","role":"assistant"}}`},
			{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"你好"}`},
			{"response.output_text.delta", `{"type":"response.output_text.delta","delta":"世界"}`},
			{"response.output_item.added", `{"type":"response.output_item.added","output_index":2,"item":{"type":"function_call","call_id":"call_7","name":"calc"}}`},
			{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":2,"delta":"{\"x\":"}`},
			{"response.function_call_arguments.delta", `{"type":"response.function_call_arguments.delta","output_index":2,"delta":"1}"}`},
			{"response.output_text.done", `{"type":"response.output_text.done","text":"你好世界"}`},
			{"response.completed", `{"type":"response.completed","response":{"id":"resp_9","model":"gpt-5","status":"completed","usage":{"input_tokens":10,"output_tokens":5,"total_tokens":15,"input_tokens_details":{"cached_tokens":4},"output_tokens_details":{"reasoning_tokens":2}}}}`},
		}
		for _, ev := range events {
			fmt.Fprintf(w, "event: %s\ndata: %s\n\n", ev.name, ev.data)
			fl.Flush()
		}
	}))

	stream, err := c.ChatStream(context.Background(), &ChatRequest{
		Model:    "gpt-5",
		Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{Effort: EffortMedium},
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
		if ev.Type == EventTextDelta || ev.Type == EventThinkingDelta {
			texts = append(texts, ev.Text)
		}
	}
	if err := stream.Err(); err != nil {
		t.Fatalf("stream error: %v", err)
	}
	wantTypes := fmt.Sprint([]EventType{
		EventMessageStart,
		EventThinkingDelta,
		EventTextDelta, EventTextDelta,
		EventToolCall, EventToolCall, EventToolCall,
		EventMessageEnd,
	})
	if got := fmt.Sprint(types); got != wantTypes {
		t.Errorf("events = %s, want %s", got, wantTypes)
	}
	if got := fmt.Sprint(texts); got != "[step one 你好 世界]" {
		t.Errorf("delta texts = %s", got)
	}

	resp, err := stream.Collect()
	if err != nil {
		t.Fatalf("Collect: %v", err)
	}
	if resp.ID != "resp_9" || resp.Text() != "你好世界" || resp.ThinkingText() != "step one" {
		t.Errorf("collected = id:%q text:%q thinking:%q", resp.ID, resp.Text(), resp.ThinkingText())
	}
	if tc := resp.ToolCalls(); len(tc) != 1 || tc[0].ToolCallID != "call_7" || tc[0].ToolName != "calc" || tc[0].Arguments != `{"x":1}` {
		t.Errorf("tool calls = %+v", tc)
	}
	if resp.StopReason != StopEnd {
		t.Errorf("stop = %q", resp.StopReason)
	}
	wantUsage := Usage{InputTokens: 10, OutputTokens: 5, TotalTokens: 15, CachedInputTokens: 4, ReasoningTokens: 2}
	if resp.Usage != wantUsage {
		t.Errorf("usage = %+v, want %+v", resp.Usage, wantUsage)
	}
}

func TestResponsesStreamFailedEvent(t *testing.T) {
	c := newResponsesClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprint(w, "event: response.created\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n")
		fmt.Fprint(w, "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"id\":\"r\",\"error\":{\"code\":\"server_error\",\"message\":\"boom\"}}}\n\n")
	}))
	stream, err := c.ChatStream(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("ChatStream: %v", err)
	}
	defer stream.Close()
	for stream.Next() {
	}
	var apiErr *APIError
	if stream.Err() == nil {
		t.Fatalf("expected failure event to surface as error")
	}
	if !asAPIError(stream.Err(), &apiErr) || apiErr.Message != "boom" {
		t.Errorf("err = %v", stream.Err())
	}
}

func asAPIError(err error, target **APIError) bool {
	if e, ok := err.(*APIError); ok {
		*target = e
		return true
	}
	return false
}

func TestResponsesToolRoundtrip(t *testing.T) {
	var input []any
	c := newResponsesClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := readPayload(t, r)
		input = payload["input"].([]any)
		fmt.Fprint(w, `{"id":"r","model":"m","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}],"usage":{"input_tokens":1,"output_tokens":1,"total_tokens":2}}`)
	}))
	req := &ChatRequest{
		Model: "m",
		Messages: []Message{
			User("weather?"),
			AssistantBlocks(ToolCall("call_1", "get_weather", `{"city":"SF"}`)),
			ToolResult("call_1", "get_weather", "sunny"),
		},
	}
	if _, err := c.Chat(context.Background(), req); err != nil {
		t.Fatalf("Chat: %v", err)
	}
	if len(input) != 3 {
		t.Fatalf("input items = %d, want 3", len(input))
	}
	fc := input[1].(map[string]any)
	if fc["type"] != "function_call" || fc["call_id"] != "call_1" || fc["name"] != "get_weather" {
		t.Errorf("function_call item = %v", fc)
	}
	fco := input[2].(map[string]any)
	if fco["type"] != "function_call_output" || fco["call_id"] != "call_1" || fco["output"] != "sunny" {
		t.Errorf("function_call_output item = %v", fco)
	}
}

func TestResponsesListModels(t *testing.T) {
	c := newResponsesClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("path = %q", r.URL.Path)
		}
		fmt.Fprint(w, `{"object":"list","data":[{"id":"gpt-5"},{"id":"o4"}]}`)
	}))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("ListModels: %v", err)
	}
	byID := map[string]ModelInfo{}
	for _, m := range models {
		byID[m.ID] = m
	}
	o4, ok := byID["o4"]
	if !ok || o4.Known || o4.Protocol != ProtoOpenAIResponses {
		t.Errorf("remote o4 wrong: %+v (ok=%v)", o4, ok)
	}
	gpt5, ok := byID["gpt-5"]
	if !ok || !gpt5.Known || !gpt5.SupportsThinking {
		t.Errorf("builtin gpt-5 wrong: %+v (ok=%v)", gpt5, ok)
	}
}
