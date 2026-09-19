package rosetta

// Regression tests for the 2026-09-13 audit report fixes (F01–F13 and the
// boundary items): stream snapshot concurrency, unlock'd end callbacks,
// error-body redaction on oversized bodies, per-API Extra reserved keys,
// the role×block matrix, media field validation, Responses audio/file_url
// semantics, Anthropic text sources, embedding/rerank wire validation and
// real-HTTP stream truncation.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

// F01: Partial() must be safe against a concurrent Next() (run with -race).
func TestAuditFixPartialConcurrentWithNext(t *testing.T) {
	s := newStream(func() (*Event, error) {
		return &Event{Type: EventTextDelta, Text: "x"}, nil
	}, nil)
	defer s.Close()
	done := make(chan struct{})
	go func() {
		defer close(done)
		for i := 0; i < 20000; i++ {
			if !s.Next() {
				return
			}
		}
	}()
	for i := 0; i < 20000; i++ {
		_ = s.Partial()
	}
	<-done
}

// F02: the end-of-stream callback runs outside the stream mutex, so a
// usage tracker that reads the stream must not deadlock.
func TestAuditFixEndCallbackMayReadStream(t *testing.T) {
	doneCh := make(chan struct{})
	var s *streamCore
	s = newStream(func() (*Event, error) { return nil, io.EOF }, func(Usage, error) {
		_ = s.Partial()
		_ = s.Err()
		_ = s.Usage()
		close(doneCh)
	})
	s.Next()
	select {
	case <-doneCh:
	case <-time.After(2 * time.Second):
		t.Fatal("onEnd callback deadlocked against the stream mutex")
	}
}

// F02 (Close path): a tracker closing the stream from onEnd must work.
func TestAuditFixEndCallbackMayClose(t *testing.T) {
	closed := make(chan struct{})
	var s *streamCore
	s = newStream(func() (*Event, error) { return nil, io.EOF }, func(Usage, error) {
		_ = s.Close()
		close(closed)
	})
	s.Next()
	select {
	case <-closed:
	case <-time.After(2 * time.Second):
		t.Fatal("onEnd callback deadlocked calling Close")
	}
}

// F10: oversized (>4KiB) JSON error bodies must still get structured
// redaction — truncation must not happen before redaction.
func TestAuditFixLongErrorBodyRedacted(t *testing.T) {
	body := []byte(`{"error":{"api_key":"provider-secret-123456","padding":"` +
		strings.Repeat("x", 5000) + `"}}`)
	raw := safeTruncateBody(body)
	if !json.Valid(raw) {
		t.Fatalf("Raw must stay valid JSON: %s", raw)
	}
	if strings.Contains(string(raw), "provider-secret-123456") {
		t.Fatalf("api_key leaked into oversized Raw: %s", raw)
	}
}

// F09: reserved keys are per API family — chat Extra["user"] is legal,
// embeddings Extra["user"] is not.
func TestAuditFixExtraReservedKeysPerAPI(t *testing.T) {
	c := newTestClient(t)
	if err := (&ChatRequest{Model: "m", Messages: []Message{User("x")},
		Extra: map[string]any{"user": "end-user-1"}}).validate(); err != nil {
		t.Fatalf("chat Extra user must be accepted: %v", err)
	}
	if _, err := c.Embed(context.Background(), &EmbeddingRequest{Model: "m", Input: []string{"x"},
		Extra: map[string]any{"user": "end-user-1"}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("embeddings Extra user must be rejected, got %v", err)
	}
	if _, err := c.Rerank(context.Background(), &RerankRequest{Model: "m", Query: "q",
		Documents: []string{"d"}, Extra: map[string]any{"documents": []string{"x"}}}); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("rerank Extra documents must be rejected, got %v", err)
	}
}

// F07: role×block matrix — unsupported combinations fail loudly instead
// of being silently dropped by the encoders.
func TestAuditFixRoleBlockMatrix(t *testing.T) {
	cases := []struct {
		name string
		msg  Message
	}{
		{"system file", Message{Role: RoleSystem, Blocks: []Block{FileRef("a.pdf", "f-1")}}},
		{"system audio", Message{Role: RoleSystem, Blocks: []Block{AudioContent("QUJD", "wav")}}},
		{"assistant image", Message{Role: RoleAssistant, Blocks: []Block{{Type: BlockImage, ImageURL: "https://x/y.png"}}}},
		{"user tool call", Message{Role: RoleUser, Blocks: []Block{ToolCall("c1", "fn", "{}")}}},
		{"tool text", Message{Role: RoleTool, Blocks: []Block{{Type: BlockText, Text: "x"}}}},
	}
	for _, tc := range cases {
		if err := tc.msg.validate(); err == nil {
			t.Errorf("%s: must be rejected", tc.name)
		}
	}
	// Supported combinations keep working.
	if err := (Message{Role: RoleUser, Blocks: []Block{{Type: BlockImage, ImageURL: "https://x/y.png"}}}).validate(); err != nil {
		t.Errorf("user image must pass: %v", err)
	}
	if err := (Message{Role: RoleAssistant, Blocks: []Block{Thinking("hmm", "sig")}}).validate(); err != nil {
		t.Errorf("assistant thinking must pass: %v", err)
	}
}

// F13: an empty text placeholder next to a valid media block is legal.
func TestAuditFixEmptyTextPlaceholderWithImage(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIChat))
	p := c.provider.(*openaiChatProvider)
	req := &ChatRequest{Model: "m", Messages: []Message{{Role: RoleUser, Blocks: []Block{
		{Type: BlockText, Text: ""},
		{Type: BlockImage, ImageURL: "https://example.com/a.png"},
	}}}}
	if err := req.validate(); err != nil {
		t.Fatalf("empty text placeholder must pass: %v", err)
	}
	if _, err := p.buildPayload(req, false, p.initialState("m")); err != nil {
		t.Fatalf("payload build must pass: %v", err)
	}
}

// B2: a file block with two sources is ambiguous and rejected.
func TestAuditFixFileDualSourceRejected(t *testing.T) {
	b := FileRef("new.pdf", "file-old")
	b.FileData = "TkVX"
	if err := b.validate(); err == nil {
		t.Fatal("FileData+FileID must be rejected")
	}
}

// B3: media payloads are syntax-checked locally.
func TestAuditFixMediaPayloadValidation(t *testing.T) {
	if err := AudioContent("not-base64!", "wav").validate(); err == nil {
		t.Error("invalid audio base64 must be rejected")
	}
	if err := AudioContent("QUJD", "wav").validate(); err != nil {
		t.Errorf("valid audio base64 must pass: %v", err)
	}
	if err := FileContent("a.pdf", "application/pdf", "data:application/pdf;base64,").validate(); err == nil {
		t.Error("empty data URL payload must be rejected")
	}
	if err := FileContent("a.pdf", "application/pdf", "data:application/pdf;base64,!!!!").validate(); err == nil {
		t.Error("bad base64 data URL payload must be rejected")
	}
	if err := FileContent("a.pdf", "application/pdf", "SkpL").validate(); err != nil {
		t.Errorf("valid raw base64 must pass: %v", err)
	}
}

// F03: Responses rejects audio; B1: it maps http(s) file URLs to file_url.
func TestAuditFixResponsesAudioAndFileURL(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	p := c.provider.(*openaiResponsesProvider)
	audioReq := &ChatRequest{Model: "m", Messages: []Message{{
		Role:   RoleUser,
		Blocks: []Block{AudioContent("QUJD", "wav")},
	}}}
	if _, err := p.buildPayload(audioReq, false, p.initialState("m")); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("responses audio must be rejected, got %v", err)
	}
	urlReq := &ChatRequest{Model: "m", Messages: []Message{{
		Role:   RoleUser,
		Blocks: []Block{FileContent("a.pdf", "application/pdf", "https://example.com/a.pdf")},
	}}}
	pl, err := p.buildPayload(urlReq, false, p.initialState("m"))
	if err != nil {
		t.Fatal(err)
	}
	parts := pl["input"].([]map[string]any)[0]["content"].([]map[string]any)
	if parts[0]["file_url"] != "https://example.com/a.pdf" {
		t.Fatalf("file_url part = %v", parts[0])
	}
}

// F08: Anthropic text/plain documents use the text source; other
// non-PDF media are rejected instead of mis-encoded as base64 PDFs.
func TestAuditFixAnthropicTextDocument(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoAnthropic))
	p := c.provider.(*anthropicProvider)
	req := &ChatRequest{Model: "m", Messages: []Message{{
		Role:   RoleUser,
		Blocks: []Block{FileContent("a.txt", "text/plain", "SGVsbG8=")},
	}}}
	pl, err := p.buildPayload(req, false, p.plan(req))
	if err != nil {
		t.Fatal(err)
	}
	doc := pl["messages"].([]map[string]any)[0]["content"].([]map[string]any)[0]
	src := doc["source"].(map[string]any)
	if src["type"] != "text" || src["data"] != "Hello" {
		t.Fatalf("text source = %v", src)
	}
	bad := &ChatRequest{Model: "m", Messages: []Message{{
		Role:   RoleUser,
		Blocks: []Block{FileContent("a.csv", "text/csv", "QUJD")},
	}}}
	if _, err := p.buildPayload(bad, false, p.plan(bad)); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("unsupported media must be rejected, got %v", err)
	}
}

// F05/F06/B5: embedding wire validation — null fields, dimension checks,
// order restoration and negative usage.
func TestAuditFixEmbeddingWireValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"null index", `{"data":[{"index":null,"embedding":[1]}]}`, "missing its index"},
		{"null element", `{"data":[{"index":0,"embedding":[null,0.5]}]}`, "element 0 is null"},
		{"non-numeric element", `{"data":[{"index":0,"embedding":["x"]}]}`, "not a number"},
		{"missing embedding", `{"data":[{"index":0}]}`, "empty vector"},
	}
	for _, tc := range cases {
		if _, err := decodeEmbeddingsResponse([]byte(tc.body), 1, 0); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// Dimension mismatch within the batch.
	if _, err := decodeEmbeddingsResponse([]byte(`{"data":[{"index":0,"embedding":[1]},{"index":1,"embedding":[1,2,3]}]}`), 2, 0); err == nil {
		t.Error("inconsistent batch dimensions must be rejected")
	}
	// Requested dimensions must match.
	if _, err := decodeEmbeddingsResponse([]byte(`{"data":[{"index":0,"embedding":[1]}]}`), 1, 2); err == nil {
		t.Error("requested-dimension mismatch must be rejected")
	}
	// Out-of-order data is restored to input order.
	resp, err := decodeEmbeddingsResponse([]byte(
		`{"data":[{"index":1,"embedding":[3,4]},{"index":0,"embedding":[1,2]}]}`), 2, 0)
	if err != nil {
		t.Fatal(err)
	}
	if resp.Data[0].Index != 0 || resp.Data[1].Index != 1 {
		t.Fatalf("order = %d,%d", resp.Data[0].Index, resp.Data[1].Index)
	}
	// Negative usage is a protocol error.
	if _, err := decodeEmbeddingsResponse([]byte(
		`{"data":[{"index":0,"embedding":[1]}],"usage":{"input_tokens":-5}}`), 1, 0); err == nil {
		t.Error("negative usage must be rejected")
	}
}

// F05/B5: rerank wire validation and score-descending re-sort.
func TestAuditFixRerankWireValidation(t *testing.T) {
	cases := []struct {
		name string
		body string
		want string
	}{
		{"null result", `{"results":[null]}`, "result 0 is null"},
		{"empty result object", `{"results":[{}]}`, "missing its index"},
		{"null score", `{"results":[{"index":0,"relevance_score":null}]}`, "missing its relevance score"},
		{"missing score", `{"results":[{"index":0}]}`, "missing its relevance score"},
	}
	for _, tc := range cases {
		if _, err := decodeRerankResponse([]byte(tc.body), 2); err == nil || !strings.Contains(err.Error(), tc.want) {
			t.Errorf("%s: err = %v, want %q", tc.name, err, tc.want)
		}
	}
	// Out-of-order results are re-sorted most relevant first.
	resp, err := decodeRerankResponse([]byte(
		`{"results":[{"index":0,"relevance_score":0.1},{"index":1,"relevance_score":0.9},{"index":2,"relevance_score":0.5}]}`), 3)
	if err != nil {
		t.Fatal(err)
	}
	want := []int{1, 2, 0}
	for i, w := range want {
		if resp.Results[i].Index != w {
			t.Fatalf("order = %v, want %v", resp.Results, want)
		}
	}
	// Negative usage is a protocol error.
	if _, err := decodeRerankResponse([]byte(
		`{"results":[{"index":0,"relevance_score":0.5}],"meta":{"tokens":{"input_tokens":-1}}}`), 1); err == nil {
		t.Error("negative usage must be rejected")
	}
}

// B6: DisableThinking normalizes a standalone manual entry.
func TestAuditFixDisableThinkingStandalone(t *testing.T) {
	r := newRegistry()
	if err := r.SetManual([]ModelInfo{{ID: "m", SupportsThinking: true, DisableThinking: true}}); err != nil {
		t.Fatal(err)
	}
	mi, ok := r.Lookup("m")
	if !ok {
		t.Fatal("model must resolve")
	}
	if mi.SupportsThinking {
		t.Fatal("DisableThinking must force SupportsThinking=false")
	}
}

// B7: a rejected endpoint with a query does not echo the query in the
// build error.
func TestAuditFixEndpointErrorRedactsQuery(t *testing.T) {
	_, err := NewClient(WithEndpoint("https://api.test/v1?api_key=sk-verysecret"), WithAPIKey("k"))
	if err == nil {
		t.Fatal("query endpoint must be rejected")
	}
	if strings.Contains(err.Error(), "sk-verysecret") {
		t.Fatalf("error echoes the query credential: %v", err)
	}
}

// F12: a real HTTP chunked stream cut before the terminal sentinel fails
// with ErrStreamTruncated (not a bare unexpected EOF) and retains the
// already-received usage.
func TestAuditFixHTTPTruncationChat(t *testing.T) {
	body := "data: {\"id\":\"r\",\"model\":\"m\",\"choices\":[{\"delta\":{\"content\":\"partial\"}}],\"usage\":{\"prompt_tokens\":5,\"completion_tokens\":2}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(body), body)
		rw.Flush()
		// Cut the connection without the final zero chunk: the client
		// observes io.ErrUnexpectedEOF.
	}))
	defer srv.Close()
	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithProtocol(ProtoOpenAIChat))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := c.ChatStream(ctx, &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ends, sawText := 0, false
	for s.Next() {
		switch ev := s.Event(); ev.Type {
		case EventTextDelta:
			sawText = ev.Text == "partial"
		case EventMessageEnd:
			ends++
		}
	}
	err = s.Err()
	if !errors.Is(err, ErrStreamTruncated) {
		t.Fatalf("err = %v, want ErrStreamTruncated", err)
	}
	if ends != 1 {
		t.Fatalf("end events = %d, want 1", ends)
	}
	if !sawText {
		t.Fatal("partial text lost")
	}
	if u := s.Usage(); u.InputTokens != 5 || u.OutputTokens != 2 {
		t.Fatalf("usage = %+v, want 5/2", u)
	}
}

// ---- 公开入口（Chat/ChatStream/Embed/Rerank → validate → prepare → provider → HTTP）回归 ----

// F07 公开入口：三协议 Chat 都在请求发出前拒绝非法角色×块组合。
func TestAuditFixPublicChatRejectsRoleBlockMatrix(t *testing.T) {
	build := func() *ChatRequest {
		return &ChatRequest{Model: "m", Messages: []Message{
			{Role: RoleSystem, Blocks: []Block{FileRef("a.pdf", "f-1")}},
			User("hi"),
		}}
	}
	for _, proto := range []Protocol{ProtoOpenAIChat, ProtoOpenAIResponses, ProtoAnthropic} {
		// endpoint 是死地址：校验必须先于任何网络请求失败。
		c := newTestClient(t, WithProtocol(proto))
		if _, err := c.Chat(context.Background(), build()); !errors.Is(err, ErrInvalidRequest) {
			t.Errorf("%s: public Chat must reject system file block, got %v", proto, err)
		}
	}
}

// F03 公开入口：Responses 的公开 Chat 拒绝音频输入。
func TestAuditFixPublicResponsesRejectsAudio(t *testing.T) {
	c := newTestClient(t, WithProtocol(ProtoOpenAIResponses))
	req := &ChatRequest{Model: "m", Messages: []Message{UserAudio("", "QUJD", "wav")}}
	if _, err := c.Chat(context.Background(), req); !errors.Is(err, ErrInvalidRequest) {
		t.Fatalf("public Chat must reject responses audio, got %v", err)
	}
}

// B1 公开入口：Responses 公开 Chat 把 http(s) 文件 URL 发为 input_file.file_url。
func TestAuditFixPublicResponsesFileURL(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		io.WriteString(w, `{"id":"r","model":"m","status":"completed","output":[{"type":"message","content":[{"type":"output_text","text":"ok"}]}],"usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()
	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithProtocol(ProtoOpenAIResponses))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Chat(ctx, &ChatRequest{Model: "m", Messages: []Message{{
		Role:   RoleUser,
		Blocks: []Block{FileContent("a.pdf", "application/pdf", "https://example.com/a.pdf")},
	}}}); err != nil {
		t.Fatal(err)
	}
	items := got["input"].([]any)
	parts := items[0].(map[string]any)["content"].([]any)
	part := parts[0].(map[string]any)
	if part["file_url"] != "https://example.com/a.pdf" || part["type"] != "input_file" {
		t.Fatalf("file_url part = %v", part)
	}
}

// F08 公开入口：Anthropic 公开 Chat 将 text/plain 编码为官方 text source。
func TestAuditFixPublicAnthropicTextDocument(t *testing.T) {
	var got map[string]any
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		json.Unmarshal(body, &got)
		io.WriteString(w, `{"id":"m","model":"m","role":"assistant","content":[{"type":"text","text":"ok"}],"stop_reason":"end_turn","usage":{"input_tokens":1,"output_tokens":1}}`)
	}))
	defer srv.Close()
	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithProtocol(ProtoAnthropic))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Chat(ctx, &ChatRequest{Model: "m", Messages: []Message{{
		Role:   RoleUser,
		Blocks: []Block{FileContent("a.txt", "text/plain", "SGVsbG8=")},
	}}}); err != nil {
		t.Fatal(err)
	}
	blocks := got["messages"].([]any)[0].(map[string]any)["content"].([]any)
	doc := blocks[0].(map[string]any)
	src := doc["source"].(map[string]any)
	if src["type"] != "text" || src["data"] != "Hello" {
		t.Fatalf("text source = %v", src)
	}
}

// F12 公开入口：Responses 真实 chunked 断流同样报 ErrStreamTruncated。
func TestAuditFixHTTPTruncationResponses(t *testing.T) {
	body := "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(body), body)
		rw.Flush()
	}))
	defer srv.Close()
	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithProtocol(ProtoOpenAIResponses))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := c.ChatStream(ctx, &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ends, sawText := 0, false
	for s.Next() {
		switch ev := s.Event(); ev.Type {
		case EventTextDelta:
			sawText = ev.Text == "partial"
		case EventMessageEnd:
			ends++
		}
	}
	if !errors.Is(s.Err(), ErrStreamTruncated) {
		t.Fatalf("err = %v, want ErrStreamTruncated", s.Err())
	}
	if ends != 1 || !sawText {
		t.Fatalf("ends=%d sawText=%v", ends, sawText)
	}
}

// F12 公开入口：Anthropic 真实 chunked 断流保留已收到的 usage。
func TestAuditFixHTTPTruncationAnthropic(t *testing.T) {
	body := "data: {\"type\":\"message_start\",\"message\":{\"id\":\"r\",\"model\":\"m\",\"usage\":{\"input_tokens\":5,\"output_tokens\":1}}}\n\n" +
		"data: {\"type\":\"content_block_delta\",\"delta\":{\"type\":\"text_delta\",\"text\":\"partial\"}}\n\n"
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, rw, err := w.(http.Hijacker).Hijack()
		if err != nil {
			return
		}
		defer conn.Close()
		fmt.Fprintf(rw, "HTTP/1.1 200 OK\r\nContent-Type: text/event-stream\r\nTransfer-Encoding: chunked\r\n\r\n%x\r\n%s\r\n", len(body), body)
		rw.Flush()
	}))
	defer srv.Close()
	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"), WithProtocol(ProtoAnthropic))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	s, err := c.ChatStream(ctx, &ChatRequest{Model: "m", Messages: []Message{User("hi")}})
	if err != nil {
		t.Fatal(err)
	}
	defer s.Close()
	ends := 0
	for s.Next() {
		if s.Event().Type == EventMessageEnd {
			ends++
		}
	}
	if !errors.Is(s.Err(), ErrStreamTruncated) {
		t.Fatalf("err = %v, want ErrStreamTruncated", s.Err())
	}
	if ends != 1 {
		t.Fatalf("end events = %d, want 1", ends)
	}
	if u := s.Usage(); u.InputTokens != 5 || u.OutputTokens != 1 {
		t.Fatalf("usage = %+v, want 5/1", u)
	}
}

// F05 公开入口：Embed/Rerank 公开调用拒绝结构异常的响应。
func TestAuditFixPublicAuxWireValidation(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, "/embeddings") {
			io.WriteString(w, `{"data":[{"index":null,"embedding":[1,2]}]}`)
			return
		}
		io.WriteString(w, `{"results":[null]}`)
	}))
	defer srv.Close()
	c, err := NewClient(WithEndpoint(srv.URL), WithAPIKey("k"))
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if _, err := c.Embed(ctx, &EmbeddingRequest{Model: "m", Input: []string{"x"}}); err == nil || !strings.Contains(err.Error(), "missing its index") {
		t.Fatalf("public Embed err = %v", err)
	}
	if _, err := c.Rerank(ctx, &RerankRequest{Model: "m", Query: "q", Documents: []string{"d"}}); err == nil || !strings.Contains(err.Error(), "result 0 is null") {
		t.Fatalf("public Rerank err = %v", err)
	}
}
