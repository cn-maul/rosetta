package rosetta

// Prompt-cache surface tests: breakpoint validation, system rendering, the
// extended-TTL beta header (typed and Extra-injected paths), usage folding
// and the beta/block coverage the earlier audit rounds left untested — the
// gap that let the Extra blind spot survive.

import (
	"context"
	"errors"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
)

// anthropicOKBody is a minimal valid Messages response.
const anthropicOKBody = `{"id":"msg_1","model":"claude-x",` +
	`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",` +
	`"usage":{"input_tokens":1,"output_tokens":1}}`

// recordingAnthropicServer answers every request with anthropicOKBody and
// records the anthropic-beta header it saw.
func recordingAnthropicServer(t *testing.T, betas *[]string, mu *sync.Mutex) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		*betas = append(*betas, r.Header.Get("anthropic-beta"))
		mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	t.Cleanup(srv.Close)
	return srv
}

// newCacheTestClient points the client at the test server's real address.
// httptest's loopback client only redirects example.com to its listener, so
// an endpoint of http://api.test (the newTestClient default) would be dialed
// for real; synctest-based tests get away with it because NewTestServer
// installs a fake network, these do not.
func newCacheTestClient(t *testing.T, srv *httptest.Server, opts ...Option) *Client {
	t.Helper()
	all := append([]Option{
		WithEndpoint(srv.URL + "/v1"),
		WithAPIKey("k"),
		WithProtocol(ProtoAnthropic),
		WithHTTPClient(srv.Client()),
	}, opts...)
	c, err := NewClient(all...)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// CacheControl's Type and TTL are validated identically whether reached
// directly or through ChatRequest.validate (message blocks and tools).
func TestCacheControlValidation(t *testing.T) {
	tests := []struct {
		name    string
		cc      CacheControl
		wantErr bool
	}{
		{"empty uses provider defaults", CacheControl{}, false},
		{"ephemeral 5m", CacheControl{Type: "ephemeral", TTL: "5m"}, false},
		{"ephemeral 1h", CacheControl{Type: "ephemeral", TTL: "1h"}, false},
		{"unsupported type", CacheControl{Type: "glacial"}, true},
		{"unsupported ttl", CacheControl{Type: "ephemeral", TTL: "2h"}, true},
		{"type ok ttl bad", CacheControl{Type: "ephemeral", TTL: "1m"}, true},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cc := tc.cc
			if got := cc.validate() != nil; got != tc.wantErr {
				t.Errorf("CacheControl.validate err = %v, want error = %v", cc.validate(), tc.wantErr)
			}
			req := &ChatRequest{Model: "m", Messages: []Message{{
				Role:   RoleUser,
				Blocks: []Block{{Type: BlockText, Text: "x", CacheControl: &cc}},
			}}}
			if got := errors.Is(req.validate(), ErrInvalidRequest); got != tc.wantErr {
				t.Errorf("ChatRequest.validate err = %v, want ErrInvalidRequest = %v", req.validate(), tc.wantErr)
			}
			withTool := &ChatRequest{Model: "m", Messages: []Message{User("x")},
				Tools: []ToolDefinition{{Name: "t", CacheControl: &cc}}}
			if got := errors.Is(withTool.validate(), ErrInvalidRequest); got != tc.wantErr {
				t.Errorf("tool breakpoint err = %v, want ErrInvalidRequest = %v", withTool.validate(), tc.wantErr)
			}
		})
	}

	// Constructor defaults are part of the public surface.
	if cc := ExtendedCache(); cc.TTL != "1h" || cc.Type != "" {
		t.Errorf("ExtendedCache() = %+v, want TTL 1h and empty Type", cc)
	}
	if cc := EphemeralCache(); cc.TTL != "" || cc.Type != "" {
		t.Errorf("EphemeralCache() = %+v, want provider defaults", cc)
	}
	if got := EphemeralCache().toWire(); got["type"] != "ephemeral" || len(got) != 1 {
		t.Errorf("EphemeralCache().toWire() = %v, want only type=ephemeral", got)
	}
	if got := ExtendedCache().toWire(); got["ttl"] != "1h" {
		t.Errorf("ExtendedCache().toWire() = %v, want ttl=1h", got)
	}
	var nilCC *CacheControl
	if nilCC.toWire() != nil {
		t.Error("nil CacheControl must render as no breakpoint")
	}
}

// renderAnthropicSystem keeps the wire-identical string form until a
// breakpoint forces the text-block array.
func TestRenderAnthropicSystem(t *testing.T) {
	if got := renderAnthropicSystem(nil); got != nil {
		t.Fatalf("no parts must render nil, got %#v", got)
	}
	plain := []anthroSysPart{{text: "a"}, {text: "b"}}
	got, ok := renderAnthropicSystem(plain).(string)
	if !ok || got != "a\n\nb" {
		t.Fatalf("unmarked parts must join into a string, got %#v", renderAnthropicSystem(plain))
	}

	marked := []anthroSysPart{{text: "a"}, {text: "b", cc: ExtendedCache()}}
	blocks, ok := renderAnthropicSystem(marked).([]map[string]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("marked parts must render as text blocks, got %#v", renderAnthropicSystem(marked))
	}
	if _, has := blocks[0]["cache_control"]; has {
		t.Errorf("unmarked segment must not carry a breakpoint: %v", blocks[0])
	}
	cc, ok := blocks[1]["cache_control"].(map[string]any)
	if !ok || cc["ttl"] != "1h" {
		t.Fatalf("breakpoint missing on the marked segment: %v", blocks[1])
	}
	if blocks[1]["type"] != "text" || blocks[1]["text"] != "b" {
		t.Errorf("segment content changed: %v", blocks[1])
	}
}

// The system field stays a plain string with no breakpoint (wire bytes
// unchanged) and becomes an array when a RoleSystem message marks one.
func TestAnthropicSystemWireShape(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)

	build := func(req *ChatRequest) map[string]any {
		t.Helper()
		payload, err := p.buildPayload(req, false, p.plan(req))
		if err != nil {
			t.Fatalf("buildPayload: %v", err)
		}
		return payload
	}

	plain := &ChatRequest{Model: "claude-x", System: "sys", Messages: []Message{User("hi")}}
	if got := build(plain)["system"]; got != "sys" {
		t.Fatalf("unmarked system must stay a string, got %#v", got)
	}

	marked := &ChatRequest{
		Model:  "claude-x",
		System: "base",
		Messages: []Message{
			{Role: RoleSystem, Blocks: []Block{{Type: BlockText, Text: "ctx", CacheControl: EphemeralCache()}}},
			User("hi"),
		},
	}
	blocks, ok := build(marked)["system"].([]map[string]any)
	if !ok || len(blocks) != 2 {
		t.Fatalf("marked system must become text blocks, got %#v", build(marked)["system"])
	}
	if blocks[1]["text"] != "ctx" {
		t.Errorf("system segment order wrong: %v", blocks)
	}
	if _, has := blocks[1]["cache_control"]; !has {
		t.Errorf("system breakpoint did not reach the wire: %v", blocks[1])
	}
}

// payloadNeedsExtendedCacheTTL must see a 1h breakpoint in every container
// shape the builders and Extra can produce.
func TestPayloadNeedsExtendedCacheTTL(t *testing.T) {
	hourly := map[string]any{"type": "ephemeral", "ttl": "1h"}
	tests := []struct {
		name string
		v    any
		want bool
	}{
		{"nil", nil, false},
		{"bare string", "1h", false},
		{"messages []map", map[string]any{"messages": []map[string]any{
			{"content": []map[string]any{{"cache_control": hourly}}},
		}}, true},
		{"nested []any", map[string]any{"system": []any{
			map[string]any{"cache_control": map[string]any{"ttl": "1h"}},
		}}, true},
		{"ttl 5m only", map[string]any{"tools": []map[string]any{
			{"cache_control": map[string]any{"type": "ephemeral", "ttl": "5m"}},
		}}, false},
		{"ttl not a string", map[string]any{"tools": []map[string]any{
			{"cache_control": map[string]any{"ttl": 1}},
		}}, false},
		{"cache_control not an object", map[string]any{"system": []any{
			map[string]any{"cache_control": "ephemeral"},
		}}, false},
	}
	for _, tc := range tests {
		if got := payloadNeedsExtendedCacheTTL(tc.v); got != tc.want {
			t.Errorf("%s: payloadNeedsExtendedCacheTTL = %v, want %v", tc.name, got, tc.want)
		}
	}
}

// R3: the beta header must follow the payload, so a 1h breakpoint written
// through Extra is not sent without it (which Anthropic rejects with a 400).
func TestAnthropicExtendedCacheTTLBetaHeader(t *testing.T) {
	hourlyCC := map[string]any{"type": "ephemeral", "ttl": "1h"}
	shortCC := map[string]any{"type": "ephemeral", "ttl": "5m"}
	textBlock := func(cc *CacheControl) *ChatRequest {
		return &ChatRequest{Model: "claude-x", Messages: []Message{{
			Role:   RoleUser,
			Blocks: []Block{{Type: BlockText, Text: "seg", CacheControl: cc}},
		}}}
	}

	tests := []struct {
		name      string
		req       func() *ChatRequest
		overrides bool
		want      bool
	}{
		{"no breakpoint", func() *ChatRequest { return textBlock(nil) }, false, false},
		{"ephemeral 5m", func() *ChatRequest { return textBlock(EphemeralCache()) }, false, false},
		{"extended typed block", func() *ChatRequest { return textBlock(ExtendedCache()) }, false, true},
		{"extended typed tool", func() *ChatRequest {
			return &ChatRequest{Model: "claude-x", Messages: []Message{User("hi")},
				Tools: []ToolDefinition{{Name: "t", CacheControl: ExtendedCache()}}}
		}, false, true},
		{"extra system []any", func() *ChatRequest {
			return &ChatRequest{Model: "claude-x", Messages: []Message{User("hi")},
				Extra: map[string]any{"system": []any{
					map[string]any{"type": "text", "text": "s", "cache_control": hourlyCC},
				}}}
		}, true, true},
		{"extra tools []map", func() *ChatRequest {
			return &ChatRequest{Model: "claude-x", Messages: []Message{User("hi")},
				Extra: map[string]any{"tools": []map[string]any{{
					"name":          "t",
					"input_schema":  map[string]any{"type": "object"},
					"cache_control": hourlyCC,
				}}}}
		}, true, true},
		{"extra 5m only", func() *ChatRequest {
			return &ChatRequest{Model: "claude-x", Messages: []Message{User("hi")},
				Extra: map[string]any{"tools": []map[string]any{{
					"name":          "t",
					"input_schema":  map[string]any{"type": "object"},
					"cache_control": shortCC,
				}}}}
		}, true, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			var betas []string
			var mu sync.Mutex
			srv := recordingAnthropicServer(t, &betas, &mu)
			var opts []Option
			if tc.overrides {
				opts = append(opts, WithExtraOverrides(true))
			}
			c := newCacheTestClient(t, srv, opts...)
			if _, err := c.Chat(context.Background(), tc.req()); err != nil {
				t.Fatalf("chat: %v", err)
			}
			mu.Lock()
			got := betas[len(betas)-1]
			mu.Unlock()
			want := ""
			if tc.want {
				want = extendedCacheTTLBeta
			}
			if got != want {
				t.Fatalf("anthropic-beta = %q, want %q", got, want)
			}
		})
	}
}

// Every block kind Anthropic accepts a breakpoint on must carry it to the
// wire, and the wire count must match the four-breakpoint ceiling input.
func TestAnthropicCacheBreakpointsReachEveryBlockKind(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)

	cc := func() *CacheControl { return ExtendedCache() }
	req := &ChatRequest{
		Model: "claude-x",
		Messages: []Message{
			{Role: RoleUser, Blocks: []Block{
				{Type: BlockImage, ImageURL: "data:image/png;base64,QUJD", CacheControl: cc()},
				{Type: BlockFile, FileID: "file-1", FileName: "a.pdf", CacheControl: cc()},
			}},
			{Role: RoleAssistant, Blocks: []Block{
				{Type: BlockToolCall, ToolCallID: "t1", ToolName: "f", Arguments: `{}`, CacheControl: cc()},
			}},
			{Role: RoleTool, Blocks: []Block{
				{Type: BlockToolResult, ToolCallID: "t1", Content: "r", CacheControl: cc()},
			}},
		},
	}
	payload, err := p.buildPayload(req, false, p.plan(req))
	if err != nil {
		t.Fatalf("buildPayload: %v", err)
	}
	msgs, ok := payload["messages"].([]map[string]any)
	if !ok || len(msgs) != 3 {
		t.Fatalf("messages = %#v, want 3 turns", payload["messages"])
	}
	contentAt := func(i, blk int) map[string]any {
		t.Helper()
		blocks, ok := msgs[i]["content"].([]map[string]any)
		if !ok || blk >= len(blocks) {
			t.Fatalf("messages[%d].content[%d] missing: %#v", i, blk, msgs[i]["content"])
		}
		return blocks[blk]
	}
	wantTypes := []struct {
		msg, blk int
		typ      string
	}{
		{0, 0, "image"},
		{0, 1, "document"},
		{1, 0, "tool_use"},
		{2, 0, "tool_result"},
	}
	for _, w := range wantTypes {
		blk := contentAt(w.msg, w.blk)
		if blk["type"] != w.typ {
			t.Errorf("messages[%d].content[%d].type = %v, want %s", w.msg, w.blk, blk["type"], w.typ)
		}
		if _, has := blk["cache_control"]; !has {
			t.Errorf("%s block lost its breakpoint: %v", w.typ, blk)
		}
	}
	if n := countCacheControl(payload); n != 4 {
		t.Fatalf("wire breakpoints = %d, want 4", n)
	}
}

// R9: a breakpoint on an empty text block would be dropped before the wire,
// so it is rejected locally instead of silently vanishing.
func TestEmptyTextBlockCacheBreakpointRejected(t *testing.T) {
	broken := &ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Blocks: []Block{
		{Type: BlockText, Text: "", CacheControl: ExtendedCache()},
		{Type: BlockImage, ImageURL: "data:image/png;base64,QUJD"},
	}}}}
	if err := broken.validate(); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("empty text block with a breakpoint: err = %v, want ErrInvalidRequest", err)
	}
	if err := broken.validate(); err == nil || !strings.Contains(err.Error(), "cache breakpoint") {
		t.Fatalf("error must explain the lost breakpoint, got %v", err)
	}

	// The placeholder itself stays legal — only the unreachable breakpoint
	// is an error.
	ok := &ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Blocks: []Block{
		{Type: BlockText, Text: ""},
		{Type: BlockImage, ImageURL: "data:image/png;base64,QUJD"},
	}}}}
	if err := ok.validate(); err != nil {
		t.Fatalf("empty text placeholder must stay valid: %v", err)
	}
}

// R7: message_delta reporting an explicit 0 must override the message_start
// baseline (value comparison would keep the stale hit count).
func TestAnthropicStreamUsageExplicitZeroOverridesBaseline(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)
	body := `data: {"type":"message_start","message":{"id":"m","model":"claude-x","usage":{"input_tokens":10,"output_tokens":1,"cache_read_input_tokens":100,"cache_creation_input_tokens":20}}}` + "\n\n" +
		`data: {"type":"message_delta","delta":{"stop_reason":"end_turn"},"usage":{"output_tokens":5,"cache_read_input_tokens":0}}` + "\n\n" +
		`data: {"type":"message_stop"}` + "\n\n"

	next := p.streamEvents(tBody(body))
	var end *Event
	for {
		ev, err := next()
		if err != nil {
			if errors.Is(err, io.EOF) {
				break
			}
			t.Fatalf("stream: %v", err)
		}
		if ev.Type == EventMessageEnd {
			end = ev
		}
	}
	if end == nil || end.Usage == nil {
		t.Fatal("no end event with usage")
	}
	u := *end.Usage
	if u.CachedInputTokens != 0 {
		t.Errorf("CachedInputTokens = %d, want 0 (explicit zero must win)", u.CachedInputTokens)
	}
	if u.CachedCreationTokens != 20 {
		t.Errorf("CachedCreationTokens = %d, want 20 (omitted field keeps the baseline)", u.CachedCreationTokens)
	}
	if u.OutputTokens != 5 {
		t.Errorf("OutputTokens = %d, want 5", u.OutputTokens)
	}
	// Input folds the (now zero) read plus the retained creation count.
	if u.InputTokens != 30 || u.TotalTokens != 35 {
		t.Errorf("Input/Total = %d/%d, want 30/35", u.InputTokens, u.TotalTokens)
	}
}

// R8: a response carrying only cache (or only reasoning) numbers did report
// usage, so it must not be filed as UsageMissing.
func TestUsageIsZeroCountsCacheAndReasoning(t *testing.T) {
	tests := []struct {
		name string
		u    Usage
		want bool
	}{
		{"nothing reported", Usage{}, true},
		{"cache read only", Usage{CachedInputTokens: 500}, false},
		{"cache creation only", Usage{CachedCreationTokens: 100}, false},
		{"reasoning only", Usage{ReasoningTokens: 7}, false},
		{"input only", Usage{InputTokens: 1}, false},
	}
	for _, tc := range tests {
		if got := tc.u.IsZero(); got != tc.want {
			t.Errorf("%s: IsZero = %v, want %v", tc.name, got, tc.want)
		}
	}

	// End to end: Anthropic reporting only cache counts must land in the
	// tracker as tokens, not as a missing-usage observation.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"msg_1","model":"claude-x",`+
			`"content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn",`+
			`"usage":{"cache_read_input_tokens":50,"cache_creation_input_tokens":10}}`)
	}))
	defer srv.Close()

	tr := NewMemoryUsageTracker()
	c := newCacheTestClient(t, srv, WithUsageTracker(tr))
	resp, err := c.Chat(context.Background(), &ChatRequest{Model: "claude-x", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}
	if resp.Usage.IsZero() {
		t.Fatal("cache-only usage must not read as zero")
	}
	snap := tr.Snapshot()
	if snap.UsageMissing != 0 {
		t.Errorf("UsageMissing = %d, want 0", snap.UsageMissing)
	}
	if snap.CachedInputTokens != 50 || snap.CachedCreationTokens != 10 || snap.InputTokens != 60 {
		t.Errorf("tracker snapshot = %+v, want 60 folded input with 50/10 cache", snap)
	}
}

// R11: a unary reply with tool calls but no stop_reason must not claim a
// clean end — an agent loop branches on StopReason.
func TestAnthropicUnaryStopReasonCorrection(t *testing.T) {
	tests := []struct {
		name string
		body string
		want StopReason
	}{
		{
			"tool call without stop_reason",
			`{"id":"m","model":"claude-x","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}]}`,
			StopToolUse,
		},
		{
			"text without stop_reason",
			`{"id":"m","model":"claude-x","content":[{"type":"text","text":"hi"}]}`,
			StopEnd,
		},
		{
			"explicit stop_reason wins",
			`{"id":"m","model":"claude-x","content":[{"type":"tool_use","id":"t1","name":"f","input":{}}],"stop_reason":"max_tokens"}`,
			StopLength,
		},
	}
	for _, tc := range tests {
		resp, err := decodeAnthropicResponse([]byte(tc.body), "POST", "http://api.test/v1/messages", "")
		if err != nil {
			t.Fatalf("%s: %v", tc.name, err)
		}
		if resp.StopReason != tc.want {
			t.Errorf("%s: StopReason = %s, want %s", tc.name, resp.StopReason, tc.want)
		}
	}
}

// R10: Extra reaches the payload after the typed fields, so it must count
// toward (or at least not bypass) the context estimate.
func TestEstimateInputTokensCountsExtra(t *testing.T) {
	base := &ChatRequest{Model: "m", Messages: []Message{User("hi")}}
	withExtra := &ChatRequest{Model: "m", Messages: []Message{User("hi")},
		Extra: map[string]any{"context": strings.Repeat("padding ", 2000)}}
	baseEst := base.estimateInputTokens(MultimediaTokenEstimates{})
	if got := withExtra.estimateInputTokens(MultimediaTokenEstimates{}); got <= baseEst {
		t.Fatalf("Extra must add to the estimate: %d vs %d", got, baseEst)
	}
}

// R10: Anthropic's 4096 output floor must take part in the strict context
// gate instead of being invisible as "0 means unlimited".
func TestAnthropicOutputFloorParticipatesInContextCheck(t *testing.T) {
	anthropicSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, anthropicOKBody)
	}))
	defer anthropicSrv.Close()
	openaiSrv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"id":"c1","model":"small","choices":[{"message":{"content":"hi"},`+
			`"finish_reason":"stop"}],"usage":{"prompt_tokens":1,"completion_tokens":1,"total_tokens":2}}`)
	}))
	defer openaiSrv.Close()

	// A tiny prompt plus the implicit 4096 max_tokens does not fit a 4000
	// window; before the fix `out` was 0 and the gate never fired. The error
	// is raised in prepare, before anything is sent.
	c := newCacheTestClient(t, anthropicSrv, WithStrictContextCheck(true),
		WithModelInfo(ModelInfo{ID: "small", ContextWindow: 4000, Known: true}))
	_, err := c.Chat(context.Background(), &ChatRequest{Model: "small", Messages: []Message{User("hi")}})
	if !errors.Is(err, ErrContextTooLong) {
		t.Fatalf("anthropic implicit output floor: err = %v, want ErrContextTooLong", err)
	}

	// OpenAI protocols still let the provider decide, so the same request
	// goes through (the input alone fits).
	oc, err := NewClient(WithEndpoint(openaiSrv.URL+"/v1"), WithAPIKey("k"),
		WithProtocol(ProtoOpenAIChat), WithHTTPClient(openaiSrv.Client()),
		WithStrictContextCheck(true),
		WithModelInfo(ModelInfo{ID: "small", ContextWindow: 4000, Known: true}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := oc.Chat(context.Background(), &ChatRequest{Model: "small", Messages: []Message{User("hi")}}); err != nil {
		t.Fatalf("openai chat must not gain an output-side gate: %v", err)
	}
}

// R6: the owner callback reads the model through the mutex while other
// goroutines drive the stream (run under -race).
func TestStreamEndCallbackModelReadIsSafe(t *testing.T) {
	tr := NewMemoryUsageTracker()
	var seen string
	var mu sync.Mutex
	var s *streamCore
	s = newStream(seqNext(
		&Event{Type: EventMessageStart, Model: "claude-x"},
		&Event{Type: EventMessageEnd, StopReason: StopEnd, Usage: &Usage{InputTokens: 3}},
	), func(u Usage, err error) {
		mu.Lock()
		seen = s.model()
		mu.Unlock()
		tr.Record(context.Background(), UsageRecord{Model: s.model(), Usage: u})
	})
	for s.Next() {
	}
	if seen != "claude-x" {
		t.Fatalf("callback saw model %q, want claude-x", seen)
	}
	snap := tr.Snapshot()
	if got := snap.ByModel["claude-x"]; got.Requests != 1 || got.InputTokens != 3 {
		t.Fatalf("callback recorded %+v, want one claude-x request with 3 input tokens", snap.ByModel)
	}
}
