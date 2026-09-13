package rosetta

// Tests for the remaining audit items: APIError.Raw sensitive-content
// redaction and the Responses adapter's compatibility downgrade retry.

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestSafeTruncateBodyRedactsSensitiveValues(t *testing.T) {
	raw := safeTruncateBody([]byte(`{
		"error": {
			"message": "Incorrect API key provided: sk-live-abcdef1234567890",
			"api_key": "sk-live-abcdef1234567890",
			"Authorization": "Bearer sk-live-abcdef1234567890",
			"nested": {"access_token": "tok_123", "model": "gpt-4o"}
		}
	}`))
	if !json.Valid(raw) {
		t.Fatalf("Raw must stay valid JSON: %s", raw)
	}
	s := string(raw)
	if strings.Contains(s, "sk-live-abcdef1234567890") {
		t.Fatalf("key material leaked into Raw: %s", s)
	}
	if strings.Contains(s, "tok_123") {
		t.Fatalf("token value leaked into Raw: %s", s)
	}
	// Non-sensitive content survives redaction for diagnostics.
	if !strings.Contains(s, "gpt-4o") || !strings.Contains(s, "[redacted]") {
		t.Fatalf("redaction must keep non-sensitive fields: %s", s)
	}
}

// Non-JSON error bodies degrade to a JSON string exactly as before.
func TestSafeTruncateBodyNonJSON(t *testing.T) {
	raw := safeTruncateBody([]byte("<html>gateway error sk-live-abcdef1234567890</html>"))
	var s string
	if err := json.Unmarshal(raw, &s); err != nil {
		t.Fatalf("non-JSON body must degrade to a JSON string: %s", raw)
	}
	if strings.Contains(s, "sk-live-abcdef1234567890") {
		t.Fatalf("key material leaked into Raw string: %s", s)
	}
}

func TestResponsesSanitize(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	model := "gpt-5"
	errWith := func(msg, errType string) *APIError {
		return &APIError{Message: msg, Type: errType}
	}

	// reasoning rejected → dropped and sticky (per model).
	st := p.initialState(model)
	if !p.sanitize(st, errWith("Unknown parameter: 'reasoning'", "invalid_request_error"), model) {
		t.Fatal("reasoning rejection must trigger a retry")
	}
	if st.reasoning {
		t.Fatal("reasoning must be dropped")
	}

	// max_output_tokens rejected → cap dropped and sticky.
	st = p.initialState(model)
	if !p.sanitize(st, errWith("max_output_tokens is not supported", "invalid_request_error"), model) {
		t.Fatal("max_output_tokens rejection must trigger a retry")
	}
	if st.maxOutput {
		t.Fatal("max_output_tokens must be dropped")
	}
	if !p.stickyNoMaxOutput[model] {
		t.Fatal("sticky no-max-output flag not set")
	}

	// A fresh request for the same model starts with the sticky downgrades
	// applied — but a different model is untouched.
	st = p.initialState(model)
	if st.reasoning || st.maxOutput {
		t.Fatalf("sticky state not applied: %+v", st)
	}
	other := p.initialState("other-model")
	if !other.reasoning || !other.maxOutput {
		t.Fatalf("sticky state must be per model, got: %+v", other)
	}

	// A reasoning *value* rejection is surfaced, never downgraded. A fresh
	// model keeps the field: sticky state must not leak across models.
	valueModel := "gpt-5-pro"
	st = p.initialState(valueModel)
	if p.sanitize(st, errWith("Unsupported value: 'reasoning.effort' does not support 'low' with this model.", "invalid_request_error"), valueModel) {
		t.Fatal("value rejections must not trigger a downgrade")
	}
	if !st.reasoning {
		t.Fatal("value rejections must not drop the reasoning field")
	}

	// Server errors and unrelated 400s never trigger downgrades.
	st = p.initialState(model)
	if p.sanitize(st, errWith("internal error", "server_error"), model) {
		t.Fatal("non-invalid_request errors must not sanitize")
	}
	st = p.initialState(model)
	if p.sanitize(st, errWith("model 'gpt-9' not found", ""), model) {
		t.Fatal("unrelated 400s must not sanitize")
	}

	// buildPayload honors the state.
	p2 := c.provider.(*openaiResponsesProvider)
	pl, err := p2.buildPayload(&ChatRequest{
		Model:           model,
		Messages:        []Message{User("hi")},
		Thinking:        &ThinkingConfig{Effort: EffortLow},
		MaxOutputTokens: 100,
	}, false, &respSendState{reasoning: false, maxOutput: false})
	if err != nil {
		t.Fatal(err)
	}
	if _, has := pl["reasoning"]; has {
		t.Fatal("reasoning must be off the wire when dropped")
	}
	if _, has := pl["max_output_tokens"]; has {
		t.Fatal("max_output_tokens must be off the wire when dropped")
	}
}
