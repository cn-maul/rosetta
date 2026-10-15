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
	"testing/synctest"
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
	pl, err := p.buildPayload(req, false, p.initialState("gpt-5"))
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
	pl, err = p.buildPayload(req, true, p.initialState("gpt-5"))
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
	// A completed response carrying a function_call reports tool_use.
	if resp.StopReason != StopToolUse {
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
	next := p.streamEvents(tBody(body))

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
	// A completed response that carried a function_call reports tool_use,
	// not a plain end_turn: status alone cannot distinguish them.
	if endEv.StopReason != StopToolUse || endEv.Usage == nil || endEv.Usage.TotalTokens != 3 {
		t.Fatalf("end event = %+v", endEv)
	}

	// response.incomplete ends the stream with StopLength.
	next = p.streamEvents(tBody(
		"data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"incomplete_details\":{\"reason\":\"max_output_tokens\"},\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n"))
	if ev, err := next(); err != nil || ev.Type != EventMessageEnd || ev.StopReason != StopLength {
		t.Fatalf("incomplete end = %+v err=%v", ev, err)
	}
}

// S01: the whole point of the change. Two summary deltas build the text,
// the terminating event delivers the sealed payload, and both land in the
// same block that Collect returns.
func TestResponsesStreamSealedReasoningRoundTrip(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\"}}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"item_id\":\"rs_1\",\"delta\":\"step one \"}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"item_id\":\"rs_1\",\"delta\":\"step two\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_1\",\"summary\":[],\"encrypted_content\":\"OPAQUE-1\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n"
	resp := collectStream(t, p, body)
	if len(resp.Content) != 1 {
		t.Fatalf("got %d blocks, want 1: %+v", len(resp.Content), resp.Content)
	}
	got := resp.Content[0]
	if got.Type != BlockThinking {
		t.Fatalf("block type = %s, want thinking", got.Type)
	}
	if got.Thinking != "step one step two" {
		t.Errorf("Thinking = %q", got.Thinking)
	}
	if got.Sealed != "OPAQUE-1" || got.SealedBy != string(ProtoOpenAIResponses) || got.ReasoningID != "rs_1" {
		t.Errorf("seal = %+v", got)
	}
}

// S02: a reasoning item that does not sit at output index 0 must still get
// its delta text and its sealed payload into one block. Before the item
// registry, the delta went to block 0 and the payload elsewhere.
func TestResponsesStreamSealedReasoningNonZeroIndex(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":1,\"item_id\":\"rs_9\",\"delta\":\"thought\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_9\",\"encrypted_content\":\"OPAQUE-9\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\"}}\n\n"
	resp := collectStream(t, p, body)
	if len(resp.Content) != 2 {
		t.Fatalf("got %d blocks, want 2: %+v", len(resp.Content), resp.Content)
	}
	var th *Block
	for i := range resp.Content {
		if resp.Content[i].Type == BlockThinking {
			th = &resp.Content[i]
		}
	}
	if th == nil {
		t.Fatal("no thinking block")
	}
	if th.Thinking != "thought" || th.Sealed != "OPAQUE-9" || th.ReasoningID != "rs_9" {
		t.Errorf("thinking block = %+v", th)
	}
}

// S10: a sealed payload with no readable text at all is still a real turn
// and must produce a block, since dropping it breaks the next replay.
func TestResponsesStreamSealedOnlyReasoning(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_s\",\"summary\":[],\"encrypted_content\":\"OPAQUE-S\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	resp := collectStream(t, p, body)
	if len(resp.Content) != 1 {
		t.Fatalf("got %d blocks, want 1: %+v", len(resp.Content), resp.Content)
	}
	got := resp.Content[0]
	if got.Thinking != "" || got.Sealed != "OPAQUE-S" || got.ReasoningID != "rs_s" {
		t.Errorf("block = %+v", got)
	}
}

// S16: the terminating response.output is the only place some emitters
// publish the final reasoning item. Its content must be delivered before
// the single end event, not dropped when completed arrives.
func TestResponsesStreamCompletedOnlyReasoning(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[" +
		"{\"type\":\"reasoning\",\"id\":\"rs_c\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"late text\"}],\"encrypted_content\":\"OPAQUE-C\"}" +
		"]}}\n\n"
	next := p.streamEvents(tBody(body))
	var evs []EventType
	for {
		ev, err := next()
		if err != nil {
			t.Fatalf("err after %v: %v", evs, err)
		}
		evs = append(evs, ev.Type)
		if ev.Type == EventMessageEnd {
			break
		}
	}
	want := []EventType{EventMessageStart, EventThinkingDelta, EventThinkingDelta, EventMessageEnd}
	if len(evs) != len(want) {
		t.Fatalf("events = %v, want %v", evs, want)
	}
	for i := range want {
		if evs[i] != want[i] {
			t.Fatalf("events = %v, want %v", evs, want)
		}
	}
}

// S14: done and completed describing the same item must not duplicate its
// text or its sealed payload.
func TestResponsesStreamDoneThenCompletedNoDuplicate(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"item_id\":\"rs_d\",\"delta\":\"once\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_d\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"once\"}],\"encrypted_content\":\"OPAQUE-D\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\",\"output\":[" +
		"{\"type\":\"reasoning\",\"id\":\"rs_d\",\"summary\":[{\"type\":\"summary_text\",\"text\":\"once\"}],\"encrypted_content\":\"OPAQUE-D\"}" +
		"]}}\n\n"
	resp := collectStream(t, p, body)
	if len(resp.Content) != 1 {
		t.Fatalf("got %d blocks, want 1: %+v", len(resp.Content), resp.Content)
	}
	got := resp.Content[0]
	if got.Thinking != "once" {
		t.Errorf("Thinking = %q, want the delta exactly once", got.Thinking)
	}
	if got.Sealed != "OPAQUE-D" {
		t.Errorf("Sealed = %q", got.Sealed)
	}
}

// S15: one item claiming two different sealed payloads is an upstream
// inconsistency, not something to silently overwrite.
func TestResponsesStreamConflictingSealFails(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_x\",\"encrypted_content\":\"ONE\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_x\",\"encrypted_content\":\"TWO\"}}\n\n"
	next := p.streamEvents(tBody(body))
	var lastErr error
	for i := 0; i < 8; i++ {
		if _, err := next(); err != nil {
			lastErr = err
			break
		}
	}
	if !errors.Is(lastErr, ErrUpstreamMalformed) {
		t.Fatalf("err = %v, want ErrUpstreamMalformed", lastErr)
	}
}

// S08: the nested item id and the top-level item_id disagreeing is an
// upstream inconsistency; picking one silently could fold one item's
// payload into another item's block.
func TestResponsesStreamIDConflictFails(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"item_id\":\"rs_a\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_b\",\"encrypted_content\":\"B\"}}\n\n"
	next := p.streamEvents(tBody(body))
	var lastErr error
	for i := 0; i < 8; i++ {
		if _, err := next(); err != nil {
			lastErr = err
			break
		}
	}
	if !errors.Is(lastErr, ErrUpstreamMalformed) {
		t.Fatalf("err = %v, want ErrUpstreamMalformed", lastErr)
	}
}

// R03: the block order a response decodes into must survive being encoded
// back as the next turn's input. Collecting the assistant text up front
// used to move a text block ahead of the reasoning and tool items that
// preceded it.
func TestResponsesBlockOrderSurvivesReplay(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := `{"id":"r1","model":"m","status":"completed","output":[
		{"type":"reasoning","id":"rs_1","summary":[{"type":"summary_text","text":"S1"}],"encrypted_content":"B1"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"A"}]},
		{"type":"function_call","call_id":"c1","name":"fn","arguments":"{\"a\":1}"},
		{"type":"reasoning","id":"rs_2","summary":[{"type":"summary_text","text":"S2"}],"encrypted_content":"B2"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"B"}]}
	]}`
	resp, err := decodeResponsesResponse([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	wantTypes := []BlockType{BlockThinking, BlockText, BlockToolCall, BlockThinking, BlockText}
	if len(resp.Content) != len(wantTypes) {
		t.Fatalf("got %d blocks, want %d", len(resp.Content), len(wantTypes))
	}
	for i := range wantTypes {
		if resp.Content[i].Type != wantTypes[i] {
			t.Fatalf("block %d = %s, want %s", i, resp.Content[i].Type, wantTypes[i])
		}
	}
	req := &ChatRequest{
		Model: "m",
		Messages: []Message{
			User("go"),
			{Role: RoleAssistant, Blocks: resp.Content},
		},
	}
	pl, err := p.buildPayload(req, false, p.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	input := pl["input"].([]map[string]any)
	var got []string
	for _, it := range input {
		switch it["type"] {
		case "message":
			role, _ := it["role"].(string)
			got = append(got, "message:"+role)
		default:
			got = append(got, it["type"].(string))
		}
	}
	want := []string{"message:user", "reasoning", "message:assistant", "function_call", "reasoning", "message:assistant"}
	if len(got) != len(want) {
		t.Fatalf("input items = %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("input items = %v, want %v", got, want)
		}
	}
	// The reasoning items keep their ids and payloads, and carry an
	// explicit summary array.
	for i, it := range input {
		if it["type"] != "reasoning" {
			continue
		}
		if _, ok := it["summary"].([]map[string]any); !ok {
			t.Errorf("reasoning item %d has no summary array: %v", i, it)
		}
	}
	if input[1]["id"] != "rs_1" || input[1]["encrypted_content"] != "B1" {
		t.Errorf("reasoning item = %v", input[1])
	}
	if input[4]["id"] != "rs_2" || input[4]["encrypted_content"] != "B2" {
		t.Errorf("second reasoning item = %v", input[4])
	}
}

// R05: a caller that built the sealed block by hand without an id keeps
// the previous behavior — the id is not invented for them.
func TestResponsesSealedThinkingWithoutIDOmitsID(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	req := &ChatRequest{
		Model: "m",
		Messages: []Message{
			{Role: RoleAssistant, Blocks: []Block{SealedThinking("s", "BLOB", string(ProtoOpenAIResponses))}},
		},
	}
	pl, err := p.buildPayload(req, false, p.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	input := pl["input"].([]map[string]any)
	if len(input) != 1 {
		t.Fatalf("got %d items, want 1", len(input))
	}
	if _, ok := input[0]["id"]; ok {
		t.Errorf("id invented for a hand-built block: %v", input[0])
	}
	if input[0]["encrypted_content"] != "BLOB" {
		t.Errorf("payload = %v", input[0])
	}
}

// §9.4 two-turn integration: the first turn streams a reasoning item and a
// tool call, the second turn replays both and the captured request must
// carry the reasoning id, its sealed payload, the call id and the item
// order exactly. Driven through the public Client API, not the decoder.
func TestResponsesTwoTurnSealedReplay(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		if !strings.Contains(string(body), "rs_turn1") {
			// First turn: stream a reasoning item plus a tool call.
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w,
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_1\",\"model\":\"gpt-5\"}}\n\n"+
					"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_turn1\"}}\n\n"+
					"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"item_id\":\"rs_turn1\",\"delta\":\"thinking\"}\n\n"+
					"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_turn1\",\"summary\":[],\"encrypted_content\":\"SEAL-1\"}}\n\n"+
					"data: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"fn\"}}\n\n"+
					"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":1,\"item_id\":\"fc_1\",\"delta\":\"{\\\"q\\\":1}\"}\n\n"+
					"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"function_call\",\"id\":\"fc_1\",\"call_id\":\"call_1\",\"name\":\"fn\",\"arguments\":\"{\\\"q\\\":1}\"}}\n\n"+
					"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":3,\"output_tokens\":2,\"total_tokens\":5}}}\n\n")
			return
		}
		// Second turn: a plain JSON answer is enough; what matters is the
		// request the client built.
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_2","model":"gpt-5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer srv.Close()

	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithProtocol(ProtoOpenAIResponses))
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()

	st, err := c.ChatStream(ctx, &ChatRequest{Model: "gpt-5", Messages: []Message{User("go")}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Content) != 2 {
		t.Fatalf("first turn blocks = %+v", first.Content)
	}
	if first.Content[0].Sealed != "SEAL-1" || first.Content[0].ReasoningID != "rs_turn1" {
		t.Fatalf("reasoning block = %+v", first.Content[0])
	}

	// Build the second turn from the first turn's own blocks, which is what
	// an agent does after running the tool.
	history := []Message{
		User("go"),
		{Role: RoleAssistant, Blocks: first.Content},
		ToolResult("call_1", "", "tool output"),
	}
	if _, err := c.Chat(ctx, &ChatRequest{Model: "gpt-5", Messages: history}); err != nil {
		t.Fatal(err)
	}

	input := inputItems(t, got)
	if len(input) != 4 {
		t.Fatalf("second-turn input = %+v", input)
	}
	if input[1]["type"] != "reasoning" || input[1]["id"] != "rs_turn1" || input[1]["encrypted_content"] != "SEAL-1" {
		t.Errorf("replayed reasoning = %v", input[1])
	}
	if input[2]["type"] != "function_call" || input[2]["call_id"] != "call_1" {
		t.Errorf("replayed tool call = %v", input[2])
	}
	if input[3]["type"] != "function_call_output" || input[3]["call_id"] != "call_1" {
		t.Errorf("replayed tool result = %v", input[3])
	}
}

// The JSON-fallback path (a stream:true request answered with a buffered
// 200 application/json body) must carry the sealed payload just as far as
// the SSE path does.
func TestResponsesTwoTurnSealedReplayJSONFallback(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		_ = json.Unmarshal(body, &got)
		if !strings.Contains(string(body), "rs_json") {
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"resp_1","model":"gpt-5","status":"completed","output":[`+
				`{"type":"reasoning","id":"rs_json","summary":[{"type":"summary_text","text":"t"}],"encrypted_content":"SEAL-J"},`+
				`{"type":"function_call","call_id":"call_j","name":"fn","arguments":"{}"}]}`)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"id":"resp_2","model":"gpt-5","status":"completed","output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"done"}]}]}`)
	}))
	defer srv.Close()

	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithProtocol(ProtoOpenAIResponses))
	if err != nil {
		t.Fatal(err)
	}
	st, err := c.ChatStream(context.Background(), &ChatRequest{Model: "gpt-5", Messages: []Message{User("go")}})
	if err != nil {
		t.Fatal(err)
	}
	first, err := st.Collect()
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Content) != 2 {
		t.Fatalf("blocks = %+v", first.Content)
	}
	if first.Content[0].Sealed != "SEAL-J" || first.Content[0].ReasoningID != "rs_json" {
		t.Fatalf("reasoning block = %+v", first.Content[0])
	}
	history := []Message{
		User("go"),
		{Role: RoleAssistant, Blocks: first.Content},
		ToolResult("call_j", "", "out"),
	}
	if _, err := c.Chat(context.Background(), &ChatRequest{Model: "gpt-5", Messages: history}); err != nil {
		t.Fatal(err)
	}
	input := inputItems(t, got)
	if len(input) != 4 || input[1]["id"] != "rs_json" || input[1]["encrypted_content"] != "SEAL-J" {
		t.Fatalf("second-turn input = %+v", input)
	}
}

// S03: two reasoning items whose deltas interleave and whose done events
// arrive in the opposite order. Each id keeps its own text and payload.
func TestResponsesStreamTwoReasoningItemsInterleaved(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"item_id\":\"rs_a\",\"delta\":\"A1 \"}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":1,\"item_id\":\"rs_b\",\"delta\":\"B1 \"}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"item_id\":\"rs_a\",\"delta\":\"A2\"}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":1,\"item_id\":\"rs_b\",\"delta\":\"B2\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_b\",\"encrypted_content\":\"SEAL-B\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_a\",\"encrypted_content\":\"SEAL-A\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	resp := collectStream(t, p, body)
	if len(resp.Content) != 2 {
		t.Fatalf("got %d blocks, want 2: %+v", len(resp.Content), resp.Content)
	}
	// Output order, not done order: rs_a sits at output index 0.
	if resp.Content[0].ReasoningID != "rs_a" || resp.Content[0].Thinking != "A1 A2" || resp.Content[0].Sealed != "SEAL-A" {
		t.Errorf("block 0 = %+v", resp.Content[0])
	}
	if resp.Content[1].ReasoningID != "rs_b" || resp.Content[1].Thinking != "B1 B2" || resp.Content[1].Sealed != "SEAL-B" {
		t.Errorf("block 1 = %+v", resp.Content[1])
	}
}

// S04: the official shape only puts the item id inside the item on
// added/done and in the top-level item_id on deltas. The two must be
// matched into one identity.
func TestResponsesStreamIDFromNestedAndTopLevel(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_n\"}}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"item_id\":\"rs_n\",\"delta\":\"text\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_n\",\"encrypted_content\":\"SEAL\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	resp := collectStream(t, p, body)
	if len(resp.Content) != 1 {
		t.Fatalf("got %d blocks, want 1: %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Thinking != "text" || resp.Content[0].Sealed != "SEAL" || resp.Content[0].ReasoningID != "rs_n" {
		t.Errorf("block = %+v", resp.Content[0])
	}
}

// S06: a later event naming a different output index for the same item id
// must not fork a second block.
func TestResponsesStreamSameIDDifferentIndex(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"item_id\":\"rs_s\",\"delta\":\"text\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":3,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_s\",\"encrypted_content\":\"SEAL\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	resp := collectStream(t, p, body)
	if len(resp.Content) != 1 {
		t.Fatalf("got %d blocks, want 1: %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Thinking != "text" || resp.Content[0].Sealed != "SEAL" {
		t.Errorf("block = %+v", resp.Content[0])
	}
}

// S18: a done event already delivered the final payload, so a stream that
// dies before completed still leaves the payload in Partial alongside the
// truncation error.
func TestResponsesStreamSealedSurvivesTruncation(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_t\",\"encrypted_content\":\"SEAL\"}}\n\n"
	next := p.streamEvents(tBody(body))
	for {
		_, err := next()
		if err != nil {
			if !errors.Is(err, ErrStreamTruncated) {
				t.Fatalf("err = %v, want ErrStreamTruncated", err)
			}
			break
		}
	}
	st := newStream(p.streamEvents(tBody(body)), nil)
	defer st.Close()
	for st.Next() {
	}
	if !errors.Is(st.Err(), ErrStreamTruncated) {
		t.Fatalf("stream Err = %v", st.Err())
	}
	got := st.Partial().Content
	if len(got) != 1 || got[0].Sealed != "SEAL" || got[0].ReasoningID != "rs_t" {
		t.Fatalf("partial after truncation = %+v", got)
	}
}

// S19: the payload on output_item.added is explicitly not final. It must
// not be published as the block's sealed state.
func TestResponsesStreamAddedSealIsNotFinal(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"reasoning\",\"id\":\"rs_a\",\"encrypted_content\":\"PARTIAL\"}}\n\n" +
		"data: {\"type\":\"response.reasoning_summary_text.delta\",\"output_index\":0,\"item_id\":\"rs_a\",\"delta\":\"text\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r\",\"status\":\"completed\"}}\n\n"
	resp := collectStream(t, p, body)
	if len(resp.Content) != 1 {
		t.Fatalf("got %d blocks, want 1: %+v", len(resp.Content), resp.Content)
	}
	if resp.Content[0].Sealed != "" {
		t.Errorf("in-flight payload published as final: %+v", resp.Content[0])
	}
	if resp.Content[0].Thinking != "text" {
		t.Errorf("Thinking = %q", resp.Content[0].Thinking)
	}
}

func TestResponsesStreamEventsErrors(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)

	// response.failed surfaces the embedded error, with request context.
	next := p.streamEvents(tBody(
		"data: {\"type\":\"response.failed\",\"response\":{\"error\":{\"message\":\"bad\",\"type\":\"api_error\"}}}\n\n"))
	_, err := next()
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Message != "bad" || apiErr.Method != "POST" || apiErr.URL == "" {
		t.Fatalf("response.failed err = %v, want APIError with request context", err)
	}

	// Bare error event.
	next = p.streamEvents(tBody(
		"data: {\"type\":\"error\",\"code\":\"srv_err\",\"message\":\"oops\"}\n\n"))
	_, err = next()
	if !errors.As(err, &apiErr) || apiErr.Message != "oops" || apiErr.Code != "srv_err" {
		t.Fatalf("err = %v", err)
	}

	// Malformed events fail the stream: silently dropping one could lose
	// text or tool-call fragments.
	next = p.streamEvents(tBody(
		"data: not-json\n\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"ok\"}\n\n"))
	if _, err := next(); err == nil {
		t.Fatal("malformed event must fail the stream")
	}
}

// Events after response.completed must not leak past MessageEnd (same
// end-discipline as the [DONE]/message_stop guards on the other two
// protocols).
func TestResponsesStreamEventsEndDiscipline(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	body := "data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"m\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"usage\":{\"input_tokens\":1,\"output_tokens\":1,\"total_tokens\":2}}}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"late\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	next := p.streamEvents(tBody(body))

	if ev, err := next(); err != nil || ev.Type != EventMessageStart {
		t.Fatalf("first = %v %v", ev, err)
	}
	if ev, err := next(); err != nil || ev.Type != EventMessageEnd || ev.StopReason != StopEnd {
		t.Fatalf("second = %v %v", ev, err)
	}
	if ev, err := next(); !errors.Is(err, io.EOF) {
		t.Fatalf("after completed got ev=%v err=%v, want io.EOF", ev, err)
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

// Full unary round trip over the Responses protocol: the request hits
// POST /responses with the unified payload and the response decodes.
func TestResponsesChatE2E(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/responses" {
				w.WriteHeader(404)
				return
			}
			if r.Header.Get("Authorization") != "Bearer k" {
				w.WriteHeader(401)
				return
			}
			body, _ := io.ReadAll(r.Body)
			var pl map[string]any
			if err := json.Unmarshal(body, &pl); err != nil {
				w.WriteHeader(400)
				return
			}
			if pl["model"] != "gpt-5" || pl["max_output_tokens"] != float64(200) {
				w.WriteHeader(400)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"id":"r1","model":"gpt-5","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"hello responses"}]}],"usage":{"input_tokens":4,"output_tokens":2,"total_tokens":6}}`)
		}))
		c := newTestClient(t, WithProtocol(ProtoOpenAIResponses), WithHTTPClient(srv.Client()))
		resp, err := c.Chat(context.Background(), &ChatRequest{
			Model:           "gpt-5",
			Messages:        []Message{User("hi")},
			MaxOutputTokens: 200,
		})
		if err != nil {
			t.Fatal(err)
		}
		if resp.Text() != "hello responses" || resp.StopReason != StopEnd {
			t.Fatalf("resp = %+v", resp)
		}
		if resp.Usage.TotalTokens != 6 {
			t.Fatalf("usage = %+v", resp.Usage)
		}
	})
}

// Streaming round trip: typed SSE events fold into unified events, usage
// arrives with response.completed and the stream ends there.
func TestResponsesStreamChatE2E(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			w.Header().Set("Content-Type", "text/event-stream")
			io.WriteString(w, "event: response.created\n"+
				"data: {\"type\":\"response.created\",\"response\":{\"id\":\"r1\",\"model\":\"gpt-5\",\"status\":\"in_progress\"}}\n\n"+
				"event: response.output_text.delta\n"+
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"hel\"}\n\n"+
				"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n"+
				"event: response.completed\n"+
				"data: {\"type\":\"response.completed\",\"response\":{\"id\":\"r1\",\"status\":\"completed\",\"usage\":{\"input_tokens\":4,\"output_tokens\":2,\"total_tokens\":6}}}\n\n")
		}))
		c := newTestClient(t, WithProtocol(ProtoOpenAIResponses), WithHTTPClient(srv.Client()))
		stream, err := c.ChatStream(context.Background(), &ChatRequest{Model: "gpt-5", Messages: []Message{User("hi")}})
		if err != nil {
			t.Fatal(err)
		}
		defer stream.Close()

		var sb strings.Builder
		var startID string
		for stream.Next() {
			switch ev := stream.Event(); ev.Type {
			case EventMessageStart:
				startID = ev.ID
			case EventTextDelta:
				sb.WriteString(ev.Text)
			}
		}
		if err := stream.Err(); err != nil {
			t.Fatal(err)
		}
		if startID != "r1" || sb.String() != "hello" {
			t.Fatalf("id=%q text=%q", startID, sb.String())
		}
		if u := stream.Usage(); u.TotalTokens != 6 {
			t.Fatalf("usage = %+v", u)
		}
		if p := stream.Partial(); p.StopReason != StopEnd {
			t.Fatalf("partial stop = %s", p.StopReason)
		}
	})
}

// Model catalog round trip over the shared OpenAI-family endpoint.
func TestResponsesListModelsE2E(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		srv := httptest.NewTestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.URL.Path != "/v1/models" {
				w.WriteHeader(404)
				return
			}
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"data":[{"id":"gpt-5"},{"id":"gpt-5-mini"}]}`)
		}))
		c := newTestClient(t, WithProtocol(ProtoOpenAIResponses), WithHTTPClient(srv.Client()))
		models, err := c.ListModels(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(models) != 2 || models[0].ID != "gpt-5" || models[1].ID != "gpt-5-mini" {
			t.Fatalf("models = %+v", models)
		}
		if models[0].Known {
			t.Fatal("remote entries must not be Known")
		}
	})
}

// collectStream drains p.streamEvents into accumulated blocks/args, mirroring
// how Stream folds events.
func collectStream(t *testing.T, p *openaiResponsesProvider, body string) *ChatResponse {
	t.Helper()
	core := &streamCore{}
	next := p.streamEvents(tBody(body))
	for {
		ev, err := next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("stream error: %v", err)
		}
		core.apply(ev)
	}
	return core.Partial()
}

func responsesProvider(t *testing.T) *openaiResponsesProvider {
	t.Helper()
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	return c.provider.(*openaiResponsesProvider)
}

// A3: the Responses error event nests details under "error".
func TestResponsesStreamNestedErrorEnvelope(t *testing.T) {
	p := responsesProvider(t)
	next := p.streamEvents(tBody(
		"data: {\"type\":\"error\",\"error\":{\"message\":\"Rate limit exceeded\",\"type\":\"rate_limit_error\",\"code\":\"rate_limit_exceeded\"}}\n\n"))
	_, err := next()
	var apiErr *APIError
	if !errors.As(err, &apiErr) {
		t.Fatalf("err = %v, want APIError", err)
	}
	if apiErr.Message != "Rate limit exceeded" || apiErr.Code != "rate_limit_exceeded" || apiErr.Type != "rate_limit_error" {
		t.Fatalf("nested error lost detail: %+v", apiErr)
	}
}

// B5 #1: a function_call announced then finalized without any argument
// delta must still carry its arguments.
func TestResponsesStreamDoneFillsMissingArgs(t *testing.T) {
	p := responsesProvider(t)
	body := "data: {\"type\":\"response.output_item.added\",\"output_index\":3,\"item\":{\"type\":\"function_call\",\"call_id\":\"c3\",\"name\":\"fn\"}}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":3,\"item\":{\"type\":\"function_call\",\"call_id\":\"c3\",\"name\":\"fn\",\"arguments\":\"{\\\"a\\\":1}\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	resp := collectStream(t, p, body)
	if len(resp.Content) != 1 || resp.Content[0].Type != BlockToolCall {
		t.Fatalf("content = %+v", resp.Content)
	}
	if resp.Content[0].Arguments != `{"a":1}` || resp.Content[0].ToolCallID != "c3" {
		t.Fatalf("done arguments not filled: %+v", resp.Content[0])
	}
}

// B5 #1 (no duplication): when deltas already streamed the arguments, the
// finalizing done must not append them a second time.
func TestResponsesStreamDoneDoesNotDuplicateArgs(t *testing.T) {
	p := responsesProvider(t)
	body := "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"c0\",\"name\":\"fn\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"output_index\":0,\"delta\":\"{\\\"a\\\":1}\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":0,\"item\":{\"type\":\"function_call\",\"call_id\":\"c0\",\"name\":\"fn\",\"arguments\":\"{\\\"a\\\":1}\"}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	resp := collectStream(t, p, body)
	if len(resp.Content) != 1 || resp.Content[0].Arguments != `{"a":1}` {
		t.Fatalf("arguments duplicated or lost: %+v", resp.Content)
	}
}

// B5 #2: a message finalized only via output_item.done (no text delta) must
// still produce its text.
func TestResponsesStreamDoneMessageText(t *testing.T) {
	p := responsesProvider(t)
	body := "data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	resp := collectStream(t, p, body)
	if resp.Text() != "hello" {
		t.Fatalf("done-only message text lost: %q", resp.Text())
	}
}

// B5 #2 (no duplication): text deltas suppress the finalizing done content.
func TestResponsesStreamDoneMessageTextNoDup(t *testing.T) {
	p := responsesProvider(t)
	body := "data: {\"type\":\"response.output_text.delta\",\"output_index\":1,\"delta\":\"hello\"}\n\n" +
		"data: {\"type\":\"response.output_item.done\",\"output_index\":1,\"item\":{\"type\":\"message\",\"content\":[{\"type\":\"output_text\",\"text\":\"hello\"}]}}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	resp := collectStream(t, p, body)
	if resp.Text() != "hello" {
		t.Fatalf("done duplicated streamed text: %q", resp.Text())
	}
}

// B5 #3: response.text.delta alias is honored.
func TestResponsesStreamTextDeltaAlias(t *testing.T) {
	p := responsesProvider(t)
	body := "data: {\"type\":\"response.text.delta\",\"output_index\":0,\"delta\":\"hi\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	resp := collectStream(t, p, body)
	if resp.Text() != "hi" {
		t.Fatalf("text.delta alias ignored: %q", resp.Text())
	}
}

// B5 #4: argument deltas keyed only by item_id coalesce onto the announced
// call even when output_index is omitted (parallel calls).
func TestResponsesStreamArgDeltaItemIDKeying(t *testing.T) {
	p := responsesProvider(t)
	body := "data: {\"type\":\"response.output_item.added\",\"output_index\":0,\"item_id\":\"A\",\"item\":{\"type\":\"function_call\",\"call_id\":\"c1\",\"name\":\"f1\"}}\n\n" +
		"data: {\"type\":\"response.output_item.added\",\"output_index\":1,\"item_id\":\"B\",\"item\":{\"type\":\"function_call\",\"call_id\":\"c2\",\"name\":\"f2\"}}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"B\",\"delta\":\"{\\\"x\\\":1}\"}\n\n" +
		"data: {\"type\":\"response.function_call_arguments.delta\",\"item_id\":\"A\",\"delta\":\"{\\\"y\\\":2}\"}\n\n" +
		"data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\"}}\n\n"
	resp := collectStream(t, p, body)
	byName := map[string]string{}
	for _, b := range resp.Content {
		if b.Type == BlockToolCall {
			byName[b.ToolName] = b.Arguments
		}
	}
	if byName["f1"] != `{"y":2}` || byName["f2"] != `{"x":1}` {
		t.Fatalf("item_id keying mismatched args: %+v", resp.Content)
	}
}

// inputItems reads the input array out of a captured request body, which
// decodes to []any of map[string]any rather than a typed slice.
func inputItems(t *testing.T, got map[string]any) []map[string]any {
	t.Helper()
	raw, ok := got["input"].([]any)
	if !ok {
		t.Fatalf("no input array in request: %v", got)
	}
	out := make([]map[string]any, 0, len(raw))
	for _, it := range raw {
		m, ok := it.(map[string]any)
		if !ok {
			t.Fatalf("input item is not an object: %v", it)
		}
		out = append(out, m)
	}
	return out
}
