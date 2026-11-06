package rosetta

import (
	"encoding/json"
	"testing"
)

// RG-03: a server-side tool is expressed with Kind + Config and rendered
// into each protocol's envelope, instead of being impossible to send.
func TestServerToolWire(t *testing.T) {
	cfg := json.RawMessage(`{"max_uses":5}`)
	req := &ChatRequest{
		Model: "m", Messages: []Message{User("hi")},
		Tools: []ToolDefinition{{Kind: "web_search", Config: cfg}},
	}

	c := newTestClient(t, WithProtocol(ProtoOpenAIChat))
	p := c.provider.(*openaiChatProvider)
	pl, err := p.buildPayload(req, false, p.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	tool := pl["tools"].([]map[string]any)[0]
	if tool["type"] != "web_search" || tool["max_uses"] != float64(5) {
		t.Fatalf("chat server tool = %v", tool)
	}

	rc := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	rp := rc.provider.(*openaiResponsesProvider)
	rpl, err := rp.buildPayload(req, false, rp.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	rtool := rpl["tools"].([]map[string]any)[0]
	if rtool["type"] != "web_search" || rtool["max_uses"] != float64(5) {
		t.Fatalf("responses server tool = %v", rtool)
	}

	ac := newTestClient(t, WithProtocol(ProtoAnthropic))
	ap := ac.provider.(*anthropicProvider)
	apl, _, err := ap.buildPayload(req, false, ap.plan(req))
	if err != nil {
		t.Fatal(err)
	}
	atool := apl["tools"].([]map[string]any)[0]
	if atool["type"] != "web_search" || atool["max_uses"] != float64(5) {
		t.Fatalf("anthropic server tool = %v", atool)
	}
}

// Extra["tools"] appends to the SDK-built list instead of being rejected.
func TestExtraToolsAppend(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat))
	p := c.provider.(*openaiChatProvider)
	req := &ChatRequest{
		Model: "m", Messages: []Message{User("hi")},
		Tools: []ToolDefinition{{Name: "fn"}},
		Extra: map[string]any{"tools": []any{
			map[string]any{"type": "web_search"},
		}},
	}
	pl, err := p.buildPayload(req, false, p.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	tools := pl["tools"].([]map[string]any)
	if len(tools) != 2 || tools[0]["type"] != "function" || tools[1]["type"] != "web_search" {
		t.Fatalf("tools = %v", tools)
	}
}

// RG-04: an unmodelled output item is preserved in ChatResponse.Unknown
// with its raw JSON, not silently skipped.
func TestResponsesUnknownItemPreserved(t *testing.T) {
	resp, err := decodeResponsesResponse([]byte(`{
		"id":"r","model":"gpt-5","status":"completed",
		"output":[
			{"type":"message","role":"assistant","content":[{"type":"output_text","text":"hi"}]},
			{"type":"web_search_call","id":"ws_1","status":"completed","action":{"type":"search","query":"golang"}}
		]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Unknown) != 1 {
		t.Fatalf("Unknown = %+v, want one server-tool item", resp.Unknown)
	}
	if resp.Unknown[0].Type != "unknown:web_search_call" || len(resp.Unknown[0].Raw) == 0 {
		t.Fatalf("unknown block = %+v", resp.Unknown[0])
	}
}

func TestAnthropicUnknownBlockPreserved(t *testing.T) {
	resp, err := decodeAnthropicResponse([]byte(`{
		"id":"m","model":"claude","stop_reason":"end_turn",
		"content":[
			{"type":"text","text":"hi"},
			{"type":"server_tool_use","id":"s1","name":"web_search","input":{"q":"x"}}
		]}`))
	if err != nil {
		t.Fatal(err)
	}
	if len(resp.Unknown) != 1 || resp.Unknown[0].Type != "unknown:server_tool_use" {
		t.Fatalf("Unknown = %+v", resp.Unknown)
	}
}

// WithUnknownEvents surfaces an unmodelled SSE event verbatim; the default
// keeps the old behaviour (ignore it).
func TestUnknownEventsOptIn(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic), WithUnknownEvents(true))
	p := c.provider.(*anthropicProvider)
	next := p.streamEvents(tBody(`data: {"type":"future_event","payload":{"x":1}}` + "\n\n"))
	ev, err := next()
	if err != nil {
		t.Fatal(err)
	}
	if ev.Type != EventUnknown || len(ev.Raw) == 0 {
		t.Fatalf("event = %+v", ev)
	}

	// Default client: the same event is skipped; the stream goes straight
	// to its synthesized end event.
	plain := newTestClient(t, WithProtocol(ProtoAnthropic))
	pp := plain.provider.(*anthropicProvider)
	ev2, err := pp.streamEvents(tBody(`data: {"type":"future_event"}` + "\n\n"))()
	if err != nil {
		t.Fatal(err)
	}
	if ev2.Type == EventUnknown {
		t.Fatal("unknown event must not surface without WithUnknownEvents")
	}
}
