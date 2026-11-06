package rosetta

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

// RG-10: the sampling/metadata fields reach the right protocol spelling.
func TestSamplingAndMetadataWire(t *testing.T) {
	seed, topK, logprobs, topLogprobs := 7, 40, true, 3

	chat := &ChatRequest{
		Model: "m", Messages: []Message{User("hi")},
		Seed: &seed, Logprobs: &logprobs, TopLogprobs: &topLogprobs,
		User: "u-1", ServiceTier: "flex",
		PresencePenalty: Float(0.5), FrequencyPenalty: Float(0.25),
	}
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat))
	p := c.provider.(*openaiChatProvider)
	pl, err := p.buildPayload(chat, false, p.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	if pl["seed"] != 7 || pl["logprobs"] != true || pl["top_logprobs"] != 3 {
		t.Fatalf("chat sampling = %v", pl)
	}
	if pl["user"] != "u-1" || pl["service_tier"] != "flex" {
		t.Fatalf("chat metadata = %v", pl)
	}
	if pl["presence_penalty"] != 0.5 || pl["frequency_penalty"] != 0.25 {
		t.Fatalf("chat penalties = %v", pl)
	}

	anth := &ChatRequest{
		Model: "m", Messages: []Message{User("hi")},
		TopK: &topK, User: "u-2",
	}
	ac := newTestClient(t, WithProtocol(ProtoAnthropic))
	ap := ac.provider.(*anthropicProvider)
	apl, _, err := ap.buildPayload(anth, false, ap.plan(anth))
	if err != nil {
		t.Fatal(err)
	}
	if _, has := apl["top_k"]; has {
		t.Fatalf("anthropic top_k must be omitted (removed in 2026-10-21), got %v", apl["top_k"])
	}
	if md, ok := apl["metadata"].(map[string]any); !ok || md["user_id"] != "u-2" {
		t.Fatalf("anthropic metadata = %v", apl["metadata"])
	}

	resp := &ChatRequest{
		Model: "m", Messages: []Message{User("hi")},
		Store: Bool(false), PreviousResponseID: "resp_1", Verbosity: "low",
	}
	rc := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	rp := rc.provider.(*openaiResponsesProvider)
	rpl, err := rp.buildPayload(resp, false, rp.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	if rpl["store"] != false || rpl["previous_response_id"] != "resp_1" {
		t.Fatalf("responses state = %v", rpl)
	}
	if text, ok := rpl["text"].(map[string]any); !ok || text["verbosity"] != "low" {
		t.Fatalf("responses text = %v", rpl["text"])
	}
}

// RG-11: image detail, multi-part tool results and document citations.
func TestMultimodalControls(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat))
	p := c.provider.(*openaiChatProvider)
	req := &ChatRequest{Model: "m", Messages: []Message{{
		Role: RoleUser,
		Blocks: []Block{
			{Type: BlockText, Text: "look"},
			{Type: BlockImage, ImageURL: "https://example.com/a.png", ImageDetail: "high"},
		},
	}}}
	pl, err := p.buildPayload(req, false, p.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	msgs := pl["messages"].([]map[string]any)
	parts := msgs[0]["content"].([]map[string]any)
	img := parts[1]["image_url"].(map[string]any)
	if img["detail"] != "high" {
		t.Fatalf("image detail = %v", img)
	}

	ac := newTestClient(t, WithProtocol(ProtoAnthropic))
	ap := ac.provider.(*anthropicProvider)
	areq := &ChatRequest{Model: "m", Messages: []Message{
		User("hi"),
		{Role: RoleTool, Blocks: []Block{{
			Type: BlockToolResult, ToolCallID: "call_1",
			ResultBlocks: []Block{
				{Type: BlockText, Text: "screenshot:"},
				{Type: BlockImage, ImageURL: "data:image/png;base64,AAAA"},
			},
		}}},
	}}
	apl, _, err := ap.buildPayload(areq, false, ap.plan(areq))
	if err != nil {
		t.Fatal(err)
	}
	// Consecutive user-role turns merge, so the tool_result rides the same
	// Anthropic message as the preceding user text.
	amsgs := apl["messages"].([]map[string]any)
	var tr map[string]any
	for _, blk := range amsgs[0]["content"].([]map[string]any) {
		if blk["type"] == "tool_result" {
			tr = blk
		}
	}
	if tr == nil {
		t.Fatalf("tool_result missing: %v", amsgs[0]["content"])
	}
	content, ok := tr["content"].([]map[string]any)
	if !ok || len(content) != 2 || content[0]["type"] != "text" || content[1]["type"] != "image" {
		t.Fatalf("multi-part tool_result = %v", tr["content"])
	}

	enabled := true
	dreq := &ChatRequest{Model: "m", Messages: []Message{{
		Role: RoleUser,
		Blocks: []Block{{
			Type: BlockFile, FileName: "doc.pdf", MimeType: "application/pdf",
			FileData: "AAAA", Citations: &enabled,
		}},
	}}}
	dpl, _, err := ap.buildPayload(dreq, false, ap.plan(dreq))
	if err != nil {
		t.Fatal(err)
	}
	doc := dpl["messages"].([]map[string]any)[0]["content"].([]map[string]any)[0]
	cit, ok := doc["citations"].(map[string]any)
	if !ok || cit["enabled"] != true {
		t.Fatalf("document citations = %v", doc["citations"])
	}
}

// RG-13: provider-specific usage fields are parsed instead of dropped.
func TestUsageExtendedFields(t *testing.T) {
	// DeepSeek prefix cache split.
	chat, err := decodeOpenAIChatResponse([]byte(`{
		"choices":[{"message":{"content":"x"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":100,"completion_tokens":5,"total_tokens":105,
			"prompt_cache_hit_tokens":70,"prompt_cache_miss_tokens":30}}`))
	if err != nil {
		t.Fatal(err)
	}
	if chat.Usage.CachedInputTokens != 70 || chat.Usage.CacheHitTokens != 70 || chat.Usage.CacheMissTokens != 30 {
		t.Fatalf("deepseek cache = %+v", chat.Usage)
	}

	// OpenAI audio/prediction/cache-write details.
	audio, err := decodeOpenAIChatResponse([]byte(`{
		"choices":[{"message":{"content":"x"},"finish_reason":"stop"}],
		"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14,
			"prompt_tokens_details":{"audio_tokens":6,"cache_write_tokens":8},
			"completion_tokens_details":{"audio_tokens":3,"accepted_prediction_tokens":2}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if audio.Usage.AudioInputTokens != 6 || audio.Usage.AudioOutputTokens != 3 || audio.Usage.AcceptedPredictionTokens != 2 {
		t.Fatalf("audio usage = %+v", audio.Usage)
	}
	if audio.Usage.CachedCreationTokens != 8 {
		t.Fatalf("cache_write_tokens must map to CachedCreationTokens, got %+v", audio.Usage)
	}

	// Anthropic 5m/1h cache-creation split.
	anth, err := decodeAnthropicResponse([]byte(`{
		"id":"m","model":"claude","stop_reason":"end_turn",
		"content":[{"type":"text","text":"x"}],
		"usage":{"input_tokens":10,"output_tokens":5,
			"cache_creation_input_tokens":300,
			"cache_creation":{"ephemeral_5m_input_tokens":100,"ephemeral_1h_input_tokens":200}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if anth.Usage.CacheCreation5mTokens != 100 || anth.Usage.CacheCreation1hTokens != 200 {
		t.Fatalf("anthropic cache split = %+v", anth.Usage)
	}
}

// RG-16b: OpenAI prompt_cache_key / prompt_cache_options reach the wire on
// both Chat and Responses, and the Responses cache-write usage is parsed.
func TestPromptCacheWire(t *testing.T) {
	chat := &ChatRequest{
		Model: "m", Messages: []Message{User("hi")},
		PromptCacheKey:     "prefix-key",
		PromptCacheOptions: &PromptCacheOptions{TTL: "30m", Mode: "explicit"},
	}
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat))
	p := c.provider.(*openaiChatProvider)
	pl, err := p.buildPayload(chat, false, p.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	if pl["prompt_cache_key"] != "prefix-key" {
		t.Fatalf("chat prompt_cache_key = %v", pl["prompt_cache_key"])
	}
	opts, ok := pl["prompt_cache_options"].(map[string]any)
	if !ok || opts["ttl"] != "30m" || opts["mode"] != "explicit" {
		t.Fatalf("chat prompt_cache_options = %v", pl["prompt_cache_options"])
	}

	resp := &ChatRequest{
		Model: "m", Messages: []Message{User("hi")}, PromptCacheKey: "rk",
	}
	rc := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	rp := rc.provider.(*openaiResponsesProvider)
	rpl, err := rp.buildPayload(resp, false, rp.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	if rpl["prompt_cache_key"] != "rk" {
		t.Fatalf("responses prompt_cache_key = %v", rpl["prompt_cache_key"])
	}

	// Responses cache_write_tokens -> CachedCreationTokens, plus the TTL split.
	rresp, err := decodeResponsesResponse([]byte(`{
		"id":"r","model":"m","status":"completed",
		"output":[{"type":"message","content":[{"type":"output_text","text":"x"}]}],
		"usage":{"input_tokens":10,"output_tokens":4,"total_tokens":14,
			"input_tokens_details":{"cached_tokens":3,"cache_write_tokens":7,
				"cache_write_tokens_details":{"ttl_30m_tokens":5,"ttl_12h_tokens":2}}}}`))
	if err != nil {
		t.Fatal(err)
	}
	if rresp.Usage.CachedInputTokens != 3 || rresp.Usage.CachedCreationTokens != 7 {
		t.Fatalf("responses cache usage = %+v", rresp.Usage)
	}
	if rresp.Usage.CacheCreation30mTokens != 5 || rresp.Usage.CacheCreation12hTokens != 2 {
		t.Fatalf("responses cache write TTL split = %+v", rresp.Usage)
	}
}

// RG-16c: an explicit OpenAI prompt-cache breakpoint rides on the content
// part for both Chat and Responses.
func TestPromptCacheBreakpoint(t *testing.T) {
	chat := &ChatRequest{Model: "m", Messages: []Message{{
		Role: RoleUser,
		Blocks: []Block{
			{Type: BlockText, Text: "sys prompt", PromptCacheBreakpoint: &PromptCacheBreakpoint{TTL: "12h"}},
			{Type: BlockText, Text: "question"},
		},
	}}}
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat))
	p := c.provider.(*openaiChatProvider)
	pl, err := p.buildPayload(chat, false, p.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	msgs := pl["messages"].([]map[string]any)
	parts := msgs[0]["content"].([]map[string]any)
	first := parts[0]["prompt_cache_breakpoint"].(map[string]any)
	if first["mode"] != "explicit" || first["ttl"] != "12h" {
		t.Fatalf("chat breakpoint = %v", parts[0])
	}
	if _, has := parts[1]["prompt_cache_breakpoint"]; has {
		t.Fatal("second block must not carry a breakpoint")
	}

	resp := &ChatRequest{Model: "m", Messages: []Message{{
		Role: RoleUser,
		Blocks: []Block{
			{Type: BlockText, Text: "sys", PromptCacheBreakpoint: &PromptCacheBreakpoint{}},
		},
	}}}
	rc := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	rp := rc.provider.(*openaiResponsesProvider)
	rpl, err := rp.buildPayload(resp, false, rp.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	items := rpl["input"].([]map[string]any)
	content := items[0]["content"].([]map[string]any)
	rbp := content[0]["prompt_cache_breakpoint"].(map[string]any)
	if rbp["mode"] != "explicit" {
		t.Fatalf("responses breakpoint = %v", content[0])
	}
	if _, has := rbp["ttl"]; has {
		t.Fatal("empty TTL must be omitted")
	}
}

// RG-20: the caller's stable idempotency key reaches the wire unchanged.
func TestIdempotencyKeyOverride(t *testing.T) {
	var got string
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		got = r.Header.Get("Idempotency-Key")
		_, _ = io.ReadAll(r.Body)
		w.Header().Set("Content-Type", "application/json")
		io.WriteString(w, `{"choices":[{"message":{"content":"ok"},"finish_reason":"stop"}]}`)
	}))
	defer srv.Close()

	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithProtocol(ProtoOpenAIChat))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), &ChatRequest{
		Model: "m", Messages: []Message{User("hi")}, IdempotencyKey: "stable-key",
	}); err != nil {
		t.Fatal(err)
	}
	if got != "stable-key" {
		t.Fatalf("Idempotency-Key = %q", got)
	}
}
