package rosetta

import (
	"errors"
	"io"
	"strings"
	"testing"
)

func TestAnthropicPlan(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)

	// No thinking: default floor of 4096 applies.
	pl := p.plan(&ChatRequest{Model: "m"})
	if pl.maxTokens != 4096 || pl.budget != 0 {
		t.Fatalf("plan = %+v", pl)
	}

	// WithDefaultMaxOutputTokens feeds the plan.
	c2 := newTestClient(t, WithProtocol(ProtoAnthropic), WithDefaultMaxOutputTokens(2048))
	p2 := c2.provider.(*anthropicProvider)
	if pl := p2.plan(&ChatRequest{Model: "m"}); pl.maxTokens != 2048 {
		t.Fatalf("maxTokens = %d", pl.maxTokens)
	}

	// Effort maps to a budget; budget >= max_tokens grows the cap.
	pl = p.plan(&ChatRequest{Model: "m", MaxOutputTokens: 4096, Thinking: &ThinkingConfig{Effort: EffortMedium}})
	if pl.budget != 8192 || pl.maxTokens != 8192+4096 {
		t.Fatalf("plan = %+v", pl)
	}

	// Explicit budget below the Anthropic floor is clamped.
	pl = p.plan(&ChatRequest{Model: "m", Thinking: &ThinkingConfig{BudgetTokens: 512}})
	if pl.budget != 1024 {
		t.Fatalf("budget = %d, want 1024", pl.budget)
	}
}

func TestAnthropicRectify(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)
	pl := p.plan(&ChatRequest{Model: "m", MaxOutputTokens: 20000, Thinking: &ThinkingConfig{BudgetTokens: 5000}})

	if pl.rectify("overloaded") {
		t.Fatal("unrelated error must not rectify")
	}
	if !pl.rectify("thinking.budget_tokens must be less than max_tokens") {
		t.Fatal("budget error must rectify")
	}
	if pl.budget != 32000 || pl.maxTokens != 64000 {
		t.Fatalf("rectified plan = %+v", pl)
	}
	if pl.rectify("thinking.budget_tokens again") {
		t.Fatal("rectify must apply once")
	}
}

func TestAnthropicBuildPayload(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)
	req := &ChatRequest{
		Model:         "claude-sonnet-4-5",
		Messages:      []Message{User("hi")},
		Temperature:   Float(0.5),
		StopSequences: []string{"END"},
		Tools:         []ToolDefinition{{Name: "fn", Description: "d"}},
	}
	pl, err := p.buildPayload(req, false, p.plan(req))
	if err != nil {
		t.Fatal(err)
	}
	if pl["max_tokens"] != 4096 || pl["temperature"] != 0.5 || pl["stop_sequences"] == nil {
		t.Fatalf("payload = %v", pl)
	}
	if _, has := pl["thinking"]; has {
		t.Fatal("thinking must be absent when not requested")
	}
	if pl["system"] != nil {
		t.Fatal("empty system must be omitted")
	}
	tools := pl["tools"].([]map[string]any)
	if tools[0]["name"] != "fn" || tools[0]["input_schema"] == nil {
		t.Fatalf("tool shape wrong: %v", tools[0])
	}

	// Thinking mode: budget present, sampling params dropped.
	req.Thinking = &ThinkingConfig{Effort: EffortHigh}
	pl, err = p.buildPayload(req, true, p.plan(req))
	if err != nil {
		t.Fatal(err)
	}
	th := pl["thinking"].(map[string]any)
	if th["type"] != "enabled" || th["budget_tokens"] != 32768 {
		t.Fatalf("thinking = %v", th)
	}
	if _, has := pl["temperature"]; has {
		t.Fatal("temperature must be dropped in thinking mode")
	}
	if _, has := pl["top_p"]; has {
		t.Fatal("top_p must be dropped in thinking mode")
	}
	if pl["stream"] != true {
		t.Fatal("stream flag missing")
	}

	// System is lifted to the top level.
	req = &ChatRequest{System: "be nice", Messages: []Message{System("also this"), User("hi")}}
	pl, err = p.buildPayload(req, false, p.plan(req))
	if err != nil {
		t.Fatal(err)
	}
	if pl["system"] != "be nice\n\nalso this" {
		t.Fatalf("system = %v", pl["system"])
	}
}

func TestAnthropicEncodeMessages(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)

	req := &ChatRequest{
		Messages: []Message{
			User("hello"),
			AssistantBlocks(
				Thinking("hmm", "sig"),
				Block{Type: BlockText, Text: "using a tool"},
				ToolCall("t1", "fn", `{"x":1}`),
			),
			ToolResult("t1", "fn", "the result"),
			{Role: RoleUser, Blocks: []Block{
				{Type: BlockText, Text: "and"},
				{Type: BlockImage, ImageURL: "data:image/png;base64,AAA"},
				{Type: BlockImage, ImageURL: "https://example.com/cat.png"},
			}},
		},
	}
	_, msgs, err := p.encodeMessages(req)
	if err != nil {
		t.Fatal(err)
	}
	if len(msgs) != 3 {
		t.Fatalf("got %d messages: %+v", len(msgs), msgs)
	}
	if msgs[0]["role"] != "user" {
		t.Fatalf("first = %v", msgs[0])
	}
	asst := msgs[1]["content"].([]map[string]any)
	if asst[0]["type"] != "thinking" || asst[0]["signature"] != "sig" {
		t.Fatalf("thinking replay wrong: %v", asst[0])
	}
	if asst[2]["type"] != "tool_use" {
		t.Fatalf("tool_use wrong: %v", asst[2])
	}
	third := msgs[2]["content"].([]map[string]any)
	if third[0]["type"] != "tool_result" || third[0]["tool_use_id"] != "t1" {
		t.Fatalf("tool_result wrong: %v", third[0])
	}
	imgs := 0
	var srcTypes []string
	for _, b := range third {
		if b["type"] == "image" {
			imgs++
			src := b["source"].(map[string]any)
			srcTypes = append(srcTypes, src["type"].(string))
		}
	}
	// The tool-result user turn and the image user turn merge into one.
	if imgs != 2 {
		t.Fatalf("expected 2 images in merged turn, got %d", imgs)
	}
	if srcTypes[0] != "base64" || srcTypes[1] != "url" {
		t.Fatalf("image sources = %v", srcTypes)
	}

	// Consecutive same-role messages merge; assistant-openers are rejected.
	if _, _, err := p.encodeMessages(&ChatRequest{Messages: []Message{Assistant("opener"), User("hi")}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("assistant opener err = %v", err)
	}

	// A user message whose blocks render to nothing is dropped, and the
	// payload still opens with a user turn.
	_, msgs, err = p.encodeMessages(&ChatRequest{
		Messages: []Message{{Role: RoleUser}, User("real")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if msgs[0]["role"] != "user" || len(msgs) != 1 {
		t.Fatalf("empty user handling wrong: %+v", msgs)
	}

	// Unsupported roles are rejected.
	if _, _, err := p.encodeMessages(&ChatRequest{Messages: []Message{{Role: Role("bogus")}}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v", err)
	}
}

func TestAnthropicImageAndToolInput(t *testing.T) {
	src, err := encodeAnthropicImage("data:image/jpeg;base64,QUJD")
	if err != nil {
		t.Fatal(err)
	}
	if src["media_type"] != "image/jpeg" || src["data"] != "QUJD" {
		t.Fatalf("base64 source = %v", src)
	}
	if _, err := encodeAnthropicImage("ftp://x"); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unsupported scheme err = %v", err)
	}
	if _, _, ok := splitDataURL("data:text/plain,hello"); ok {
		t.Fatal("non-base64 data URL must not split")
	}
	if m := parseToolInput(`{"a":1}`); m["a"] != float64(1) {
		t.Fatalf("parseToolInput = %v", m)
	}
	if m := parseToolInput("not json"); len(m) != 0 {
		t.Fatalf("invalid args must fall back to empty object: %v", m)
	}
}

func TestAnthropicDecodeResponse(t *testing.T) {
	body := []byte(`{
		"id": "msg_1", "model": "claude-sonnet-4-5",
		"content": [
			{"type": "thinking", "thinking": "hmm", "signature": "sig"},
			{"type": "text", "text": "hi"},
			{"type": "tool_use", "id": "t1", "name": "fn", "input": {"x": 1}},
			{"type": "redacted_thinking", "data": "xxx"}
		],
		"stop_reason": "max_tokens",
		"usage": {"input_tokens": 9, "output_tokens": 4, "cache_read_input_tokens": 2}
	}`)
	resp, err := decodeAnthropicResponse(body)
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Content) != 3 {
		t.Fatalf("blocks = %d (redacted_thinking must be skipped)", len(resp.Content))
	}
	if resp.Content[0].Thinking != "hmm" || resp.Content[0].Signature != "sig" {
		t.Fatalf("thinking block = %+v", resp.Content[0])
	}
	if resp.Content[2].ToolCallID != "t1" || resp.Content[2].Arguments != `{"x": 1}` {
		t.Fatalf("tool block = %+v", resp.Content[2])
	}
	if resp.StopReason != StopLength {
		t.Fatalf("stop = %s", resp.StopReason)
	}
	if resp.Usage.InputTokens != 9 || resp.Usage.CachedInputTokens != 2 || resp.Usage.TotalTokens != 13 {
		t.Fatalf("usage = %+v", resp.Usage)
	}

	// Missing stop_reason on a unary 200 is a clean end.
	resp, err = decodeAnthropicResponse([]byte(`{"content":[{"type":"text","text":"x"}]}`))
	if err != nil {
		t.Fatal(err)
	}
	if resp.StopReason != StopEnd {
		t.Fatalf("stop = %s, want StopEnd", resp.StopReason)
	}

	// Inline error with 200.
	_, err = decodeAnthropicResponse([]byte(`{"type":"error","error":{"type":"overloaded_error","message":"busy"}}`))
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "busy" {
		t.Fatalf("err = %v", err)
	}
}

func TestAnthropicStreamEvents(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)
	body := "event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"id\":\"msg_1\",\"model\":\"claude\",\"usage\":{\"input_tokens\":10}}}\n\n" +
		"event: ping\ndata: {\"type\":\"ping\"}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"Hi\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"thinking_delta\",\"thinking\":\"th\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"sig\"}}\n\n" +
		"event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":2,\"content_block\":{\"type\":\"tool_use\",\"id\":\"t1\",\"name\":\"fn\"}}\n\n" +
		"event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":2,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"[1,2\"}}\n\n" +
		"event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":7}}\n\n" +
		"event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"
	next := p.streamEvents(strings.NewReader(body))

	want := []EventType{EventMessageStart, EventTextDelta, EventThinkingDelta, EventThinkingDelta, EventToolCall, EventToolCall, EventMessageEnd}
	var got []EventType
	var endEv *Event
	for {
		ev, err := next()
		if err != nil {
			t.Fatalf("unexpected error at %v: %v", got, err)
		}
		got = append(got, ev.Type)
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
	if endEv.StopReason != StopToolUse || endEv.Usage == nil || endEv.Usage.InputTokens != 10 || endEv.Usage.OutputTokens != 7 {
		t.Fatalf("end event = %+v", endEv)
	}

	// Signature rides the thinking event.
	next = p.streamEvents(strings.NewReader("data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"signature_delta\",\"signature\":\"S\"}}\n\n"))
	if ev, err := next(); err != nil || ev.Signature != "S" {
		t.Fatalf("signature event = %+v err=%v", ev, err)
	}
}

// A malformed error event with no error body must surface a generic
// APIError, never a nil event (which used to panic the consumer).
func TestAnthropicStreamErrorWithoutBody(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)
	next := p.streamEvents(strings.NewReader("data: {\"type\":\"error\"}\n\n"))
	ev, err := next()
	if err == nil {
		t.Fatalf("expected an error, got event %+v", ev)
	}
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want *APIError", err)
	}
	if ev != nil {
		t.Fatalf("event must be nil on error, got %+v", ev)
	}
}

// Data after message_stop must not leak past MessageEnd.
func TestAnthropicStreamEventsEndDiscipline(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)
	body := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"c\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"x\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n" +
		"data: {\"type\":\"message_stop\"}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"late\"}}\n\n"
	next := p.streamEvents(strings.NewReader(body))
	for {
		ev, err := next()
		if errors.Is(err, io.EOF) {
			return // clean end: nothing after message_stop
		}
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if ev.Type == EventMessageEnd {
			continue
		}
		if ev.Text == "late" {
			t.Fatal("text after message_stop leaked past MessageEnd")
		}
	}
}

func TestMapAnthropicStop(t *testing.T) {
	tests := map[string]StopReason{
		"end_turn": StopEnd, "stop_sequence": StopEnd, "max_tokens": StopLength,
		"tool_use": StopToolUse, "refusal": StopRefusal, "whatever": StopOther,
	}
	for in, want := range tests {
		if got := mapAnthropicStop(in); got != want {
			t.Errorf("mapAnthropicStop(%q) = %s, want %s", in, got, want)
		}
	}
}
