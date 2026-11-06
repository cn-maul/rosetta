package rosetta

import (
	"errors"
	"testing"
)

// RG-01: one ResponseFormat maps onto three different wire shapes.
func TestResponseFormatWireMapping(t *testing.T) {
	schema := []byte(`{"type":"object","properties":{"name":{"type":"string"}},"required":["name"]}`)
	strict := true
	req := &ChatRequest{
		Model: "m", Messages: []Message{User("hi")},
		ResponseFormat: &ResponseFormat{Type: "json_schema", Name: "person", Schema: schema, Strict: &strict},
	}

	// Chat: nested under response_format.json_schema.
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat))
	p := c.provider.(*openaiChatProvider)
	pl, err := p.buildPayload(req, false, p.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	rf := pl["response_format"].(map[string]any)
	if rf["type"] != "json_schema" {
		t.Fatalf("chat response_format = %v", rf)
	}
	spec := rf["json_schema"].(map[string]any)
	if spec["name"] != "person" || spec["strict"] != true {
		t.Fatalf("chat json_schema = %v", spec)
	}

	// Responses: flat under text.format.
	rc := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	rp := rc.provider.(*openaiResponsesProvider)
	rpl, err := rp.buildPayload(req, false, rp.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	format := rpl["text"].(map[string]any)["format"].(map[string]any)
	if format["type"] != "json_schema" || format["name"] != "person" || format["strict"] != true {
		t.Fatalf("responses text.format = %v", format)
	}

	// Anthropic: output_config.format, schema only.
	ac := newTestClient(t, WithProtocol(ProtoAnthropic))
	ap := ac.provider.(*anthropicProvider)
	apl, _, err := ap.buildPayload(req, false, ap.plan(req))
	if err != nil {
		t.Fatal(err)
	}
	aformat := apl["output_config"].(map[string]any)["format"].(map[string]any)
	if aformat["type"] != "json_schema" || aformat["schema"] == nil {
		t.Fatalf("anthropic output_config.format = %v", aformat)
	}
}

// Anthropic has no json_object mode; say so instead of sending a field the
// API rejects.
func TestAnthropicJSONObjectFormatUnsupported(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)
	req := &ChatRequest{
		Model: "m", Messages: []Message{User("hi")},
		ResponseFormat: &ResponseFormat{Type: "json_object"},
	}
	_, _, err := p.buildPayload(req, false, p.plan(req))
	if !errors.Is(err, ErrNotSupported) {
		t.Fatalf("err = %v, want ErrNotSupported", err)
	}
}

func TestResponseFormatValidation(t *testing.T) {
	cases := []struct {
		name string
		rf   *ResponseFormat
		bad  bool
	}{
		{"json_schema without name", &ResponseFormat{Type: "json_schema", Schema: []byte(`{}`)}, true},
		{"json_schema without schema", &ResponseFormat{Type: "json_schema", Name: "x"}, true},
		{"json_schema with invalid schema", &ResponseFormat{Type: "json_schema", Name: "x", Schema: []byte(`{`)}, true},
		{"unknown type", &ResponseFormat{Type: "xml"}, true},
		{"json_object", &ResponseFormat{Type: "json_object"}, false},
		{"text", &ResponseFormat{Type: "text"}, false},
		{"valid json_schema", &ResponseFormat{Type: "json_schema", Name: "x", Schema: []byte(`{}`)}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			req := &ChatRequest{Model: "m", Messages: []Message{User("hi")}, ResponseFormat: tc.rf}
			err := req.validate()
			if tc.bad && !errors.Is(err, ErrInvalidRequest) {
				t.Fatalf("err = %v, want ErrInvalidRequest", err)
			}
			if !tc.bad && err != nil {
				t.Fatalf("unexpected err: %v", err)
			}
		})
	}
}
