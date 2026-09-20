package rosetta

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"math"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// B10: the exported MemoryUsageTracker must be usable at its zero value —
// Record lazily creates its maps instead of panicking on a nil map.
func TestAuditMemoryUsageTrackerZeroValueUsable(t *testing.T) {
	var tr MemoryUsageTracker
	tr.Record(context.Background(), UsageRecord{
		Model: "m", Protocol: ProtoOpenAIChat,
		Usage: Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
	})
	snap := tr.Snapshot()
	if snap.TotalTokens != 3 || snap.ByModel["m"].Requests != 1 {
		t.Fatalf("zero-value tracker lost the record: %+v", snap)
	}
}

// C21: a hostile total_tokens near MaxInt64 must saturate, not make the
// running aggregate wrap negative on `+=`.
func TestAuditUsageTrackerSaturates(t *testing.T) {
	tr := NewMemoryUsageTracker()
	rec := UsageRecord{Model: "m", Usage: Usage{TotalTokens: math.MaxInt64}}
	tr.Record(context.Background(), rec)
	tr.Record(context.Background(), rec)
	if got := tr.Snapshot().TotalTokens; got < 0 || got != 2*maxAggregateTokens {
		t.Fatalf("aggregate = %d, want saturated %d (non-negative)", got, 2*maxAggregateTokens)
	}
}

// B13 / B14 / C4 / C19 / C2 / C3: validate must reject, locally with
// ErrInvalidRequest, the request shapes that previously slipped through to a
// json.Marshal transport failure, an int64-wrapped context gate, an upstream
// 400, or a silently coerced tool/image payload.
func TestAuditChatRequestValidationHardening(t *testing.T) {
	cases := []struct {
		name   string
		mutate func(*ChatRequest)
	}{
		{"nan temperature", func(r *ChatRequest) { r.Temperature = Float(math.NaN()) }},
		{"inf top_p", func(r *ChatRequest) { r.TopP = Float(math.Inf(1)) }},
		{"huge max-output", func(r *ChatRequest) { r.MaxOutputTokens = maxWireInt + 1 }},
		{"huge budget", func(r *ChatRequest) { r.Thinking = &ThinkingConfig{BudgetTokens: maxWireInt + 1} }},
		{"empty tool name", func(r *ChatRequest) { r.Tools = []ToolDefinition{{Name: ""}} }},
		{"duplicate tool name", func(r *ChatRequest) { r.Tools = []ToolDefinition{{Name: "x"}, {Name: "x"}} }},
		{"invalid tool params", func(r *ChatRequest) { r.Tools = []ToolDefinition{{Name: "x", Parameters: json.RawMessage("{oops")}} }},
		{"empty stop sequence", func(r *ChatRequest) { r.StopSequences = []string{""} }},
		{"non-serializable extra", func(r *ChatRequest) { r.Extra = map[string]any{"c": make(chan int)} }},
		{"bad cache type", func(r *ChatRequest) {
			r.Messages = []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockText, Text: "x", CacheControl: &CacheControl{Type: "glacial"}}}}}
		}},
		{"image file scheme", func(r *ChatRequest) {
			r.Messages = []Message{{Role: RoleUser, Blocks: []Block{{Type: BlockImage, ImageURL: "file:///etc/passwd"}}}}
		}},
		{"tool args not object", func(r *ChatRequest) {
			r.Messages = []Message{AssistantBlocks(ToolCall("t", "f", "[1,2]"))}
		}},
	}
	for _, tc := range cases {
		r := &ChatRequest{Model: "m", Messages: []Message{User("hi")}}
		tc.mutate(r)
		if err := r.validate(); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: err = %v, want ErrInvalidRequest", tc.name, err)
		}
	}
}

// B14: the strict context gate must reject an output cap that once wrapped
// the `in+out <= window` arithmetic into a false "fits".
func TestAuditStrictContextGateNoWrap(t *testing.T) {
	c, err := NewClient(WithEndpoint("http://api.test/v1"), WithAPIKey("k"),
		WithStrictContextCheck(true),
		WithModelInfo(ModelInfo{ID: "tiny", ContextWindow: 100, Known: true}))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Chat(context.Background(), &ChatRequest{
		Model: "tiny", Messages: []Message{User("hi")}, MaxOutputTokens: maxWireInt,
	})
	if !errors.Is(err, ErrContextTooLong) {
		t.Fatalf("gate must reject oversized output, got %v", err)
	}
}

// C5: WithMaxTokensField must reject a typo at NewClient rather than silently
// dropping the output cap.
func TestAuditMaxTokensFieldWhitelist(t *testing.T) {
	if _, err := NewClient(WithEndpoint("http://api.test/v1"), WithAPIKey("k"),
		WithMaxTokensField("max_token")); err == nil {
		t.Fatal("a mis-named max-tokens field must fail NewClient")
	}
	for _, ok := range []string{"", "max_tokens", "max_completion_tokens"} {
		if _, err := NewClient(WithEndpoint("http://api.test/v1"), WithAPIKey("k"),
			WithMaxTokensField(ok)); err != nil {
			t.Fatalf("valid field %q rejected: %v", ok, err)
		}
	}
}

// C7: reserved-key sets are per protocol — a key no adapter emits
// ("tool_choice") is settable via Extra everywhere, while each protocol still
// guards exactly its own SDK-managed fields.
func TestAuditPerProtocolReservedKeys(t *testing.T) {
	sets := map[string]map[string]bool{
		"chat":      openaiChatReservedPayloadKeys,
		"responses": openaiResponsesReservedPayloadKeys,
		"anthropic": anthropicReservedPayloadKeys,
	}
	for name, set := range sets {
		if set["tool_choice"] {
			t.Errorf("%s must not reserve tool_choice (no adapter emits it)", name)
		}
	}
	if !openaiResponsesReservedPayloadKeys["input"] {
		t.Error("responses must reserve input")
	}
	if openaiChatReservedPayloadKeys["input"] || anthropicReservedPayloadKeys["input"] {
		t.Error("input must not be reserved on chat/anthropic payloads")
	}
	if !anthropicReservedPayloadKeys["stop_sequences"] || anthropicReservedPayloadKeys["stop"] {
		t.Error("anthropic must reserve stop_sequences, not stop")
	}
	if !openaiChatReservedPayloadKeys["stop"] || openaiChatReservedPayloadKeys["stop_sequences"] {
		t.Error("chat must reserve stop, not stop_sequences")
	}
	// End-to-end through mergeExtra.
	payload := map[string]any{"model": "m"}
	if err := mergeExtra(payload, map[string]any{"tool_choice": "any"}, false, anthropicReservedPayloadKeys); err != nil {
		t.Fatalf("anthropic Extra[tool_choice] rejected: %v", err)
	}
	if err := mergeExtra(payload, map[string]any{"model": "x"}, false, anthropicReservedPayloadKeys); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("anthropic Extra[model] must still be rejected, got %v", err)
	}
}

// C6: models.json must reject unknown keys and empty ids instead of decoding
// them to silent zero values.
func TestAuditParseModelsFileStrict(t *testing.T) {
	dir := t.TempDir()
	write := func(name, body string) string {
		p := filepath.Join(dir, name)
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
		return p
	}
	if _, err := parseModelsFile(write("typo.json", `{"models":[{"id":"a","context_windows":123}]}`)); err == nil {
		t.Fatal("an unknown key (typo) must be rejected")
	}
	if _, err := parseModelsFile(write("empty.json", `{"models":[{"id":""}]}`)); err == nil {
		t.Fatal("an entry with an empty id must be rejected")
	}
	if _, err := parseModelsFile(write("good.json", `{"models":[{"id":"a","context_window":131072,"aliases":["aa"]}]}`)); err != nil {
		t.Fatalf("valid file rejected: %v", err)
	}
}

// B16: a sparse explicit WithModelInfo must merge field-by-field with a
// models-file entry of the same id rather than zeroing its context window,
// output cap and aliases.
func TestAuditManualLayerFieldMerge(t *testing.T) {
	f := filepath.Join(t.TempDir(), "models.json")
	if err := os.WriteFile(f, []byte(`{"models":[{"id":"my-model","context_window":131072,"max_output_tokens":8192,"aliases":["mm"]}]}`), 0o600); err != nil {
		t.Fatal(err)
	}
	c, err := NewClient(WithEndpoint("http://api.test/v1"), WithAPIKey("k"),
		WithModelsFile(f), WithModelInfo(ModelInfo{ID: "my-model", SupportsThinking: true}))
	if err != nil {
		t.Fatal(err)
	}
	mi, ok := c.registry.Lookup("my-model")
	if !ok {
		t.Fatal("model vanished")
	}
	if mi.ContextWindow != 131072 || mi.MaxOutputTokens != 8192 {
		t.Fatalf("sparse override clobbered file fields: %+v", mi)
	}
	if !mi.SupportsThinking {
		t.Fatalf("explicit SupportsThinking lost: %+v", mi)
	}
	if _, ok := c.registry.Lookup("mm"); !ok {
		t.Fatal("file alias lost")
	}
}

// C23: a remote DisableThinking claim must not revoke a manual
// SupportsThinking declaration.
func TestAuditManualThinkingNotRevokedByRemote(t *testing.T) {
	r := newRegistry()
	if err := r.SetManual([]ModelInfo{{ID: "m", SupportsThinking: true}}); err != nil {
		t.Fatal(err)
	}
	if err := r.SetRemote([]ModelInfo{{ID: "m", DisableThinking: true}}); err != nil {
		t.Fatal(err)
	}
	mi, _ := r.Lookup("m")
	if !mi.SupportsThinking {
		t.Fatalf("remote DisableThinking revoked a manual SupportsThinking claim: %+v", mi)
	}
}

// B11: a 200 /models response without a "data" field is a malformed catalog,
// not an empty one, and must not be reported as a successful refresh.
func TestAuditListModelsRequiresData(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"object":"list"}`)
	}))
	defer srv.Close()
	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ListModels(context.Background()); err == nil {
		t.Fatal("a catalog missing its data field must error, not silently succeed")
	}
}

// C20: an Extra override replaces the wire count even when it arrives as a
// shape the SDK never produces ([]any / float64), and an override to an empty
// list is a hard error rather than a skipped range check.
func TestAuditWireCoercion(t *testing.T) {
	if n, ok := wireSliceLen([]any{"a", "b"}); !ok || n != 2 {
		t.Errorf("[]any length = %d/%v, want 2/true", n, ok)
	}
	if n, ok := wireSliceLen([]string{}); !ok || n != 0 {
		t.Errorf("empty slice length = %d/%v, want 0/true", n, ok)
	}
	if _, ok := wireSliceLen("a string"); ok {
		t.Error("a scalar string is not a slice")
	}
	if v, ok := wireInt(float64(5)); !ok || v != 5 {
		t.Errorf("float coercion = %d/%v, want 5/true", v, ok)
	}
	if _, ok := wireInt(nil); ok {
		t.Error("nil must not coerce")
	}
}

func TestAuditEmbeddingEmptyOverrideRejected(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"data":[{"index":0,"embedding":[0.1]}]}`)
	}))
	defer srv.Close()
	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithExtraOverrides(true))
	if err != nil {
		t.Fatal(err)
	}
	_, err = c.Embed(context.Background(), &EmbeddingRequest{
		Model: "m", Input: []string{"a"},
		Extra: map[string]any{"input": []string{}},
	})
	if !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("an override to empty input must be a hard error, got %v", err)
	}
}

// B15: the estimator must count a thinking block's signature and the opaque
// payload of a redacted_thinking block, which are replayed verbatim every
// turn — "never undercount" applies to them too.
func TestAuditEstimatorCountsSignatureAndRedacted(t *testing.T) {
	base := &ChatRequest{Model: "m", Messages: []Message{AssistantBlocks(Thinking("short", ""))}}
	withSig := &ChatRequest{Model: "m", Messages: []Message{AssistantBlocks(Thinking("short", strings.Repeat("A", 4000)))}}
	redacted := &ChatRequest{Model: "m", Messages: []Message{AssistantBlocks(Block{Type: BlockRedactedThinking, Thinking: strings.Repeat("B", 4000)})}}

	est := MultimediaTokenEstimates{}
	b := base.estimateInputTokens(est)
	if s := withSig.estimateInputTokens(est); s <= b {
		t.Errorf("signature bytes must raise the estimate: sig=%d base=%d", s, b)
	}
	if r := redacted.estimateInputTokens(est); r <= b {
		t.Errorf("redacted_thinking payload must raise the estimate: red=%d base=%d", r, b)
	}
}
