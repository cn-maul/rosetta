package rosetta

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
)

func TestThinkingGate(t *testing.T) {
	// Builtin gpt-4o is Known and cannot think: strict refusal by default.
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request must not be dispatched when thinking is unsupported")
	}))
	_, err := c.Chat(context.Background(), &ChatRequest{
		Model: "gpt-4o", Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{Effort: EffortHigh},
	})
	if !errors.Is(err, ErrThinkingUnsupported) {
		t.Fatalf("want ErrThinkingUnsupported, got %v", err)
	}

	// Fallback mode degrades silently: no reasoning_effort on the wire.
	var gotPayload map[string]any
	c2 := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		gotPayload = readPayload(t, r)
		fmt.Fprint(w, `{"id":"1","model":"gpt-4o","choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}), WithThinkingFallback(true))
	if _, err := c2.Chat(context.Background(), &ChatRequest{
		Model: "gpt-4o", Messages: []Message{User("hi")},
		Thinking: &ThinkingConfig{Effort: EffortHigh},
	}); err != nil {
		t.Fatalf("Chat with fallback: %v", err)
	}
	if _, has := gotPayload["reasoning_effort"]; has {
		t.Errorf("thinking config must be dropped for non-thinking models")
	}

	// Thinking-capable builtin and unknown models pass through untouched.
	c3 := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		payload := readPayload(t, r)
		if payload["reasoning_effort"] != "high" {
			t.Errorf("reasoning_effort = %v", payload["reasoning_effort"])
		}
		fmt.Fprint(w, `{"id":"1","model":"m","choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	for _, model := range []string{"deepseek-reasoner", "my-custom-model"} {
		if _, err := c3.Chat(context.Background(), &ChatRequest{
			Model: model, Messages: []Message{User("hi")},
			Thinking: &ThinkingConfig{Effort: EffortHigh},
		}); err != nil {
			t.Fatalf("Chat %s: %v", model, err)
		}
	}
}

func TestContextWindowCheck(t *testing.T) {
	longChinese := make([]byte, 0, 300)
	for len(longChinese) < 300 {
		longChinese = append(longChinese, "上下文测试"...)
	}
	req := &ChatRequest{
		Model:    "tiny-model",
		Messages: []Message{User(string(longChinese))},
	}

	// Strict mode: heuristic estimate (>= 120 CJK tokens) must exceed 50.
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Errorf("request must not be dispatched in strict mode")
	}), WithModelInfo(ModelInfo{ID: "tiny-model", ContextWindow: 50}), WithStrictContextCheck(true))
	if _, err := c.Chat(context.Background(), req); !errors.Is(err, ErrContextTooLong) {
		t.Fatalf("want ErrContextTooLong, got %v", err)
	}

	// Default warn-only mode: request proceeds.
	c2 := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"id":"1","model":"tiny-model","choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}), WithModelInfo(ModelInfo{ID: "tiny-model", ContextWindow: 50}))
	if _, err := c2.Chat(context.Background(), req); err != nil {
		t.Fatalf("warn-only mode must proceed: %v", err)
	}

	// MaxOutputTokens participates in the check (input 10 + output 100 > 50).
	reqOut := &ChatRequest{Model: "tiny-model", Messages: []Message{User("hi")}, MaxOutputTokens: 100}
	if _, err := c.Chat(context.Background(), reqOut); !errors.Is(err, ErrContextTooLong) {
		t.Errorf("want ErrContextTooLong for input+output, got %v", err)
	}
}

func TestManualModelsAndAliases(t *testing.T) {
	// Manual layer overrides builtin fields, fills on top of others.
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}),
		WithModelInfo(
			ModelInfo{ID: "gpt-5", ContextWindow: 999}, // sparse overlay on a thinking builtin
			ModelInfo{ID: "custom-prod", ContextWindow: 8192, SupportsThinking: true, Aliases: []string{"prod"}},
		))

	// Alias resolution for builtin entries; sparse manual overlay keeps
	// the builtin thinking flag (OR semantics) while the window is overridden.
	mi, err := c.ModelInfo(context.Background(), "gpt-5-2025-08-07")
	if err != nil {
		t.Fatalf("alias lookup: %v", err)
	}
	if mi.ID != "gpt-5" || mi.ContextWindow != 999 {
		t.Errorf("merged builtin = %+v (alias must resolve, manual window must win)", mi)
	}
	if !mi.SupportsThinking {
		t.Errorf("builtin thinking flag must survive a sparse manual overlay")
	}

	mi, err = c.ModelInfo(context.Background(), "prod")
	if err != nil || mi.ID != "custom-prod" || !mi.SupportsThinking || mi.ContextWindow != 8192 {
		t.Errorf("manual model via alias = %+v, err %v", mi, err)
	}

	if _, err := c.ModelInfo(context.Background(), "nope"); !errors.Is(err, ErrUnknownModel) {
		t.Errorf("want ErrUnknownModel, got %v", err)
	}
}

func TestModelsFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "models.json")
	if err := os.WriteFile(path, []byte(`{"models":[{"id":"file-model","context_window":777,"max_output_tokens":128,"supports_thinking":false}]}`), 0o644); err != nil {
		t.Fatal(err)
	}
	c := newTestClient(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {}), WithModelsFile(path))
	mi, err := c.ModelInfo(context.Background(), "file-model")
	if err != nil || !mi.Known || mi.ContextWindow != 777 {
		t.Errorf("file model = %+v, err %v", mi, err)
	}
	if _, err := NewClient(WithAPIKey("k"), WithEndpoint("http://x"), WithModelsFile(filepath.Join(dir, "missing.json"))); err == nil {
		t.Errorf("missing models file must fail construction")
	}
}

func TestVendorPresets(t *testing.T) {
	c, err := NewClient(WithVendor("deepseek"), WithAPIKey("k"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Endpoint() != "https://api.deepseek.com/v1" || c.Protocol() != ProtoOpenAIChat {
		t.Errorf("deepseek preset: endpoint=%q protocol=%q", c.Endpoint(), c.Protocol())
	}

	c, err = NewClient(WithVendor("anthropic"), WithAPIKey("k"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Protocol() != ProtoAnthropic {
		t.Errorf("anthropic preset protocol = %q", c.Protocol())
	}

	// Explicit options win over the preset.
	c, err = NewClient(WithVendor("deepseek"), WithEndpoint("http://localhost:9999"), WithAPIKey("k"))
	if err != nil {
		t.Fatalf("NewClient: %v", err)
	}
	if c.Endpoint() != "http://localhost:9999" {
		t.Errorf("explicit endpoint overridden: %q", c.Endpoint())
	}

	if _, err := NewClient(WithVendor("nope"), WithAPIKey("k")); err == nil {
		t.Errorf("unknown vendor must fail")
	}
}

func TestDetectProtocol(t *testing.T) {
	// Known-host table: no network involved.
	if p, err := DetectProtocol(context.Background(), "https://api.anthropic.com/v1", "k"); err != nil || p != ProtoAnthropic {
		t.Errorf("anthropic host detect = %q, err %v", p, err)
	}
	if p, err := DetectProtocol(context.Background(), "https://api.deepseek.com", "k"); err != nil || p != ProtoOpenAIChat {
		t.Errorf("deepseek host detect = %q, err %v", p, err)
	}

	// Active probe: OpenAI-style catalog.
	openaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/v1/models" {
			t.Errorf("probe path = %q", r.URL.Path)
		}
		if r.Header.Get("Authorization") != "Bearer probe-key" {
			t.Errorf("probe must try Bearer first, got %q", r.Header.Get("Authorization"))
		}
		fmt.Fprint(w, `{"data":[{"id":"m","object":"model"}]}`)
	}))
	defer openaiSrv.Close()
	if p, err := DetectProtocol(context.Background(), openaiSrv.URL, "probe-key"); err != nil || p != ProtoOpenAIChat {
		t.Errorf("openai probe = %q, err %v", p, err)
	}

	// Active probe: Anthropic-style catalog (type:"model").
	anthroSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		fmt.Fprint(w, `{"data":[{"id":"claude-x","type":"model","display_name":"X"}]}`)
	}))
	defer anthroSrv.Close()
	if p, err := DetectProtocol(context.Background(), anthroSrv.URL, "probe-key"); err != nil || p != ProtoAnthropic {
		t.Errorf("anthropic probe = %q, err %v", p, err)
	}

	// Dead endpoint: falls back to OpenAI Chat, the compatibility default.
	if p, err := DetectProtocol(context.Background(), "http://127.0.0.1:1", ""); err != nil || p != ProtoOpenAIChat {
		t.Errorf("fallback detect = %q, err %v", p, err)
	}
}

func TestDetectClient(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.URL.Path == "/v1/models":
			fmt.Fprint(w, `{"data":[{"id":"m","object":"model"}]}`)
		case r.URL.Path == "/v1/chat/completions":
			fmt.Fprint(w, `{"id":"1","model":"m","choices":[{"index":0,"message":{"content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
		default:
			t.Errorf("unexpected path %q", r.URL.Path)
		}
	}))
	defer srv.Close()

	c, err := DetectClient(context.Background(), WithEndpoint(srv.URL), WithAPIKey("k"))
	if err != nil {
		t.Fatalf("DetectClient: %v", err)
	}
	if c.Protocol() != ProtoOpenAIChat {
		t.Errorf("protocol = %q", c.Protocol())
	}
	resp, err := c.Chat(context.Background(), &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err != nil || resp.Text() != "ok" {
		t.Errorf("chat after detect: resp=%v err=%v", resp, err)
	}
}

func TestEstimateTokens(t *testing.T) {
	if got := EstimateTokens("hello world!"); got != 3 { // 12 ASCII chars / 4
		t.Errorf("ascii estimate = %d, want 3", got)
	}
	if got := EstimateTokens("你好世界"); got != 4 { // 1 token per CJK char
		t.Errorf("cjk estimate = %d, want 4", got)
	}
	mixed := EstimateTokens("abc你好") // 3/4 + 2 = 2
	if mixed != 2 {
		t.Errorf("mixed estimate = %d, want 2", mixed)
	}
}
