package rosetta

// Cross-release performance-comparison harness. Every benchmark here
// compiles unchanged against multiple released tags (it sticks to
// functions whose signatures are stable across releases), so the same
// code can be measured on the previous tag and the current one:
//
//	git worktree add /tmp/rosetta-old <old-tag>
//	cp bench_release_cmp_test.go /tmp/rosetta-old/
//	cd /tmp/rosetta-old && go test -run '^$' -bench . -count 6 > old.txt
//	go test -run '^$' -bench . -count 6 > new.txt
//	benchstat old.txt new.txt
//
// It is not part of the published API surface; it exists to keep release
// comparisons honest (identical benchmark code on both sides).

import (
	"bytes"
	"context"
	"encoding/base64"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/cn-maul/rosetta/internal/sse"
)

// ---------- SSE event scanning ----------

func benchChunkSSE(n int) []byte {
	var b bytes.Buffer
	for i := 0; i < n; i++ {
		fmt.Fprintf(&b, "data: {\"id\":\"chatcmpl-b\",\"model\":\"gpt-4o\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"你好，世界 hello %d\"}}]}\n\n", i)
	}
	return b.Bytes()
}

func BenchmarkSSEScan200Events(b *testing.B) {
	body := benchChunkSSE(200)
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		sc := sse.New(bytes.NewReader(body))
		n := 0
		for {
			if _, err := sc.Next(); err != nil {
				break
			}
			n++
		}
		if n != 200 {
			b.Fatalf("scanned %d events, want 200", n)
		}
	}
}

// ---------- base64 media validation ----------

func BenchmarkIsBase64_1MiB(b *testing.B) {
	raw := bytes.Repeat([]byte{0xA5, 0x3C, 0x7E, 0x11, 0xC3, 0x09, 0x50, 0xFF}, 1<<17) // 1 MiB
	s := base64.StdEncoding.EncodeToString(raw)
	b.SetBytes(int64(len(s)))
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		if !isBase64(s) {
			b.Fatal("expected valid base64")
		}
	}
}

// ---------- context estimation ----------

func benchLongRequest() *ChatRequest {
	msgs := []Message{System("你是一个严谨的编程助手，回答时注意边界条件与错误处理。")}
	for i := 0; i < 20; i++ {
		msgs = append(msgs,
			User(fmt.Sprintf("第 %d 轮：请解释这段代码的执行顺序，注意 goroutine 泄漏与 context 取消路径 %d", i, i)),
			Assistant("这段代码首先启动一个 worker goroutine，然后通过 channel 收集结果；在错误路径上它会取消 context 并返回包装后的 sentinel error，调用方可以用 errors.Is 区分。"),
		)
	}
	msgs = append(msgs, User("综合以上对话给出结论"))
	return &ChatRequest{Model: "gpt-4o", Messages: msgs}
}

func BenchmarkEstimateInputTokens_20Turns(b *testing.B) {
	req := benchLongRequest()
	b.ReportAllocs()
	for i := 0; i < b.N; i++ {
		_ = req.estimateInputTokens(MultimediaTokenEstimates{})
	}
}

// ---------- end-to-end unary round trip (real loopback HTTP) ----------

func benchUnaryServer() *httptest.Server {
	body := []byte(`{"id":"chatcmpl-b","model":"gpt-4o","choices":[{"index":0,"message":{"role":"assistant","content":"ok"},"finish_reason":"stop"}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
	}))
}

func benchClient(b *testing.B, url string) *Client {
	c, err := NewClient(WithEndpoint(url), WithAPIKey("sk-bench"))
	if err != nil {
		b.Fatal(err)
	}
	return c
}

func BenchmarkChatUnaryRTT(b *testing.B) {
	srv := benchUnaryServer()
	defer srv.Close()
	c := benchClient(b, srv.URL)
	req := &ChatRequest{Model: "gpt-4o", Messages: []Message{User("hi")}}
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		if _, err := c.Chat(context.Background(), req); err != nil {
			b.Fatal(err)
		}
	}
}

// Parallel is where the default transport's connection pool matters:
// MaxIdleConnsPerHost below GOMAXPROCS forces constant dial/close churn.
func BenchmarkChatUnaryRTTParallel(b *testing.B) {
	srv := benchUnaryServer()
	defer srv.Close()
	c := benchClient(b, srv.URL)
	b.ReportAllocs()
	b.ResetTimer()
	b.RunParallel(func(pb *testing.PB) {
		// One request value per goroutine: validate() memoizes state on it.
		req := &ChatRequest{Model: "gpt-4o", Messages: []Message{User("hi")}}
		for pb.Next() {
			if _, err := c.Chat(context.Background(), req); err != nil {
				b.Fatal(err)
			}
		}
	})
}

// ---------- end-to-end streaming round trip ----------

func benchStreamServer(n int) *httptest.Server {
	body := benchChunkSSE(n)
	body = append(body, []byte(`data: {"id":"chatcmpl-b","model":"gpt-4o","choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`+"\n\n")...)
	body = append(body, []byte(`data: {"id":"chatcmpl-b","model":"gpt-4o","choices":[{"index":0,"delta":{}}],"usage":{"prompt_tokens":10,"completion_tokens":5,"total_tokens":15}}`+"\n\n")...)
	body = append(body, []byte("data: [DONE]\n\n")...)
	return httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write(body)
	}))
}

func BenchmarkChatStream200Events(b *testing.B) {
	srv := benchStreamServer(200)
	defer srv.Close()
	c := benchClient(b, srv.URL)
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		req := &ChatRequest{Model: "gpt-4o", Messages: []Message{User("hi")}}
		st, err := c.ChatStream(context.Background(), req)
		if err != nil {
			b.Fatal(err)
		}
		if _, err := st.Collect(); err != nil {
			b.Fatal(err)
		}
	}
}
