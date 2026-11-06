package rosetta

import (
	"errors"
	"testing"
)

// RG-02: tool selection is translated per protocol instead of passed
// through. "required" is OpenAI's spelling; Anthropic calls it "any" and
// rejects the OpenAI word with a 400.
func TestToolChoiceWireMapping(t *testing.T) {
	strict := true
	noParallel := true
	req := &ChatRequest{
		Model:    "m",
		Messages: []Message{User("hi")},
		Tools: []ToolDefinition{{
			Name:       "lookup",
			Parameters: []byte(`{"type":"object","properties":{}}`),
			Strict:     &strict,
		}},
		ToolChoice:        &ToolChoice{Mode: ToolChoiceRequired, DisableParallel: &noParallel},
		ParallelToolCalls: Bool(true), // DisableParallel must win
	}

	// OpenAI Chat: nested function object, explicit parallel_tool_calls.
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat))
	p := c.provider.(*openaiChatProvider)
	pl, err := p.buildPayload(req, false, p.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	if pl["tool_choice"] != "required" {
		t.Fatalf("chat tool_choice = %v", pl["tool_choice"])
	}
	if pl["parallel_tool_calls"] != false {
		t.Fatalf("chat parallel_tool_calls = %v, want false (DisableParallel wins)", pl["parallel_tool_calls"])
	}
	fn := pl["tools"].([]map[string]any)[0]["function"].(map[string]any)
	if fn["strict"] != true {
		t.Fatalf("chat strict = %v", fn["strict"])
	}

	// Responses: flat function tool and flat tool_choice.
	rc := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	rp := rc.provider.(*openaiResponsesProvider)
	rpl, err := rp.buildPayload(req, false, rp.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	tool := rpl["tools"].([]map[string]any)[0]
	if tool["strict"] != true {
		t.Fatalf("responses strict = %v", tool["strict"])
	}
	forced := &ChatRequest{
		Model: "m", Messages: []Message{User("hi")},
		Tools:      []ToolDefinition{{Name: "lookup"}},
		ToolChoice: &ToolChoice{Mode: ToolChoiceTool, Name: "lookup"},
	}
	rpl, err = rp.buildPayload(forced, false, rp.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	tc := rpl["tool_choice"].(map[string]any)
	if tc["type"] != "function" || tc["name"] != "lookup" {
		t.Fatalf("responses forced tool_choice = %v", tc)
	}

	// Anthropic: "required" becomes "any"; parallel rides inside the object.
	ac := newTestClient(t, WithProtocol(ProtoAnthropic))
	ap := ac.provider.(*anthropicProvider)
	apl, _, err := ap.buildPayload(req, false, ap.plan(req))
	if err != nil {
		t.Fatal(err)
	}
	atc := apl["tool_choice"].(map[string]any)
	if atc["type"] != "any" {
		t.Fatalf("anthropic tool_choice = %v, want type any", atc)
	}
	if atc["disable_parallel_tool_use"] != true {
		t.Fatalf("anthropic disable_parallel = %v", atc["disable_parallel_tool_use"])
	}
}

// Anthropic refuses extended thinking combined with a forced tool choice.
// The SDK must say so locally rather than forward a request the API 400s.
func TestAnthropicThinkingWithForcedToolChoiceRejected(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)
	req := &ChatRequest{
		Model:      "claude-sonnet-4-5",
		Messages:   []Message{User("hi")},
		Tools:      []ToolDefinition{{Name: "lookup"}},
		Thinking:   &ThinkingConfig{Effort: EffortLow},
		ToolChoice: &ToolChoice{Mode: ToolChoiceRequired},
	}
	_, _, err := p.buildPayload(req, false, p.plan(req))
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("err = %v, want ErrInvalidRequest", err)
	}
}

func TestToolChoiceValidation(t *testing.T) {
	base := func(tc *ToolChoice) *ChatRequest {
		return &ChatRequest{Model: "m", Messages: []Message{User("hi")}, ToolChoice: tc}
	}
	cases := []struct {
		name string
		req  *ChatRequest
		bad  bool
	}{
		{"required without tools", base(&ToolChoice{Mode: ToolChoiceRequired}), true},
		{"tool without name", &ChatRequest{
			Model: "m", Messages: []Message{User("hi")},
			Tools: []ToolDefinition{{Name: "a"}}, ToolChoice: &ToolChoice{Mode: ToolChoiceTool},
		}, true},
		{"tool name not offered", &ChatRequest{
			Model: "m", Messages: []Message{User("hi")},
			Tools: []ToolDefinition{{Name: "a"}}, ToolChoice: &ToolChoice{Mode: ToolChoiceTool, Name: "b"},
		}, true},
		{"name on auto", base(&ToolChoice{Mode: ToolChoiceAuto, Name: "a"}), true},
		{"unknown mode", base(&ToolChoice{Mode: "sometimes"}), true},
		{"valid forced tool", &ChatRequest{
			Model: "m", Messages: []Message{User("hi")},
			Tools: []ToolDefinition{{Name: "a"}}, ToolChoice: &ToolChoice{Mode: ToolChoiceTool, Name: "a"},
		}, false},
		{"auto without tools", base(&ToolChoice{Mode: ToolChoiceAuto}), false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.req.validate()
			if tc.bad && !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if !tc.bad && err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
		})
	}
}
