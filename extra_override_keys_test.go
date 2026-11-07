package rosetta

import (
	"encoding/json"
	"errors"
	"testing"
)

// WithExtraOverrideKeys scopes Extra overrides to listed keys; every other
// reserved key keeps its collision check.
func TestExtraOverrideKeysScoped(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat), WithExtraOverrideKeys("messages"))
	p := c.provider.(*openaiChatProvider)

	// "messages" is whitelisted → passes and overrides the SDK-built field.
	pl, err := p.buildPayload(&ChatRequest{
		Model:    "m",
		Messages: []Message{User("hi")},
		Extra:    map[string]any{"messages": []any{map[string]any{"role": "user", "content": "override"}}},
	}, false, p.initialState("m"))
	if err != nil {
		t.Fatalf("whitelisted messages must pass: %v", err)
	}
	if _, ok := pl["messages"]; !ok {
		t.Fatal("overridden messages missing from payload")
	}

	// "stream" is not whitelisted → still rejected.
	if _, err := p.buildPayload(&ChatRequest{
		Model:    "m",
		Messages: []Message{User("hi")},
		Extra:    map[string]any{"stream": true},
	}, false, p.initialState("m")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("non-whitelisted stream must be rejected, got %v", err)
	}
}

// Empty whitelist allows nothing; WithExtraOverrides(true) still allows all.
func TestExtraOverrideKeysEmptyAndAll(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat), WithExtraOverrideKeys())
	p := c.provider.(*openaiChatProvider)
	if _, err := p.buildPayload(&ChatRequest{
		Model:    "m",
		Messages: []Message{User("hi")},
		Extra:    map[string]any{"stream": true},
	}, false, p.initialState("m")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty whitelist must reject stream, got %v", err)
	}

	c2 := newTestClient(t, WithProtocol(ProtoOpenAIChat), WithExtraOverrides(true))
	p2 := c2.provider.(*openaiChatProvider)
	if _, err := p2.buildPayload(&ChatRequest{
		Model:    "m",
		Messages: []Message{User("hi")},
		Extra:    map[string]any{"stream": true},
	}, false, p2.initialState("m")); err != nil {
		t.Fatalf("WithExtraOverrides(true) must still allow stream, got %v", err)
	}
}

// "tools" follows the whitelist too: not whitelisted → append; whitelisted
// → replace the SDK-built list.
func TestExtraOverrideKeysToolsSemantics(t *testing.T) {
	req := func() *ChatRequest {
		return &ChatRequest{
			Model:    "m",
			Messages: []Message{User("hi")},
			Tools:    []ToolDefinition{{Name: "fn1", Parameters: json.RawMessage(`{"type":"object","properties":{}}`)}},
			Extra: map[string]any{"tools": []map[string]any{{
				"type":     "function",
				"function": map[string]any{"name": "fn2", "parameters": map[string]any{"type": "object"}},
			}}},
		}
	}

	c := newTestClient(t, WithProtocol(ProtoOpenAIChat))
	p := c.provider.(*openaiChatProvider)
	pl, err := p.buildPayload(req(), false, p.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	if tools, ok := pl["tools"].([]map[string]any); !ok || len(tools) != 2 {
		t.Fatalf("append: tools = %v", pl["tools"])
	}

	c2 := newTestClient(t, WithProtocol(ProtoOpenAIChat), WithExtraOverrideKeys("tools"))
	p2 := c2.provider.(*openaiChatProvider)
	pl2, err := p2.buildPayload(req(), false, p2.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	if tools, ok := pl2["tools"].([]map[string]any); !ok || len(tools) != 1 {
		t.Fatalf("replace: tools = %v", pl2["tools"])
	}
}

// The whitelist applies uniformly across the three chat protocols: a key
// reserved on a given protocol is only unguarded when listed.
func TestExtraOverrideKeysThreeProtocols(t *testing.T) {
	req := func() *ChatRequest {
		return &ChatRequest{Model: "m", Messages: []Message{User("hi")}, Extra: map[string]any{"stream": true}}
	}

	c1 := newTestClient(t, WithProtocol(ProtoOpenAIChat), WithExtraOverrideKeys("messages"))
	p1 := c1.provider.(*openaiChatProvider)
	if _, err := p1.buildPayload(req(), false, p1.initialState("m")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("chat: stream must be rejected, got %v", err)
	}

	c2 := newTestClient(t, WithProtocol(ProtoOpenAIResponses), WithExtraOverrideKeys("messages"))
	p2 := c2.provider.(*openaiResponsesProvider)
	if _, err := p2.buildPayload(req(), false, p2.initialState("m")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("responses: stream must be rejected, got %v", err)
	}

	c3 := newTestClient(t, WithProtocol(ProtoAnthropic), WithExtraOverrideKeys("messages"))
	p3 := c3.provider.(*anthropicProvider)
	if _, _, err := p3.buildPayload(req(), false, p3.plan(req())); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("anthropic: stream must be rejected, got %v", err)
	}
}
