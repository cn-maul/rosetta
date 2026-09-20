package rosetta

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
)

// A1: an Anthropic client must never re-send x-api-key to a cross-host
// redirect target, whether using the SDK default client or a caller-supplied
// one.
func TestAuditAnthropicKeyNotLeakedOnCrossHostRedirect(t *testing.T) {
	const secret = "sk-ant-SUPERSECRETKEY123"
	var hit atomic.Int32
	var gotKey atomic.Value
	target := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hit.Add(1)
		gotKey.Store(r.Header.Get("x-api-key"))
		w.WriteHeader(http.StatusOK)
		_, _ = w.Write([]byte(`{"id":"m","content":[]}`))
	}))
	defer target.Close()
	// Make the target a different HOSTNAME (127.0.0.1 -> localhost) so the
	// cross-host guard fires.
	targetHost := strings.Replace(target.URL, "127.0.0.1", "localhost", 1)
	origin := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Location", targetHost+"/v1/messages")
		w.WriteHeader(http.StatusFound)
	}))
	defer origin.Close()

	req := &ChatRequest{Model: "m", Messages: []Message{User("hi")}}

	c, err := NewClient(WithEndpoint(origin.URL), WithAPIKey(secret), WithProtocol(ProtoAnthropic))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.Chat(context.Background(), req); err == nil {
		t.Fatal("expected the cross-host redirect to be refused")
	}

	// A caller-supplied bare client must be guarded too.
	c2, err := NewClient(WithEndpoint(origin.URL), WithAPIKey(secret),
		WithProtocol(ProtoAnthropic), WithHTTPClient(&http.Client{}))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c2.Chat(context.Background(), req); err == nil {
		t.Fatal("expected the caller client to be guarded against cross-host redirect")
	}

	if hit.Load() != 0 {
		t.Fatalf("cross-host target must never be contacted; leaked x-api-key=%q", gotKey.Load())
	}
}

// C1: displayEndpoint must strip userinfo as well as query, and NewClient's
// rejection error must not echo a pasted credential.
func TestAuditDisplayEndpointStripsUserinfo(t *testing.T) {
	got := displayEndpoint("https://sk-abc123:pw@example.com/v1?k=secret")
	if strings.Contains(got, "sk-abc123") || strings.Contains(got, "pw") || strings.Contains(got, "secret") {
		t.Fatalf("displayEndpoint leaked credentials: %q", got)
	}
	if _, err := NewClient(WithEndpoint("https://sk-abc123:pw@example.com/v1")); err != nil {
		if strings.Contains(err.Error(), "sk-abc123") {
			t.Fatalf("NewClient error echoed userinfo: %v", err)
		}
	}
}

// B12: an explicit WithProtocol must win over DetectClient's probe, which must
// not even run.
func TestAuditDetectClientHonorsExplicitProtocol(t *testing.T) {
	var probeHit atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		probeHit.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"data":[{"id":"gpt-4o","object":"model"}]}`))
	}))
	defer srv.Close()
	c, err := DetectClient(context.Background(), WithEndpoint(srv.URL), WithAPIKey("k"),
		WithProtocol(ProtoOpenAIResponses))
	if err != nil {
		t.Fatal(err)
	}
	if c.Protocol() != ProtoOpenAIResponses {
		t.Fatalf("protocol = %q, want openai-responses", c.Protocol())
	}
	if probeHit.Load() != 0 {
		t.Fatalf("explicit protocol must skip probing; probe made %d requests", probeHit.Load())
	}
}

// C8: classification streams the first data element, so it survives unrelated
// leading keys and a catalog far larger than the read limit.
func TestAuditClassifyCatalog(t *testing.T) {
	big := strings.Repeat("x", 5<<20)
	if proto, ok := classifyCatalog(strings.NewReader(`{"foo":"bar","data":[{"type":"model","id":"claude","pad":"` + big + `"}],"after":{"z":1}}`)); !ok || proto != ProtoAnthropic {
		t.Fatalf("oversized anthropic catalog: proto=%v ok=%v", proto, ok)
	}
	if proto, ok := classifyCatalog(strings.NewReader(`{"data":[{"id":"g","object":"model"}]}`)); !ok || proto != ProtoOpenAIChat {
		t.Fatalf("openai catalog: proto=%v ok=%v", proto, ok)
	}
	if _, ok := classifyCatalog(strings.NewReader(`{"data":[]}`)); ok {
		t.Fatal("empty data must not classify")
	}
	if _, ok := classifyCatalog(strings.NewReader(`{"data":[{"id":"x"}]}`)); ok {
		t.Fatal("unclassifiable first element must not classify")
	}
}

// B2: a clean EOF that arrives after message_delta (which already carried the
// authoritative stop_reason) is a complete stream — it must NOT be reported as
// truncated, even though message_stop never arrived. Only an EOF before any
// terminal (stop_reason still unset) is a truncation.
func TestAuditAnthropicEOFAfterDeltaIsClean(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)

	// Terminal message_delta with a stop_reason, then EOF without message_stop.
	body := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"c\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"hi\"}}\n\n" +
		"data: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"end_turn\"}}\n\n"
	s := newStream(p.streamEvents(tBody(body)), nil)
	for s.Next() {
	}
	if err := s.Err(); err != nil {
		t.Fatalf("EOF after message_delta must be a clean end, got %v", err)
	}
	if p := s.Partial(); p.Text() != "hi" || p.StopReason != StopEnd {
		t.Fatalf("partial = %+v", p)
	}

	// EOF with no terminal at all: truncated, surfaced via ErrStreamTruncated.
	body2 := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"m\",\"model\":\"c\"}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"par\"}}\n\n"
	s2 := newStream(p.streamEvents(tBody(body2)), nil)
	for s2.Next() {
	}
	if !errors.Is(s2.Err(), ErrStreamTruncated) {
		t.Fatalf("EOF without terminal = %v, want ErrStreamTruncated", s2.Err())
	}
	if got := s2.Partial().Text(); got != "par" {
		t.Fatalf("partial kept after truncation = %q, want %q", got, "par")
	}
}

// B3: a stream:true request answered with a buffered 200 application/json body
// must be recognized as such (not misread as an empty SSE stream) and replayed
// losslessly as a one-shot unified stream.
func TestAuditBufferedJSONResponseReplay(t *testing.T) {
	if !bufferedJSONResponse("application/json") {
		t.Fatal("application/json must be recognized as buffered")
	}
	if !bufferedJSONResponse("application/json; charset=utf-8") {
		t.Fatal("parameterized json must be recognized as buffered")
	}
	if bufferedJSONResponse("text/event-stream") {
		t.Fatal("text/event-stream must not be buffered")
	}
	if bufferedJSONResponse("Text/Event-Stream; charset=utf-8") {
		t.Fatal("case-insensitive event-stream must not be buffered")
	}
	if bufferedJSONResponse("") {
		t.Fatal("absent content-type must fall back to (legacy) SSE, not buffered")
	}

	cr := &ChatResponse{
		ID: "x", Model: "m",
		Content: []Block{
			{Type: BlockText, Text: "hello"},
			{Type: BlockToolCall, ToolCallID: "t", ToolName: "f", Arguments: `{"a":1}`},
		},
		StopReason: StopEnd,
		Usage:      Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3},
	}
	resp, err := bufferedStream(cr).Collect()
	if err != nil {
		t.Fatal(err)
	}
	if resp.ID != "x" || resp.Model != "m" || resp.StopReason != StopEnd {
		t.Fatalf("header lost: %+v", resp)
	}
	if resp.Text() != "hello" {
		t.Fatalf("Text = %q", resp.Text())
	}
	tcs := resp.ToolCalls()
	if len(tcs) != 1 || tcs[0].ToolCallID != "t" || tcs[0].Arguments != `{"a":1}` {
		t.Fatalf("tool call lost: %+v", tcs)
	}
	if resp.Usage != (Usage{InputTokens: 1, OutputTokens: 2, TotalTokens: 3}) {
		t.Fatalf("usage lost: %+v", resp.Usage)
	}
}

// B7/C10: redactJSON keeps valid JSON valid while (a) round-tripping number
// literals byte-for-byte via UseNumber, (b) redacting sensitive keys of ANY
// value type (object/number/array), and (c) masking credential patterns that
// appear inside otherwise-insensitive string values.
func TestAuditRedactJSONPreservesNumbersAndMasksEverywhere(t *testing.T) {
	out := string(redactJSON([]byte(`{"big":12345678901234567890,"exp":1e300}`)))
	if !strings.Contains(out, "12345678901234567890") || !strings.Contains(out, "1e300") {
		t.Fatalf("number literals corrupted through float64: %s", out)
	}

	out = string(redactJSON([]byte(`{"api_key":{"x":1},"token":99,"meta":{"access_token":[1,2]}}`)))
	if strings.Contains(out, `"x":1`) || strings.Contains(out, `"token":99`) || strings.Contains(out, "[1,2]") {
		t.Fatalf("non-string sensitive values survived: %s", out)
	}
	if !strings.Contains(out, "[redacted]") {
		t.Fatalf("sensitive keys were not redacted: %s", out)
	}

	out = string(redactJSON([]byte(`{"note":"aws AKIAIOSFODNN7EXAMPLE gh ghp_ABCDEFGHIJKLMNOPQRSTUVWX slack xoxb-1234567890-1234"}`)))
	for _, leak := range []string{"AKIAIOSFODNN7EXAMPLE", "ghp_ABCDEFGHIJKLMNOPQRSTUVWX", "xoxb-1234567890"} {
		if strings.Contains(out, leak) {
			t.Fatalf("pattern %q not masked: %s", leak, out)
		}
	}
}

// B7/C10: a body that is not valid JSON (or that survives decoding only as
// opaque bytes) must still have credential patterns masked — redaction never
// degrades to returning the raw bytes untouched.
func TestAuditSafeTruncateBodyMasksNonJSON(t *testing.T) {
	raw := safeTruncateBody([]byte(`gateway said: your key sk-ant-APIKEY12345678 was rejected`))
	if bytes.Contains(raw, []byte("sk-ant-APIKEY")) {
		t.Fatalf("non-JSON body leaked a key: %s", raw)
	}
}

// C9: safeURL strips the query and masks credentials embedded in the path
// (e.g. a proxy routing key), so neither reaches an error string or log.
func TestAuditSafeURLMasksPathSecret(t *testing.T) {
	got := safeURL("http://gw.test/proxy/sk-ant-APIKEYVALUE12345678/messages?k=ghp_ABCDEFGHIJKLMNOPQRSTUV")
	if strings.Contains(got, "sk-ant-APIKEY") || strings.Contains(got, "ghp_ABCDEF") {
		t.Fatalf("safeURL leaked credentials: %q", got)
	}
}

// C17: accumulation is keyed by the provider content-block index, so
// out-of-order deltas merge into the correct block instead of concatenating
// into whichever block happens to be current.
func TestAuditStreamIndexKeyedMerge(t *testing.T) {
	s := newStream(seqNext(
		&Event{Type: EventThinkingDelta, Text: "A", BlockIndex: 0},
		&Event{Type: EventThinkingDelta, Text: "X", BlockIndex: 1},
		&Event{Type: EventThinkingDelta, Text: "B", BlockIndex: 0},
		&Event{Type: EventThinkingDelta, Signature: "s0", BlockIndex: 0},
	), nil)
	for s.Next() {
	}
	if err := s.Err(); err != nil {
		t.Fatal(err)
	}
	blocks := s.Partial().Content
	if len(blocks) != 2 {
		t.Fatalf("blocks = %d, want 2: %+v", len(blocks), blocks)
	}
	if blocks[0].Type != BlockThinking || blocks[0].Thinking != "AB" || blocks[0].Signature != "s0" {
		t.Fatalf("block 0 wrong: %+v", blocks[0])
	}
	if blocks[1].Type != BlockThinking || blocks[1].Thinking != "X" || blocks[1].Signature != "" {
		t.Fatalf("block 1 wrong: %+v", blocks[1])
	}
}

// B8: distinct content blocks are capped. Exceeding maxStreamBlocks trips the
// stream with ErrStreamOverflow rather than letting a hostile provider grow
// memory without limit; the partial accumulated so far is still retrievable.
func TestAuditStreamOverflowBlockCap(t *testing.T) {
	evs := make([]*Event, 0, maxStreamBlocks+3)
	evs = append(evs, &Event{Type: EventMessageStart, ID: "m", Model: "c"})
	for i := 0; i < maxStreamBlocks+2; i++ {
		evs = append(evs, &Event{Type: EventTextDelta, Text: "x", BlockIndex: i})
	}
	evs = append(evs, &Event{Type: EventMessageEnd, StopReason: StopEnd})

	s := newStream(seqNext(evs...), nil)
	for s.Next() {
	}
	if !errors.Is(s.Err(), ErrStreamOverflow) {
		t.Fatalf("Err = %v, want ErrStreamOverflow", s.Err())
	}
	if n := len(s.Partial().Content); n < maxStreamBlocks {
		t.Fatalf("partial dropped blocks before overflow: %d", n)
	}
}

// B8: the byte cap is enforced on the total volume, charged against the
// accumulation budget rather than per event.
func TestAuditStreamOverflowBytesCap(t *testing.T) {
	big := strings.Repeat("x", (maxStreamAccumBytes>>1)+1)
	s := newStream(seqNext(
		&Event{Type: EventTextDelta, Text: big, BlockIndex: 0},
		&Event{Type: EventTextDelta, Text: big, BlockIndex: 1},
	), nil)
	for s.Next() {
	}
	if !errors.Is(s.Err(), ErrStreamOverflow) {
		t.Fatalf("Err = %v, want ErrStreamOverflow", s.Err())
	}
}
