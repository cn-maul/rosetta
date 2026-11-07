package rosetta

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
)

// The built-in parser reads the OpenRouter field shapes: nested top_provider
// context window, and the reasoning object form with supported_efforts.
func TestParseModelCapabilitiesOpenRouter(t *testing.T) {
	raw := json.RawMessage(`{
		"id": "openrouter/foo",
		"top_provider": {"context_length": 200000, "max_completion_tokens": 8000},
		"reasoning": {"mandatory": false, "supported_efforts": ["high","medium","low"], "default_effort": "medium"}
	}`)
	cap := parseModelCapabilities(raw)
	if cap.ContextWindow != 200000 {
		t.Fatalf("ContextWindow = %d, want 200000", cap.ContextWindow)
	}
	if cap.MaxOutputTokens != 8000 {
		t.Fatalf("MaxOutputTokens = %d, want 8000", cap.MaxOutputTokens)
	}
	if !reflect.DeepEqual(cap.EffortLevels, []string{"high", "medium", "low"}) {
		t.Fatalf("EffortLevels = %v", cap.EffortLevels)
	}
	if !cap.SupportsThinking || !cap.ThinkingReported {
		t.Fatalf("supports/thinking-reported = %v/%v, want true/true", cap.SupportsThinking, cap.ThinkingReported)
	}
}

// Three-state: "explicitly false" and "absent" must be distinguishable via
// ThinkingReported, otherwise every model whose vendor omits the field would
// have its reasoning stripped.
func TestModelCapabilitiesThreeState(t *testing.T) {
	cases := []struct {
		name             string
		raw              string
		wantSupported    bool
		wantReported     bool
		wantEffortLevels []string
	}{
		{"absent", `{"id":"a"}`, false, false, nil},
		{"explicit false", `{"id":"a","supports_thinking":false}`, false, true, nil},
		{"explicit true", `{"id":"a","supports_thinking":true}`, true, true, nil},
		{"reasoning bare bool", `{"id":"a","reasoning":true}`, true, true, nil},
		{"reasoning obj mandatory false", `{"id":"a","reasoning":{"mandatory":false}}`, false, true, nil},
		{"effort levels only", `{"id":"a","supported_efforts":["low","high"]}`, true, true, []string{"low", "high"}},
	}
	for _, tc := range cases {
		cap := parseModelCapabilities(json.RawMessage(tc.raw))
		if cap.SupportsThinking != tc.wantSupported || cap.ThinkingReported != tc.wantReported {
			t.Errorf("%s: supports/reported = %v/%v, want %v/%v",
				tc.name, cap.SupportsThinking, cap.ThinkingReported, tc.wantSupported, tc.wantReported)
		}
		if !reflect.DeepEqual(cap.EffortLevels, tc.wantEffortLevels) {
			t.Errorf("%s: EffortLevels = %v, want %v", tc.name, cap.EffortLevels, tc.wantEffortLevels)
		}
	}
}

// flexReasoning is lenient: an unrecognized shape reads as "unknown" rather
// than failing the whole catalog, so a vendor using the field unexpectedly
// cannot break ListModels for every other model.
func TestFlexReasoningLenientShapes(t *testing.T) {
	for _, raw := range []string{
		`"reasoning": "yes"`,
		`"reasoning": 1`,
		`"reasoning": []`,
		`"reasoning": null`,
	} {
		cap := parseModelCapabilities(json.RawMessage(`{"id":"a",` + raw + `}`))
		if cap.ThinkingReported {
			t.Errorf("%s: unexpected report on unknown shape", raw)
		}
	}
}

// Domain-root endpoint: /v1/models returns 404, so ListModels must fall back
// to /models. A 500 must not fall back (that's an upstream fault).
func TestListModelsPathFallback(t *testing.T) {
	// 404 on /v1/models → fall back to /models.
	var hitV1, hitPlain bool
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			hitV1 = true
			w.WriteHeader(404)
		case "/models":
			hitPlain = true
			w.Header().Set("Content-Type", "application/json")
			io.WriteString(w, `{"data":[{"id":"m"}]}`)
		default:
			w.WriteHeader(404)
		}
	}))
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat), WithEndpoint(srv.URL))
	models, err := c.ListModels(context.Background())
	if err != nil {
		t.Fatalf("fallback ListModels: %v", err)
	}
	if !hitV1 || !hitPlain || len(models) != 1 || models[0].ID != "m" {
		t.Fatalf("hitV1=%v hitPlain=%v models=%+v", hitV1, hitPlain, models)
	}
	srv.Close()

	// 500 on /v1/models must NOT fall back to /models.
	hitPlain = false
	srv2 := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch r.URL.Path {
		case "/v1/models":
			w.WriteHeader(500)
		case "/models":
			hitPlain = true
			w.WriteHeader(200)
		default:
			w.WriteHeader(404)
		}
	}))
	defer srv2.Close()
	c2 := newTestClient(t, WithProtocol(ProtoOpenAIChat), WithEndpoint(srv2.URL))
	if _, err := c2.ListModels(context.Background()); err == nil {
		t.Fatal("500 must surface as an error, not fall back")
	}
	if hitPlain {
		t.Fatal("must not fall back to /models on a 5xx")
	}
}
