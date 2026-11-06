package rosetta

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// P1-1a: BlockThinking must be replayed as reasoning_content on the same
// assistant message as content/tool_calls; ordinary turns must stay
// byte-identical (no reasoning_content key), and a thinking-only turn keeps
// the G6 error.
func TestOpenAIChatReplayReasoningContent(t *testing.T) {
	c := newTestClient(t)
	p := c.provider.(*openaiChatProvider)

	// thinking + text on one turn.
	msgs, err := p.encodeMessages(&ChatRequest{Messages: []Message{{
		Role: RoleAssistant,
		Blocks: []Block{
			{Type: BlockThinking, Thinking: "think step 1"},
			{Type: BlockText, Text: "the answer"},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	a := msgs[0]
	if a["reasoning_content"] != "think step 1" {
		t.Fatalf("reasoning_content = %v, want %q", a["reasoning_content"], "think step 1")
	}
	if a["content"] != "the answer" {
		t.Fatalf("content = %v, want %q", a["content"], "the answer")
	}

	// thinking + tool call (no text) → reasoning_content + tool_calls, no content.
	msgs2, err := p.encodeMessages(&ChatRequest{Messages: []Message{{
		Role: RoleAssistant,
		Blocks: []Block{
			{Type: BlockThinking, Thinking: "plan"},
			{Type: BlockToolCall, ToolCallID: "call_1", ToolName: "get_weather", Arguments: `{"city":"x"}`},
		},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	a2 := msgs2[0]
	if a2["reasoning_content"] != "plan" {
		t.Fatalf("reasoning_content = %v, want %q", a2["reasoning_content"], "plan")
	}
	if _, ok := a2["content"]; ok {
		t.Fatal("tool-call turn must not carry an empty content key")
	}
	tcs, ok := a2["tool_calls"].([]map[string]any)
	if !ok || len(tcs) != 1 || tcs[0]["id"] != "call_1" {
		t.Fatalf("tool_calls = %v", a2["tool_calls"])
	}

	// plain assistant turn → no reasoning_content key at all.
	msgs3, err := p.encodeMessages(&ChatRequest{Messages: []Message{{
		Role:   RoleAssistant,
		Blocks: []Block{{Type: BlockText, Text: "hi"}},
	}}})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := msgs3[0]["reasoning_content"]; ok {
		t.Fatal("ordinary turn must not emit reasoning_content")
	}

	// thinking-only turn → G6 error, not a degenerate message.
	if _, err := p.encodeMessages(&ChatRequest{Messages: []Message{{
		Role:   RoleAssistant,
		Blocks: []Block{{Type: BlockThinking, Thinking: "x"}},
	}}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("thinking-only turn err = %v, want ErrInvalidRequest", err)
	}

	// redacted-only turn → still G6 error (no text to replay).
	if _, err := p.encodeMessages(&ChatRequest{Messages: []Message{{
		Role:   RoleAssistant,
		Blocks: []Block{{Type: BlockRedactedThinking}},
	}}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("redacted-only turn err = %v, want ErrInvalidRequest", err)
	}
}

// P2: the target Anthropic API version is exported and stays the single
// source of truth for the internal header.
func TestAnthropicVersionExported(t *testing.T) {
	if AnthropicVersion != "2026-10-21" {
		t.Fatalf("AnthropicVersion = %q", AnthropicVersion)
	}
	if anthropicVersion != AnthropicVersion {
		t.Fatal("internal anthropicVersion must alias the exported constant")
	}
}

// P2: ListModels fills capabilities only when a parser is installed; the
// default stays no-guessing. The parser runs for every protocol.
func TestModelCapabilityParser(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/models" && r.URL.Path != "/v1/models" {
			w.WriteHeader(404)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"data":[{"id":"deepseek-chat","context_length":65536,"max_output_tokens":8192}]}`)
	}))
	defer srv.Close()

	// Default: no guessing, capability fields stay zero.
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat), WithEndpoint(srv.URL))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].ID != "deepseek-chat" || models[0].ContextWindow != 0 || models[0].MaxOutputTokens != 0 {
		t.Fatalf("default models = %+v", models)
	}

	parse := func(_ string, raw json.RawMessage) ModelInfo {
		var m struct {
			Ctx int `json:"context_length"`
			Max int `json:"max_output_tokens"`
		}
		_ = json.Unmarshal(raw, &m)
		return ModelInfo{ContextWindow: m.Ctx, MaxOutputTokens: m.Max}
	}

	// With parser on the OpenAI-compat protocol.
	c2 := newTestClient(t, WithProtocol(ProtoOpenAIChat), WithEndpoint(srv.URL), WithModelCapabilityParser(parse))
	models2, err := c2.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	m := models2[0]
	if m.ContextWindow != 65536 || m.MaxOutputTokens != 8192 {
		t.Fatalf("parsed models = %+v", m)
	}
	if m.ID != "deepseek-chat" {
		t.Fatalf("ID must stay SDK-owned, got %q", m.ID)
	}
	if m.Known {
		t.Fatal("remote entries must not become Known via the parser")
	}

	// With parser on the Anthropic protocol (generic hook + pagination).
	c3 := newTestClient(t, WithProtocol(ProtoAnthropic), WithEndpoint(srv.URL), WithModelCapabilityParser(parse))
	models3, err := c3.ListModels(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(models3) != 1 || models3[0].ContextWindow != 65536 {
		t.Fatalf("anthropic parsed models = %+v", models3)
	}
}
